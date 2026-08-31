-- Attach the CONCURRENTLY-built unique index as external_issue_sync_run's primary key.
ALTER TABLE external_issue_sync_run ADD CONSTRAINT external_issue_sync_run_pkey PRIMARY KEY USING INDEX external_issue_sync_run_pkey_uidx;
