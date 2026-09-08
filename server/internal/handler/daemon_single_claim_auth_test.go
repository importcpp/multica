package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Exercise the real authentication middleware: machine identity must come
// from the credential, not a caller-supplied daemon_id or identity header.
func TestClaimTaskByRuntime_DaemonOwnership(t *testing.T) {
	tests := []struct {
		name           string
		credential     string
		daemonID       string
		unboundRuntime bool
		otherWorkspace bool
		nonMember      bool
		expired        bool
		revoked        bool
		missingRuntime bool
		wantStatus     int
	}{
		{name: "own machine", credential: "daemon", daemonID: "machine-b", wantStatus: http.StatusOK},
		{name: "peer machine cannot spoof ownership", credential: "daemon", daemonID: "machine-a", wantStatus: http.StatusNotFound},
		{name: "missing machine identity", credential: "daemon", wantStatus: http.StatusNotFound},
		{name: "unbound cloud runtime", credential: "daemon", daemonID: "machine-a", unboundRuntime: true, wantStatus: http.StatusOK},
		{name: "cross workspace", credential: "daemon", daemonID: "machine-b", otherWorkspace: true, wantStatus: http.StatusNotFound},
		{name: "expired daemon token", credential: "daemon", daemonID: "machine-b", expired: true, wantStatus: http.StatusUnauthorized},
		{name: "revoked daemon token", credential: "daemon", daemonID: "machine-b", revoked: true, wantStatus: http.StatusUnauthorized},
		{name: "missing runtime", credential: "daemon", daemonID: "machine-b", missingRuntime: true, wantStatus: http.StatusNotFound},
		{name: "PAT member compatibility", credential: "pat", wantStatus: http.StatusOK},
		{name: "PAT nonmember denied", credential: "pat", nonMember: true, wantStatus: http.StatusNotFound},
		{name: "JWT member compatibility", credential: "jwt", wantStatus: http.StatusOK},
		{name: "JWT nonmember denied", credential: "jwt", nonMember: true, wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtimeCols := testutil.Cols{"daemon_id": "machine-b"}
			if tt.unboundRuntime {
				runtimeCols["daemon_id"] = nil
			}
			runtimeID := dbfx.Runtime(t, "single claim ownership", runtimeCols)
			agentID := dbfx.Agent(t, "single claim ownership", runtimeID)
			issueID := dbfx.Issue(t, "single claim ownership")
			taskID := dbfx.Task(t, agentID, testutil.Cols{
				"runtime_id": runtimeID,
				"issue_id":   issueID,
			})
			// Claiming can create a task token. It has no FK-based cleanup.
			dbfx.Cleanup(t, `DELETE FROM task_token WHERE task_id = $1`, taskID)

			credentialWorkspaceID := testWorkspaceID
			if tt.otherWorkspace {
				credentialWorkspaceID = dbfx.Workspace(t, "other claim workspace", "claim-"+uuid.NewString())
			}
			credentialUserID := testUserID
			if tt.nonMember {
				credentialUserID = dbfx.User(t, "nonmember", uuid.NewString()+"@example.com")
			}
			expiresAt := time.Now().Add(time.Hour)
			if tt.expired {
				expiresAt = time.Now().Add(-time.Hour)
			}
			var token string
			switch tt.credential {
			case "daemon":
				token = "mdt_" + uuid.NewString()
				dbfx.Insert(t, "daemon_token", testutil.Cols{
					"token_hash":   auth.HashToken(token),
					"workspace_id": credentialWorkspaceID,
					"daemon_id":    tt.daemonID,
					"expires_at":   expiresAt,
				})
				if tt.revoked {
					dbfx.Exec(t, `DELETE FROM daemon_token WHERE token_hash = $1`, auth.HashToken(token))
				}
			case "pat":
				token = "mul_" + uuid.NewString()
				dbfx.Insert(t, "personal_access_token", testutil.Cols{
					"user_id":      credentialUserID,
					"name":         "single claim ownership",
					"token_hash":   auth.HashToken(token),
					"token_prefix": token[:12],
					"expires_at":   expiresAt,
				})
			case "jwt":
				var err error
				token, err = jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
					"sub": credentialUserID,
					"exp": expiresAt.Unix(),
				}).SignedString(auth.JWTSecret())
				if err != nil {
					t.Fatalf("sign JWT: %v", err)
				}
			}

			router := chi.NewRouter()
			router.Use(middleware.DaemonAuth(testHandler.Queries, nil, nil, nil))
			router.Post("/api/daemon/runtimes/{runtimeId}/tasks/claim", testHandler.ClaimTaskByRuntime)
			requestedRuntimeID := runtimeID
			if tt.missingRuntime {
				requestedRuntimeID = uuid.NewString()
			}
			req := testutil.JSONRequest(http.MethodPost, "/api/daemon/runtimes/"+requestedRuntimeID+"/tasks/claim?daemon_id=machine-b", map[string]string{
				"daemon_id": "machine-b",
			})
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Daemon-ID", "machine-b")
			req.Header.Set("X-Workspace-ID", testWorkspaceID)
			response := testutil.Call(t, router.ServeHTTP, req)

			var status string
			var dispatched, prepareLease bool
			dbfx.QueryRow(t, `SELECT status, dispatched_at IS NOT NULL, prepare_lease_expires_at IS NOT NULL FROM agent_task_queue WHERE id = $1`, taskID).
				Scan(&status, &dispatched, &prepareLease)
			tokenCount := dbfx.Count(t, `SELECT count(*) FROM task_token WHERE task_id = $1`, taskID)
			if tt.wantStatus != http.StatusOK {
				if status != "queued" || dispatched || prepareLease || tokenCount != 0 {
					t.Fatalf("denied claim mutated task: status=%s dispatched=%v prepare_lease=%v tokens=%d (HTTP %d)", status, dispatched, prepareLease, tokenCount, response.Code)
				}
				response.Want(tt.wantStatus)
				return
			}

			var body struct {
				Task *struct {
					ID        string `json:"id"`
					AuthToken string `json:"auth_token"`
				} `json:"task"`
			}
			response.Want(http.StatusOK).JSON(&body)
			if body.Task == nil || body.Task.ID != taskID || body.Task.AuthToken == "" {
				t.Fatalf("authorized claim did not return the queued task and token: %s", response.Text())
			}
			if status != "dispatched" || !dispatched || tokenCount != 1 {
				t.Fatalf("authorized claim not finalized: status=%s dispatched=%v tokens=%d", status, dispatched, tokenCount)
			}
		})
	}
}
