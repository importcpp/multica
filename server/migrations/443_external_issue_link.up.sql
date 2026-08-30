-- Authoritative mapping between one remote issue and its Multica issue, plus the
-- sync baseline used for three-way conflict detection. This table — not
-- owner/repo/number — is the dedup source of truth, keyed on the provider's
-- stable issue id so a repo rename/transfer or credential reconnect never makes
-- a duplicate.
--
-- issue_id is nullable: a local delete clears it and stamps local_deleted_at as
-- a tombstone so a later reconcile does NOT resurrect the issue (deleting the
-- link would). source_id is a mutable pointer (a moved repo can rebind).
-- title_baseline_hash / body_baseline_hash record the content at the last sync
-- so the applier can tell "only remote changed" from "local edited" per field
-- and never silently clobber a local edit. No FKs by repository policy.
CREATE TABLE external_issue_link (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    provider TEXT NOT NULL,
    instance_key TEXT NOT NULL,
    external_issue_id TEXT NOT NULL,
    source_id UUID,
    issue_id UUID,
    display_number BIGINT NOT NULL DEFAULT 0,
    external_html_url TEXT NOT NULL DEFAULT '',
    remote_state TEXT NOT NULL DEFAULT '',
    remote_updated_at TIMESTAMPTZ,
    title_baseline_hash TEXT NOT NULL DEFAULT '',
    body_baseline_hash TEXT NOT NULL DEFAULT '',
    title_conflict BOOLEAN NOT NULL DEFAULT false,
    body_conflict BOOLEAN NOT NULL DEFAULT false,
    title_local_owned BOOLEAN NOT NULL DEFAULT false,
    body_local_owned BOOLEAN NOT NULL DEFAULT false,
    moved BOOLEAN NOT NULL DEFAULT false,
    local_deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
