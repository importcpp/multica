package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

func seedSyncWorkerFixture(t *testing.T, pages [][]map[string]any) (workspaceID, sourceID string, srv *httptest.Server, hits *int64) {
	t.Helper()
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())

	var userID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1,$2) RETURNING id`,
		"eis-worker", "eis-worker-"+suffix+"@test.local").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description) VALUES ($1,$2,'') RETURNING id`,
		"eis-worker-ws", "eis-w-"+suffix[:8]).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	var installationID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO github_installation (workspace_id, installation_id, account_login, account_type, connected_by_id)
		VALUES ($1, $2, 'acme', 'Organization', $3) RETURNING id`,
		workspaceID, time.Now().UnixNano()%1_000_000_000, userID).Scan(&installationID); err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO external_issue_source
			(workspace_id, provider, instance_key, credential_id, repository_external_id, repository_full_path, configured_by_user_id, filter)
		VALUES ($1,'github','github.com',$2,'555','acme/widgets',$3,'{"state":"open"}')
		RETURNING id`, workspaceID, installationID, userID).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_sync_run WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_link WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_source WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM issue WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM github_installation WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM workspace WHERE id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM "user" WHERE id=$1`, userID)
	})

	var counter int64
	hits = &counter
	// Fake GitHub REST: /repos/... for resolve (unused here since source is
	// pre-seeded), and the issues list endpoint returning the scripted pages.
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&counter, 1)
		page := 0
		if p := r.URL.Query().Get("p"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		w.Header().Set("Content-Type", "application/json")
		if page >= len(pages) {
			fmt.Fprint(w, `[]`)
			return
		}
		if page+1 < len(pages) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/widgets/issues?p=%d>; rel="next"`, srvBase(r), page+1))
		}
		writeIssuesJSON(w, pages[page])
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(externalissue.SetGitHubAPIBaseForTest(srv.URL))
	return workspaceID, sourceID, srv, hits
}

func srvBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func writeIssuesJSON(w http.ResponseWriter, issues []map[string]any) {
	fmt.Fprint(w, "[")
	for i, iss := range issues {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprintf(w, `{"id":%d,"number":%d,"title":%q,"state":"open","html_url":"h","updated_at":"2026-01-01T00:00:00Z"}`,
			iss["id"], iss["number"], iss["title"])
	}
	fmt.Fprint(w, "]")
}

func issue(id, number int, title string) map[string]any {
	return map[string]any{"id": id, "number": number, "title": title}
}

func queueRun(t *testing.T, workspaceID, sourceID string) string {
	t.Helper()
	var runID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO external_issue_sync_run (workspace_id, source_id, kind, state, filter_snapshot)
		VALUES ($1,$2,'backfill','queued','{"state":"open"}') RETURNING id`,
		workspaceID, sourceID).Scan(&runID); err != nil {
		t.Fatalf("queue run: %v", err)
	}
	return runID
}

func withFakeToken(t *testing.T) {
	t.Helper()
	prev := mintGitHubIssuesReadToken
	mintGitHubIssuesReadToken = func(ctx context.Context, installationID int64) (string, func(), error) {
		return "fake-token", func() {}, nil
	}
	t.Cleanup(func() { mintGitHubIssuesReadToken = prev })
}

func runState(t *testing.T, runID string) (state string, imported, total int64) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT state, imported_count, total_seen FROM external_issue_sync_run WHERE id=$1`, runID).
		Scan(&state, &imported, &total); err != nil {
		t.Fatalf("read run: %v", err)
	}
	return
}

func countWsIssues(t *testing.T, workspaceID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue WHERE workspace_id=$1`, workspaceID).Scan(&n); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	return n
}

func TestSyncWorkerDrainsPagesAndSucceeds(t *testing.T) {
	pages := [][]map[string]any{
		{issue(1, 1, "a"), issue(2, 2, "b")},
		{issue(3, 3, "c")},
	}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	w := NewExternalIssueSyncWorker(testHandler)
	// Drain to completion (each ProcessNext claims and drains up to the page
	// budget; two pages fit in one claim here).
	for i := 0; i < 5; i++ {
		worked, err := w.ProcessNext(context.Background())
		if err != nil {
			t.Fatalf("ProcessNext: %v", err)
		}
		if !worked {
			break
		}
		if st, _, _ := runState(t, runID); st == "succeeded" {
			break
		}
	}
	state, imported, total := runState(t, runID)
	if state != "succeeded" {
		t.Fatalf("run state = %q, want succeeded", state)
	}
	if imported != 3 || total != 3 {
		t.Fatalf("imported=%d total=%d, want 3/3", imported, total)
	}
	if n := countWsIssues(t, workspaceID); n != 3 {
		t.Fatalf("issue rows = %d, want 3", n)
	}
}

func TestSyncWorkerResumesFromCursorAfterInterruption(t *testing.T) {
	// Build more pages than one claim drains so the run is requeued mid-import,
	// then a second claim resumes from the persisted cursor.
	var pages [][]map[string]any
	id := 1
	for p := 0; p < syncRunPagesPerClaim+3; p++ {
		pages = append(pages, []map[string]any{issue(id, id, fmt.Sprintf("i%d", id))})
		id++
	}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	w := NewExternalIssueSyncWorker(testHandler)

	// First claim: drains the page budget, then requeues (still queued, not done).
	if _, err := w.ProcessNext(context.Background()); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	st, importedAfter1, _ := runState(t, runID)
	if st != "queued" {
		t.Fatalf("after first claim state = %q, want queued (requeued mid-import)", st)
	}
	if importedAfter1 != int64(syncRunPagesPerClaim) {
		t.Fatalf("after first claim imported = %d, want %d (one per page)", importedAfter1, syncRunPagesPerClaim)
	}

	// Subsequent claims finish it, resuming from the saved cursor with no dup.
	for i := 0; i < 5; i++ {
		if _, err := w.ProcessNext(context.Background()); err != nil {
			t.Fatalf("resume claim: %v", err)
		}
		if st, _, _ := runState(t, runID); st == "succeeded" {
			break
		}
	}
	state, imported, _ := runState(t, runID)
	if state != "succeeded" {
		t.Fatalf("final state = %q, want succeeded", state)
	}
	if imported != int64(len(pages)) {
		t.Fatalf("imported = %d, want %d", imported, len(pages))
	}
	if n := countWsIssues(t, workspaceID); n != len(pages) {
		t.Fatalf("issue rows = %d, want %d (cursor resume, no dup)", n, len(pages))
	}
}

func TestSyncWorkerHonorsCancel(t *testing.T) {
	pages := [][]map[string]any{{issue(1, 1, "a")}}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	// Request cancel before the worker claims it.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE external_issue_sync_run SET cancel_requested = true WHERE id=$1`, runID); err != nil {
		t.Fatalf("set cancel: %v", err)
	}
	w := NewExternalIssueSyncWorker(testHandler)
	if _, err := w.ProcessNext(context.Background()); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if state, _, _ := runState(t, runID); state != "cancelled" {
		t.Fatalf("run state = %q, want cancelled", state)
	}
	if n := countWsIssues(t, workspaceID); n != 0 {
		t.Fatalf("cancelled-before-drain should import nothing, got %d", n)
	}
}
