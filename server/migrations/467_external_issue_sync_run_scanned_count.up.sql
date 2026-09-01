-- Progress visibility for the value-anchored superset scan. total_seen counts
-- only issues that MATCHED the requested state (what will be imported); a
-- state=open import of a huge repo still walks every closed issue, so total_seen
-- can sit near 0 for a long time. scanned_count records how many remote issues
-- the worker has actually visited (matched or filtered), so the UI can show real
-- forward progress instead of an apparently-stuck 0.
ALTER TABLE external_issue_sync_run
    ADD COLUMN IF NOT EXISTS scanned_count BIGINT NOT NULL DEFAULT 0;
