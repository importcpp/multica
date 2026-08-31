package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if err := h.ExternalIssueSync.UseRemoteFields(ctx, util.MustParseUUID(workspaceID), util.MustParseUUID(issueID), remote.Title, remote.Body, true, false); err != nil {
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
