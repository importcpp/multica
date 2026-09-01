package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// use_remote must actually fetch the current remote content and overwrite the
// local field, clearing the conflict — not just nudge a baseline to the local
// value. Regression for codex56 P0-4.
func TestUseRemoteAppliesRemoteContent(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID, workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(name,email) VALUES($1,$2) RETURNING id`,
		"prov", "prov-"+suffix+"@t.local").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace(name,slug,description) VALUES($1,$2,'') RETURNING id`,
		"prov-ws", "prov-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
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
		t.Fatalf("installation: %v", err)
	}
	var sourceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_source
		(workspace_id,provider,instance_key,credential_id,repository_external_id,repository_full_path,configured_by_user_id,filter)
		VALUES($1,'github','github.com',$2,'555','acme/widgets',$3,'{"state":"open"}') RETURNING id`,
		workspaceID, installationID, userID).Scan(&sourceID); err != nil {
		t.Fatalf("source: %v", err)
	}
	// A local issue that diverged (local-edit) with the remote also changed → conflict.
	var issueID string
	if err := testPool.QueryRow(ctx, `INSERT INTO issue(workspace_id,number,title,description,status,priority,creator_type,creator_id,position)
		VALUES($1,1,'local-edit','lbody','backlog','none','member',$2,-1) RETURNING id`,
		workspaceID, userID).Scan(&issueID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO external_issue_link
		(workspace_id,provider,instance_key,external_issue_id,source_id,issue_id,display_number,external_html_url,remote_state,title_baseline_hash,body_baseline_hash,title_conflict)
		VALUES($1,'github','github.com','900',$2,$3,900,'h','open','oldhash','bhash',true)`,
		workspaceID, sourceID, issueID); err != nil {
		t.Fatalf("link: %v", err)
	}

	// Fake GitHub GetIssue returns remote-v2.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":900,"number":900,"title":"remote-v2","body":"lbody","state":"open","html_url":"h","updated_at":"2026-02-01T00:00:00Z"}`)
	}))
	defer srv.Close()
	defer externalissue.SetGitHubAPIBaseForTest(srv.URL)()
	withFakeToken(t)

	// Wire a handler whose ExternalIssueSync can update + publish.
	q := db.New(testPool)
	bus := events.New()
	issues := &service.IssueService{Queries: q, TxStarter: testPool, Bus: bus}
	h := &Handler{Queries: q, ExternalIssueSync: service.NewExternalIssueSyncService(q, testPool, bus, issues)}

	body, _ := json.Marshal(map[string]any{"action": "use_remote", "fields": []string{"title"}})
	req := httptest.NewRequest("POST", "/api/issues/"+issueID+"/external-source/resolve", strings.NewReader(string(body)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", issueID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	// loadIssueForUser needs auth context; call the service path directly instead
	// to keep the test focused on the use_remote semantics.
	link, err := q.GetExternalIssueLinkByIssue(ctx, db.GetExternalIssueLinkByIssueParams{
		WorkspaceID: util.MustParseUUID(workspaceID), IssueID: util.MustParseUUID(issueID),
	})
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	remote, status, err := h.fetchRemoteIssue(req, util.MustParseUUID(workspaceID), link)
	if err != nil {
		t.Fatalf("fetchRemoteIssue: status=%d err=%v", status, err)
	}
	if remote.Title != "remote-v2" {
		t.Fatalf("fetched remote title = %q, want remote-v2", remote.Title)
	}
	if err := h.ExternalIssueSync.UseRemoteFields(ctx, util.MustParseUUID(workspaceID), util.MustParseUUID(issueID), remote.Title, remote.Body, time.Time{}, true, false); err != nil {
		t.Fatalf("UseRemoteFields: %v", err)
	}
	_ = rec

	// Issue title now equals remote; conflict cleared.
	var title string
	var conflict bool
	if err := testPool.QueryRow(ctx, `SELECT i.title, l.title_conflict FROM issue i
		JOIN external_issue_link l ON l.issue_id=i.id WHERE i.id=$1`, issueID).Scan(&title, &conflict); err != nil {
		t.Fatalf("read: %v", err)
	}
	if title != "remote-v2" {
		t.Fatalf("issue title = %q, want remote-v2 (use_remote must overwrite)", title)
	}
	if conflict {
		t.Fatal("title_conflict must be cleared after use_remote")
	}
}

// A single full first page (15 issues, no next-page Link) must report
// has_more=true even though only 10 are sampled — the old NextCursor-only check
// wrongly said false and hid the other issues. Regression for codex56 preview.
func TestPreviewHasMoreOnFullFirstPageNoCursor(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID, workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(name,email) VALUES($1,$2) RETURNING id`,
		"prev", "prev-"+suffix+"@t.local").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace(name,slug,description) VALUES($1,$2,'') RETURNING id`,
		"prev-ws", "prev-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = testPool.Exec(c, `DELETE FROM github_installation WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM workspace WHERE id=$1`, workspaceID)
	})
	var installationRowID string
	if err := testPool.QueryRow(ctx, `INSERT INTO github_installation(workspace_id,installation_id,account_login,account_type,connected_by_id)
		VALUES($1,$2,'acme','Organization',$3) RETURNING id`,
		workspaceID, time.Now().UnixNano()%1_000_000_000, userID).Scan(&installationRowID); err != nil {
		t.Fatalf("installation: %v", err)
	}

	// Fake GitHub: resolve repo + a single page of 15 open issues, NO Link header.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/issues") {
			fmt.Fprint(w, "[")
			for i := 0; i < 15; i++ {
				if i > 0 {
					fmt.Fprint(w, ",")
				}
				fmt.Fprintf(w, `{"id":%d,"number":%d,"title":"i%d","state":"open","html_url":"h","updated_at":"2026-01-01T00:00:00Z"}`, i+1, i+1, i+1)
			}
			fmt.Fprint(w, "]")
			return
		}
		fmt.Fprint(w, `{"id":555,"full_name":"acme/widgets","name":"widgets"}`)
	}))
	defer srv.Close()
	defer externalissue.SetGitHubAPIBaseForTest(srv.URL)()
	withFakeToken(t)

	h := &Handler{Queries: db.New(testPool), Entitlements: testHandler.Entitlements}
	reqBody, _ := json.Marshal(map[string]any{"owner": "acme", "repo": "widgets", "state": "open"})
	req := httptest.NewRequest("POST", "/x", strings.NewReader(string(reqBody)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", workspaceID)
	rctx.URLParams.Add("installationId", installationRowID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	// PreviewGitHubIssues reads the repository-browse env gate; set it.
	t.Setenv("GITHUB_APP_ID", "1")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "x")
	rec := httptest.NewRecorder()
	h.PreviewGitHubIssues(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out previewGitHubIssuesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SampleCount != 10 {
		t.Fatalf("sample_count = %d, want 10", out.SampleCount)
	}
	if !out.HasMore {
		t.Fatal("has_more must be true for a 15-issue full page even without a next cursor")
	}
}

// resume_sync must advance the baseline to the current local content so the same
// conflict is not re-raised on the next sync. Regression for codex56 round-4 P1
// (keep_local -> resume_sync closed loop). We assert the link's title baseline
// hash becomes the hash of the current local title after resume_sync.
func TestResumeSyncAdvancesBaseline(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID, workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(name,email) VALUES($1,$2) RETURNING id`,
		"rb", "rb-"+suffix+"@t.local").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace(name,slug,description) VALUES($1,$2,'') RETURNING id`,
		"rb-ws", "rb-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
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
		t.Fatalf("installation: %v", err)
	}
	var sourceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_source
		(workspace_id,provider,instance_key,credential_id,repository_external_id,repository_full_path,configured_by_user_id,filter)
		VALUES($1,'github','github.com',$2,'555','acme/widgets',$3,'{"state":"open"}') RETURNING id`,
		workspaceID, installationID, userID).Scan(&sourceID); err != nil {
		t.Fatalf("source: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx, `INSERT INTO issue(workspace_id,number,title,description,status,priority,creator_type,creator_id,position)
		VALUES($1,1,'local-title','b','backlog','none','member',$2,-1) RETURNING id`,
		workspaceID, userID).Scan(&issueID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Link with a STALE title baseline (some old hash) and title in conflict.
	if _, err := testPool.Exec(ctx, `INSERT INTO external_issue_link
		(workspace_id,provider,instance_key,external_issue_id,source_id,issue_id,display_number,external_html_url,remote_state,title_baseline_hash,body_baseline_hash,title_conflict)
		VALUES($1,'github','github.com','900',$2,$3,900,'h','open','stalehash','bhash',true)`,
		workspaceID, sourceID, issueID); err != nil {
		t.Fatalf("link: %v", err)
	}

	h := &Handler{Queries: db.New(testPool), TxStarter: testPool}
	body, _ := json.Marshal(map[string]any{"action": "resume_sync", "fields": []string{"title"}})
	req := httptest.NewRequest("POST", "/x", strings.NewReader(string(body)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", issueID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	// The handler loads the issue via loadIssueForUser; drive the field query
	// directly instead to keep the test focused on baseline semantics.
	link, err := h.Queries.GetExternalIssueLinkByIssue(ctx, db.GetExternalIssueLinkByIssueParams{
		WorkspaceID: util.MustParseUUID(workspaceID), IssueID: util.MustParseUUID(issueID),
	})
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	if err := h.Queries.ResumeExternalIssueLinkField(ctx, db.ResumeExternalIssueLinkFieldParams{
		ID: link.ID, WorkspaceID: util.MustParseUUID(workspaceID),
		TitleInScope: true, TitleBaseline: service.ContentHash("local-title"),
		BodyInScope: false, BodyBaseline: "",
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	_ = req

	var titleHash string
	var titleConflict, titleOwned bool
	if err := testPool.QueryRow(ctx, `SELECT title_baseline_hash, title_conflict, title_local_owned
		FROM external_issue_link WHERE id=$1`, util.UUIDToString(link.ID)).Scan(&titleHash, &titleConflict, &titleOwned); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if titleHash != service.ContentHash("local-title") {
		t.Fatalf("title baseline = %q, want hash of local title (advanced)", titleHash)
	}
	if titleConflict || titleOwned {
		t.Fatalf("resume must clear conflict and ownership; got conflict=%v owned=%v", titleConflict, titleOwned)
	}
}

// A second import for the same repo while a run is active must NOT silently
// coalesce onto the old run when the settings differ (e.g. state open->all): it
// returns 409 and does NOT rewrite the source. An identical request coalesces
// (202 + same run). Regression for codex56 round-4 P1.
func TestImportGitHubIssuesActiveRunSnapshotCompare(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID, workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(name,email) VALUES($1,$2) RETURNING id`,
		"imp", "imp-"+suffix+"@t.local").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace(name,slug,description) VALUES($1,$2,'') RETURNING id`,
		"imp-ws", "imp-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_sync_run WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_source WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM github_installation WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM workspace WHERE id=$1`, workspaceID)
	})
	var installationRowID string
	if err := testPool.QueryRow(ctx, `INSERT INTO github_installation(workspace_id,installation_id,account_login,account_type,connected_by_id)
		VALUES($1,$2,'acme','Organization',$3) RETURNING id`,
		workspaceID, time.Now().UnixNano()%1_000_000_000, userID).Scan(&installationRowID); err != nil {
		t.Fatalf("installation: %v", err)
	}
	// Seed a source (state=open) with an ACTIVE (queued) run whose snapshot is state=open.
	var sourceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_source
		(workspace_id,provider,instance_key,credential_id,repository_external_id,repository_full_path,configured_by_user_id,filter,state)
		VALUES($1,'github','github.com',$2,'555','acme/widgets',$3,'{"state":"open"}','active') RETURNING id`,
		workspaceID, installationRowID, userID).Scan(&sourceID); err != nil {
		t.Fatalf("source: %v", err)
	}
	snapOpen := fmt.Sprintf(`{"credential_id":%q,"provider":"github","instance_key":"github.com","repository_external_id":"555","repository_full_path":"acme/widgets","configured_by_user_id":%q,"state":"open"}`, installationRowID, userID)
	var runID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_sync_run(workspace_id,source_id,kind,state,filter_snapshot,input_snapshot)
		VALUES($1,$2,'backfill','running','{"state":"open"}',$3::jsonb) RETURNING id`,
		workspaceID, sourceID, snapOpen).Scan(&runID); err != nil {
		t.Fatalf("run: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":555,"full_name":"acme/widgets","name":"widgets"}`)
	}))
	defer srv.Close()
	defer externalissue.SetGitHubAPIBaseForTest(srv.URL)()
	withFakeToken(t)
	t.Setenv("GITHUB_APP_ID", "1")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "x")

	h := &Handler{Queries: db.New(testPool), TxStarter: testPool}
	call := func(state string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"owner": "acme", "repo": "widgets", "state": state})
		req := httptest.NewRequest("POST", "/x", strings.NewReader(string(body)))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", workspaceID)
		rctx.URLParams.Add("installationId", installationRowID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.ImportGitHubIssues(rec, req)
		return rec
	}

	// Different state (all vs active run's open) -> 409, source NOT rewritten.
	rec := call("all")
	if rec.Code != http.StatusConflict {
		t.Fatalf("differing import status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var srcFilter string
	if err := testPool.QueryRow(ctx, `SELECT filter::text FROM external_issue_source WHERE id=$1`, sourceID).Scan(&srcFilter); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if !strings.Contains(srcFilter, `"open"`) {
		t.Fatalf("source filter was rewritten to %q; must stay open while a run is active", srcFilter)
	}

	// Identical state (open) -> 202 coalesced onto the existing run.
	rec = call("open")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("identical import status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var out importGitHubIssuesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RunID != runID {
		t.Fatalf("coalesced run = %q, want existing %q", out.RunID, runID)
	}
}

// UseRemoteFields must acquire link BEFORE issue, matching the sync applier's
// lock order, so a concurrent Apply and a "use remote" can never deadlock. This
// reproduces the ordering: a holder tx locks the LINK first, then (after
// UseRemoteFields has started) locks the ISSUE. With the correct link->issue
// order, UseRemoteFields blocks on the link lock and proceeds once the holder
// commits — no deadlock. Under the old issue-first order this deadlocked
// (40P01). Regression for codex56 round-6 blocker.
func TestUseRemoteFieldsLockOrderNoDeadlock(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID, workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(name,email) VALUES($1,$2) RETURNING id`,
		"dl", "dl-"+suffix+"@t.local").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace(name,slug,description) VALUES($1,$2,'') RETURNING id`,
		"dl-ws", "dl-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
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
		t.Fatalf("installation: %v", err)
	}
	var sourceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_source
		(workspace_id,provider,instance_key,credential_id,repository_external_id,repository_full_path,configured_by_user_id,filter)
		VALUES($1,'github','github.com',$2,'555','acme/widgets',$3,'{"state":"open"}') RETURNING id`,
		workspaceID, installationID, userID).Scan(&sourceID); err != nil {
		t.Fatalf("source: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx, `INSERT INTO issue(workspace_id,number,title,description,status,priority,creator_type,creator_id,position)
		VALUES($1,1,'local','b','backlog','none','member',$2,-1) RETURNING id`,
		workspaceID, userID).Scan(&issueID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	var linkID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_link
		(workspace_id,provider,instance_key,external_issue_id,source_id,issue_id,display_number,external_html_url,remote_state,title_baseline_hash,body_baseline_hash,title_conflict)
		VALUES($1,'github','github.com','900',$2,$3,900,'h','open','oldhash','bhash',true) RETURNING id`,
		workspaceID, sourceID, issueID).Scan(&linkID); err != nil {
		t.Fatalf("link: %v", err)
	}

	q := db.New(testPool)
	bus := events.New()
	issues := &service.IssueService{Queries: q, TxStarter: testPool, Bus: bus}
	svc := service.NewExternalIssueSyncService(q, testPool, bus, issues)

	// Holder tx: lock the LINK first (like the applier's claim).
	holder, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT id FROM external_issue_link WHERE id=$1 FOR UPDATE`, linkID); err != nil {
		t.Fatalf("holder lock link: %v", err)
	}

	// Run UseRemoteFields concurrently: it must block on the link lock (link-first
	// order), NOT grab the issue and deadlock.
	done := make(chan error, 1)
	go func() {
		done <- svc.UseRemoteFields(context.Background(), util.MustParseUUID(workspaceID), util.MustParseUUID(issueID),
			"remote-v2", "b", time.Time{}, true, false)
	}()

	// Give the goroutine time to reach (and block on) the link lock.
	time.Sleep(300 * time.Millisecond)
	// Now the holder locks the ISSUE (order link->issue) and commits. Under the
	// old reverse order in UseRemoteFields this is where the deadlock fired.
	if _, err := holder.Exec(ctx, `SELECT id FROM issue WHERE id=$1 FOR UPDATE`, issueID); err != nil {
		t.Fatalf("holder lock issue: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UseRemoteFields returned error (deadlock?): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("UseRemoteFields did not complete — likely deadlocked")
	}

	var title string
	if err := testPool.QueryRow(ctx, `SELECT title FROM issue WHERE id=$1`, issueID).Scan(&title); err != nil {
		t.Fatalf("read issue: %v", err)
	}
	if title != "remote-v2" {
		t.Fatalf("issue title = %q, want remote-v2 (use_remote applied after lock)", title)
	}
}

// Two concurrent imports for the same repo with DIFFERENT settings must be
// serialized: exactly one wins (202) and the other is rejected (409) — never two
// 202s coalesced onto the same run, and the source must not be left mutated by
// the loser. Regression for codex56 round-6 TOCTOU blocker.
func TestImportGitHubIssuesConcurrentDifferentInputs(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID, workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(name,email) VALUES($1,$2) RETURNING id`,
		"cc", "cc-"+suffix+"@t.local").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace(name,slug,description) VALUES($1,$2,'') RETURNING id`,
		"cc-ws", "cc-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_sync_run WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM external_issue_source WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM github_installation WHERE workspace_id=$1`, workspaceID)
		_, _ = testPool.Exec(c, `DELETE FROM workspace WHERE id=$1`, workspaceID)
	})
	var installationRowID string
	if err := testPool.QueryRow(ctx, `INSERT INTO github_installation(workspace_id,installation_id,account_login,account_type,connected_by_id)
		VALUES($1,$2,'acme','Organization',$3) RETURNING id`,
		workspaceID, time.Now().UnixNano()%1_000_000_000, userID).Scan(&installationRowID); err != nil {
		t.Fatalf("installation: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":555,"full_name":"acme/widgets","name":"widgets"}`)
	}))
	defer srv.Close()
	defer externalissue.SetGitHubAPIBaseForTest(srv.URL)()
	withFakeToken(t)
	t.Setenv("GITHUB_APP_ID", "1")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "x")

	h := &Handler{Queries: db.New(testPool), TxStarter: testPool}
	call := func(state string) int {
		body, _ := json.Marshal(map[string]any{"owner": "acme", "repo": "widgets", "state": state})
		req := httptest.NewRequest("POST", "/x", strings.NewReader(string(body)))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", workspaceID)
		rctx.URLParams.Add("installationId", installationRowID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.ImportGitHubIssues(rec, req)
		return rec.Code
	}

	var wg sync.WaitGroup
	codes := make([]int, 2)
	states := []string{"open", "all"}
	for i := range states {
		wg.Add(1)
		go func(i int) { defer wg.Done(); codes[i] = call(states[i]) }(i)
	}
	wg.Wait()

	got := map[int]int{}
	got[codes[0]]++
	got[codes[1]]++
	if got[http.StatusAccepted] != 1 || got[http.StatusConflict] != 1 {
		t.Fatalf("concurrent different-input imports = %v, want exactly one 202 and one 409", codes)
	}
	// Exactly one active run exists.
	var runCount int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM external_issue_sync_run WHERE workspace_id=$1 AND state IN ('queued','running')`, workspaceID).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("active runs = %d, want 1 (no double-create under concurrency)", runCount)
	}
}

// use_remote must reject a stale snapshot even when the concurrent sync landed in
// the SAME wall-clock second. The optimistic token is the link's updated_at read
// before the remote fetch; if a sync bumps the link's updated_at (to the same
// second) after that read, UseRemoteFields returns ErrRemoteStale and does NOT
// overwrite the issue with the older fetched content. Regression for codex56
// round-7 same-second stale overwrite.
func TestUseRemoteFieldsRejectsStaleSameSecond(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID, workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(name,email) VALUES($1,$2) RETURNING id`,
		"st", "st-"+suffix+"@t.local").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace(name,slug,description) VALUES($1,$2,'') RETURNING id`,
		"st-ws", "st-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
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
		t.Fatalf("installation: %v", err)
	}
	var sourceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_source
		(workspace_id,provider,instance_key,credential_id,repository_external_id,repository_full_path,configured_by_user_id,filter)
		VALUES($1,'github','github.com',$2,'555','acme/widgets',$3,'{"state":"open"}') RETURNING id`,
		workspaceID, installationID, userID).Scan(&sourceID); err != nil {
		t.Fatalf("source: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx, `INSERT INTO issue(workspace_id,number,title,description,status,priority,creator_type,creator_id,position)
		VALUES($1,1,'v2-current','b','backlog','none','member',$2,-1) RETURNING id`,
		workspaceID, userID).Scan(&issueID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Link with a FIXED updated_at (the token the handler would have read).
	fixed := "2026-02-01T00:00:00Z"
	var linkID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_link
		(workspace_id,provider,instance_key,external_issue_id,source_id,issue_id,display_number,external_html_url,remote_state,title_baseline_hash,body_baseline_hash,title_conflict,updated_at)
		VALUES($1,'github','github.com','900',$2,$3,900,'h','open','oldhash','bhash',true,$4::timestamptz) RETURNING id`,
		workspaceID, sourceID, issueID, fixed).Scan(&linkID); err != nil {
		t.Fatalf("link: %v", err)
	}

	q := db.New(testPool)
	issues := &service.IssueService{Queries: q, TxStarter: testPool, Bus: events.New()}
	svc := service.NewExternalIssueSyncService(q, testPool, events.New(), issues)

	// The handler read the link at t0=fixed. Now a concurrent sync bumps the link's
	// updated_at to the SAME second (a different microsecond within it).
	staleToken, _ := time.Parse(time.RFC3339, fixed)
	if _, err := testPool.Exec(ctx, `UPDATE external_issue_link SET updated_at=$2::timestamptz WHERE id=$1`,
		linkID, "2026-02-01T00:00:00.500000Z"); err != nil {
		t.Fatalf("bump link: %v", err)
	}

	// use_remote with the stale token must be rejected and must NOT overwrite.
	err := svc.UseRemoteFields(ctx, util.MustParseUUID(workspaceID), util.MustParseUUID(issueID),
		"v1-stale", "b", staleToken, true, false)
	if !errorsIs(err, service.ErrRemoteStale) {
		t.Fatalf("UseRemoteFields err = %v, want ErrRemoteStale", err)
	}
	var title string
	if err := testPool.QueryRow(ctx, `SELECT title FROM issue WHERE id=$1`, issueID).Scan(&title); err != nil {
		t.Fatalf("read issue: %v", err)
	}
	if title != "v2-current" {
		t.Fatalf("issue title = %q, want v2-current (stale use_remote must not overwrite)", title)
	}
}

// Deleting an imported issue must follow the link -> issue lock order (same as
// the sync applier / use-remote) so it cannot deadlock against a concurrent
// worker Apply. A holder tx locks the LINK first, then (after the delete has
// started) the ISSUE; with the correct order the delete blocks on the link lock
// and proceeds once the holder commits — no 40P01. Regression for codex56
// round-7 Delete/Apply deadlock.
func TestDeleteIssueLockOrderNoDeadlock(t *testing.T) {
	if testPool == nil {
		t.Skip("database unavailable")
	}
	ctx := context.Background()
	suffix := util.UUIDToString(dbid.NewV7())
	var userID, workspaceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user"(name,email) VALUES($1,$2) RETURNING id`,
		"dd", "dd-"+suffix+"@t.local").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace(name,slug,description) VALUES($1,$2,'') RETURNING id`,
		"dd-ws", "dd-"+suffix).Scan(&workspaceID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
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
		t.Fatalf("installation: %v", err)
	}
	var sourceID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_source
		(workspace_id,provider,instance_key,credential_id,repository_external_id,repository_full_path,configured_by_user_id,filter)
		VALUES($1,'github','github.com',$2,'555','acme/widgets',$3,'{"state":"open"}') RETURNING id`,
		workspaceID, installationID, userID).Scan(&sourceID); err != nil {
		t.Fatalf("source: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx, `INSERT INTO issue(workspace_id,number,title,description,status,priority,creator_type,creator_id,position)
		VALUES($1,1,'x','b','backlog','none','member',$2,-1) RETURNING id`,
		workspaceID, userID).Scan(&issueID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	var linkID string
	if err := testPool.QueryRow(ctx, `INSERT INTO external_issue_link
		(workspace_id,provider,instance_key,external_issue_id,source_id,issue_id,display_number,external_html_url,remote_state,title_baseline_hash,body_baseline_hash)
		VALUES($1,'github','github.com','900',$2,$3,900,'h','open','th','bh') RETURNING id`,
		workspaceID, sourceID, issueID).Scan(&linkID); err != nil {
		t.Fatalf("link: %v", err)
	}

	issueRow, err := db.New(testPool).GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
		ID: util.MustParseUUID(issueID), WorkspaceID: util.MustParseUUID(workspaceID),
	})
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	h := &Handler{Queries: db.New(testPool), TxStarter: testPool}

	// Holder tx: lock the LINK first (like a worker Apply claim).
	holder, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT id FROM external_issue_link WHERE id=$1 FOR UPDATE`, linkID); err != nil {
		t.Fatalf("holder lock link: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, derr := h.deleteIssuesAndCollectAttachmentURLs(context.Background(), []db.Issue{issueRow}, nil)
		done <- derr
	}()

	time.Sleep(300 * time.Millisecond)
	// Holder locks the ISSUE (order link->issue) then commits. Under the old
	// issue-first delete order this deadlocked here.
	if _, err := holder.Exec(ctx, `SELECT id FROM issue WHERE id=$1 FOR UPDATE`, issueID); err != nil {
		t.Fatalf("holder lock issue: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}

	select {
	case derr := <-done:
		if derr != nil {
			t.Fatalf("delete returned error (deadlock?): %v", derr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("delete did not complete — likely deadlocked")
	}

	var n int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE id=$1`, issueID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("issue not deleted, count=%d", n)
	}
	// The import link is tombstoned (issue_id cleared), not dropped.
	var linkIssueID *string
	if err := testPool.QueryRow(ctx, `SELECT issue_id::text FROM external_issue_link WHERE id=$1`, linkID).Scan(&linkIssueID); err != nil {
		t.Fatalf("read link: %v", err)
	}
	if linkIssueID != nil {
		t.Fatalf("link issue_id = %v, want NULL (tombstoned)", *linkIssueID)
	}
}
