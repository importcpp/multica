-- Fixpoint reconcile support for the issue-import worker. A page-cursor scan can
-- MISS an issue that shifts into an already-consumed page between claims (e.g. an
-- earlier issue closes, pushing a later one back a page that the worker already
-- passed). After the forward scan exhausts its cursor, the worker re-scans from
-- the start and repeats until a full pass discovers zero NEW ledger identities.
--
-- reconcile_baseline is the ledger total_seen recorded at the START of the
-- current scan pass, so the worker can tell, after a pass completes, whether that
-- pass surfaced any new issue. -1 = the initial forward scan (implicit baseline
-- 0); >= 0 = a reconcile pass with that starting total.
ALTER TABLE external_issue_sync_run
    ADD COLUMN IF NOT EXISTS reconcile_baseline BIGINT NOT NULL DEFAULT -1;
