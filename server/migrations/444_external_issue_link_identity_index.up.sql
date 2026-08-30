-- Dedup key: one Multica issue per remote issue, on stable identity (never
-- owner/name/number). This is the unique index Apply relies on to make
-- concurrent claims race-safe.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_external_issue_link_identity
    ON external_issue_link (workspace_id, provider, instance_key, external_issue_id);
