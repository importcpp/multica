-- Attach the CONCURRENTLY-built unique index as external_issue_link's primary key.
ALTER TABLE external_issue_link ADD CONSTRAINT external_issue_link_pkey PRIMARY KEY USING INDEX external_issue_link_pkey_uidx;
