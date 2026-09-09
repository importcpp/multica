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
	failNext   map[pgtype.UUID]bool
}

func (c *childDoneCatalog) ListIssueStatusEntries(ctx context.Context, arg db.ListIssueStatusEntriesParams) ([]db.IssueStatus, error) {
	c.reads[arg.WorkspaceID]++
	if c.failNext[arg.WorkspaceID] {
		delete(c.failNext, arg.WorkspaceID)
		return nil, errors.New("test transient catalog failure")
	}
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
	if got, err := first(issue); got != "approved" || err != nil {
		t.Fatalf("unknown key resolved to %q, err=%v", got, err)
	}
	dbfx.Insert(t, "issue_status", testutil.Cols{"workspace_id": ws, "key": "approved", "name": "Approved", "category": "done", "color": "#123456"})
	if got, err := first(issue); got != "approved" || err != nil {
		t.Fatalf("one pass changed its snapshot to %q, err=%v", got, err)
	}
	if got, err := h.childStatusResolver(ctx)(issue); got != "done" || err != nil {
		t.Fatalf("next pass did not refresh: %q, err=%v", got, err)
	}
	if issue.Status != "approved" {
		t.Fatal("resolver mutated the raw status")
	}
	if catalog.reads[issue.WorkspaceID] != 2 || catalog.pointReads != 0 {
		t.Fatalf("catalog counts: %+v, point=%d", catalog.reads, catalog.pointReads)
	}
}

func TestChildDoneCatalogFailureSkipsNotification(t *testing.T) {
	ctx := context.Background()
	for _, batch := range []bool{false, true} {
		for _, tc := range []struct {
			name, previous, current, parent string
			siblingStage                    int
		}{
			{name: "child_transition", previous: "working", current: "done", parent: "parked"},
			{name: "child_cancellation", previous: "working", current: "cancelled", parent: "parked"},
			{name: "parent_backlog", previous: "in_progress", current: "done", parent: "parked"},
			{name: "parent_terminal", previous: "in_progress", current: "done", parent: "approved"},
			{name: "stage_barrier", previous: "in_progress", current: "done", parent: "in_progress", siblingStage: 1},
			{name: "progress_summary", previous: "in_progress", current: "done", parent: "in_progress", siblingStage: 2},
		} {
			if batch && tc.previous == "working" {
				continue // The batch notifier receives already-collected transitions.
			}
			t.Run(fmt.Sprintf("batch=%t/%s", batch, tc.name), func(t *testing.T) {
				ws := dbfx.Workspace(t, "Transient status read", "child-status-failure")
				fx := testutil.New(testPool, ws, testUserID)
				for key, category := range map[string]string{"working": "in_progress", "parked": "backlog", "approved": "done"} {
					fx.Insert(t, "issue_status", testutil.Cols{"workspace_id": ws, "key": key, "name": key, "category": category, "color": "#123456"})
				}
				agentID := fx.Agent(t, "Parent assignee", fx.Runtime(t, "Parent runtime"))
				parentID := fx.Issue(t, "Parent", testutil.Cols{"status": tc.parent, "assignee_type": "agent", "assignee_id": agentID})
				fx.Cleanup(t, "DELETE FROM comment WHERE issue_id = $1", parentID)
				fx.Cleanup(t, "DELETE FROM agent_task_queue WHERE issue_id = $1", parentID)
				childCols := testutil.Cols{"parent_issue_id": parentID, "status": tc.current}
				if tc.siblingStage != 0 {
					childCols["stage"] = 1
					fx.Issue(t, "Custom-status sibling", testutil.Cols{"parent_issue_id": parentID, "status": "approved", "stage": tc.siblingStage})
				}
				childID := fx.Issue(t, "Completed child", childCols)
				child, err := testHandler.Queries.GetIssue(ctx, parseUUID(childID))
				if err != nil {
					t.Fatal(err)
				}
				previous := child
				previous.Status = tc.previous
				catalog := &childDoneCatalog{
					Querier: testHandler.Queries, reads: map[pgtype.UUID]int{},
					failNext: map[pgtype.UUID]bool{parseUUID(ws): true},
				}
				h := *testHandler
				h.IssueStatusCatalog = catalog
				notify := func() {
					if batch {
						h.notifyParentsOfBatchChildDone(ctx, []db.Issue{child})
					} else {
						h.notifyParentOfChildDone(ctx, previous, child)
					}
				}
				notify()
				if got := countSystemCommentsOn(t, parentID); got != 0 {
					t.Errorf("failed catalog read posted %d comments, want none", got)
				}
				if got := countPendingTasksForAgent(t, parentID, agentID); got != 0 {
					t.Errorf("failed catalog read queued %d parent runs, want none", got)
				}
				if got := catalog.reads[parseUUID(ws)]; got != 1 {
					t.Errorf("failed pass made %d catalog reads, want 1", got)
				}
				// A subsequent notification pass gets a fresh catalog. This is not an
				// automatic retry of the already-committed child status update.
				notify()
				want := 0
				if tc.parent == "in_progress" {
					want = 1
				}
				if got := countSystemCommentsOn(t, parentID); got != want {
					t.Errorf("after recovery: %d comments, want %d", got, want)
				}
				if got := countPendingTasksForAgent(t, parentID, agentID); got != want {
					t.Errorf("after recovery: %d parent runs, want %d", got, want)
				}
				if catalog.reads[parseUUID(ws)] != 2 || catalog.pointReads != 0 {
					t.Errorf("catalog counts: %+v, point=%d", catalog.reads, catalog.pointReads)
				}
			})
		}
	}
}

func TestBatchChildDoneCatalogFailureIsolation(t *testing.T) {
	ctx := context.Background()
	failedWS := dbfx.Workspace(t, "Failed catalog", "child-failed-catalog")
	healthyWS := dbfx.Workspace(t, "Healthy catalog", "child-healthy-catalog")
	for _, ws := range []string{failedWS, healthyWS} {
		fx := testutil.New(testPool, ws, testUserID)
		for key, category := range map[string]string{"working": "in_progress", "parked": "backlog"} {
			fx.Insert(t, "issue_status", testutil.Cols{"workspace_id": ws, "key": key, "name": key, "category": category, "color": "#123456"})
		}
	}
	cases := []struct {
		workspace, status string
		want              int
	}{
		{failedWS, "working", 0}, // First read fails; skip this parent.
		{failedWS, "parked", 0},  // Cached failure must not bypass this guard.
		{healthyWS, "working", 1},
		{failedWS, "in_progress", 1}, // Built-in-only groups need no catalog.
	}
	var completed []db.Issue
	var parents []string
	for _, tc := range cases {
		fx := testutil.New(testPool, tc.workspace, testUserID)
		parentID := fx.Issue(t, "Batch parent", testutil.Cols{"status": tc.status})
		parents = append(parents, parentID)
		fx.Cleanup(t, "DELETE FROM comment WHERE issue_id = $1", parentID)
		childID := fx.Issue(t, "Batch child", testutil.Cols{"parent_issue_id": parentID, "status": "done"})
		child, err := testHandler.Queries.GetIssue(ctx, parseUUID(childID))
		if err != nil {
			t.Fatal(err)
		}
		completed = append(completed, child)
	}
	catalog := &childDoneCatalog{
		Querier: testHandler.Queries, reads: map[pgtype.UUID]int{},
		failNext: map[pgtype.UUID]bool{parseUUID(failedWS): true},
	}
	h := *testHandler
	h.IssueStatusCatalog = catalog
	h.notifyParentsOfBatchChildDone(ctx, completed)
	for i, tc := range cases {
		if got := countSystemCommentsOn(t, parents[i]); got != tc.want {
			t.Errorf("group %d (%s): %d comments, want %d", i, tc.status, got, tc.want)
		}
	}
	for _, ws := range []string{failedWS, healthyWS} {
		if got := catalog.reads[parseUUID(ws)]; got != 1 {
			t.Errorf("workspace %s made %d catalog reads, want 1", ws, got)
		}
	}
	if catalog.pointReads != 0 {
		t.Errorf("per-key reads = %d, want 0", catalog.pointReads)
	}
}
