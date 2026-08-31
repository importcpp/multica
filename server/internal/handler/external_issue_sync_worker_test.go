package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
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
		`{"credential_id":%q,"provider":"github","instance_key":"github.com","repository_external_id":%q,"repository_full_path":%q,"configured_by_user_id":%q,"state":"open"}`,
		credentialID, repoExternalID, repoFullPath, configuredBy)
	var runID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO external_issue_sync_run (workspace_id, source_id, kind, state, filter_snapshot, input_snapshot)
		VALUES ($1,$2,'backfill','queued','{"state":"open"}',$3::jsonb) RETURNING id`,
		workspaceID, sourceID, snapshot).Scan(&runID); err != nil {
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

// A worker whose lease was stolen must not advance/finish the run: its fenced
// writes update zero rows and it aborts without corrupting the new owner's run.
func TestSyncWorkerLeaseFencingStopsStaleWriter(t *testing.T) {
	// Enough pages that one claim yields (requeues) rather than finishing, so we
	// can steal the lease between the stale worker's pages.
	var pages [][]map[string]any
	for i := 1; i <= syncRunPagesPerClaim+2; i++ {
		pages = append(pages, []map[string]any{issue(i, i, fmt.Sprintf("i%d", i))})
	}
	workspaceID, sourceID, _, _ := seedSyncWorkerFixture(t, pages)
	withFakeToken(t)
	runID := queueRun(t, workspaceID, sourceID)

	stale := NewExternalIssueSyncWorker(testHandler)
	// Claim as the stale worker.
	claimed, err := stale.h.Queries.ClaimNextExternalIssueSyncRun(context.Background(),
		claimParams(stale))
	if err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	// A new owner steals the lease (simulating reclaim after lease expiry).
	if _, err := testPool.Exec(context.Background(),
		`UPDATE external_issue_sync_run SET worker_id = 'new-owner' WHERE id=$1`, runID); err != nil {
		t.Fatalf("steal lease: %v", err)
	}
	// The stale worker's fenced advance must report a lost lease and change nothing.
	advErr := stale.advance(context.Background(), claimed, "", runCounts{imported: 999}, 0)
	if !errorsIs(advErr, errLeaseLost) {
		t.Fatalf("stale advance err = %v, want errLeaseLost", advErr)
	}
	var imported int64
	var owner string
	if err := testPool.QueryRow(context.Background(),
		`SELECT imported_count, worker_id FROM external_issue_sync_run WHERE id=$1`, runID).
		Scan(&imported, &owner); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if imported == 999 {
		t.Fatal("stale worker's counts must not overwrite the reclaimed run")
	}
	if owner != "new-owner" {
		t.Fatalf("worker_id = %q, want new-owner (stale writer must not touch it)", owner)
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
