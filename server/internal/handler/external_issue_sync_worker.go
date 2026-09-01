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
	"github.com/multica-ai/multica/server/internal/integrations/githubapi"
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
	// syncHeartbeatEvery is how many items into a page the worker renews its
	// lease. It bounds how far a stale (reclaimed) worker can drift before it
	// detects the steal and stops.
	syncHeartbeatEvery = 25
	// syncMaxErrorSamples caps the per-run failed-item diagnostics persisted to
	// error_sample so a pathological repo can't bloat the row.
	syncMaxErrorSamples = 20
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
		return w.finishRun(ctx, run, "cancelled", nil)
	}

	// Read execution inputs from the run's immutable snapshot, not the live
	// (mutable) source: a second import request can change the source's
	// credential / target project / filter mid-run, and re-reading it here would
	// silently redirect an in-flight import. The snapshot is captured at enqueue.
	snap, err := decodeRunInput(run.InputSnapshot)
	if err != nil || !snap.CredentialID.Valid {
		return w.finishRun(ctx, run, "failed", []string{"run input snapshot missing or invalid"})
	}

	provider, ok := externalissue.For(snap.Provider)
	if !ok {
		return w.finishRun(ctx, run, "failed", []string{"provider unavailable"})
	}

	// Resolve credentials through the provider-neutral resolver so the worker
	// stays GitHub-agnostic: a GitLab run would use a different resolver without
	// any change here. ErrCredentialUnavailable => needs_reauth; any other error
	// (rate limit / network) => requeue.
	resolver, ok := w.h.credentialResolver(snap.Provider)
	if !ok {
		return w.finishRun(ctx, run, "failed", []string{"no credential resolver for provider"})
	}
	creds, err := resolver.Resolve(ctx, externalissue.CredentialRef{
		Provider:    snap.Provider,
		WorkspaceID: util.UUIDToString(run.WorkspaceID),
		ID:          util.UUIDToString(snap.CredentialID),
	})
	if err != nil {
		if errors.Is(err, externalissue.ErrCredentialUnavailable) {
			return w.finishRun(ctx, run, "needs_reauth", []string{"credential unavailable; reconnect the account"})
		}
		// Honor an explicit Retry-After when the credential mint was throttled,
		// instead of a fixed backoff.
		return w.requeueRun(ctx, run, credentialRetryDelay(err))
	}

	repo := externalissue.Repository{
		InstanceKey: snap.InstanceKey,
		ExternalID:  snap.RepositoryExternalID,
		FullPath:    snap.RepositoryFullPath,
	}
	// Scan the STABLE SUPERSET (state=all) and filter to the requested state
	// locally. A filtered scan (state=open) over "created asc" is NOT stable:
	// closing an issue removes it from the filtered set and shifts every later
	// issue into an already-consumed page, so a page-cursor scan can permanently
	// miss a still-matching issue. state=all never loses a row on close and
	// created_at is immutable, so page positions are stable and a single forward
	// pass is complete — no fixpoint re-scan needed.
	filter := externalissue.IssueFilter{State: "all"}
	wantState := snap.State
	if wantState == "" {
		wantState = "open"
	}

	// errorSamples accumulates ACROSS claims: seed from the run's persisted
	// sample so a failure in an earlier claim survives into the final state.
	errorSamples := decodeErrorSamples(run.ErrorSample)
	cursor := externalissue.Cursor(run.Cursor)
	// scanned counts every remote issue VISITED (matched or filtered), seeded from
	// prior claims, so the UI shows forward progress during a long superset scan
	// even when few issues match the requested state.
	scanned := run.ScannedCount

	for page := 0; page < syncRunPagesPerClaim; page++ {
		// Re-check cancellation between pages so a cancel lands promptly.
		if fresh, err := w.h.Queries.GetExternalIssueSyncRun(ctx, db.GetExternalIssueSyncRunParams{
			ID: run.ID, WorkspaceID: run.WorkspaceID,
		}); err == nil && fresh.CancelRequested {
			counts, cerr := w.ledgerCounts(ctx, run.ID)
			if cerr != nil {
				return cerr
			}
			if err := w.advance(ctx, run, cursor, counts, scanned); err != nil {
				return err
			}
			return w.finishRun(ctx, run, "cancelled", errorSamples)
		}

		result, listErr := provider.ListIssues(ctx, creds, repo, filter, cursor)
		if listErr != nil {
			return w.handleListError(ctx, run, listErr)
		}
		if result.IncompleteBucket && !hasIncompleteMarker(errorSamples) {
			// A full page fell entirely within one updated_at second: the overflow
			// cannot be enumerated safely, so the scan will skip past it. Record a
			// durable marker (survives claims via error_sample) so the run finishes
			// PARTIAL, never a falsely-complete succeeded.
			errorSamples = append(errorSamples, incompleteBucketMarker)
		}
		for i, iss := range result.Issues {
			scanned++
			// Superset scan: skip issues that don't match the requested state
			// locally, so they never enter the ledger/counts (matches "import only
			// <state>"). Skipping is O(1) and does not touch the DB.
			if !importStateMatches(iss.State, wantState) {
				continue
			}
			// In-page heartbeat: renew the lease periodically so a healthy but
			// slow page is not reclaimed. The authoritative ownership check is
			// inside Apply's own transaction (fence), so this is only a liveness
			// signal — a stolen lease is caught atomically by Apply, not here.
			if i%syncHeartbeatEvery == 0 {
				if err := w.renewLease(ctx, run); err != nil {
					return err
				}
			}
			outcome, applyErr := w.h.ExternalIssueSync.Apply(ctx, service.ApplyParams{
				WorkspaceID: run.WorkspaceID,
				SourceID:    run.SourceID,
				ProjectID:   snap.TargetProjectID,
				CreatorID:   snap.ConfiguredByUserID,
				Remote:      toRemoteIssue(snap.Provider, iss, repo),
				RunID:       run.ID,
				WorkerID:    w.id,
			})
			if applyErr != nil {
				if errors.Is(applyErr, service.ErrRunFenced) {
					// The run's lease was stolen or it was cancelled; Apply rolled
					// back and wrote nothing. Stop this claim — the new owner (or
					// the cancel finalizer) drives the run.
					return errLeaseLost
				}
				if service.IsQuotaError(applyErr) {
					// This item was NOT accounted (its tx rolled back). Persist the
					// PAGE-START cursor; resume re-fetches this page and the ledger
					// (keyed on stable issue id) skips exactly the already-accounted
					// items regardless of any membership shift.
					counts, cerr := w.ledgerCounts(ctx, run.ID)
					if cerr != nil {
						return cerr
					}
					if err := w.advance(ctx, run, cursor, counts, scanned); err != nil {
						return err
					}
					return w.finishRun(ctx, run, "quota_blocked", errorSamples)
				}
				// A per-item failure is recorded in the ledger (via Apply's failed
				// outcome path) — but Apply returns the error without recording for
				// non-outcome failures, so record it here and sample it.
				if err := w.recordFailedItem(ctx, run, iss.ExternalID); err != nil {
					return err
				}
				if len(errorSamples) < syncMaxErrorSamples {
					errorSamples = append(errorSamples, fmt.Sprintf("issue #%d: %v", iss.Number, applyErr))
				}
				continue
			}
			_ = outcome // accounting is in the ledger, not a local counter
		}
		cursor = result.NextCursor
		if cursor == "" {
			// End of a single, complete forward pass over the stable superset:
			// aggregate the ledger once (authoritative counts) and finish. No
			// re-scan is needed because the superset ordering is stable.
			counts, cerr := w.ledgerCounts(ctx, run.ID)
			if cerr != nil {
				return cerr
			}
			if err := w.advance(ctx, run, cursor, counts, scanned); err != nil {
				return err
			}
			if err := w.persistErrorSamples(ctx, run, errorSamples); err != nil {
				return err
			}
			state := "succeeded"
			if counts.failed > 0 || hasIncompleteMarker(errorSamples) {
				// A skipped same-second overflow bucket makes the scan incomplete →
				// partial, so the UI never shows a possibly-lossy scan as succeeded.
				state = "partial"
			}
			return w.finishRun(ctx, run, state, errorSamples)
		}
		// Interior page: keep the resume cursor crash-safe at O(1) without
		// re-aggregating the ledger every page (that scan grows with the run). The
		// run-row counts refresh authoritatively at the next exit / claim start.
		if err := w.checkpointCursor(ctx, run, cursor, scanned); err != nil {
			return err
		}
		if err := w.persistErrorSamples(ctx, run, errorSamples); err != nil {
			return err
		}
	}

	// Hit the per-claim page budget with more to do: aggregate the ledger once
	// (authoritative counts for the polled UI + next claim's baseline) and yield
	// the run back to the queue so cancel/other runs get a turn. It resumes from
	// the saved cursor.
	counts, cerr := w.ledgerCounts(ctx, run.ID)
	if cerr != nil {
		return cerr
	}
	if err := w.advance(ctx, run, cursor, counts, scanned); err != nil {
		return err
	}
	return w.requeueRun(ctx, run, 0)
}

// importStateMatches reports whether a superset-scanned issue should be imported
// for the requested filter state. The worker fetches state=all (a stable
// superset) and filters locally, so "open"/"closed" import only that state while
// "all" imports everything.
func importStateMatches(s externalissue.State, want string) bool {
	switch want {
	case "all":
		return true
	case "closed":
		return s == externalissue.StateClosed
	default: // "open"
		return s == externalissue.StateOpen
	}
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

// errLeaseLost signals the run's lease was reclaimed by another worker between
// claim and this write. The caller stops touching the run; the new owner drives
// it. It is not surfaced as a processing error.
var errLeaseLost = errors.New("external-issue sync run lease lost")

// advance persists the cursor + counts and refreshes the lease, fenced on this
// worker's id. A zero-row update means the lease was stolen (or the run was
// cancelled/finished elsewhere): return errLeaseLost so the caller aborts
// WITHOUT finalizing — never mark succeeded on an unsaved cursor.
func (w *ExternalIssueSyncWorker) advance(ctx context.Context, run db.ExternalIssueSyncRun, cursor externalissue.Cursor, c runCounts, scanned int64) error {
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
		ScannedCount:   scanned,
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

// checkpointCursor persists the resume cursor + scanned progress and renews the
// lease, without re-aggregating the identity ledger. Used for interior pages so
// per-page cost stays O(1) as the run grows; fenced on worker_id.
func (w *ExternalIssueSyncWorker) checkpointCursor(ctx context.Context, run db.ExternalIssueSyncRun, cursor externalissue.Cursor, scanned int64) error {
	lease := pgtype.Timestamptz{Time: time.Now().Add(syncRunLease), Valid: true}
	rows, err := w.h.Queries.CheckpointExternalIssueSyncRunCursor(ctx, db.CheckpointExternalIssueSyncRunCursorParams{
		ID:             run.ID,
		WorkerID:       w.id,
		Cursor:         string(cursor),
		LeaseExpiresAt: lease,
		ScannedCount:   scanned,
	})
	if err != nil {
		return fmt.Errorf("checkpoint cursor: %w", err)
	}
	if rows == 0 {
		slog.Warn("external-issue sync worker: lease lost on cursor checkpoint", "run_id", util.UUIDToString(run.ID))
		return errLeaseLost
	}
	return nil
}

// ledgerCounts derives the run's counts by aggregating the run-item ledger, so
// counts reflect stable-identity accounting rather than a page position.
func (w *ExternalIssueSyncWorker) ledgerCounts(ctx context.Context, runID pgtype.UUID) (runCounts, error) {
	row, err := w.h.Queries.CountExternalIssueSyncRunItems(ctx, runID)
	if err != nil {
		return runCounts{}, fmt.Errorf("count ledger: %w", err)
	}
	return runCounts{
		imported:  row.Imported,
		updated:   row.Updated,
		conflicts: row.Conflicts,
		skipped:   row.Skipped,
		failed:    row.Failed,
		total:     row.Total,
	}, nil
}

// recordFailedItem records a failed outcome in the ledger for a remote issue
// whose Apply returned a non-fenced, non-quota error (so Apply's own tx rolled
// back without writing a ledger row). The write is FENCED on run ownership: a
// stale/reclaimed/cancelled worker must not append failures to a run it no longer
// drives, so when the fence blocks the write this returns errLeaseLost to stop
// the claim (mirrors the success-path ledger write, which is fenced inside
// Apply's own transaction).
func (w *ExternalIssueSyncWorker) recordFailedItem(ctx context.Context, run db.ExternalIssueSyncRun, externalID string) error {
	ok, err := w.h.Queries.RecordFailedExternalIssueSyncRunItemFenced(ctx, db.RecordFailedExternalIssueSyncRunItemFencedParams{
		ID:              run.ID,
		WorkspaceID:     run.WorkspaceID,
		ExternalIssueID: externalID,
		WorkerID:        w.id,
	})
	if err != nil {
		return fmt.Errorf("record failed ledger item: %w", err)
	}
	if !ok {
		return errLeaseLost
	}
	return nil
}

// decodeErrorSamples reads the persisted JSON error_sample array so samples
// accumulate across claims instead of resetting each claim.
func decodeErrorSamples(raw []byte) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// incompleteBucketMarker is a sentinel error sample recorded when the provider
// reports it skipped a same-second overflow bucket it could not enumerate. It
// persists across claims via error_sample, so the run finishes partial (not
// succeeded) and the user is told the scan was incomplete.
const incompleteBucketMarker = "scan incomplete: a same-second update bucket exceeded one page and was skipped"

func hasIncompleteMarker(samples []string) bool {
	for _, s := range samples {
		if s == incompleteBucketMarker {
			return true
		}
	}
	return false
}

// persistErrorSamples writes the accumulated sample at a page checkpoint (fenced)
// so it survives a page-budget requeue into the next claim; a lost lease is a
// no-op here (advance already returned errLeaseLost in that case).
func (w *ExternalIssueSyncWorker) persistErrorSamples(ctx context.Context, run db.ExternalIssueSyncRun, samples []string) error {
	if samples == nil {
		samples = []string{}
	}
	b, err := json.Marshal(samples)
	if err != nil {
		return nil
	}
	if _, err := w.h.Queries.SetExternalIssueSyncRunErrorSample(ctx, db.SetExternalIssueSyncRunErrorSampleParams{
		ID: run.ID, WorkerID: w.id, ErrorSample: b,
	}); err != nil {
		return fmt.Errorf("persist error sample: %w", err)
	}
	return nil
}

// renewLease is the in-page heartbeat: it extends the lease mid-page, fenced on
// this worker's id. A zero-row renew means another worker reclaimed the run, so
// this worker returns errLeaseLost and stops applying immediately — bounding the
// window in which a stale worker's Apply side effects (counts, events) can drift
// to at most syncHeartbeatEvery items.
func (w *ExternalIssueSyncWorker) renewLease(ctx context.Context, run db.ExternalIssueSyncRun) error {
	lease := pgtype.Timestamptz{Time: time.Now().Add(syncRunLease), Valid: true}
	rows, err := w.h.Queries.RenewExternalIssueSyncRunLease(ctx, db.RenewExternalIssueSyncRunLeaseParams{
		ID: run.ID, WorkerID: w.id, LeaseExpiresAt: lease,
	})
	if err != nil {
		return fmt.Errorf("renew lease: %w", err)
	}
	if rows == 0 {
		slog.Warn("external-issue sync worker: lease lost on renew", "run_id", util.UUIDToString(run.ID))
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
			return w.finishRun(ctx, run, "needs_reauth", []string{e.Error()})
		}
	}
	return w.finishRun(ctx, run, "failed", []string{listErr.Error()})
}

// credentialRetryDelay honors a githubapi.RateLimitError's Retry-After when a
// credential mint was throttled, else falls back to the fixed backoff.
func credentialRetryDelay(err error) time.Duration {
	var rl *githubapi.RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		return rl.RetryAfter
	}
	return syncRateLimitBackoff
}

// finishRun finalizes a run, fenced on this worker's id. samples is a bounded
// list of per-item failure diagnostics persisted to error_sample. A lost lease
// is a no-op (the new owner owns the outcome), not an error.
func (w *ExternalIssueSyncWorker) finishRun(ctx context.Context, run db.ExternalIssueSyncRun, state string, samples []string) error {
	if samples == nil {
		samples = []string{}
	}
	sample, err := json.Marshal(samples)
	if err != nil {
		sample = []byte("[]")
	}
	// A non-resumable terminal run (succeeded/partial/cancelled) will never
	// re-scan and its counts are already snapshotted on the run row, so finish AND
	// purge its identity ledger in ONE atomic statement. Doing it atomically (vs.
	// finish-then-best-effort-delete) means a crash can't leave a finished run with
	// an orphaned ledger that is never reclaimed to clean up. Resumable terminal
	// states keep their ledger so resume can still dedup already-accounted issues.
	if isNonResumableTerminal(state) {
		finished, err := w.h.Queries.FinishExternalIssueSyncRunAndPurgeLedger(ctx, db.FinishExternalIssueSyncRunAndPurgeLedgerParams{
			ID: run.ID, WorkerID: w.id, State: state, ErrorSample: sample,
		})
		if err != nil {
			return fmt.Errorf("finish run %s: %w", state, err)
		}
		if finished == 0 {
			slog.Warn("external-issue sync worker: lease lost on finish", "run_id", util.UUIDToString(run.ID))
		}
		return nil
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

// isNonResumableTerminal reports whether a finished run can never be resumed, so
// its identity ledger is safe to delete. quota_blocked / needs_reauth / failed
// are terminal-for-now but resumable, so they keep their ledger.
func isNonResumableTerminal(state string) bool {
	switch state {
	case "succeeded", "partial", "cancelled":
		return true
	default:
		return false
	}
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
