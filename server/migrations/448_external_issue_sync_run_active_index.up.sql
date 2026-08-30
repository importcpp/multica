-- At most one active (queued/running) run per source. Enforced with a partial
-- unique index on the terminal-excluded states so a completed run does not
-- block the next one.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_external_issue_sync_run_active
    ON external_issue_sync_run (source_id)
    WHERE state IN ('queued', 'running');
