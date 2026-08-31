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
	"github.com/multica-ai/multica/server/internal/issueposition"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
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
}

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
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
		if err := tx.Commit(ctx); err != nil {
			return OutcomeFailed, fmt.Errorf("commit unbound no-op: %w", err)
		}
		return OutcomeSkipped, nil
	}
	outcome, issue, ws, changed, err := s.updateBoundIssue(ctx, qtx, p, link)
	if err != nil {
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

	number, err := AllocateIssueNumber(ctx, qtx, p.WorkspaceID, ResolveIssueCountPolicy(ctx, s.Entitlements, p.WorkspaceID))
	if err != nil {
		return OutcomeFailed, db.Issue{}, db.Workspace{}, fmt.Errorf("allocate issue number: %w", err)
	}
	position, err := issueposition.NextTopPosition(ctx, tx, p.WorkspaceID, status)
	if err != nil {
		return OutcomeFailed, db.Issue{}, db.Workspace{}, fmt.Errorf("next top position: %w", err)
	}

	creatorType := "member"
	issue, err := qtx.CreateIssue(ctx, db.CreateIssueParams{
		ID:          dbid.NewV7(),
		WorkspaceID: p.WorkspaceID,
		Title:       r.Title,
		Description: pgtype.Text{String: r.Body, Valid: true},
		Status:      status,
		Priority:    "none",
		CreatorType: creatorType,
		CreatorID:   p.CreatorID,
		Position:    position,
		Number:      number,
		ProjectID:   p.ProjectID,
	})
	if err != nil {
		return OutcomeFailed, db.Issue{}, db.Workspace{}, fmt.Errorf("create issue: %w", err)
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
		updated, err := qtx.UpdateIssue(ctx, db.UpdateIssueParams{
			ID:            issue.ID,
			Title:         pgtype.Text{String: newTitle, Valid: true},
			Description:   pgtype.Text{String: newBody, Valid: true},
			AssigneeType:  issue.AssigneeType,
			AssigneeID:    issue.AssigneeID,
			StartDate:     issue.StartDate,
			DueDate:       issue.DueDate,
			ParentIssueID: issue.ParentIssueID,
			ProjectID:     issue.ProjectID,
			Stage:         issue.Stage,
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
