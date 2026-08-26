-- Lower fillfactor on agent_runtime so heartbeat updates keep free space on
-- each heap page for HOT chains. With migration 432 removing the last_seen_at
-- index, the heartbeat UPDATE is HOT-eligible; leaving ~10% free space per page
-- lets the new row version stay on the same page instead of forcing a non-HOT
-- placement once a page fills. Existing pages adopt the setting as they are
-- updated or rewritten; no rewrite is forced here.
ALTER TABLE agent_runtime SET (fillfactor = 90);
