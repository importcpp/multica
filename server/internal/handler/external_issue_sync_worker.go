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

	source, err := w.h.Queries.GetExternalIssueSource(ctx, db.GetExternalIssueSourceParams{
		ID: run.SourceID, WorkspaceID: run.WorkspaceID,
	})
	if err != nil {
		return w.finishRun(ctx, run, "failed", "source not found")
	}
	if !source.CredentialID.Valid {
		return w.finishRun(ctx, run, "failed", "source has no credential; reconnect the installation")
	}

	// Resolve installation -> fresh issues:read token for this claim. Tokens are
	// short-lived, so we never persist them; each claim mints its own.
	installation, err := w.h.Queries.GetGitHubInstallationByID(ctx, source.CredentialID)
	if err != nil || installation.WorkspaceID != run.WorkspaceID {
		return w.finishRun(ctx, run, "needs_reauth", "installation unavailable")
	}
	token, revoke, err := mintGitHubIssuesReadToken(ctx, installation.InstallationID)
	if err != nil {
		if errors.Is(err, errGitHubIssuesPermission) {
			return w.finishRun(ctx, run, "needs_reauth", "installation has not granted Issues read")
		}
		return w.requeueRun(ctx, run, syncRateLimitBackoff, "github auth failed")
	}
	defer revoke()

	provider, ok := externalissue.For(source.Provider)
	if !ok {
		return w.finishRun(ctx, run, "failed", "provider unavailable")
	}
	creds := externalissue.Credentials{Token: token}
	repo := externalissue.Repository{
		InstanceKey: source.InstanceKey,
		ExternalID:  source.RepositoryExternalID,
		FullPath:    source.RepositoryFullPath,
	}
	filter := externalissue.IssueFilter{State: filterStateFromSnapshot(run.FilterSnapshot)}

	counts := runCounts{
		imported:  run.ImportedCount,
		updated:   run.UpdatedCount,
		conflicts: run.ConflictCount,
		skipped:   run.SkippedCount,
		failed:    run.FailedCount,
		total:     run.TotalSeen,
	}
	cursor := externalissue.Cursor(run.Cursor)

	for page := 0; page < syncRunPagesPerClaim; page++ {
		// Re-check cancellation between pages so a cancel lands promptly.
		if fresh, err := w.h.Queries.GetExternalIssueSyncRun(ctx, db.GetExternalIssueSyncRunParams{
			ID: run.ID, WorkspaceID: run.WorkspaceID,
		}); err == nil && fresh.CancelRequested {
			w.advance(ctx, run.ID, cursor, counts, false)
			return w.finishRun(ctx, run, "cancelled", "")
		}

		result, listErr := provider.ListIssues(ctx, creds, repo, filter, cursor)
		if listErr != nil {
			return w.handleListError(ctx, run, cursor, counts, listErr)
		}
		for _, iss := range result.Issues {
			counts.total++
			outcome, applyErr := w.h.ExternalIssueSync.Apply(ctx, service.ApplyParams{
				WorkspaceID: run.WorkspaceID,
				SourceID:    source.ID,
				ProjectID:   source.TargetProjectID,
				CreatorID:   source.ConfiguredByUserID,
				Remote:      toRemoteIssue(iss, repo),
			})
			if applyErr != nil {
				if service.IsQuotaError(applyErr) {
					w.advance(ctx, run.ID, cursor, counts, true)
					return w.finishRun(ctx, run, "quota_blocked", "workspace issue quota reached")
				}
				counts.failed++
				continue
			}
			counts.tally(outcome)
		}
		cursor = result.NextCursor
		w.advance(ctx, run.ID, cursor, counts, true)
		if cursor == "" {
			return w.finishRun(ctx, run, "succeeded", "")
		}
	}

	// Hit the per-claim page budget with more to do: yield the run back to the
	// queue so cancel/other runs get a turn. It resumes from the saved cursor.
	return w.requeueRun(ctx, run, 0, "")
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

// advance persists the cursor + counts, and (when refreshLease) extends the
// lease so a long but healthy run is not reclaimed mid-drain.
func (w *ExternalIssueSyncWorker) advance(ctx context.Context, runID pgtype.UUID, cursor externalissue.Cursor, c runCounts, refreshLease bool) {
	lease := pgtype.Timestamptz{}
	if refreshLease {
		lease = pgtype.Timestamptz{Time: time.Now().Add(syncRunLease), Valid: true}
	}
	if err := w.h.Queries.AdvanceExternalIssueSyncRun(ctx, db.AdvanceExternalIssueSyncRunParams{
		ID:             runID,
		Cursor:         string(cursor),
		ImportedCount:  c.imported,
		UpdatedCount:   c.updated,
		ConflictCount:  c.conflicts,
		SkippedCount:   c.skipped,
		FailedCount:    c.failed,
		TotalSeen:      c.total,
		LeaseExpiresAt: lease,
	}); err != nil {
		slog.Warn("external-issue sync worker: advance run", "run_id", util.UUIDToString(runID), "error", err)
	}
}

// handleListError maps a provider list failure to the right run outcome: a
// rate-limit requeues with the provider's Retry-At; a transient error requeues
// with a short backoff; anything else fails the run.
func (w *ExternalIssueSyncWorker) handleListError(ctx context.Context, run db.ExternalIssueSyncRun, cursor externalissue.Cursor, c runCounts, listErr error) error {
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
			return w.requeueRun(ctx, run, delay, "rate limited")
		case externalissue.ErrTransient:
			return w.requeueRun(ctx, run, syncRateLimitBackoff, "transient error")
		case externalissue.ErrUnauthorized, externalissue.ErrForbidden:
			return w.finishRun(ctx, run, "needs_reauth", e.Error())
		}
	}
	return w.finishRun(ctx, run, "failed", listErr.Error())
}

func (w *ExternalIssueSyncWorker) finishRun(ctx context.Context, run db.ExternalIssueSyncRun, state, errMsg string) error {
	sample := []byte("[]")
	if errMsg != "" {
		if b, err := json.Marshal([]string{errMsg}); err == nil {
			sample = b
		}
	}
	if err := w.h.Queries.FinishExternalIssueSyncRun(ctx, db.FinishExternalIssueSyncRunParams{
		ID: run.ID, State: state, ErrorSample: sample,
	}); err != nil {
		return fmt.Errorf("finish run %s: %w", state, err)
	}
	return nil
}

func (w *ExternalIssueSyncWorker) requeueRun(ctx context.Context, run db.ExternalIssueSyncRun, delay time.Duration, _ string) error {
	next := pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true}
	if err := w.h.Queries.RequeueExternalIssueSyncRun(ctx, db.RequeueExternalIssueSyncRunParams{
		ID: run.ID, NextAttemptAt: next,
	}); err != nil {
		return fmt.Errorf("requeue run: %w", err)
	}
	// Wake ourselves if the delay is immediate so a page-budget yield continues
	// promptly instead of waiting for the poll tick.
	if delay == 0 {
		w.Notify()
	}
	return nil
}

// filterStateFromSnapshot reads {"state":"open|closed|all"} from a run's filter
// snapshot, defaulting to open.
func filterStateFromSnapshot(snapshot []byte) string {
	var f struct {
		State string `json:"state"`
	}
	if len(snapshot) > 0 {
		_ = json.Unmarshal(snapshot, &f)
	}
	switch f.State {
	case "open", "closed", "all":
		return f.State
	default:
		return "open"
	}
}
