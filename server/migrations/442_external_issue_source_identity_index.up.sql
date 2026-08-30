-- Dedup key: one source per (workspace, provider, instance, remote repo). Also
-- serves list-by-workspace since workspace_id leads. Concurrent + single
-- statement per repository migration rules.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_external_issue_source_identity
    ON external_issue_source (workspace_id, provider, instance_key, repository_external_id);
