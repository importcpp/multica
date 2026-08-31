-- Intra-page progress marker. When a page is interrupted mid-way (quota hit on
-- item N+1), the run saves the PAGE-START cursor plus the count of items already
-- accounted on that page (page_offset = N). On resume the worker re-fetches the
-- same page and SKIPS the first page_offset items without re-applying or
-- re-counting them, so counts stay exact across a resume instead of double-
-- counting or reclassifying imports as skips. Reset to 0 when a full page
-- commits (cursor advances).
ALTER TABLE external_issue_sync_run
    ADD COLUMN IF NOT EXISTS page_offset INT NOT NULL DEFAULT 0;
