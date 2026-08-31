-- Attach the CONCURRENTLY-built unique index as external_issue_source's primary key.
ALTER TABLE external_issue_source ADD CONSTRAINT external_issue_source_pkey PRIMARY KEY USING INDEX external_issue_source_pkey_uidx;
