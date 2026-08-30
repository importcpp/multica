-- Redelivery dedup: one row per (source, delivery id).
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_external_issue_sync_event_delivery
    ON external_issue_sync_event (source_id, delivery_id);
