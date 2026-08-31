package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

const (
	// syncWorkerPollInterval is the recovery-sweep cadence. Notify() wakes the
	// loop immediately after a run is enqueued locally; the ticker is the
	// backstop that reclaims runs enqueued on another replica or left behind by
	// a crashed owner whose lease expired.
	syncWorkerPollInterval = 15 * time.Second
	// syncRunLease bounds how long one claim may hold a run before another
	// worker can reclaim it. The worker refreshes the lease every page, so this
	// only fires when the owner actually died mid-page.
	syncRunLease = 2 * time.Minute
	// syncRunPagesPerClaim bounds how many pages one claim drains before it
	// yields the run back to the queue. This keeps a single huge repository from
	// monopolizing a worker and gives cancel/quota checks a natural cadence;
	// the cursor is persisted so the requeued run resumes exactly where it
	// stopped.
	syncRunPagesPerClaim = 10
	// syncRateLimitBackoff is the floor delay before a rate-limited run is
	// retried when the provider gives no explicit Retry-At.
	syncRateLimitBackoff = 60 * time.Second
)

// ExternalIssueSyncWorker drains queued external-issue backfill runs out of
// band. Claim + lease + cursor all live in Postgres (external_issue_sync_run),
// so a restart or replica failover simply reclaims expired runs and resumes
// from the persisted cursor — the in-memory Notify is only a latency hint. It
// mirrors WebhookDeliveryWorker's lifecycle.
type ExternalIssueSyncWorker struct {
	h      *Handler
	id     string
	notify chan struct{}
	done   chan struct{}
}

func NewExternalIssueSyncWorker(h *Handler) *ExternalIssueSyncWorker {
	return &ExternalIssueSyncWorker{
		h:      h,
		id:     "eis-" + util.UUIDToString(dbid.NewV7()),
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

func (w *ExternalIssueSyncWorker) Notify() {
	if w == nil {
		return
	}
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// Run owns the single-goroutine claim loop. SKIP LOCKED in the claim query lets
// multiple replicas run this safely; one worker per process is enough because a
// claim drains many pages.
func (w *ExternalIssueSyncWorker) Run(ctx context.Context) {
	if w == nil {
		return
	}
	defer close(w.done)
	if w.h == nil || w.h.Queries == nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.runLoop(ctx)
	}()
	wg.Wait()
}

func (w *ExternalIssueSyncWorker) runLoop(ctx context.Context) {
	ticker := time.NewTicker(syncWorkerPollInterval)
	defer ticker.Stop()
	for {
		worked, err := w.ProcessNext(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("external-issue sync worker: process run", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-w.notify:
		case <-ticker.C:
		}
	}
}

// WaitWithTimeout reports whether the worker fully stopped within timeout.
func (w *ExternalIssueSyncWorker) WaitWithTimeout(timeout time.Duration) bool {
	if w == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.done:
		return true
	case <-timer.C:
		return false
	}
}

// ProcessNext claims one due run and drains up to syncRunPagesPerClaim pages.
// It returns worked=true when it advanced a run, so the loop keeps pulling.
// Exported so tests drive the durable queue synchronously.
func (w *ExternalIssueSyncWorker) ProcessNext(ctx context.Context) (bool, error) {
	lease := pgtype.Timestamptz{Time: time.Now().Add(syncRunLease), Valid: true}
	run, err := w.h.Queries.ClaimNextExternalIssueSyncRun(ctx, db.ClaimNextExternalIssueSyncRunParams{
		WorkerID:       w.id,
		LeaseExpiresAt: lease,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim run: %w", err)
	}
	if err := w.drainRun(ctx, run); err != nil {
		if errors.Is(err, errLeaseLost) {
			// Another worker reclaimed this run; that's expected under failover,
			// not a processing failure. Keep pulling.
			return true, nil
		}
		return true, err
	}
	return true, nil
}

// drainRun advances one claimed run: honor cancel, mint a token, and apply up to
// syncRunPagesPerClaim pages, persisting the cursor and counts after each page.
func (w *ExternalIssueSyncWorker) drainRun(ctx context.Context, run db.ExternalIssueSyncRun) error {
	if run.CancelRequested {
		return w.finishRun(ctx, run, "cancelled", "")
	}

	// Read execution inputs from the run's immutable snapshot, not the live
	// (mutable) source: a second import request can change the source's
	// credential / target project / filter mid-run, and re-reading it here would
	// silently redirect an in-flight import. The snapshot is captured at enqueue.
	snap, err := decodeRunInput(run.InputSnapshot)
	if err != nil || !snap.CredentialID.Valid {
		return w.finishRun(ctx, run, "failed", "run input snapshot missing or invalid")
	}

	// Resolve installation -> fresh issues:read token for this claim. Tokens are
	// short-lived, so we never persist them; each claim mints its own.
	installation, err := w.h.Queries.GetGitHubInstallationByID(ctx, snap.CredentialID)
	if err != nil || installation.WorkspaceID != run.WorkspaceID {
		return w.finishRun(ctx, run, "needs_reauth", "installation unavailable")
	}
	token, revoke, err := mintGitHubIssuesReadToken(ctx, installation.InstallationID)
	if err != nil {
		if errors.Is(err, errGitHubIssuesPermission) {
			return w.finishRun(ctx, run, "needs_reauth", "installation has not granted Issues read")
		}
		return w.requeueRun(ctx, run, syncRateLimitBackoff)
	}
	defer revoke()

	provider, ok := externalissue.For(snap.Provider)
	if !ok {
		return w.finishRun(ctx, run, "failed", "provider unavailable")
	}
	creds := externalissue.Credentials{Token: token}
	repo := externalissue.Repository{
		InstanceKey: snap.InstanceKey,
		ExternalID:  snap.RepositoryExternalID,
		FullPath:    snap.RepositoryFullPath,
	}
	filter := externalissue.IssueFilter{State: snap.State}

	counts := runCounts{
		imported:  run.ImportedCount,
		updated:   run.UpdatedCount,
		conflicts: run.ConflictCount,
		skipped:   run.SkippedCount,
		failed:    run.FailedCount,
		total:     run.TotalSeen,
	}
	cursor := externalissue.Cursor(run.Cursor)
	sawError := false

	for page := 0; page < syncRunPagesPerClaim; page++ {
		// Re-check cancellation between pages so a cancel lands promptly.
		if fresh, err := w.h.Queries.GetExternalIssueSyncRun(ctx, db.GetExternalIssueSyncRunParams{
			ID: run.ID, WorkspaceID: run.WorkspaceID,
		}); err == nil && fresh.CancelRequested {
			// Checkpoint must commit before we finalize; if the lease was
			// stolen, stop without finishing.
			if err := w.advance(ctx, run, cursor, counts); err != nil {
				return err
			}
			return w.finishRun(ctx, run, "cancelled", "")
		}

		result, listErr := provider.ListIssues(ctx, creds, repo, filter, cursor)
		if listErr != nil {
			return w.handleListError(ctx, run, listErr)
		}
		for _, iss := range result.Issues {
			counts.total++
			outcome, applyErr := w.h.ExternalIssueSync.Apply(ctx, service.ApplyParams{
				WorkspaceID: run.WorkspaceID,
				SourceID:    run.SourceID,
				ProjectID:   snap.TargetProjectID,
				CreatorID:   snap.ConfiguredByUserID,
				Remote:      toRemoteIssue(iss, repo),
			})
			if applyErr != nil {
				if service.IsQuotaError(applyErr) {
					// Persist progress up to this page's start BEFORE finalizing,
					// so a resume continues from a committed cursor. If the
					// checkpoint fails (lost lease), do not finalize.
					if err := w.advance(ctx, run, cursor, counts); err != nil {
						return err
					}
					return w.finishRun(ctx, run, "quota_blocked", "workspace issue quota reached")
				}
				counts.failed++
				sawError = true
				continue
			}
			counts.tally(outcome)
		}
		cursor = result.NextCursor
		// Checkpoint the page. A failed checkpoint (lost lease or DB error) MUST
		// abort: continuing would let the issues we just applied be reprocessed
		// (or the run be marked succeeded with an unsaved cursor).
		if err := w.advance(ctx, run, cursor, counts); err != nil {
			return err
		}
		if cursor == "" {
			state := "succeeded"
			if sawError {
				state = "partial"
			}
			return w.finishRun(ctx, run, state, "")
		}
	}

	// Hit the per-claim page budget with more to do: yield the run back to the
	// queue so cancel/other runs get a turn. It resumes from the saved cursor.
	return w.requeueRun(ctx, run, 0)
}

// runInputSnapshot is the immutable execution input captured on the run at
// enqueue time (see migration 453). The worker reads this instead of the live
// source so a mid-run source mutation cannot redirect the import.
type runInputSnapshot struct {
	CredentialID          pgtype.UUID `json:"-"`
	CredentialIDStr       string      `json:"credential_id"`
	Provider              string      `json:"provider"`
	InstanceKey           string      `json:"instance_key"`
	RepositoryExternalID  string      `json:"repository_external_id"`
	RepositoryFullPath    string      `json:"repository_full_path"`
	TargetProjectID       pgtype.UUID `json:"-"`
	TargetProjectIDStr    string      `json:"target_project_id"`
	ConfiguredByUserID    pgtype.UUID `json:"-"`
	ConfiguredByUserIDStr string      `json:"configured_by_user_id"`
	State                 string      `json:"state"`
}

func decodeRunInput(raw []byte) (runInputSnapshot, error) {
	var s runInputSnapshot
	if len(raw) == 0 {
		return s, errors.New("empty snapshot")
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	if s.CredentialIDStr != "" {
		if u, err := util.ParseUUID(s.CredentialIDStr); err == nil {
			s.CredentialID = u
		}
	}
	if s.TargetProjectIDStr != "" {
		if u, err := util.ParseUUID(s.TargetProjectIDStr); err == nil {
			s.TargetProjectID = u
		}
	}
	if s.ConfiguredByUserIDStr != "" {
		if u, err := util.ParseUUID(s.ConfiguredByUserIDStr); err == nil {
			s.ConfiguredByUserID = u
		}
	}
	if s.State != "open" && s.State != "closed" && s.State != "all" {
		s.State = "open"
	}
	return s, nil
}

type runCounts struct {
	imported, updated, conflicts, skipped, failed, total int64
}

func (c *runCounts) tally(o service.ApplyOutcome) {
	switch o {
	case service.OutcomeImported:
		c.imported++
	case service.OutcomeUpdated:
		c.updated++
	case service.OutcomeConflict:
		c.conflicts++
	case service.OutcomeSkipped:
		c.skipped++
	default:
		c.failed++
	}
}

// errLeaseLost signals the run's lease was reclaimed by another worker between
// claim and this write. The caller stops touching the run; the new owner drives
// it. It is not surfaced as a processing error.
var errLeaseLost = errors.New("external-issue sync run lease lost")

// advance persists the cursor + counts and refreshes the lease, fenced on this
// worker's id. A zero-row update means the lease was stolen (or the run was
// cancelled/finished elsewhere): return errLeaseLost so the caller aborts
// WITHOUT finalizing — never mark succeeded on an unsaved cursor.
func (w *ExternalIssueSyncWorker) advance(ctx context.Context, run db.ExternalIssueSyncRun, cursor externalissue.Cursor, c runCounts) error {
	lease := pgtype.Timestamptz{Time: time.Now().Add(syncRunLease), Valid: true}
	rows, err := w.h.Queries.AdvanceExternalIssueSyncRun(ctx, db.AdvanceExternalIssueSyncRunParams{
		ID:             run.ID,
		WorkerID:       w.id,
		Cursor:         string(cursor),
		ImportedCount:  c.imported,
		UpdatedCount:   c.updated,
		ConflictCount:  c.conflicts,
		SkippedCount:   c.skipped,
		FailedCount:    c.failed,
		TotalSeen:      c.total,
		LeaseExpiresAt: lease,
	})
	if err != nil {
		return fmt.Errorf("advance run: %w", err)
	}
	if rows == 0 {
		slog.Warn("external-issue sync worker: lease lost on advance", "run_id", util.UUIDToString(run.ID))
		return errLeaseLost
	}
	return nil
}

// handleListError maps a provider list failure to the right run outcome: a
// rate-limit requeues with the provider's Retry-At; a transient error requeues
// with a short backoff; anything else fails the run.
func (w *ExternalIssueSyncWorker) handleListError(ctx context.Context, run db.ExternalIssueSyncRun, listErr error) error {
	var e *externalissue.Error
	if errors.As(listErr, &e) {
		switch e.Kind {
		case externalissue.ErrRateLimited:
			delay := syncRateLimitBackoff
			if !e.RetryAt.IsZero() {
				if d := time.Until(e.RetryAt); d > 0 {
					delay = d
				}
			}
			return w.requeueRun(ctx, run, delay)
		case externalissue.ErrTransient:
			return w.requeueRun(ctx, run, syncRateLimitBackoff)
		case externalissue.ErrUnauthorized, externalissue.ErrForbidden:
			return w.finishRun(ctx, run, "needs_reauth", e.Error())
		}
	}
	return w.finishRun(ctx, run, "failed", listErr.Error())
}

// finishRun finalizes a run, fenced on this worker's id. A lost lease is a
// no-op (the new owner owns the outcome), not an error.
func (w *ExternalIssueSyncWorker) finishRun(ctx context.Context, run db.ExternalIssueSyncRun, state, errMsg string) error {
	sample := []byte("[]")
	if errMsg != "" {
		if b, err := json.Marshal([]string{errMsg}); err == nil {
			sample = b
		}
	}
	rows, err := w.h.Queries.FinishExternalIssueSyncRun(ctx, db.FinishExternalIssueSyncRunParams{
		ID: run.ID, WorkerID: w.id, State: state, ErrorSample: sample,
	})
	if err != nil {
		return fmt.Errorf("finish run %s: %w", state, err)
	}
	if rows == 0 {
		slog.Warn("external-issue sync worker: lease lost on finish", "run_id", util.UUIDToString(run.ID))
	}
	return nil
}

// requeueRun returns the run to the queue, fenced on this worker's id.
func (w *ExternalIssueSyncWorker) requeueRun(ctx context.Context, run db.ExternalIssueSyncRun, delay time.Duration) error {
	next := pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true}
	rows, err := w.h.Queries.RequeueExternalIssueSyncRun(ctx, db.RequeueExternalIssueSyncRunParams{
		ID: run.ID, WorkerID: w.id, NextAttemptAt: next,
	})
	if err != nil {
		return fmt.Errorf("requeue run: %w", err)
	}
	if rows == 0 {
		slog.Warn("external-issue sync worker: lease lost on requeue", "run_id", util.UUIDToString(run.ID))
		return nil
	}
	// Wake ourselves if the delay is immediate so a page-budget yield continues
	// promptly instead of waiting for the poll tick.
	if delay == 0 {
		w.Notify()
	}
	return nil
}
