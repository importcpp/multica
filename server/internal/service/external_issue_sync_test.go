package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func newExternalIssuePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type applyFixture struct {
	pool        *pgxpool.Pool
	svc         *ExternalIssueSyncService
	queries     *db.Queries
	workspaceID pgtype.UUID
	sourceID    pgtype.UUID
	userID      pgtype.UUID
}

func seedApplyFixture(t *testing.T, pool *pgxpool.Pool) applyFixture {
	t.Helper()
	ctx := context.Background()
	var userID, workspaceID string
	suffix := util.UUIDToString(dbid.NewV7())
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"ext-issue-user", "ext-issue-"+suffix+"@test.local").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description) VALUES ($1, $2, '') RETURNING id`,
		"ext-issue-ws", "ext-issue-"+suffix[:8]).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	var sourceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO external_issue_source
			(workspace_id, provider, instance_key, repository_external_id, repository_full_path, configured_by_user_id)
		VALUES ($1, 'github', 'github.com', '999', 'o/r', $2)
		RETURNING id`, workspaceID, userID).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM external_issue_link WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(c, `DELETE FROM external_issue_source WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(c, `DELETE FROM issue WHERE workspace_id = $1`, workspaceID)
		_, _ = pool.Exec(c, `DELETE FROM workspace WHERE id = $1`, workspaceID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE id = $1`, userID)
	})

	q := db.New(pool)
	bus := events.New()
	issues := &IssueService{Queries: q, TxStarter: pool, Bus: bus}
	svc := NewExternalIssueSyncService(q, pool, bus, issues)
	return applyFixture{
		pool:        pool,
		svc:         svc,
		queries:     q,
		workspaceID: util.MustParseUUID(workspaceID),
		sourceID:    util.MustParseUUID(sourceID),
		userID:      util.MustParseUUID(userID),
	}
}

func (f applyFixture) remote(externalID string, number int64, title, body string, updated time.Time) RemoteIssue {
	return RemoteIssue{
		ExternalID:  externalID,
		Provider:    "github",
		InstanceKey: "github.com",
		Number:      number,
		Title:       title,
		Body:        body,
		State:       "open",
		HTMLURL:     "https://github.com/o/r/issues/1",
		UpdatedAt:   updated,
	}
}

func (f applyFixture) params(r RemoteIssue) ApplyParams {
	return ApplyParams{
		WorkspaceID: f.workspaceID,
		SourceID:    f.sourceID,
		CreatorID:   f.userID,
		Remote:      r,
	}
}

func TestApplyImportsThenUpdatesWithoutDuplicate(t *testing.T) {
	pool := newExternalIssuePool(t)
	f := seedApplyFixture(t, pool)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)

	// First apply imports.
	out, err := f.svc.Apply(ctx, f.params(f.remote("100", 1, "Title A", "Body A", t0)))
	if err != nil || out != OutcomeImported {
		t.Fatalf("first apply = (%v, %v), want imported", out, err)
	}
	if n := f.countIssues(t); n != 1 {
		t.Fatalf("after import, issue count = %d, want 1", n)
	}

	// Re-apply the SAME content is a no-op skip, not a duplicate.
	out, err = f.svc.Apply(ctx, f.params(f.remote("100", 1, "Title A", "Body A", t0)))
	if err != nil || out != OutcomeSkipped {
		t.Fatalf("identical re-apply = (%v, %v), want skipped", out, err)
	}
	if n := f.countIssues(t); n != 1 {
		t.Fatalf("after identical re-apply, issue count = %d, want 1", n)
	}

	// Remote title change updates the SAME issue, no duplicate.
	out, err = f.svc.Apply(ctx, f.params(f.remote("100", 1, "Title A2", "Body A", t0.Add(time.Minute))))
	if err != nil || out != OutcomeUpdated {
		t.Fatalf("remote-change apply = (%v, %v), want updated", out, err)
	}
	if n := f.countIssues(t); n != 1 {
		t.Fatalf("after update, issue count = %d, want 1", n)
	}
	if title := f.issueTitle(t); title != "Title A2" {
		t.Fatalf("issue title = %q, want %q", title, "Title A2")
	}
}

func TestApplyKeepsLocalEditAndFlagsConflict(t *testing.T) {
	pool := newExternalIssuePool(t)
	f := seedApplyFixture(t, pool)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)

	if _, err := f.svc.Apply(ctx, f.params(f.remote("200", 2, "Orig", "Body", t0))); err != nil {
		t.Fatalf("import: %v", err)
	}
	// User edits the title locally.
	link, err := f.queries.GetExternalIssueLinkByIdentity(ctx, db.GetExternalIssueLinkByIdentityParams{
		WorkspaceID: f.workspaceID, Provider: "github", InstanceKey: "github.com", ExternalIssueID: "200",
	})
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE issue SET title = 'Locally Edited' WHERE id = $1`, link.IssueID); err != nil {
		t.Fatalf("local edit: %v", err)
	}

	// Remote ALSO changes the title -> conflict, local wins, flagged.
	out, err := f.svc.Apply(ctx, f.params(f.remote("200", 2, "Remote Changed", "Body", t0.Add(time.Minute))))
	if err != nil || out != OutcomeConflict {
		t.Fatalf("conflicting apply = (%v, %v), want conflict", out, err)
	}
	if title := f.issueTitle(t); title != "Locally Edited" {
		t.Fatalf("issue title = %q, want local edit preserved", title)
	}
	link, _ = f.queries.GetExternalIssueLinkByIdentity(ctx, db.GetExternalIssueLinkByIdentityParams{
		WorkspaceID: f.workspaceID, Provider: "github", InstanceKey: "github.com", ExternalIssueID: "200",
	})
	if !link.TitleConflict {
		t.Fatal("link.title_conflict should be true")
	}
}

func TestApplyOutOfOrderEventDoesNotRegress(t *testing.T) {
	pool := newExternalIssuePool(t)
	f := seedApplyFixture(t, pool)
	ctx := context.Background()
	t1 := time.Now().Add(-time.Hour)

	if _, err := f.svc.Apply(ctx, f.params(f.remote("300", 3, "New", "Body", t1))); err != nil {
		t.Fatalf("import: %v", err)
	}
	// Update to newer content at t1+1m.
	if _, err := f.svc.Apply(ctx, f.params(f.remote("300", 3, "Newer", "Body", t1.Add(time.Minute)))); err != nil {
		t.Fatalf("update: %v", err)
	}
	// A STALE event at t1 must not overwrite the newer title.
	out, err := f.svc.Apply(ctx, f.params(f.remote("300", 3, "Stale", "Body", t1)))
	if err != nil || out != OutcomeSkipped {
		t.Fatalf("stale apply = (%v, %v), want skipped", out, err)
	}
	if title := f.issueTitle(t); title != "Newer" {
		t.Fatalf("issue title = %q, want %q (stale must not regress)", title, "Newer")
	}
}

func TestApplyTombstoneNotResurrected(t *testing.T) {
	pool := newExternalIssuePool(t)
	f := seedApplyFixture(t, pool)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)

	if _, err := f.svc.Apply(ctx, f.params(f.remote("400", 4, "T", "B", t0))); err != nil {
		t.Fatalf("import: %v", err)
	}
	// Delete the issue through the real DeleteIssue query, which must tombstone
	// the link (clear issue_id, stamp local_deleted_at) rather than drop it.
	link, err := f.queries.GetExternalIssueLinkByIdentity(ctx, db.GetExternalIssueLinkByIdentityParams{
		WorkspaceID: f.workspaceID, Provider: "github", InstanceKey: "github.com", ExternalIssueID: "400",
	})
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	if err := f.queries.DeleteIssue(ctx, db.DeleteIssueParams{ID: link.IssueID, WorkspaceID: f.workspaceID}); err != nil {
		t.Fatalf("delete issue: %v", err)
	}
	// The link must survive as a tombstone.
	link, err = f.queries.GetExternalIssueLinkByIdentity(ctx, db.GetExternalIssueLinkByIdentityParams{
		WorkspaceID: f.workspaceID, Provider: "github", InstanceKey: "github.com", ExternalIssueID: "400",
	})
	if err != nil {
		t.Fatalf("link should survive delete as a tombstone: %v", err)
	}
	if link.IssueID.Valid || !link.LocalDeletedAt.Valid {
		t.Fatalf("link should be tombstoned (issue_id cleared, local_deleted_at set): %+v", link)
	}

	// A later sync must NOT resurrect the deleted issue.
	out, err := f.svc.Apply(ctx, f.params(f.remote("400", 4, "T", "B", t0.Add(time.Minute))))
	if err != nil || out != OutcomeSkipped {
		t.Fatalf("tombstoned apply = (%v, %v), want skipped", out, err)
	}
	if n := f.countIssues(t); n != 0 {
		t.Fatalf("tombstoned issue must not be resurrected, count = %d", n)
	}
}

func (f applyFixture) countIssues(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue WHERE workspace_id = $1`, f.workspaceID).Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	return n
}

func (f applyFixture) issueTitle(t *testing.T) string {
	t.Helper()
	var title string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT title FROM issue WHERE workspace_id = $1 LIMIT 1`, f.workspaceID).Scan(&title); err != nil {
		t.Fatalf("issue title: %v", err)
	}
	return title
}

// A local edit that commits WHILE an Apply update is mid-flight must not be
// silently lost: the row lock forces the concurrent editor to serialize after
// Apply, and because Apply took the older baseline it records a conflict and
// keeps the (now newer) local value rather than overwriting it. This exercises
// the FOR UPDATE lock in updateBoundIssue (P0-1).
func TestApplyConcurrentLocalEditNotLost(t *testing.T) {
	pool := newExternalIssuePool(t)
	f := seedApplyFixture(t, pool)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)

	if _, err := f.svc.Apply(ctx, f.params(f.remote("700", 7, "Orig", "Body", t0))); err != nil {
		t.Fatalf("import: %v", err)
	}
	link, err := f.queries.GetExternalIssueLinkByIdentity(ctx, db.GetExternalIssueLinkByIdentityParams{
		WorkspaceID: f.workspaceID, Provider: "github", InstanceKey: "github.com", ExternalIssueID: "700",
	})
	if err != nil {
		t.Fatalf("get link: %v", err)
	}

	// Hold the issue row lock in a separate tx, start Apply (which will block on
	// LockIssueForDescriptionUpdate), then commit a local title edit and release
	// the lock. Apply must then observe the local change and not overwrite it.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM issue WHERE id=$1 FOR UPDATE`, link.IssueID); err != nil {
		t.Fatalf("lock row: %v", err)
	}

	applyDone := make(chan ApplyOutcome, 1)
	applyErr := make(chan error, 1)
	go func() {
		out, err := f.svc.Apply(ctx, f.params(f.remote("700", 7, "Remote Wins?", "Body", t0.Add(time.Minute))))
		applyDone <- out
		applyErr <- err
	}()

	// Give Apply time to reach the lock and block on it.
	time.Sleep(300 * time.Millisecond)
	// Commit a local edit while Apply is blocked, then release the lock.
	if _, err := tx.Exec(ctx, `UPDATE issue SET title='Locally Edited', revision=revision+1 WHERE id=$1`, link.IssueID); err != nil {
		t.Fatalf("local edit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit local edit: %v", err)
	}

	out := <-applyDone
	if err := <-applyErr; err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out != OutcomeConflict {
		t.Fatalf("apply outcome = %v, want conflict (local edit + remote change)", out)
	}
	if title := f.issueTitle(t); title != "Locally Edited" {
		t.Fatalf("local edit was overwritten: title = %q", title)
	}
}
