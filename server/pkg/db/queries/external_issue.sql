-- =====================
-- External Issue Source
-- =====================

-- name: UpsertExternalIssueSource :one
-- Create or reuse the one source per (workspace, provider, instance, remote
-- repo). Reselecting the same repo (e.g. from another project) reuses the row
-- and refreshes its mutable binding/display fields rather than creating a
-- competing source.
INSERT INTO external_issue_source (
    workspace_id, provider, instance_key, credential_id,
    repository_external_id, repository_full_path, target_project_id,
    filter, mode, state, configured_by_user_id
) VALUES (
    $1, $2, $3, sqlc.narg('credential_id'),
    $4, $5, sqlc.narg('target_project_id'),
    $6, $7, $8, sqlc.narg('configured_by_user_id')
)
ON CONFLICT (workspace_id, provider, instance_key, repository_external_id) DO UPDATE SET
    credential_id = EXCLUDED.credential_id,
    repository_full_path = EXCLUDED.repository_full_path,
    target_project_id = EXCLUDED.target_project_id,
    filter = EXCLUDED.filter,
    mode = EXCLUDED.mode,
    state = EXCLUDED.state,
    updated_at = now()
RETURNING *;

-- name: GetExternalIssueSource :one
SELECT * FROM external_issue_source
WHERE id = $1 AND workspace_id = $2;

-- name: GetExternalIssueSourceByIdentity :one
-- Look up the existing source for a repo BEFORE upserting, so an import request
-- can compare inputs against an already-active run without first mutating the
-- source row. Matches the UpsertExternalIssueSource conflict key.
SELECT * FROM external_issue_source
WHERE workspace_id = $1 AND provider = $2 AND instance_key = $3 AND repository_external_id = $4;

-- name: ListExternalIssueSourcesByWorkspace :many
SELECT * FROM external_issue_source
WHERE workspace_id = $1
ORDER BY created_at DESC;

-- name: UpdateExternalIssueSourceReconcile :exec
UPDATE external_issue_source SET
    last_reconciled_at = sqlc.narg('last_reconciled_at'),
    next_reconcile_at = sqlc.narg('next_reconcile_at'),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: SetExternalIssueSourceState :exec
UPDATE external_issue_source SET
    state = $3,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: PauseExternalIssueSourcesByCredential :exec
-- Disconnecting a credential does NOT delete sources or their imported issues:
-- it clears the mutable binding and pauses, preserving provenance so a reconnect
-- is idempotent. Workspace-scoped so a shared credential id can't reach across
-- tenants.
UPDATE external_issue_source SET
    credential_id = NULL,
    state = 'needs_reauth',
    updated_at = now()
WHERE workspace_id = $1 AND credential_id = $2;

-- name: ClearExternalIssueSourceProject :exec
-- Project delete detaches the target so later imports don't land in a dead
-- project; already-imported issues follow the existing project-delete semantics.
UPDATE external_issue_source SET
    target_project_id = NULL,
    state = 'needs_project',
    updated_at = now()
WHERE workspace_id = $1 AND target_project_id = $2;

-- name: DeleteExternalIssueSourcesByWorkspace :exec
DELETE FROM external_issue_source WHERE workspace_id = $1;

-- =====================
-- External Issue Link
-- =====================

-- name: GetExternalIssueLinkByIdentity :one
SELECT * FROM external_issue_link
WHERE workspace_id = $1 AND provider = $2 AND instance_key = $3 AND external_issue_id = $4;

-- name: GetExternalIssueLinkByIssue :one
SELECT * FROM external_issue_link
WHERE workspace_id = $1 AND issue_id = $2;

-- name: ResumeExternalIssueLinkField :exec
-- resume_sync for the scoped fields: clear local ownership + conflict AND advance
-- the baseline to the CURRENT local content hash. Advancing the baseline is what
-- makes resume meaningful — otherwise local still differs from the stale baseline
-- and the very next sync re-raises the same conflict. After this, local == the
-- new baseline, so a later remote change flows in cleanly (remoteChanged &&
-- !localChanged -> take remote). Out-of-scope fields are untouched.
UPDATE external_issue_link SET
    title_local_owned   = CASE WHEN sqlc.arg('title_in_scope') THEN false ELSE title_local_owned END,
    body_local_owned    = CASE WHEN sqlc.arg('body_in_scope')  THEN false ELSE body_local_owned  END,
    title_conflict      = CASE WHEN sqlc.arg('title_in_scope') THEN false ELSE title_conflict END,
    body_conflict       = CASE WHEN sqlc.arg('body_in_scope')  THEN false ELSE body_conflict  END,
    title_baseline_hash = CASE WHEN sqlc.arg('title_in_scope') THEN sqlc.arg('title_baseline') ELSE title_baseline_hash END,
    body_baseline_hash  = CASE WHEN sqlc.arg('body_in_scope')  THEN sqlc.arg('body_baseline')  ELSE body_baseline_hash  END,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: ResolveExternalIssueLinkField :exec
-- Per-field conflict resolution. For a field IN SCOPE ($3 title, $5 body) the
-- ownership is set to the given value ($4/$6) and the conflict flag is cleared —
-- both keep_local and resume_sync resolve the conflict, so clearing does not
-- depend on the ownership direction (the old query only cleared when ownership
-- became true, so resume_sync left the conflict showing). Out-of-scope fields
-- are left completely untouched.
UPDATE external_issue_link SET
    title_local_owned = CASE WHEN sqlc.arg('title_in_scope') THEN sqlc.arg('title_owned') ELSE title_local_owned END,
    body_local_owned  = CASE WHEN sqlc.arg('body_in_scope')  THEN sqlc.arg('body_owned')  ELSE body_local_owned  END,
    title_conflict    = CASE WHEN sqlc.arg('title_in_scope') THEN false ELSE title_conflict END,
    body_conflict     = CASE WHEN sqlc.arg('body_in_scope')  THEN false ELSE body_conflict  END,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;



-- name: ClaimExternalIssueLink :one
-- Race-safe claim of a remote issue on stable identity. Two concurrent
-- claimers both reach ON CONFLICT, but only the row that was actually INSERTED
-- has xmax = 0; the conflicter gets the existing row with inserted = false. The
-- caller creates a Multica issue only when inserted = true, so a manual import
-- racing a webhook can never create two issues for one remote issue. On a
-- conflict the existing binding (issue_id, tombstone) is returned unchanged so
-- the caller can route to the update / ignored path.
INSERT INTO external_issue_link (
    workspace_id, provider, instance_key, external_issue_id,
    source_id, display_number, external_html_url,
    remote_state, remote_updated_at
) VALUES (
    $1, $2, $3, $4,
    sqlc.narg('source_id'), $5, $6,
    $7, sqlc.narg('remote_updated_at')
)
ON CONFLICT (workspace_id, provider, instance_key, external_issue_id) DO UPDATE SET
    updated_at = now()
RETURNING *, (xmax = 0) AS inserted;

-- name: BindExternalIssueLinkIssue :exec
-- Bind a freshly created Multica issue to a link the winner claimed.
UPDATE external_issue_link SET
    issue_id = $2,
    source_id = sqlc.narg('source_id'),
    display_number = $3,
    external_html_url = $4,
    remote_state = $5,
    remote_updated_at = sqlc.narg('remote_updated_at'),
    title_baseline_hash = $6,
    body_baseline_hash = $7,
    title_conflict = false,
    body_conflict = false,
    moved = false,
    local_deleted_at = NULL,
    updated_at = now()
WHERE id = $1;

-- name: UpdateExternalIssueLinkSync :exec
-- Advance the sync baseline after a content update, and record any per-field
-- conflict / local-ownership decision the applier made.
UPDATE external_issue_link SET
    display_number = $2,
    external_html_url = $3,
    remote_state = $4,
    remote_updated_at = sqlc.narg('remote_updated_at'),
    title_baseline_hash = $5,
    body_baseline_hash = $6,
    title_conflict = $7,
    body_conflict = $8,
    title_local_owned = $9,
    body_local_owned = $10,
    updated_at = now()
WHERE id = $1;

-- name: MarkExternalIssueLinkMoved :exec
UPDATE external_issue_link SET moved = true, updated_at = now()
WHERE id = $1;

-- name: ListExternalIssueLinksBySource :many
SELECT * FROM external_issue_link
WHERE source_id = $1
ORDER BY created_at ASC;

-- name: DeleteExternalIssueLinksByWorkspace :exec
DELETE FROM external_issue_link WHERE workspace_id = $1;

-- =====================
-- External Issue Sync Run
-- =====================

-- name: CreateExternalIssueSyncRun :one
INSERT INTO external_issue_sync_run (
    workspace_id, source_id, kind, state, filter_snapshot, input_snapshot, cutoff
) VALUES (
    $1, $2, $3, $4, $5, $6, sqlc.narg('cutoff')
)
RETURNING *;

-- name: GetExternalIssueSyncRun :one
SELECT * FROM external_issue_sync_run
WHERE id = $1 AND workspace_id = $2;

-- name: ClaimNextExternalIssueSyncRun :one
-- Worker claim: take one due queued/running run, lease it, and move it to
-- running. SKIP LOCKED lets N workers claim disjoint runs without blocking.
UPDATE external_issue_sync_run SET
    state = 'running',
    worker_id = $1,
    lease_expires_at = $2,
    attempt = attempt + 1,
    started_at = COALESCE(started_at, now()),
    updated_at = now()
WHERE id = (
    SELECT id FROM external_issue_sync_run
    WHERE state IN ('queued', 'running')
      AND next_attempt_at <= now()
      AND (lease_expires_at IS NULL OR lease_expires_at < now())
    ORDER BY next_attempt_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: CheckpointExternalIssueSyncRunCursor :execrows
-- Lightweight interior-page checkpoint: persist ONLY the cursor and refreshed
-- lease, without re-aggregating the identity ledger. Fenced on worker_id. Full
-- count aggregation is expensive (a scan of the run's ledger) and only needs to
-- happen at claim EXIT points, so interior pages use this to keep the resume
-- cursor crash-safe at O(1); the counts on the run row may lag within a claim and
-- are refreshed authoritatively at the next exit / next claim start.
UPDATE external_issue_sync_run SET
    cursor = $2,
    lease_expires_at = $3,
    updated_at = now()
WHERE id = $1 AND worker_id = sqlc.arg('worker_id');

-- name: AdvanceExternalIssueSyncRun :execrows
-- Persist page progress: new cursor, refreshed lease, and the running counts.
-- Fenced on worker_id: if the lease was stolen by another worker, this updates
-- zero rows and the caller must stop writing to the run. page_offset records how
-- many items of the CURRENT page are already accounted (0 once a full page
-- commits) so a mid-page resume does not re-count.
UPDATE external_issue_sync_run SET
    cursor = $2,
    imported_count = $3,
    updated_count = $4,
    conflict_count = $5,
    skipped_count = $6,
    failed_count = $7,
    total_seen = $8,
    lease_expires_at = $9,
    page_offset = sqlc.arg('page_offset'),
    updated_at = now()
WHERE id = $1 AND worker_id = sqlc.arg('worker_id');

-- name: RenewExternalIssueSyncRunLease :execrows
-- In-page heartbeat: extend the lease mid-page, fenced on worker_id AND on the
-- run still being running and not cancel-requested. Zero rows means the lease was
-- reclaimed OR a cancel/terminal transition landed concurrently, so this worker
-- must stop applying immediately rather than drift until the page boundary.
UPDATE external_issue_sync_run SET
    lease_expires_at = $2,
    updated_at = now()
WHERE id = $1 AND worker_id = sqlc.arg('worker_id')
  AND state = 'running'
  AND cancel_requested = false;

-- name: FinishExternalIssueSyncRun :execrows
-- Fenced on worker_id so a reclaimed run is not finalized by its former owner.
UPDATE external_issue_sync_run SET
    state = $2,
    error_sample = $3,
    lease_expires_at = NULL,
    worker_id = '',
    finished_at = now(),
    updated_at = now()
WHERE id = $1 AND worker_id = sqlc.arg('worker_id');

-- name: FinishExternalIssueSyncRunAndPurgeLedger :one
-- Atomic finish + ledger purge for a NON-RESUMABLE terminal state
-- (succeeded/partial/cancelled): the run row transition and the delete of its
-- identity ledger happen in ONE statement, so a crash can never leave a finished
-- run with an orphaned ledger (the old two-step "finish then best-effort delete"
-- leaked rows permanently when it died between the steps, since a terminal run is
-- never re-claimed). Fenced on worker_id. Returns finished=1 when this worker
-- actually finalized the run (0 = lease lost), independent of how many ledger
-- rows were purged.
WITH finished AS (
    UPDATE external_issue_sync_run AS r SET
        state = $2,
        error_sample = $3,
        lease_expires_at = NULL,
        worker_id = '',
        finished_at = now(),
        updated_at = now()
    WHERE r.id = $1 AND r.worker_id = sqlc.arg('worker_id')
    RETURNING r.id AS run_id
), purged AS (
    DELETE FROM external_issue_sync_run_item
    WHERE run_id IN (SELECT run_id FROM finished)
    RETURNING 1
)
SELECT (SELECT count(*) FROM finished) AS finished;

-- name: RequeueExternalIssueSyncRun :execrows
-- Fenced on worker_id; returns the run to the queue only if this worker still
-- owns the lease.
UPDATE external_issue_sync_run SET
    state = 'queued',
    worker_id = '',
    lease_expires_at = NULL,
    next_attempt_at = $2,
    updated_at = now()
WHERE id = $1 AND worker_id = sqlc.arg('worker_id');

-- name: RequestExternalIssueSyncRunCancel :exec
UPDATE external_issue_sync_run SET
    cancel_requested = true,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: ResumeExternalIssueSyncRun :one
-- Re-queue a paused run (quota_blocked / needs_reauth / failed) from its SAVED
-- cursor rather than starting a fresh run, so counts and cursor continue. The
-- input_snapshot is REWRITTEN from the current (re-validated) source so a
-- reconnect that minted a new credential UUID is picked up — a needs_reauth run
-- otherwise keeps reading the dead credential and loops. repo/filter/project
-- execution semantics are carried in the caller-supplied snapshot. Only a
-- non-active run in a resumable state can be resumed; the partial unique active
-- index still guarantees at most one queued/running run per source afterwards.
UPDATE external_issue_sync_run SET
    state = 'queued',
    cancel_requested = false,
    worker_id = '',
    lease_expires_at = NULL,
    next_attempt_at = now(),
    finished_at = NULL,
    input_snapshot = sqlc.arg('input_snapshot'),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2
  AND state IN ('quota_blocked', 'needs_reauth', 'failed')
RETURNING *;

-- name: ListExternalIssueSyncRunsBySource :many
SELECT * FROM external_issue_sync_run
WHERE source_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: DeleteExternalIssueSyncRunsByWorkspace :exec
DELETE FROM external_issue_sync_run WHERE workspace_id = $1;

-- =====================
-- External Issue Sync Event
-- =====================

-- name: EnqueueExternalIssueSyncEvent :one
-- Webhook inbox insert. Deduped on (source, delivery id): a redelivery is a
-- no-op that returns the existing row.
INSERT INTO external_issue_sync_event (
    workspace_id, source_id, delivery_id, external_issue_id, remote_updated_at
) VALUES (
    $1, $2, $3, $4, sqlc.narg('remote_updated_at')
)
ON CONFLICT (source_id, delivery_id) DO NOTHING
RETURNING *;

-- name: ClaimNextExternalIssueSyncEvent :one
UPDATE external_issue_sync_event SET
    state = 'running',
    worker_id = $1,
    lease_expires_at = $2,
    attempt = attempt + 1,
    updated_at = now()
WHERE id = (
    SELECT id FROM external_issue_sync_event
    WHERE state = 'queued'
      AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: FinishExternalIssueSyncEvent :exec
UPDATE external_issue_sync_event SET
    state = $2,
    lease_expires_at = NULL,
    worker_id = '',
    updated_at = now()
WHERE id = $1;

-- name: DeleteExternalIssueSyncEventsByWorkspace :exec
DELETE FROM external_issue_sync_event WHERE workspace_id = $1;

-- =====================
-- External Issue Sync Run Item ledger (identity-based accounting)
-- =====================

-- name: LockExternalIssueSyncRunForApply :one
-- Fence the Apply transaction on run ownership: take the run row FOR UPDATE and
-- return it only when this worker still owns a live, non-cancelled lease. A
-- caller that gets pgx.ErrNoRows rolls the whole tx back and creates nothing, so
-- a stolen lease / cancellation lands atomically with the issue create instead
-- of drifting until the next heartbeat.
SELECT * FROM external_issue_sync_run
WHERE id = $1
  AND worker_id = sqlc.arg('worker_id')
  AND state = 'running'
  AND cancel_requested = false
  AND (lease_expires_at IS NULL OR lease_expires_at > now())
FOR UPDATE;

-- name: UpsertExternalIssueSyncRunItem :one
-- Record (run, remote issue) -> outcome on stable identity. inserted=true only
-- when this is the first time the issue was accounted in this run, so the caller
-- bumps counts exactly once even if a re-fetched page replays the issue. On
-- replay the outcome is kept STICKY by precedence (failed < skipped < conflict <
-- updated < imported): an already-imported issue re-seen as a skipped no-op on
-- resume keeps "imported", and a prior "failed" can be upgraded on retry —
-- neither double-counts nor reclassifies a committed create as a skip.
INSERT INTO external_issue_sync_run_item (run_id, workspace_id, external_issue_id, outcome)
VALUES ($1, $2, $3, $4)
ON CONFLICT (run_id, external_issue_id) DO UPDATE SET
    outcome = CASE
        WHEN array_position(ARRAY['failed','skipped','conflict','updated','imported'], EXCLUDED.outcome)
           > array_position(ARRAY['failed','skipped','conflict','updated','imported'], external_issue_sync_run_item.outcome)
        THEN EXCLUDED.outcome
        ELSE external_issue_sync_run_item.outcome
    END
RETURNING (xmax = 0) AS inserted;

-- name: RecordFailedExternalIssueSyncRunItemFenced :one
-- Fenced failed-accounting: record a 'failed' outcome ONLY while this worker
-- still owns a live, non-cancelled, running run. The INSERT is gated on the run
-- row via a WHERE EXISTS so a stale/reclaimed/cancelled worker writes NOTHING
-- (the success-path ledger write is already fenced inside Apply's tx; this closes
-- the failure path that previously wrote unconditionally). Returns fenced=false
-- when the guard blocked the write so the caller stops this claim.
WITH owned AS (
    SELECT 1 AS ok FROM external_issue_sync_run r
    WHERE r.id = $1
      AND r.worker_id = sqlc.arg('worker_id')
      AND r.state = 'running'
      AND r.cancel_requested = false
      AND (r.lease_expires_at IS NULL OR r.lease_expires_at > now())
), ins AS (
    INSERT INTO external_issue_sync_run_item (run_id, workspace_id, external_issue_id, outcome)
    SELECT $1, $2, $3, 'failed' FROM owned
    ON CONFLICT (run_id, external_issue_id) DO UPDATE SET
        outcome = CASE
            WHEN array_position(ARRAY['failed','skipped','conflict','updated','imported'], EXCLUDED.outcome)
               > array_position(ARRAY['failed','skipped','conflict','updated','imported'], external_issue_sync_run_item.outcome)
            THEN EXCLUDED.outcome
            ELSE external_issue_sync_run_item.outcome
        END
    RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM owned) AS fenced_ok;

-- name: CountExternalIssueSyncRunItems :one
-- Derive run counts by aggregating the ledger, so counts never depend on a
-- page position. Returns imported/updated/conflict/skipped/failed/total.
SELECT
    COUNT(*) FILTER (WHERE outcome = 'imported')  AS imported,
    COUNT(*) FILTER (WHERE outcome = 'updated')   AS updated,
    COUNT(*) FILTER (WHERE outcome = 'conflict')  AS conflicts,
    COUNT(*) FILTER (WHERE outcome = 'skipped')   AS skipped,
    COUNT(*) FILTER (WHERE outcome = 'failed')    AS failed,
    COUNT(*)                                       AS total
FROM external_issue_sync_run_item
WHERE run_id = $1;

-- name: ExternalIssueSyncRunItemExists :one
SELECT EXISTS (
    SELECT 1 FROM external_issue_sync_run_item
    WHERE run_id = $1 AND external_issue_id = $2
) AS exists;

-- name: DeleteExternalIssueSyncRunItemsByWorkspace :exec
DELETE FROM external_issue_sync_run_item WHERE workspace_id = $1;

-- name: SetExternalIssueSyncRunErrorSample :execrows
-- Persist the accumulated error sample mid-run (page checkpoint), fenced on
-- worker_id so a reclaimed run isn't written by its former owner. Kept separate
-- from advance so the sample survives a page-budget requeue into the next claim.
UPDATE external_issue_sync_run SET
    error_sample = $2,
    updated_at = now()
WHERE id = $1 AND worker_id = sqlc.arg('worker_id');
