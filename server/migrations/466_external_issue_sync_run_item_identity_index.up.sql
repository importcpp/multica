-- One ledger row per (run, remote issue). The applier upserts on this key, so a
-- re-fetched page never double-accounts an issue and derived counts stay exact.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_external_issue_sync_run_item_identity
    ON external_issue_sync_run_item (run_id, external_issue_id);
