-- List-by-source for reconcile and per-source cleanup.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_external_issue_link_source
    ON external_issue_link (source_id);
