package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ExternalIssueSyncService applies a normalized remote issue to Multica. It is
// the single, atomic entry point shared by the manual-import worker and (later)
// the webhook path, so a manual import racing a webhook can never create two
// Multica issues for one remote issue, nor leave an orphaned issue with no link.
//
// It deliberately reuses IssueService for the create/update building blocks
// rather than opening a raw-SQL side door: creation goes through the same row
// shape as IssueService.Create, and the update path publishes the same
// issue:updated snapshot the rest of the app consumes.
type ExternalIssueSyncService struct {
	Queries   *db.Queries
	TxStarter TxStarter
	Bus       *events.Bus
	Issues    *IssueService
	// Entitlements supplies the workspace issue-count policy so an import
	// respects the same quota as any other create. Nil is the self-hosted
	// unlimited path.
	Entitlements entitlement.Provider
}

func NewExternalIssueSyncService(q *db.Queries, tx TxStarter, bus *events.Bus, issues *IssueService) *ExternalIssueSyncService {
	svc := &ExternalIssueSyncService{Queries: q, TxStarter: tx, Bus: bus, Issues: issues}
	if issues != nil {
		svc.Entitlements = issues.Entitlements
	}
	return svc
}

// IsQuotaError reports whether err is the workspace issue-limit error, so the
// worker can move a run to quota_blocked (preserving cursor) instead of marking
// every remaining issue as failed.
func IsQuotaError(err error) bool {
	var e *IssueLimitReachedError
	return errors.As(err, &e)
}

// ApplyOutcome is what Apply did with one remote issue, for run counters.
type ApplyOutcome int

const (
	OutcomeImported ApplyOutcome = iota
	OutcomeUpdated
	OutcomeConflict
	OutcomeSkipped // no local change needed, or a tombstoned/ignored issue
	OutcomeFailed
)

// RemoteIssue is the provider-neutral input to Apply. The worker maps
// externalissue.Issue onto this so this service does not import the provider
// package (keeping the dependency direction service <- worker).
type RemoteIssue struct {
	ExternalID  string
	Provider    string
	InstanceKey string
	Number      int64
	Title       string
	Body        string
	State       string // "open" | "closed"
	HTMLURL     string
	UpdatedAt   time.Time // remote update timestamp; zero if unknown
}

// ApplyParams carries the source binding for one Apply call.
type ApplyParams struct {
	WorkspaceID pgtype.UUID
	SourceID    pgtype.UUID
	ProjectID   pgtype.UUID // target project; may be invalid
	CreatorID   pgtype.UUID // the source's configured_by member; may be invalid
	Remote      RemoteIssue
	// RunID + WorkerID fence the Apply transaction to the owning run: when RunID
	// is valid, Apply locks the run FOR UPDATE requiring this worker still owns a
	// live, non-cancelled lease, and records the outcome in the run-item ledger
	// (keyed on the remote issue's stable id) in the SAME transaction. A stolen
	// lease or a cancellation therefore rolls the whole create/update back —
	// nothing is written — instead of drifting to the next heartbeat.
	RunID    pgtype.UUID
	WorkerID string
}

// ErrRunFenced means the owning run's lease was lost or it was cancelled between
// claim and this Apply, so the transaction was rolled back without any write.
// The worker treats it as "stop this claim", not a per-issue failure.
var ErrRunFenced = errors.New("external-issue sync run fence lost")

// ContentHash is the canonical baseline hash for external-issue content fields
// (title/body): sha256 hex. The sync applier and the conflict-resolution handler
// must agree on it so a resume that advances the baseline to the local value
// matches what the next sync computes.
func ContentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func contentHash(s string) string {
	return ContentHash(s)
}

// Apply upserts one remote issue into Multica in a single transaction and
// returns what it did. The published issue:updated / issue:created event, if
// any, is emitted after commit so subscribers never observe an uncommitted row.
func (s *ExternalIssueSyncService) Apply(ctx context.Context, p ApplyParams) (ApplyOutcome, error) {
	r := p.Remote
	if r.Title == "" {
		return OutcomeFailed, errors.New("remote issue has empty title")
	}

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return OutcomeFailed, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	// Fence on run ownership INSIDE this transaction. If the lease was stolen or
	// the run cancelled, the lock returns no row and we abort before creating
	// anything — the create/update and its accounting are all-or-nothing with the
	// ownership check.
	if p.RunID.Valid {
		if _, err := qtx.LockExternalIssueSyncRunForApply(ctx, db.LockExternalIssueSyncRunForApplyParams{
			ID: p.RunID, WorkerID: p.WorkerID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return OutcomeFailed, ErrRunFenced
			}
			return OutcomeFailed, fmt.Errorf("fence run: %w", err)
		}
	}

	var remoteUpdated pgtype.Timestamptz
	if !r.UpdatedAt.IsZero() {
		remoteUpdated = pgtype.Timestamptz{Time: r.UpdatedAt, Valid: true}
	}

	// Claim the remote issue on stable identity. Only the true inserter (xmax=0)
	// proceeds to create a Multica issue; a concurrent claimer reads the
	// existing binding and falls through to the update / ignored path.
	link, err := qtx.ClaimExternalIssueLink(ctx, db.ClaimExternalIssueLinkParams{
		WorkspaceID:     p.WorkspaceID,
		Provider:        r.Provider,
		InstanceKey:     r.InstanceKey,
		ExternalIssueID: r.ExternalID,
		SourceID:        p.SourceID,
		DisplayNumber:   r.Number,
		ExternalHtmlUrl: r.HTMLURL,
		RemoteState:     r.State,
		RemoteUpdatedAt: remoteUpdated,
	})
	if err != nil {
		return OutcomeFailed, fmt.Errorf("claim link: %w", err)
	}

	// A tombstoned link (locally deleted) must not be resurrected by sync.
	if link.LocalDeletedAt.Valid {
		if err := s.recordLedger(ctx, qtx, p, "skipped"); err != nil {
			return OutcomeFailed, err
		}
		if err := tx.Commit(ctx); err != nil {
			return OutcomeFailed, fmt.Errorf("commit tombstone no-op: %w", err)
		}
		return OutcomeSkipped, nil
	}

	if link.Inserted {
		outcome, issue, ws, err := s.createBoundIssue(ctx, tx, qtx, p, link)
		if err != nil {
			return OutcomeFailed, err
		}
		if err := s.recordLedger(ctx, qtx, p, ledgerOutcome(outcome)); err != nil {
			return OutcomeFailed, err
		}
		if err := tx.Commit(ctx); err != nil {
			return OutcomeFailed, fmt.Errorf("commit create: %w", err)
		}
		s.publishCreated(ctx, issue, ws, p.CreatorID)
		return outcome, nil
	}

	// Existing binding: this is the update path (or a no-op / conflict).
	if !link.IssueID.Valid {
		// Claimed by a concurrent create that has not yet bound its issue, or a
		// prior create that failed after claiming. Leave it for the next pass.
		if err := s.recordLedger(ctx, qtx, p, "skipped"); err != nil {
			return OutcomeFailed, err
		}
		if err := tx.Commit(ctx); err != nil {
			return OutcomeFailed, fmt.Errorf("commit unbound no-op: %w", err)
		}
		return OutcomeSkipped, nil
	}
	outcome, issue, ws, changed, err := s.updateBoundIssue(ctx, qtx, p, link)
	if err != nil {
		return OutcomeFailed, err
	}
	if err := s.recordLedger(ctx, qtx, p, ledgerOutcome(outcome)); err != nil {
		return OutcomeFailed, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OutcomeFailed, fmt.Errorf("commit update: %w", err)
	}
	if changed {
		s.publishUpdated(ctx, issue, ws)
	}
	return outcome, nil
}

// recordLedger upserts the (run, remote issue) ledger row inside the Apply tx so
// run counts are derived from stable identity, not a page position. A no-op when
// there is no run (e.g. the direct-import path in tests).
func (s *ExternalIssueSyncService) recordLedger(ctx context.Context, qtx *db.Queries, p ApplyParams, outcome string) error {
	if !p.RunID.Valid {
		return nil
	}
	if _, err := qtx.UpsertExternalIssueSyncRunItem(ctx, db.UpsertExternalIssueSyncRunItemParams{
		RunID:           p.RunID,
		WorkspaceID:     p.WorkspaceID,
		ExternalIssueID: p.Remote.ExternalID,
		Outcome:         outcome,
	}); err != nil {
		return fmt.Errorf("record ledger item: %w", err)
	}
	return nil
}

// ledgerOutcome maps an ApplyOutcome to its ledger string.
func ledgerOutcome(o ApplyOutcome) string {
	switch o {
	case OutcomeImported:
		return "imported"
	case OutcomeUpdated:
		return "updated"
	case OutcomeConflict:
		return "conflict"
	case OutcomeSkipped:
		return "skipped"
	default:
		return "failed"
	}
}

// createBoundIssue creates a Multica issue for a freshly claimed link and binds
// it, all inside the caller's transaction. New issues land in backlog with no
// assignee (the P0 default) — the service does NOT infer a default status, so we
// pass it explicitly.
func (s *ExternalIssueSyncService) createBoundIssue(ctx context.Context, tx pgx.Tx, qtx *db.Queries, p ApplyParams, link db.ClaimExternalIssueLinkRow) (ApplyOutcome, db.Issue, db.Workspace, error) {
	r := p.Remote
	const status = "backlog"

	if p.ProjectID.Valid {
		if _, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
			ID: p.ProjectID, WorkspaceID: p.WorkspaceID,
		}); err != nil {
			return OutcomeFailed, db.Issue{}, db.Workspace{}, ErrProjectNotFound
		}
	}

	// Reuse the shared issue-create core (numbering with the workspace count
	// policy, top-of-column position, row insert) instead of a second raw
	// CreateIssue path. Imported issues land in backlog/none with no assignee.
	issue, err := createIssueRowInTx(ctx, tx, qtx, issueRowInput{
		WorkspaceID: p.WorkspaceID,
		Title:       r.Title,
		Description: pgtype.Text{String: r.Body, Valid: true},
		Status:      status,
		Priority:    "none",
		CreatorType: "member",
		CreatorID:   p.CreatorID,
		ProjectID:   p.ProjectID,
		CountPolicy: ResolveIssueCountPolicy(ctx, s.Entitlements, p.WorkspaceID),
	})
	if err != nil {
		return OutcomeFailed, db.Issue{}, db.Workspace{}, err
	}

	var remoteUpdated pgtype.Timestamptz
	if !r.UpdatedAt.IsZero() {
		remoteUpdated = pgtype.Timestamptz{Time: r.UpdatedAt, Valid: true}
	}
	if err := qtx.BindExternalIssueLinkIssue(ctx, db.BindExternalIssueLinkIssueParams{
		ID:                link.ID,
		IssueID:           issue.ID,
		SourceID:          p.SourceID,
		DisplayNumber:     r.Number,
		ExternalHtmlUrl:   r.HTMLURL,
		RemoteState:       r.State,
		RemoteUpdatedAt:   remoteUpdated,
		TitleBaselineHash: contentHash(r.Title),
		BodyBaselineHash:  contentHash(r.Body),
	}); err != nil {
		return OutcomeFailed, db.Issue{}, db.Workspace{}, fmt.Errorf("bind link: %w", err)
	}

	ws, err := qtx.GetWorkspace(ctx, p.WorkspaceID)
	if err != nil {
		return OutcomeFailed, db.Issue{}, db.Workspace{}, fmt.Errorf("load workspace: %w", err)
	}
	return OutcomeImported, issue, ws, nil
}

// updateBoundIssue applies remote title/body changes to an already-linked issue
// with per-field three-way conflict detection, inside the caller's transaction.
// It returns changed=true only when the issue row actually moved, so the caller
// publishes issue:updated exactly when there is something to broadcast.
func (s *ExternalIssueSyncService) updateBoundIssue(ctx context.Context, qtx *db.Queries, p ApplyParams, link db.ClaimExternalIssueLinkRow) (ApplyOutcome, db.Issue, db.Workspace, bool, error) {
	r := p.Remote

	// Reject an out-of-order event: never let an older remote snapshot regress
	// newer content already recorded on the link.
	if link.RemoteUpdatedAt.Valid && !r.UpdatedAt.IsZero() && r.UpdatedAt.Before(link.RemoteUpdatedAt.Time) {
		return OutcomeSkipped, db.Issue{}, db.Workspace{}, false, nil
	}

	// Lock the issue row FOR UPDATE before reading it, so the conflict decision
	// and the write see a consistent row: a concurrent local edit either
	// commits before this lock (and is seen as a local change -> conflict) or
	// blocks until this tx commits. Reading without the lock let a local edit
	// landing between read and write be silently overwritten.
	issue, err := qtx.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{
		ID: link.IssueID, WorkspaceID: p.WorkspaceID,
	})
	if err != nil {
		return OutcomeFailed, db.Issue{}, db.Workspace{}, false, fmt.Errorf("lock linked issue: %w", err)
	}

	newTitle, titleConflict, titleOwned := resolveField(
		issue.Title, r.Title, link.TitleBaselineHash, link.TitleLocalOwned)
	newBody, bodyConflict, bodyOwned := resolveField(
		issue.Description.String, r.Body, link.BodyBaselineHash, link.BodyLocalOwned)

	changed := newTitle != issue.Title || newBody != issue.Description.String
	outcome := OutcomeUpdated
	if titleConflict || bodyConflict {
		outcome = OutcomeConflict
	} else if !changed {
		outcome = OutcomeSkipped
	}

	if changed {
		// Reuse the shared, transaction-aware content update core so an import
		// update goes through the same primitive as a Public API edit, inside this
		// applier's transaction. No ExpectedRevision: the FOR UPDATE lock above
		// already serializes against concurrent local edits.
		updated, err := s.Issues.UpdateContentInTx(ctx, qtx, issue, IssueContentPatch{
			Title:       &newTitle,
			Description: &newBody,
		})
		if err != nil {
			return OutcomeFailed, db.Issue{}, db.Workspace{}, false, fmt.Errorf("update issue: %w", err)
		}
		issue = updated
	}

	// Advance the baseline to the remote content for any field we accepted; a
	// conflicted or local-owned field keeps its old baseline so the same
	// divergence is not re-reported every pass.
	titleBaseline := link.TitleBaselineHash
	if !titleConflict && !titleOwned {
		titleBaseline = contentHash(r.Title)
	}
	bodyBaseline := link.BodyBaselineHash
	if !bodyConflict && !bodyOwned {
		bodyBaseline = contentHash(r.Body)
	}

	var remoteUpdated pgtype.Timestamptz
	if !r.UpdatedAt.IsZero() {
		remoteUpdated = pgtype.Timestamptz{Time: r.UpdatedAt, Valid: true}
	}
	if err := qtx.UpdateExternalIssueLinkSync(ctx, db.UpdateExternalIssueLinkSyncParams{
		ID:                link.ID,
		DisplayNumber:     r.Number,
		ExternalHtmlUrl:   r.HTMLURL,
		RemoteState:       r.State,
		RemoteUpdatedAt:   remoteUpdated,
		TitleBaselineHash: titleBaseline,
		BodyBaselineHash:  bodyBaseline,
		TitleConflict:     titleConflict,
		BodyConflict:      bodyConflict,
		TitleLocalOwned:   titleOwned,
		BodyLocalOwned:    bodyOwned,
	}); err != nil {
		return OutcomeFailed, db.Issue{}, db.Workspace{}, false, fmt.Errorf("update link sync: %w", err)
	}

	ws, err := qtx.GetWorkspace(ctx, p.WorkspaceID)
	if err != nil {
		return OutcomeFailed, db.Issue{}, db.Workspace{}, false, fmt.Errorf("load workspace: %w", err)
	}
	return outcome, issue, ws, changed, nil
}

// resolveField implements the per-field three-way merge for one text field:
//
//   - once a field is local-owned (the user chose "keep local"), remote changes
//     never overwrite it — they are only surfaced, so we keep the local value
//     and re-mark ownership;
//   - only remote changed vs baseline  -> take remote;
//   - only local changed vs baseline   -> keep local, no conflict;
//   - both changed                     -> keep local, flag conflict;
//   - neither changed                  -> no-op.
//
// It returns the value to store, whether the field is in conflict, and whether
// the field should remain local-owned.
func resolveField(local, remote, baselineHash string, localOwned bool) (value string, conflict bool, owned bool) {
	if localOwned {
		return local, false, true
	}
	// If local and remote already agree there is nothing to reconcile — never
	// report a conflict when both sides converged on the same final value, even
	// if each diverged from the baseline independently.
	if local == remote {
		return local, false, false
	}
	localChanged := contentHash(local) != baselineHash
	remoteChanged := contentHash(remote) != baselineHash
	switch {
	case remoteChanged && !localChanged:
		return remote, false, false
	case localChanged && !remoteChanged:
		return local, false, false
	case localChanged && remoteChanged:
		return local, true, false
	default:
		return local, false, false
	}
}

func (s *ExternalIssueSyncService) publishCreated(ctx context.Context, issue db.Issue, ws db.Workspace, actorID pgtype.UUID) {
	if s.Bus == nil {
		return
	}
	s.Bus.Publish(events.Event{
		Type:        protocol.EventIssueCreated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "member",
		ActorID:     util.UUIDToString(actorID),
		Payload: map[string]any{
			"issue": IssueToMapResolved(ctx, s.Queries, issue, ws.IssuePrefix),
		},
	})
}

func (s *ExternalIssueSyncService) publishUpdated(ctx context.Context, issue db.Issue, ws db.Workspace) {
	if s.Bus == nil {
		return
	}
	s.Bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "member",
		ActorID:     "",
		Payload: map[string]any{
			"issue":            IssueToMapResolved(ctx, s.Queries, issue, ws.IssuePrefix),
			"assignee_changed": false,
			"status_changed":   false,
			"project_changed":  false,
		},
	})
}

// ErrRemoteStale means the link changed between the handler's pre-fetch read and
// acquiring the write lock (a concurrent sync advanced it), so the fetched remote
// snapshot may be stale. The caller re-fetches and retries, or surfaces a 409.
var ErrRemoteStale = errors.New("remote snapshot is stale; re-fetch and retry")

// UseRemoteFields overwrites the linked issue's title and/or body with the
// caller-provided remote content (fetched by the handler via the provider),
// through the shared transaction. Lock order is link -> issue (matching the sync
// applier) so a concurrent Apply and a "use remote" can never deadlock.
//
// linkToken is the link's updated_at as read BEFORE the remote fetch. After
// locking the link we verify it is unchanged: GitHub timestamps are second-
// granular, so a same-second remote update cannot be told apart by RemoteUpdatedAt
// alone — an optimistic token on the link row's own updated_at (bumped on every
// sync/resolve write) is what proves the fetched snapshot is still current. If it
// changed, we return ErrRemoteStale instead of overwriting with possibly-stale
// content.
func (s *ExternalIssueSyncService) UseRemoteFields(ctx context.Context, workspaceID pgtype.UUID, issueID pgtype.UUID, remoteTitle, remoteBody string, linkToken time.Time, applyTitle, applyBody bool) error {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	// Lock the link FIRST (link -> issue order), so this can never deadlock
	// against the sync applier which also acquires link before issue.
	link, err := qtx.LockExternalIssueLinkByIssue(ctx, db.LockExternalIssueLinkByIssueParams{
		WorkspaceID: workspaceID, IssueID: issueID,
	})
	if err != nil {
		return fmt.Errorf("lock link: %w", err)
	}
	// Optimistic-token stale guard: the link's updated_at is bumped by every sync
	// apply and conflict-resolution write. If it moved since the handler read it
	// (pre-fetch), a concurrent sync recorded newer remote content — even within
	// the same wall-clock second — so the fetched snapshot may be stale. Bail so
	// the caller re-fetches, rather than clobbering the newer content with older.
	if !linkToken.IsZero() && link.UpdatedAt.Valid && !link.UpdatedAt.Time.Equal(linkToken) {
		return ErrRemoteStale
	}
	issue, err := qtx.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{
		ID: issueID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return fmt.Errorf("lock issue: %w", err)
	}

	newTitle := issue.Title
	newBody := issue.Description.String
	if applyTitle {
		newTitle = remoteTitle
	}
	if applyBody {
		newBody = remoteBody
	}
	changed := newTitle != issue.Title || newBody != issue.Description.String
	if changed {
		// Reuse the shared, transaction-aware content update core (see
		// updateBoundIssue): the FOR UPDATE lock above serializes local edits, so
		// no ExpectedRevision is needed.
		updated, err := s.Issues.UpdateContentInTx(ctx, qtx, issue, IssueContentPatch{
			Title:       &newTitle,
			Description: &newBody,
		})
		if err != nil {
			return fmt.Errorf("update issue: %w", err)
		}
		issue = updated
	}

	// Advance baseline to the applied remote content and clear conflict +
	// ownership for the applied fields; leave untouched fields as-is.
	titleBaseline := link.TitleBaselineHash
	if applyTitle {
		titleBaseline = contentHash(newTitle)
	}
	bodyBaseline := link.BodyBaselineHash
	if applyBody {
		bodyBaseline = contentHash(newBody)
	}
	if err := qtx.UpdateExternalIssueLinkSync(ctx, db.UpdateExternalIssueLinkSyncParams{
		ID:                link.ID,
		DisplayNumber:     link.DisplayNumber,
		ExternalHtmlUrl:   link.ExternalHtmlUrl,
		RemoteState:       link.RemoteState,
		RemoteUpdatedAt:   link.RemoteUpdatedAt,
		TitleBaselineHash: titleBaseline,
		BodyBaselineHash:  bodyBaseline,
		TitleConflict:     link.TitleConflict && !applyTitle,
		BodyConflict:      link.BodyConflict && !applyBody,
		TitleLocalOwned:   link.TitleLocalOwned && !applyTitle,
		BodyLocalOwned:    link.BodyLocalOwned && !applyBody,
	}); err != nil {
		return fmt.Errorf("update link sync: %w", err)
	}

	ws, err := qtx.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit use-remote: %w", err)
	}
	if changed {
		s.publishUpdated(ctx, issue, ws)
	}
	return nil
}
