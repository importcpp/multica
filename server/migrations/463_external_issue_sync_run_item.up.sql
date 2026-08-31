-- Per-run, per-remote-issue ledger. Each row records that one remote issue
-- (identified by its STABLE external id, not a page position) was accounted for
-- in a run, with the outcome. Counts are derived by aggregating this ledger, so
-- a resume that re-fetches a page whose membership shifted (issues closed /
-- reopened between pause and resume, changing sort order) can never skip an
-- unprocessed issue by array index or double-count an already-processed one:
-- the applier consults the ledger by external id.
--
-- No FK by repo policy; run/workspace cleanup deletes these rows explicitly.
CREATE TABLE external_issue_sync_run_item (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    run_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    external_issue_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
