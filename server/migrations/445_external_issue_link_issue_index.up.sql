-- Reverse lookup for the DeleteIssue tombstone sweep and issue-detail source
-- badge. Partial: only links still bound to a live issue.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_external_issue_link_issue
    ON external_issue_link (workspace_id, issue_id) WHERE issue_id IS NOT NULL;
