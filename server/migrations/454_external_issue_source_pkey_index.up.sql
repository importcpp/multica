-- Backing index for external_issue_source's primary key, attached in the next migration via
-- PRIMARY KEY USING INDEX. Own single-statement CONCURRENTLY migration per repo
-- convention (see migrations 327-329). IF NOT EXISTS covers the crash-after-
-- build retry; the INVALID leftover of an interrupted build is removed first by
-- the cleanup hook registered in cmd/migrate.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS external_issue_source_pkey_uidx ON external_issue_source (id);
