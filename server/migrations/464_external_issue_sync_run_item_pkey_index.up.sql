-- Backing index for the primary key, attached next migration; concurrent + own
-- single statement per repo rule.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS external_issue_sync_run_item_pkey_uidx
    ON external_issue_sync_run_item (id);
