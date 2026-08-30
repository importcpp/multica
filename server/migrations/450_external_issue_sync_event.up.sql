-- Durable webhook inbox: a webhook enqueues a row here and returns 2xx fast; a
-- worker later fetches the authoritative issue by id and runs the same Apply.
-- We persist only the pointer (source, delivery id, external issue id, remote
-- timestamp) plus lease/retry state — never the raw payload long-term, which may
-- be stale/out-of-order and carries no authority. delivery_id dedups redeliveries.
-- No FKs, no CHECK — per repository policy.
CREATE TABLE external_issue_sync_event (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    source_id UUID NOT NULL,
    delivery_id TEXT NOT NULL,
    external_issue_id TEXT NOT NULL,
    remote_updated_at TIMESTAMPTZ,
    state TEXT NOT NULL DEFAULT 'queued',
    attempt INT NOT NULL DEFAULT 0,
    worker_id TEXT NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
