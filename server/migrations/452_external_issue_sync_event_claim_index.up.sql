-- Worker claim scan for queued events whose backoff has elapsed.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_external_issue_sync_event_claim
    ON external_issue_sync_event (next_attempt_at)
    WHERE state = 'queued';
