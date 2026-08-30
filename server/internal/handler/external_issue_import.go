package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// importIssuesMaxPages bounds one synchronous import so a huge repository cannot
// hold an HTTP request (or the API rate budget) open indefinitely. Each page is
// up to 100 issues. A run persists its cursor, so a follow-up sync resumes where
// this one stopped rather than restarting — the async worker (PR6) removes the
// bound entirely.
const importIssuesMaxPages = 20

// mintGitHubIssuesReadToken exchanges the App JWT for an installation token
// scoped to issues:read + metadata:read. It reuses the same signing/header/
// revoke helpers as the repository-browse path in github.go rather than copying
// a third auth implementation.
func mintGitHubIssuesReadToken(ctx context.Context, installationID int64) (string, func(), error) {
	appJWT, err := signGitHubAppJWT(time.Now())
	if err != nil {
		return "", func() {}, err
	}
	if appJWT == "" {
		return "", func() {}, errors.New("github App JWT credentials unavailable")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", strings.TrimRight(githubAPIBase, "/"), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(`{"permissions":{"issues":"read","metadata":"read"}}`))
	if err != nil {
		return "", func() {}, err
	}
	setGitHubAPIHeaders(req, appJWT)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", func() {}, fmt.Errorf("create installation token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, githubAPIResponseLimit))
		// 403 here usually means the installation has not granted the newly
		// requested Issues permission yet.
		if resp.StatusCode == http.StatusForbidden {
			return "", func() {}, errGitHubIssuesPermission
		}
		return "", func() {}, fmt.Errorf("create installation token: github status %d", resp.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, githubAPIResponseLimit)).Decode(&body); err != nil {
		return "", func() {}, fmt.Errorf("decode installation token: %w", err)
	}
	if body.Token == "" {
		return "", func() {}, errors.New("github returned an empty installation token")
	}
	revoke := func() { revokeGitHubInstallationToken(client, body.Token) }
	return body.Token, revoke, nil
}

var errGitHubIssuesPermission = errors.New("github installation has not granted Issues:read")

// actingUserID resolves the authenticated member UUID from the request, or an
// invalid UUID when there is no session (matches the github-connect behavior).
func actingUserID(r *http.Request) pgtype.UUID {
	if userID := requestUserID(r); userID != "" {
		if u, err := parseStrictUUID(userID); err == nil {
			return u
		}
	}
	return pgtype.UUID{}
}

type importGitHubIssuesRequest struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	State     string `json:"state"`      // open | closed | all; default open
	ProjectID string `json:"project_id"` // optional target project UUID
}

type importGitHubIssuesResponse struct {
	SourceID  string `json:"source_id"`
	RunID     string `json:"run_id"`
	Imported  int64  `json:"imported"`
	Updated   int64  `json:"updated"`
	Conflicts int64  `json:"conflicts"`
	Skipped   int64  `json:"skipped"`
	Failed    int64  `json:"failed"`
	Total     int64  `json:"total"`
	Truncated bool   `json:"truncated"`
}

// ImportGitHubIssues resolves the installation + repository, creates (or reuses)
// a sync source and a run, and drives a bounded synchronous backfill through the
// provider-neutral externalissue adapter and the atomic Apply. Admin-gated at
// the router.
func (h *Handler) ImportGitHubIssues(w http.ResponseWriter, r *http.Request) {
	workspaceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	installationUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "installationId"), "installationId")
	if !ok {
		return
	}
	if !isGitHubRepositoryBrowseConfigured() {
		writeError(w, http.StatusServiceUnavailable, "github app is not configured for repository access")
		return
	}

	var req importGitHubIssuesRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Owner = strings.TrimSpace(req.Owner)
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Owner == "" || req.Repo == "" {
		writeError(w, http.StatusBadRequest, "owner and repo are required")
		return
	}
	state := strings.ToLower(strings.TrimSpace(req.State))
	switch state {
	case "", "open":
		state = "open"
	case "closed", "all":
	default:
		writeError(w, http.StatusBadRequest, "state must be open, closed, or all")
		return
	}

	// Cross-workspace installation is masked as 404 (matches the browse path).
	row, err := h.Queries.GetGitHubInstallationByID(r.Context(), installationUUID)
	if err != nil || row.WorkspaceID != workspaceUUID {
		writeError(w, http.StatusNotFound, "installation not found")
		return
	}

	var projectID pgtype.UUID
	if strings.TrimSpace(req.ProjectID) != "" {
		pid, ok := parseUUIDOrBadRequest(w, req.ProjectID, "project_id")
		if !ok {
			return
		}
		projectID = pid
	}

	token, revoke, err := mintGitHubIssuesReadToken(r.Context(), row.InstallationID)
	if err != nil {
		if errors.Is(err, errGitHubIssuesPermission) {
			writeErrorCode(w, http.StatusForbidden, "github_issues_permission",
				"this GitHub installation has not granted Issues read access; re-authorize the app to enable issue import")
			return
		}
		writeError(w, http.StatusBadGateway, "failed to authenticate with github")
		return
	}
	defer revoke()

	provider, ok := externalissue.For("github")
	if !ok {
		writeError(w, http.StatusInternalServerError, "github provider unavailable")
		return
	}
	creds := externalissue.Credentials{Token: token}
	repo, err := provider.ResolveRepository(r.Context(), creds, externalissue.RepositoryRef{FullPath: req.Owner + "/" + req.Repo})
	if err != nil {
		writeError(w, mapExternalIssueStatus(err), "failed to resolve repository")
		return
	}

	// Create or reuse the source (dedup on stable identity).
	source, err := h.Queries.UpsertExternalIssueSource(r.Context(), db.UpsertExternalIssueSourceParams{
		WorkspaceID:          workspaceUUID,
		Provider:             "github",
		InstanceKey:          repo.InstanceKey,
		RepositoryExternalID: repo.ExternalID,
		RepositoryFullPath:   repo.FullPath,
		TargetProjectID:      projectID,
		Filter:               []byte(fmt.Sprintf(`{"state":%q}`, state)),
		Mode:                 "manual",
		State:                "active",
		ConfiguredByUserID:   actingUserID(r),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create import source")
		return
	}

	run, err := h.Queries.CreateExternalIssueSyncRun(r.Context(), db.CreateExternalIssueSyncRunParams{
		WorkspaceID:    workspaceUUID,
		SourceID:       source.ID,
		Kind:           "backfill",
		State:          "running",
		FilterSnapshot: []byte(fmt.Sprintf(`{"state":%q}`, state)),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create import run")
		return
	}

	resp := importGitHubIssuesResponse{
		SourceID: util.UUIDToString(source.ID),
		RunID:    util.UUIDToString(run.ID),
	}

	var cursor externalissue.Cursor
	filter := externalissue.IssueFilter{State: state}
	for page := 0; page < importIssuesMaxPages; page++ {
		result, err := provider.ListIssues(r.Context(), creds, repo, filter, cursor)
		if err != nil {
			_ = h.finishRun(r.Context(), run.ID, "failed", err.Error())
			writeError(w, mapExternalIssueStatus(err), "failed to list issues")
			return
		}
		for _, iss := range result.Issues {
			resp.Total++
			outcome, applyErr := h.ExternalIssueSync.Apply(r.Context(), service.ApplyParams{
				WorkspaceID: workspaceUUID,
				SourceID:    source.ID,
				ProjectID:   projectID,
				CreatorID:   actingUserID(r),
				Remote:      toRemoteIssue(iss, repo),
			})
			if applyErr != nil {
				resp.Failed++
				continue
			}
			switch outcome {
			case service.OutcomeImported:
				resp.Imported++
			case service.OutcomeUpdated:
				resp.Updated++
			case service.OutcomeConflict:
				resp.Conflicts++
			case service.OutcomeSkipped:
				resp.Skipped++
			default:
				resp.Failed++
			}
		}
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
		if page == importIssuesMaxPages-1 {
			resp.Truncated = true
		}
	}

	finalState := "succeeded"
	if resp.Truncated {
		finalState = "partial"
	}
	_ = h.Queries.AdvanceExternalIssueSyncRun(r.Context(), db.AdvanceExternalIssueSyncRunParams{
		ID:            run.ID,
		Cursor:        string(cursor),
		ImportedCount: resp.Imported,
		UpdatedCount:  resp.Updated,
		ConflictCount: resp.Conflicts,
		SkippedCount:  resp.Skipped,
		FailedCount:   resp.Failed,
		TotalSeen:     resp.Total,
	})
	_ = h.finishRun(r.Context(), run.ID, finalState, "")

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) finishRun(ctx context.Context, runID pgtype.UUID, state, errMsg string) error {
	sample := []byte("[]")
	if errMsg != "" {
		if b, err := json.Marshal([]string{errMsg}); err == nil {
			sample = b
		}
	}
	return h.Queries.FinishExternalIssueSyncRun(ctx, db.FinishExternalIssueSyncRunParams{
		ID:          runID,
		State:       state,
		ErrorSample: sample,
	})
}

func toRemoteIssue(iss externalissue.Issue, repo externalissue.Repository) service.RemoteIssue {
	var updated time.Time
	if iss.RemoteUpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, iss.RemoteUpdatedAt); err == nil {
			updated = t
		}
	}
	return service.RemoteIssue{
		ExternalID:  iss.ExternalID,
		Provider:    "github",
		InstanceKey: repo.InstanceKey,
		Number:      iss.Number,
		Title:       iss.Title,
		Body:        iss.Body,
		State:       string(iss.State),
		HTMLURL:     iss.HTMLURL,
		UpdatedAt:   updated,
	}
}

// mapExternalIssueStatus translates the provider error taxonomy into an HTTP
// status for the import endpoints.
func mapExternalIssueStatus(err error) int {
	var e *externalissue.Error
	if !errors.As(err, &e) {
		return http.StatusBadGateway
	}
	switch e.Kind {
	case externalissue.ErrUnauthorized, externalissue.ErrForbidden:
		return http.StatusBadGateway
	case externalissue.ErrNotFound:
		return http.StatusNotFound
	case externalissue.ErrRateLimited, externalissue.ErrTransient:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}
