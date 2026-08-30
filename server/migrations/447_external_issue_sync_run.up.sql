-- One durable execution of a source's backfill or reconcile. Holds the opaque
-- provider cursor so a worker restart resumes mid-repo instead of restarting;
-- holds a lease (worker_id/lease_expires_at) so at most one worker advances a
-- run and a dead worker's run can be reclaimed. Counts and a bounded error
-- sample feed the progress UI. kind/state validated in application code, no
-- CHECK, no FKs — per repository policy.
CREATE TABLE external_issue_sync_run (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    source_id UUID NOT NULL,
    kind TEXT NOT NULL DEFAULT 'backfill',
    state TEXT NOT NULL DEFAULT 'queued',
    filter_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(filter_snapshot) = 'object'),
    cutoff TIMESTAMPTZ,
    cursor TEXT NOT NULL DEFAULT '',
    imported_count BIGINT NOT NULL DEFAULT 0,
    updated_count BIGINT NOT NULL DEFAULT 0,
    conflict_count BIGINT NOT NULL DEFAULT 0,
    skipped_count BIGINT NOT NULL DEFAULT 0,
    failed_count BIGINT NOT NULL DEFAULT 0,
    total_seen BIGINT NOT NULL DEFAULT 0,
    error_sample JSONB NOT NULL DEFAULT '[]'::jsonb,
    attempt INT NOT NULL DEFAULT 0,
    worker_id TEXT NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    cancel_requested BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
