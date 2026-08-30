-- Worker claim scan: cheapest-to-run queued/running rows whose next_attempt_at
-- has arrived, ordered by that column.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_external_issue_sync_run_claim
    ON external_issue_sync_run (next_attempt_at)
    WHERE state IN ('queued', 'running');
