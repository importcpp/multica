-- Attach the CONCURRENTLY-built unique index as external_issue_sync_event's primary key.
ALTER TABLE external_issue_sync_event ADD CONSTRAINT external_issue_sync_event_pkey PRIMARY KEY USING INDEX external_issue_sync_event_pkey_uidx;
