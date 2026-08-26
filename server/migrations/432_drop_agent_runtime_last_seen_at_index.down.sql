-- Recreate the plain btree index on agent_runtime.last_seen_at (migration 115).
-- Single-statement CONCURRENTLY per repo convention.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_runtime_last_seen_at
    ON agent_runtime (last_seen_at);
