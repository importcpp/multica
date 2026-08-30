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

-- name: PauseExternalIssueSourcesByProviderInstance :exec
-- Disconnecting a provider account (e.g. removing a GitHub installation) pauses
-- that provider/instance's sources for the workspace without deleting them or
-- their imported issues, so a reconnect + re-import stays idempotent. Provenance
-- (links) is preserved.
UPDATE external_issue_source SET
    credential_id = NULL,
    state = 'needs_reauth',
    updated_at = now()
WHERE workspace_id = $1 AND provider = $2 AND instance_key = $3;

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
    workspace_id, source_id, kind, state, filter_snapshot, cutoff
) VALUES (
    $1, $2, $3, $4, $5, sqlc.narg('cutoff')
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

-- name: AdvanceExternalIssueSyncRun :exec
-- Persist page progress: new cursor, refreshed lease, and the running counts.
UPDATE external_issue_sync_run SET
    cursor = $2,
    imported_count = $3,
    updated_count = $4,
    conflict_count = $5,
    skipped_count = $6,
    failed_count = $7,
    total_seen = $8,
    lease_expires_at = $9,
    updated_at = now()
WHERE id = $1;

-- name: FinishExternalIssueSyncRun :exec
UPDATE external_issue_sync_run SET
    state = $2,
    error_sample = $3,
    lease_expires_at = NULL,
    worker_id = '',
    finished_at = now(),
    updated_at = now()
WHERE id = $1;

-- name: RequeueExternalIssueSyncRun :exec
UPDATE external_issue_sync_run SET
    state = 'queued',
    worker_id = '',
    lease_expires_at = NULL,
    next_attempt_at = $2,
    updated_at = now()
WHERE id = $1;

-- name: RequestExternalIssueSyncRunCancel :exec
UPDATE external_issue_sync_run SET
    cancel_requested = true,
    updated_at = now()
WHERE id = $1 AND workspace_id = $2;

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
