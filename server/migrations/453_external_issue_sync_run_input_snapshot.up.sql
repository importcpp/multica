-- Immutable execution inputs captured at enqueue time. The worker reads these
-- instead of re-reading the (mutable) external_issue_source on every claim, so a
-- second import request that changes the source's credential / target project /
-- filter mid-run cannot silently redirect an in-flight run. JSON shape:
--   {"credential_id","provider","instance_key","repository_external_id",
--    "repository_full_path","target_project_id","state"}
ALTER TABLE external_issue_sync_run
    ADD COLUMN IF NOT EXISTS input_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb;
