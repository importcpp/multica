package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type childDoneCatalog struct {
	issuestatus.Querier
	reads      map[pgtype.UUID]int
	pointReads int
	fail       bool
}

func (c *childDoneCatalog) ListIssueStatusEntries(ctx context.Context, arg db.ListIssueStatusEntriesParams) ([]db.IssueStatus, error) {
	c.reads[arg.WorkspaceID]++
	if c.fail {
		return nil, errors.New("test catalog unavailable")
	}
	return c.Querier.ListIssueStatusEntries(ctx, arg)
}

func (c *childDoneCatalog) GetIssueStatusEntryByKey(ctx context.Context, arg db.GetIssueStatusEntryByKeyParams) (db.IssueStatus, error) {
	c.pointReads++
	if c.fail {
		return db.IssueStatus{}, errors.New("test catalog unavailable")
	}
	return c.Querier.GetIssueStatusEntryByKey(ctx, arg)
}

func TestChildDoneStatusResolver(t *testing.T) {
	ctx := context.Background()
	for _, batch := range []bool{false, true} {
		for _, staged := range []bool{false, true} {
			for _, mode := range []string{"custom", "builtins", "unknown", "unavailable"} {
				t.Run(fmt.Sprintf("batch=%t/staged=%t/%s", batch, staged, mode), func(t *testing.T) {
					catalog := &childDoneCatalog{Querier: testHandler.Queries, reads: map[pgtype.UUID]int{}, fail: mode == "unavailable"}
					h := *testHandler
					h.IssueStatusCatalog = catalog
					var completed []db.Issue
					type expectedParent struct {
						id       string
						comments int
					}
					var parents []expectedParent
					var workspaces []pgtype.UUID
					for workspace := 0; workspace < 2; workspace++ {
						ws := dbfx.Workspace(t, "Child resolver", fmt.Sprintf("child-resolver-%t-%t-%s-%d", batch, staged, mode, workspace), testutil.Cols{"issue_prefix": "CHD"})
						workspaces = append(workspaces, parseUUID(ws))
						fixture := testutil.New(testPool, ws, testUserID)
						for key, category := range map[string]string{"approved": "done", "dropped": "cancelled", "review": "in_review"} {
							if workspace == 1 && key == "approved" {
								category = "in_progress"
							}
							cols := testutil.Cols{"workspace_id": ws, "key": key, "name": key, "category": category, "color": "#123456"}
							if key == "dropped" {
								cols["archived_at"] = testutil.Raw("now()")
							}
							fixture.Insert(t, "issue_status", cols)
						}
						parentCount := 1
						if batch {
							parentCount = 2
						}
						for range parentCount {
							parentStatus := "review"
							statuses := []string{"approved", "dropped", "approved"}
							wantComments := 0
							if mode == "builtins" {
								parentStatus = "in_progress"
								statuses = []string{"done", "cancelled", "done"}
								wantComments = 1
							} else if mode == "custom" && workspace == 0 {
								wantComments = 1
							} else if mode == "unknown" {
								statuses[0] = "missing"
							}
							parentID := fixture.Issue(t, "Resolver parent", testutil.Cols{"status": parentStatus})
							parents = append(parents, expectedParent{parentID, wantComments})
							for _, status := range statuses {
								cols := testutil.Cols{"parent_issue_id": parentID, "status": status}
								if staged {
									cols["stage"] = 1
								}
								id := fixture.Issue(t, "Resolver child", cols)
								row, err := h.Queries.GetIssue(ctx, parseUUID(id))
								if err != nil {
									t.Fatal(err)
								}
								completed = append(completed, row)
							}
							if staged {
								fixture.Issue(t, "Parked next stage", testutil.Cols{"parent_issue_id": parentID, "status": "backlog", "stage": 2})
							}
							fixture.Cleanup(t, "DELETE FROM comment WHERE issue_id = $1", parentID)
						}
						if !batch {
							last := completed[len(completed)-1]
							prev := last
							prev.Status = "in_progress"
							h.notifyParentOfChildDone(ctx, prev, last)
						}
					}
					if batch {
						h.notifyParentsOfBatchChildDone(ctx, completed)
					}
					wantReads := 1
					if mode == "builtins" {
						wantReads = 0
					}
					for _, ws := range workspaces {
						if got := catalog.reads[ws]; got != wantReads {
							t.Errorf("workspace %s catalog reads = %d, want %d", uuidToString(ws), got, wantReads)
						}
					}
					if catalog.pointReads != 0 {
						t.Errorf("per-key reads = %d, want 0", catalog.pointReads)
					}
					for _, parent := range parents {
						if got := countSystemCommentsOn(t, parent.id); got != parent.comments {
							t.Errorf("parent %s comments = %d, want %d", parent.id, got, parent.comments)
						}
						if staged && parent.comments == 1 {
							var content string
							dbfx.QueryRow(t, "SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system'", parent.id).Scan(&content)
							if !strings.Contains(content, "Stage 1") || !strings.Contains(content, "Stage 2") {
								t.Errorf("lost stage progress: %s", content)
							}
						}
					}
				})
			}
		}
	}
}

func TestChildStatusResolverRefreshesForNextPass(t *testing.T) {
	ctx := context.Background()
	ws := dbfx.Workspace(t, "Resolver refresh", "child-resolver-refresh")
	catalog := &childDoneCatalog{Querier: testHandler.Queries, reads: map[pgtype.UUID]int{}}
	h := *testHandler
	h.IssueStatusCatalog = catalog
	issue := db.Issue{WorkspaceID: parseUUID(ws), Status: "approved"}
	first := h.childStatusResolver(ctx)
	if got := first(issue); got != "approved" {
		t.Fatalf("unknown key resolved to %q", got)
	}
	dbfx.Insert(t, "issue_status", testutil.Cols{"workspace_id": ws, "key": "approved", "name": "Approved", "category": "done", "color": "#123456"})
	if got := first(issue); got != "approved" {
		t.Fatalf("one pass changed its snapshot to %q", got)
	}
	if got := h.childStatusResolver(ctx)(issue); got != "done" {
		t.Fatalf("next pass did not refresh: %q", got)
	}
	if issue.Status != "approved" {
		t.Fatal("resolver mutated the raw status")
	}
	if catalog.reads[issue.WorkspaceID] != 2 || catalog.pointReads != 0 {
		t.Fatalf("catalog counts: %+v, point=%d", catalog.reads, catalog.pointReads)
	}
}
