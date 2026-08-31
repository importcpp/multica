-- Provider-neutral configuration of one "pull issues from an external tracker"
-- source: which provider/instance/repository, under which credential, into
-- which Multica project, with what filter. One row per (workspace, provider,
-- instance, remote repository); reselecting the same repo in another project
-- reuses this row rather than creating a competitor.
--
-- No FKs by repository policy: credential_id is a mutable binding (disconnect
-- rebinds/clears it, it does not delete the source), and issue/project/workspace
-- lifecycle cleanup is done explicitly in application code. provider and state
-- are validated in the application, not by a CHECK, so a new provider or state
-- ships without a migration.
--
-- instance_key is the normalized, validated platform origin ("github.com", or a
-- validated GitLab instance host) — never a raw user-supplied URL.
-- repository_external_id is the provider's stable numeric id, so a repository
-- rename or transfer does not orphan the source; repository_full_path is the
-- mutable display path.
CREATE TABLE external_issue_source (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    provider TEXT NOT NULL,
    instance_key TEXT NOT NULL,
    credential_id UUID,
    repository_external_id TEXT NOT NULL,
    repository_full_path TEXT NOT NULL,
    target_project_id UUID,
    filter JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(filter) = 'object'),
    mode TEXT NOT NULL DEFAULT 'manual',
    state TEXT NOT NULL DEFAULT 'active',
    configured_by_user_id UUID,
    last_reconciled_at TIMESTAMPTZ,
    next_reconcile_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
