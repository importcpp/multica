package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
	"github.com/multica-ai/multica/server/internal/integrations/githubapi"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// errorsIs is a tiny alias so the test reads cleanly next to the worker's
// sentinel error.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

func claimParams(w *ExternalIssueSyncWorker) db.ClaimNextExternalIssueSyncRunParams {
	return db.ClaimNextExternalIssueSyncRunParams{
		WorkerID:       w.id,
		LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(2 * time.Minute), Valid: true},
	}
}

func resumeParams(workspaceID, runID string) db.ResumeExternalIssueSyncRunParams {
	return db.ResumeExternalIssueSyncRunParams{
		ID:            util.MustParseUUID(runID),
		WorkspaceID:   util.MustParseUUID(workspaceID),
		InputSnapshot: []byte(`{"credential_id":"","provider":"github","instance_key":"github.com","repository_external_id":"555","repository_full_path":"acme/widgets","state":"open"}`),
	}
}

// resumeParamsForSource rebuilds the run's input snapshot from the seeded
// source's real credential, mirroring what the Resume handler does so the
// resumed run has a valid credential for the worker.
func resumeParamsForSource(workspaceID, runID, sourceID string) db.ResumeExternalIssueSyncRunParams {
	var credentialID, configuredBy string
	_ = testPool.QueryRow(context.Background(),
		`SELECT COALESCE(credential_id::text,''), COALESCE(configured_by_user_id::text,'') FROM external_issue_source WHERE id=$1`, sourceID).
		Scan(&credentialID, &configuredBy)
	snap := fmt.Sprintf(`{"credential_id":%q,"provider":"github","instance_key":"github.com","repository_external_id":"555","repository_full_path":"acme/widgets","configured_by_user_id":%q,"state":"open"}`, credentialID, configuredBy)
	return db.ResumeExternalIssueSyncRunParams{
		ID:            util.MustParseUUID(runID),
		WorkspaceID:   util.MustParseUUID(workspaceID),
		InputSnapshot: []byte(snap),
	}
}

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
	// Register cleanup immediately after each successful insert so a later insert
	// failing (e.g. missing table on an un-migrated DB) can't leak this row.
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description) VALUES ($1,$2,'') RETURNING id`,
		"eis-worker-ws", "eis-w-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_sync_run_item WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_sync_run WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_link WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_source WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM issue WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM github_installation WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM workspace WHERE id=$1`, workspaceID)
	})
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

	var counter int64
	hits = &counter
	// Fake GitHub REST: /repos/... for resolve (unused here since source is
	// pre-seeded), and the issues list endpoint returning issues under the
	// adapter's value-anchored keyset. per_page is forced to 1 so each fixture
	// issue is its own page, preserving the cross-claim pagination the worker
	// tests exercise.
	t.Cleanup(externalissue.SetMaxPerPageForTest(1))
	flat := flattenPages(pages)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&counter, 1)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/repos/") && !strings.Contains(r.URL.Path, "/issues") {
			fmt.Fprint(w, `{"id":555,"full_name":"acme/widgets","name":"widgets"}`)
			return
		}
		writeKeysetIssuesPage(w, r, flat)
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

// issueUpdatedAt returns a per-issue updated_at, distinct and increasing by the
// issue's numeric id so the keyset (sort=updated&direction=asc) enumeration has a
// stable total order. A fixture may override via the "updated_at" key.
func issueUpdatedAt(iss map[string]any) string {
	if s, ok := iss["updated_at"].(string); ok && s != "" {
		return s
	}
	id := 0
	switch v := iss["id"].(type) {
	case int:
		id = v
	}
	// Base epoch + id seconds; id is small in tests so this stays well-formed.
	return time.Date(2026, 1, 1, 0, 0, id, 0, time.UTC).Format(time.RFC3339)
}

func issueState(iss map[string]any) string {
	if s, ok := iss["state"].(string); ok && s != "" {
		return s
	}
	return "open"
}

// writeKeysetIssuesPage emulates the GitHub issues list under the adapter's
// value-anchored keyset: it filters the fixture issues to updated_at > since
// (GitHub `since` is EXCLUSIVE — "updated after"), sorts ascending, and returns
// the per_page slice for the requested page. This is what makes the fixture
// faithful to the real delete/transfer-safe enumeration (a removed issue simply
// drops out of the filtered set).
func writeKeysetIssuesPage(w http.ResponseWriter, r *http.Request, issues []map[string]any) {
	perPage := 100
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		since, _ = time.Parse(time.RFC3339, v)
	}
	// Filter to updated_at > since (exclusive), keeping fixture order (id-ascending
	// = updated_at-ascending by issueUpdatedAt).
	var matched []map[string]any
	for _, iss := range issues {
		t, _ := time.Parse(time.RFC3339, issueUpdatedAt(iss))
		if since.IsZero() || t.After(since) {
			matched = append(matched, iss)
		}
	}
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(matched) {
		start = len(matched)
	}
	if end > len(matched) {
		end = len(matched)
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, "[")
	for i, iss := range matched[start:end] {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprintf(w, `{"id":%d,"number":%d,"title":%q,"state":%q,"html_url":"h","updated_at":%q}`,
			iss["id"], iss["number"], iss["title"], issueState(iss), issueUpdatedAt(iss))
	}
	fmt.Fprint(w, "]")
}

// flattenPages collapses the legacy [][]page fixture shape into a flat issue list
// for the keyset responder. Page boundaries no longer matter to the value-anchored
// enumeration; per_page is controlled by SetMaxPerPageForTest.
func flattenPages(pages [][]map[string]any) []map[string]any {
	var out []map[string]any
	for _, p := range pages {
		out = append(out, p...)
	}
	return out
}

func issue(id, number int, title string) map[string]any {
	return map[string]any{"id": id, "number": number, "title": title}
}

func queueRun(t *testing.T, workspaceID, sourceID string) string {
	return queueRunState(t, workspaceID, sourceID, "open")
}

func queueRunState(t *testing.T, workspaceID, sourceID, state string) string {
	t.Helper()
	// Build the run input snapshot from the seeded source, mirroring what the
	// import handler captures at enqueue time.
	var credentialID, repoExternalID, repoFullPath, configuredBy string
	if err := testPool.QueryRow(context.Background(),
		`SELECT credential_id, repository_external_id, repository_full_path, COALESCE(configured_by_user_id::text,'')
		   FROM external_issue_source WHERE id=$1`, sourceID).
		Scan(&credentialID, &repoExternalID, &repoFullPath, &configuredBy); err != nil {
		t.Fatalf("read source for snapshot: %v", err)
	}
	snapshot := fmt.Sprintf(
		`{"credential_id":%q,"provider":"github","instance_key":"github.com","repository_external_id":%q,"repository_full_path":%q,"configured_by_user_id":%q,"state":%q}`,
		credentialID, repoExternalID, repoFullPath, configuredBy, state)
	var runID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO external_issue_sync_run (workspace_id, source_id, kind, state, filter_snapshot, input_snapshot)
		VALUES ($1,$2,'backfill','queued',$3::jsonb,$4::jsonb) RETURNING id`,
		workspaceID, sourceID, fmt.Sprintf(`{"state":%q}`, state), snapshot).Scan(&runID); err != nil {
		t.Fatalf("queue run: %v", err)
	}
	return runID
}

func withFakeToken(t *testing.T) {
	t.Helper()
	prev := mintGitHubIssuesReadToken
	mintGitHubIssuesReadToken = func(ctx context.Context, app *githubapi.Client, installationID int64) (string, func(), error) {
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
	// Progress was made but the run did not finish in one claim. (Exact per-claim
	// count is not asserted: the value-anchored keyset re-fetches the inclusive
	// `since` boundary issue each page — negligible at per_page=100, but at the
	// test's per_page=1 it means fewer than one NEW import per page.)
	if importedAfter1 <= 0 || importedAfter1 >= int64(len(pages)) {
		t.Fatalf("after first claim imported = %d, want progress in (0, %d)", importedAfter1, len(pages))
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

// A worker whose lease was stolen must not advance/finish the run: its fenced
// writes update zero rows and it aborts without corrupting the new owner's run.
// A stale worker whose lease was stolen (or whose run was cancelled) must not
// create any issue: the fence lives INSIDE Apply's transaction, so an Apply from
// the former owner rolls back and returns ErrRunFenced, creating nothing. This
// is the real takeover codex56's P0 reproduction exercised — not a direct
// advance() call.
func TestSyncWorkerApplyFenceStopsStaleWriter(t *testing.T) {
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, [][]map[string]any{{issue(1, 1, "a")}})
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	stale := NewExternalIssueSyncWorker(testHandler)
	claimed, err := stale.h.Queries.ClaimNextExternalIssueSyncRun(context.Background(), claimParams(stale))
	if err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	// New owner steals the lease AND the run is cancelled — the two conditions
	// the fence must catch, applied between claim and the first Apply.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE external_issue_sync_run SET worker_id='new-owner', cancel_requested=true WHERE id=$1`, runID); err != nil {
		t.Fatalf("steal lease + cancel: %v", err)
	}
	// The stale worker's Apply must be fenced and create nothing.
	_, applyErr := testHandler.ExternalIssueSync.Apply(context.Background(), service.ApplyParams{
		WorkspaceID: claimed.WorkspaceID,
		SourceID:    claimed.SourceID,
		Remote: service.RemoteIssue{
			ExternalID: "1", Provider: "github", InstanceKey: "github.com",
			Number: 1, Title: "a", State: "open",
		},
		RunID:    claimed.ID,
		WorkerID: stale.id,
	})
	if !errorsIs(applyErr, service.ErrRunFenced) {
		t.Fatalf("stale Apply err = %v, want ErrRunFenced", applyErr)
	}
	if n := countWsIssues(t, workspaceID); n != 0 {
		t.Fatalf("fenced Apply must create nothing, got %d issues", n)
	}
	// Ledger stayed empty too (nothing accounted).
	var items int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM external_issue_sync_run_item WHERE run_id=$1`, runID).Scan(&items); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if items != 0 {
		t.Fatalf("fenced Apply must not write a ledger row, got %d", items)
	}
}

// A quota_blocked run resumes from its saved cursor rather than starting over.
func TestSyncWorkerQuotaBlockedResumes(t *testing.T) {
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, [][]map[string]any{{issue(1, 1, "a")}})
	runID := queueRun(t, workspaceID, sourceID)
	// Put the run in quota_blocked with a saved cursor and a partial count.
	if _, err := testPool.Exec(context.Background(), `
		UPDATE external_issue_sync_run
		SET state='quota_blocked', cursor='saved-cursor', imported_count=5, total_seen=5
		WHERE id=$1`, runID); err != nil {
		t.Fatalf("set quota_blocked: %v", err)
	}
	resumed, err := testHandler.Queries.ResumeExternalIssueSyncRun(context.Background(),
		resumeParams(workspaceID, runID))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.State != "queued" {
		t.Fatalf("resumed state = %q, want queued", resumed.State)
	}
	if resumed.Cursor != "saved-cursor" {
		t.Fatalf("resume must preserve cursor, got %q", resumed.Cursor)
	}
	if resumed.ImportedCount != 5 {
		t.Fatalf("resume must preserve counts, imported = %d", resumed.ImportedCount)
	}
}

// limitProvider is a fake entitlement.Provider that enforces a fixed issue
// limit, so a test can deterministically drive Apply into quota_blocked.
type limitProvider struct{ limit int }

func (p limitProvider) Gate(ctx context.Context, workspaceID uuid.UUID, name entitlement.GateName) entitlement.Decision {
	lim := p.limit
	return entitlement.Decision{Gate: entitlement.Gate{Action: entitlement.ActionEnforce, Limit: &lim}}
}

// Real quota resume: a page with 2 unique remote issues under a limit of 1 must
// import the first, hit quota on the second (quota_blocked), and after resume
// import the second WITHOUT re-counting the first. Regression for the round-2
// double-count reproduction (final should be imported=2, total=2, not
// imported=2/skipped=1/total=3).
func TestSyncWorkerQuotaResumeNoDoubleCount(t *testing.T) {
	pages := [][]map[string]any{{issue(1, 1, "a"), issue(2, 2, "b")}}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	// Build a worker whose sync service enforces a 1-issue quota.
	q := db.New(testPool)
	bus := events.New()
	issues := &service.IssueService{Queries: q, TxStarter: testPool, Bus: bus, Entitlements: limitProvider{limit: 1}}
	sync := service.NewExternalIssueSyncService(q, testPool, bus, issues)
	h := &Handler{Queries: q, ExternalIssueSync: sync}
	w := NewExternalIssueSyncWorker(h)

	// First drain: import #1, quota on #2 -> quota_blocked, one issue in DB.
	if _, err := w.ProcessNext(context.Background()); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if st, imported, total := runState(t, runID); st != "quota_blocked" || imported != 1 || total != 1 {
		t.Fatalf("after quota: state=%q imported=%d total=%d, want quota_blocked/1/1", st, imported, total)
	}
	if n := countWsIssues(t, workspaceID); n != 1 {
		t.Fatalf("after quota, issue rows = %d, want 1", n)
	}

	// Raise the limit and resume: the re-fetched page must skip the accounted
	// prefix (#1) and import only #2, ending succeeded with exact counts. Rebuild
	// the run's input snapshot from the source (as the Resume handler does) so
	// the worker still has a valid credential.
	issues.Entitlements = limitProvider{limit: 100}
	sync.Entitlements = limitProvider{limit: 100}
	if _, err := q.ResumeExternalIssueSyncRun(context.Background(), resumeParamsForSource(workspaceID, runID, sourceID)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := w.ProcessNext(context.Background()); err != nil {
			t.Fatalf("resume drain: %v", err)
		}
		if st, _, _ := runState(t, runID); st == "succeeded" {
			break
		}
	}
	st, imported, total := runState(t, runID)
	if st != "succeeded" || imported != 2 || total != 2 {
		var sample string
		_ = testPool.QueryRow(context.Background(), `SELECT error_sample::text FROM external_issue_sync_run WHERE id=$1`, runID).Scan(&sample)
		t.Fatalf("after resume: state=%q imported=%d total=%d, want succeeded/2/2 (no double-count); errors=%s", st, imported, total, sample)
	}
	if n := countWsIssues(t, workspaceID); n != 2 {
		t.Fatalf("final issue rows = %d, want 2", n)
	}
}

// needs_reauth resume must rebind the credential from the current source. We
// simulate a reconnect by pointing the run's snapshot at a DEAD credential
// UUID, marking the run needs_reauth, then resuming with a snapshot rebuilt from
// the (live) source. The resumed run must drain against the live credential
// instead of looping back to needs_reauth. Regression for the round-2 reauth
// reproduction.
func TestSyncWorkerReauthResumeRebindsCredential(t *testing.T) {
	pages := [][]map[string]any{{issue(1, 1, "a")}}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	// Simulate a needs_reauth stop whose snapshot names a now-dead credential.
	deadCred := util.UUIDToString(dbid.NewV7())
	deadSnap := fmt.Sprintf(`{"credential_id":%q,"provider":"github","instance_key":"github.com","repository_external_id":"555","repository_full_path":"acme/widgets","state":"open"}`, deadCred)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE external_issue_sync_run SET state='needs_reauth', input_snapshot=$2::jsonb WHERE id=$1`,
		runID, deadSnap); err != nil {
		t.Fatalf("set needs_reauth: %v", err)
	}

	w := NewExternalIssueSyncWorker(testHandler)
	// Resume rebuilds the snapshot from the source's live credential.
	if _, err := testHandler.Queries.ResumeExternalIssueSyncRun(context.Background(),
		resumeParamsForSource(workspaceID, runID, sourceID)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := w.ProcessNext(context.Background()); err != nil {
			t.Fatalf("resume drain: %v", err)
		}
		if st, _, _ := runState(t, runID); st == "succeeded" {
			break
		}
	}
	if st, imported, _ := runState(t, runID); st != "succeeded" || imported != 1 {
		t.Fatalf("after reauth resume: state=%q imported=%d, want succeeded/1 (rebound credential)", st, imported)
	}
}

// Mutable page membership across a resume: page [A,B] with quota=1 imports A,
// hits quota on B; during the pause A leaves the result set and the page becomes
// [B,C]. Positional skipping would drop B; the identity ledger keys on the stable
// external id, so resume imports B and C and never skips an unprocessed issue.
// Regression for codex56's P0-2 reproduction.
func TestSyncWorkerMutablePageResumeNoSkip(t *testing.T) {
	// A dynamic fake whose page contents we can change between drains.
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID, workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(name,email) VALUES($1,$2) RETURNING id`,
		"eis-mut", "eis-mut-"+suffix+"@test.local").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace(name,slug,description) VALUES($1,$2,'') RETURNING id`,
		"eis-mut-ws", "eis-mut-"+suffix[:8]).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_sync_run_item WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_sync_run WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_link WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_source WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM issue WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM github_installation WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM workspace WHERE id=$1`, workspaceID)
	})
	var installationID string
	if err := testPool.QueryRow(ctx, `INSERT INTO github_installation(workspace_id,installation_id,account_login,account_type,connected_by_id)
		VALUES($1,$2,'acme','Organization',$3) RETURNING id`,
		workspaceID, time.Now().UnixNano()%1_000_000_000, userID).Scan(&installationID); err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	var sourceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_source
		(workspace_id,provider,instance_key,credential_id,repository_external_id,repository_full_path,configured_by_user_id,filter)
		VALUES($1,'github','github.com',$2,'555','acme/widgets',$3,'{"state":"open"}') RETURNING id`,
		workspaceID, installationID, userID).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	// The remote set flips after the first drain: [A,B] -> [B,C] (A leaves, C
	// appears). Served under the value-anchored keyset with per_page=1 so B and C
	// each get their own page.
	t.Cleanup(externalissue.SetMaxPerPageForTest(1))
	live := []map[string]any{issue(101, 101, "A"), issue(102, 102, "B")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repos/") && !strings.Contains(r.URL.Path, "/issues") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":555,"full_name":"acme/widgets","name":"widgets"}`)
			return
		}
		writeKeysetIssuesPage(w, r, live)
	}))
	defer srv.Close()
	defer externalissue.SetGitHubAPIBaseForTest(srv.URL)()
	withFakeToken(t)

	runID := queueRun(t, workspaceID, sourceID)

	q := db.New(testPool)
	bus := events.New()
	issues := &service.IssueService{Queries: q, TxStarter: testPool, Bus: bus, Entitlements: limitProvider{limit: 1}}
	sync := service.NewExternalIssueSyncService(q, testPool, bus, issues)
	h := &Handler{Queries: q, ExternalIssueSync: sync}
	w := NewExternalIssueSyncWorker(h)

	// First drain: import A, quota on B.
	if _, err := w.ProcessNext(ctx); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if st, imported, _ := runState(t, runID); st != "quota_blocked" || imported != 1 {
		t.Fatalf("after quota: state=%q imported=%d, want quota_blocked/1", st, imported)
	}

	// A leaves the set; remote becomes [B,C]. Raise limit, resume.
	live = []map[string]any{issue(102, 102, "B"), issue(103, 103, "C")}
	issues.Entitlements = limitProvider{limit: 100}
	sync.Entitlements = limitProvider{limit: 100}
	if _, err := q.ResumeExternalIssueSyncRun(ctx, resumeParamsForSource(workspaceID, runID, sourceID)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := w.ProcessNext(ctx); err != nil {
			t.Fatalf("resume drain: %v", err)
		}
		if st, _, _ := runState(t, runID); st == "succeeded" {
			break
		}
	}
	// B must NOT be skipped: A, B, C all imported (3 issues), state succeeded.
	if n := countWsIssues(t, workspaceID); n != 3 {
		t.Fatalf("issue rows = %d, want 3 (A,B,C — B not skipped by position)", n)
	}
	if st, imported, _ := runState(t, runID); st != "succeeded" || imported != 3 {
		t.Fatalf("final: state=%q imported=%d, want succeeded/3", st, imported)
	}
}

// Resume must preserve the run's ORIGINAL target project even when the source
// was repointed at a different project between pause and resume — driven through
// the real ResumeSyncRun handler (not raw params). Regression for codex56 P0-3.
func TestResumeSyncRunPreservesOriginalProject(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, [][]map[string]any{{issue(1, 1, "a")}})
	withFakeToken(t)

	// Two projects: A (the run's original target) and B (source repointed later).
	var projA, projB string
	if err := testPool.QueryRow(ctx, `INSERT INTO project(workspace_id,title) VALUES($1,'A') RETURNING id`, workspaceID).Scan(&projA); err != nil {
		t.Fatalf("seed project A: %v", err)
	}
	if err := testPool.QueryRow(ctx, `INSERT INTO project(workspace_id,title) VALUES($1,'B') RETURNING id`, workspaceID).Scan(&projB); err != nil {
		t.Fatalf("seed project B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM project WHERE workspace_id=$1`, workspaceID)
	})

	// A quota_blocked run whose snapshot targets project A.
	var credentialID, configuredBy string
	_ = testPool.QueryRow(ctx, `SELECT credential_id::text, configured_by_user_id::text FROM external_issue_source WHERE id=$1`, sourceID).Scan(&credentialID, &configuredBy)
	snap := fmt.Sprintf(`{"credential_id":%q,"provider":"github","instance_key":"github.com","repository_external_id":"555","repository_full_path":"acme/widgets","target_project_id":%q,"configured_by_user_id":%q,"state":"open"}`, credentialID, projA, configuredBy)
	var runID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_sync_run(workspace_id,source_id,kind,state,filter_snapshot,input_snapshot,cursor)
		VALUES($1,$2,'backfill','quota_blocked','{"state":"open"}',$3::jsonb,'saved') RETURNING id`,
		workspaceID, sourceID, snap).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// The source is repointed at project B after the run paused.
	if _, err := testPool.Exec(ctx, `UPDATE external_issue_source SET target_project_id=$2 WHERE id=$1`, sourceID, projB); err != nil {
		t.Fatalf("repoint source: %v", err)
	}

	// Drive the real handler.
	req := httptest.NewRequest("POST", "/api/workspaces/"+workspaceID+"/external-issue-sync-runs/"+runID+"/resume", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", workspaceID)
	rctx.URLParams.Add("runId", runID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	testHandler.ResumeSyncRun(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resume status = %d, body=%s, want 202", rec.Code, rec.Body.String())
	}

	// The resumed run's snapshot must still target project A, not B.
	var newSnap string
	if err := testPool.QueryRow(ctx, `SELECT input_snapshot::text FROM external_issue_sync_run WHERE id=$1`, runID).Scan(&newSnap); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !strings.Contains(newSnap, projA) || strings.Contains(newSnap, projB) {
		t.Fatalf("resume must preserve original project A (%s), not adopt B (%s); snapshot=%s", projA, projB, newSnap)
	}
}

// seedSyncWorkerFixtureDynamic is like seedSyncWorkerFixture but the page set is
// read live from *pages on every request, so a test can mutate the remote issue
// list BETWEEN worker claims (e.g. a shift that moves an issue into an
// already-consumed page).
func seedSyncWorkerFixtureDynamic(t *testing.T, pages *[][]map[string]any) (workspaceID, sourceID string) {
	t.Helper()
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1,$2) RETURNING id`,
		"eis-dyn", "eis-dyn-"+suffix+"@test.local").Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description) VALUES ($1,$2,'') RETURNING id`,
		"eis-dyn-ws", "eis-d-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_sync_run_item WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_sync_run WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_link WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_source WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM issue WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM github_installation WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM workspace WHERE id=$1`, workspaceID)
	})
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
	t.Cleanup(externalissue.SetMaxPerPageForTest(1))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/repos/") && !strings.Contains(r.URL.Path, "/issues") {
			fmt.Fprint(w, `{"id":555,"full_name":"acme/widgets","name":"widgets"}`)
			return
		}
		writeKeysetIssuesPage(w, r, flattenPages(*pages))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(externalissue.SetGitHubAPIBaseForTest(srv.URL))
	return workspaceID, sourceID
}

// Deleting/transferring an issue mid-scan (it VANISHES from the source set, not
// just closes) must NOT drop a still-present issue. The value-anchored keyset
// (sort=updated&direction=asc&since=<max updated_at seen>) enumerates by VALUE,
// so a removed earlier issue only shrinks the result set — a survivor keeps its
// updated_at and still appears at/after the anchor. Reproduction of codex56
// round-6 P0: N issues, first claim consumes the earlier ones, then the earliest
// is DELETED; every survivor must still be imported and the run must not falsely
// report succeeded-with-a-missing-tail.
func TestSyncWorkerKeysetNoLossOnMidScanDelete(t *testing.T) {
	n := syncRunPagesPerClaim + 3
	var pages [][]map[string]any
	for i := 0; i < n; i++ {
		id := i + 1
		pages = append(pages, []map[string]any{issue(id, id, fmt.Sprintf("i%d", id))})
	}
	live := pages
	workspaceID, sourceID := seedSyncWorkerFixtureDynamic(t, &live)
	withFakeToken(t)
	// Import state=all so no local state filtering interferes; this isolates the
	// keyset delete-safety property.
	runID := queueRunState(t, workspaceID, sourceID, "all")

	w := NewExternalIssueSyncWorker(testHandler)
	// First claim drains the per-claim page budget, then requeues.
	if _, err := w.ProcessNext(context.Background()); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if st, _, _ := runState(t, runID); st != "queued" {
		t.Fatalf("after first claim state = %q, want queued", st)
	}
	importedSoFar := countWsIssues(t, workspaceID)
	if importedSoFar == 0 || importedSoFar >= n {
		t.Fatalf("first claim imported %d, want partial progress in (0,%d)", importedSoFar, n)
	}

	// DELETE the earliest issue: remove it from the served set entirely (as a
	// GitHub permanent-delete or transfer-out would). Under a page-offset scan
	// this shifts a later, not-yet-seen issue into an already-consumed page and it
	// is lost. Under the keyset it just shrinks the set.
	live = live[1:]

	// Drain to terminal.
	for i := 0; i < 40; i++ {
		if _, err := w.ProcessNext(context.Background()); err != nil {
			t.Fatalf("drain claim: %v", err)
		}
		if st, _, _ := runState(t, runID); st == "succeeded" || st == "partial" {
			break
		}
	}
	if state, _, _ := runState(t, runID); state != "succeeded" {
		t.Fatalf("final state = %q, want succeeded", state)
	}
	// Assert EVERY identity is present, not a loose count: id=1 (the earliest) was
	// imported during the first claim before it was deleted, and ids 2..n are
	// survivors the keyset must still reach. Check each external_issue_id in the
	// link table so a silently-dropped survivor cannot pass.
	rows, err := testPool.Query(context.Background(),
		`SELECT external_issue_id FROM external_issue_link WHERE workspace_id=$1`, workspaceID)
	if err != nil {
		t.Fatalf("query links: %v", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var extID string
		if err := rows.Scan(&extID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		present[extID] = true
	}
	for id := 1; id <= n; id++ {
		if !present[fmt.Sprintf("%d", id)] {
			t.Fatalf("issue identity %d missing after mid-scan delete; present=%v", id, present)
		}
	}
	if got := countWsIssues(t, workspaceID); got != n {
		t.Fatalf("imported issues = %d, want exactly %d (id=1 imported before delete + all survivors)", got, n)
	}
}

// Failed accounting must be fenced on run ownership: a worker whose lease was
// taken over and whose run was cancelled must NOT append 'failed' ledger rows.
// Regression for codex56 round-4 P0 #2.
func TestSyncWorkerFailedAccountingIsFenced(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	pages := [][]map[string]any{{issue(1, 1, "a")}}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	// Put the run into the state a taken-over + cancelled run would be in for the
	// stale worker: owned by a DIFFERENT worker id and cancel_requested.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE external_issue_sync_run SET worker_id='other-worker', state='running',
		 cancel_requested=true, lease_expires_at=now()+interval '2 minutes' WHERE id=$1`, runID); err != nil {
		t.Fatalf("simulate takeover: %v", err)
	}

	staleRun, err := testHandler.Queries.GetExternalIssueSyncRun(context.Background(), db.GetExternalIssueSyncRunParams{
		ID: parseUUID(runID), WorkspaceID: parseUUID(workspaceID),
	})
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	// The stale worker (its own id, not 'other-worker') tries to record a failure.
	w := NewExternalIssueSyncWorker(testHandler)
	gotErr := w.recordFailedItem(context.Background(), staleRun, "999")
	if gotErr == nil {
		t.Fatal("recordFailedItem on a taken-over/cancelled run must return an error (fence), got nil")
	}
	// No ledger row may have been written.
	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM external_issue_sync_run_item WHERE run_id=$1`, runID).Scan(&count); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if count != 0 {
		t.Fatalf("fenced failed-accounting wrote %d ledger rows, want 0", count)
	}
}

// Resume must be REJECTED when the reconnected credential/installation can no
// longer reach the repository, instead of being accepted and only failing later
// in the worker. Regression for codex56 round-4 (resume credential re-resolve).
func TestResumeSyncRunRejectsInaccessibleRepo(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, [][]map[string]any{{issue(1, 1, "a")}})
	withFakeToken(t)

	// Point the provider at a server that 404s the repo resolve: the reconnected
	// installation no longer includes this repository.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(externalissue.SetGitHubAPIBaseForTest(srv.URL))

	var credentialID, configuredBy string
	_ = testPool.QueryRow(ctx, `SELECT credential_id::text, configured_by_user_id::text FROM external_issue_source WHERE id=$1`, sourceID).Scan(&credentialID, &configuredBy)
	snap := fmt.Sprintf(`{"credential_id":%q,"provider":"github","instance_key":"github.com","repository_external_id":"555","repository_full_path":"acme/widgets","configured_by_user_id":%q,"state":"open"}`, credentialID, configuredBy)
	var runID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_sync_run(workspace_id,source_id,kind,state,filter_snapshot,input_snapshot,cursor)
		VALUES($1,$2,'backfill','needs_reauth','{"state":"open"}',$3::jsonb,'saved') RETURNING id`,
		workspaceID, sourceID, snap).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/workspaces/"+workspaceID+"/external-issue-sync-runs/"+runID+"/resume", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", workspaceID)
	rctx.URLParams.Add("runId", runID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	testHandler.ResumeSyncRun(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("resume status = %d, body=%s, want 409 (inaccessible repo)", rec.Code, rec.Body.String())
	}
	// The run must remain in its paused state, not be re-queued.
	var state string
	if err := testPool.QueryRow(ctx, `SELECT state FROM external_issue_sync_run WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "needs_reauth" {
		t.Fatalf("run state = %q, want needs_reauth (resume rejected, not queued)", state)
	}
}

func ledgerRowCount(t *testing.T, runID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM external_issue_sync_run_item WHERE run_id=$1`, runID).Scan(&n); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	return n
}

// A run that finishes in a non-resumable terminal state (succeeded) must have its
// identity ledger deleted — counts are already snapshotted on the run row and it
// can never re-scan, so the per-issue rows are dead weight. Terminal retention
// for codex56 round-4 ledger-bloat item.
func TestSyncWorkerCleansLedgerOnSuccess(t *testing.T) {
	pages := [][]map[string]any{{issue(1, 1, "a"), issue(2, 2, "b")}}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	w := NewExternalIssueSyncWorker(testHandler)
	for i := 0; i < 8; i++ {
		if _, err := w.ProcessNext(context.Background()); err != nil {
			t.Fatalf("ProcessNext: %v", err)
		}
		if st, _, _ := runState(t, runID); st == "succeeded" {
			break
		}
	}
	state, imported, total := runState(t, runID)
	if state != "succeeded" {
		t.Fatalf("state = %q, want succeeded", state)
	}
	// Counts survive on the run row even though the ledger is gone.
	if imported != 2 || total != 2 {
		t.Fatalf("imported=%d total=%d, want 2/2 (counts snapshotted on run row)", imported, total)
	}
	if n := ledgerRowCount(t, runID); n != 0 {
		t.Fatalf("ledger rows after success = %d, want 0 (terminal retention)", n)
	}
}

// A run that stops in a RESUMABLE terminal state (quota_blocked) must KEEP its
// ledger so a later resume can still skip already-accounted issues.
func TestSyncWorkerKeepsLedgerOnQuotaBlocked(t *testing.T) {
	// One issue succeeds, then quota blocks the second.
	pages := [][]map[string]any{{issue(1, 1, "a"), issue(2, 2, "b")}}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	// A worker whose sync service enforces a 1-issue quota so the 2nd issue trips
	// quota_blocked (mirrors TestSyncWorkerQuotaResumeNoDoubleCount).
	q := db.New(testPool)
	bus := events.New()
	issues := &service.IssueService{Queries: q, TxStarter: testPool, Bus: bus, Entitlements: limitProvider{limit: 1}}
	sync := service.NewExternalIssueSyncService(q, testPool, bus, issues)
	h := &Handler{Queries: q, ExternalIssueSync: sync}
	w := NewExternalIssueSyncWorker(h)

	if _, err := w.ProcessNext(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	state, _, _ := runState(t, runID)
	if state != "quota_blocked" {
		t.Fatalf("run state = %q, want quota_blocked", state)
	}
	if n := ledgerRowCount(t, runID); n == 0 {
		t.Fatal("ledger rows after quota_blocked = 0, want > 0 (resumable state must keep ledger)")
	}
}

// The superset scan (state=all) must import ONLY issues matching the requested
// state — closed issues in the superset are filtered locally when importing open.
func TestSyncWorkerSupersetScanFiltersRequestedState(t *testing.T) {
	// One page mixing open and closed; the source filter is state=open (default
	// from seedSyncWorkerFixture's source row).
	pages := [][]map[string]any{{
		issue(1, 1, "open-a"),
		{"id": 2, "number": 2, "title": "closed-b", "state": "closed"},
		issue(3, 3, "open-c"),
	}}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	w := NewExternalIssueSyncWorker(testHandler)
	for i := 0; i < 8; i++ {
		if _, err := w.ProcessNext(context.Background()); err != nil {
			t.Fatalf("ProcessNext: %v", err)
		}
		if st, _, _ := runState(t, runID); st == "succeeded" {
			break
		}
	}
	state, imported, total := runState(t, runID)
	if state != "succeeded" {
		t.Fatalf("state = %q, want succeeded", state)
	}
	// Only the two open issues import; the closed one is filtered out (not counted).
	if imported != 2 || total != 2 {
		t.Fatalf("imported=%d total=%d, want 2/2 (closed issue filtered out)", imported, total)
	}
	if n := countWsIssues(t, workspaceID); n != 2 {
		t.Fatalf("issue rows = %d, want 2", n)
	}
}

// A same-second overflow bucket (more issues in one updated_at second than a page
// holds) cannot be safely enumerated, so the provider skips past it and the run
// finishes PARTIAL with an explanatory error sample — never a falsely-complete
// succeeded. Regression for codex56 round-7 (no reconcile consumer exists yet, so
// an incomplete scan must not report success).
func TestSyncWorkerSameSecondOverflowMarksPartial(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	// per_page=2 with three issues sharing ONE second => the first full page is an
	// overflow bucket the keyset cannot split.
	sameSecond := "2026-02-01T00:00:00Z"
	pages := [][]map[string]any{{
		{"id": 1, "number": 1, "title": "a", "state": "open", "updated_at": sameSecond},
		{"id": 2, "number": 2, "title": "b", "state": "open", "updated_at": sameSecond},
		{"id": 3, "number": 3, "title": "c", "state": "open", "updated_at": sameSecond},
	}}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	// seedSyncWorkerFixture forces per_page=1; override to 2 so the same-second
	// page is a genuine overflow (>=2 items in one second).
	t.Cleanup(externalissue.SetMaxPerPageForTest(2))
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	w := NewExternalIssueSyncWorker(testHandler)
	for i := 0; i < 8; i++ {
		if _, err := w.ProcessNext(context.Background()); err != nil {
			t.Fatalf("ProcessNext: %v", err)
		}
		if st, _, _ := runState(t, runID); st == "succeeded" || st == "partial" {
			break
		}
	}
	state, _, _ := runState(t, runID)
	if state != "partial" {
		t.Fatalf("state = %q, want partial (incomplete same-second bucket)", state)
	}
	var sample string
	if err := testPool.QueryRow(context.Background(),
		`SELECT error_sample::text FROM external_issue_sync_run WHERE id=$1`, runID).Scan(&sample); err != nil {
		t.Fatalf("read error_sample: %v", err)
	}
	if !strings.Contains(sample, "same-second") {
		t.Fatalf("error_sample = %q, want the incomplete-bucket marker", sample)
	}
}
