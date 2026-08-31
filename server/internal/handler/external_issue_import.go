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
	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
	"github.com/multica-ai/multica/server/internal/integrations/githubapi"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// mintGitHubIssuesReadToken returns an installation token scoped to
// issues:read + metadata:read via the shared githubapi client (token cache +
// singleflight + rate-limit handling live there — no third auth copy). The
// returned token is cached by the client, so the revoke func is a no-op;
// revoking would defeat the shared cache. A 403 on mint means the installation
// has not granted the newly requested Issues permission. It is a package var so
// worker/handler tests can inject a fake token without a real GitHub App key.
var mintGitHubIssuesReadToken = func(ctx context.Context, app *githubapi.Client, installationID int64) (string, func(), error) {
	if !app.Enabled() {
		return "", func() {}, errors.New("github App JWT credentials unavailable")
	}
	token, err := app.InstallationTokenScoped(ctx, installationID, githubapi.TokenPermissions{
		"issues": "read", "metadata": "read",
	})
	if err != nil {
		var se *githubapi.StatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusForbidden {
			return "", func() {}, errGitHubIssuesPermission
		}
		return "", func() {}, err
	}
	return token, func() {}, nil
}

var errGitHubIssuesPermission = errors.New("github installation has not granted Issues:read")

type previewGitHubIssuesResponse struct {
	// Sample is the first page of issue titles (PRs filtered) so the user sees
	// what will be imported before confirming.
	Sample []previewSampleIssue `json:"sample"`
	// SampleCount is len(Sample); FirstPageFull + HasMore indicate the repo has
	// more than one page (a best-effort "there are more than this" signal — we
	// deliberately do NOT scan the whole repo to produce an exact count).
	SampleCount int  `json:"sample_count"`
	HasMore     bool `json:"has_more"`
	// CapacityRemaining is how many more issues the workspace can hold before the
	// issue-count limit (−1 = unlimited). CapacityLimited says a limit applies.
	CapacityRemaining int64 `json:"capacity_remaining"`
	CapacityLimited   bool  `json:"capacity_limited"`
}

type previewSampleIssue struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
}

// PreviewGitHubIssues returns a sample + best-effort estimate + capacity hint so
// the user can confirm before starting an import. It fetches only the first page
// (no full scan): an exact precount would double the API cost by walking the
// whole repo. Admin-gated at the router.
func (h *Handler) PreviewGitHubIssues(w http.ResponseWriter, r *http.Request) {
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
	row, err := h.Queries.GetGitHubInstallationByID(r.Context(), installationUUID)
	if err != nil || row.WorkspaceID != workspaceUUID {
		writeError(w, http.StatusNotFound, "installation not found")
		return
	}
	token, revoke, err := mintGitHubIssuesReadToken(r.Context(), h.GHApp, row.InstallationID)
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
	page, err := provider.ListIssues(r.Context(), creds, repo, externalissue.IssueFilter{State: state}, "")
	if err != nil {
		writeError(w, mapExternalIssueStatus(err), "failed to list issues")
		return
	}
	resp := previewGitHubIssuesResponse{
		HasMore:           page.NextCursor != "",
		CapacityRemaining: -1,
	}
	for _, iss := range page.Issues {
		if len(resp.Sample) >= 10 {
			break
		}
		resp.Sample = append(resp.Sample, previewSampleIssue{
			Number: iss.Number, Title: iss.Title, State: string(iss.State),
		})
	}
	resp.SampleCount = len(resp.Sample)

	// Capacity hint from the workspace issue-count policy.
	policy := service.ResolveIssueCountPolicy(r.Context(), h.Entitlements, workspaceUUID)
	if policy.Action == entitlement.ActionEnforce && policy.Limit > 0 {
		used, err := service.CountIssueUsage(r.Context(), h.Queries, workspaceUUID, policy)
		if err == nil {
			remaining := policy.Limit - used
			if remaining < 0 {
				remaining = 0
			}
			resp.CapacityLimited = true
			resp.CapacityRemaining = remaining
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

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
	SourceID string `json:"source_id"`
	RunID    string `json:"run_id"`
	State    string `json:"state"`
}

// ImportGitHubIssues validates the installation + repository, creates (or
// reuses) a provider-neutral sync source and a QUEUED run, then returns 202 so
// a persistent worker drains the backfill out of band. It never paginates to
// completion inside the request: a large repository would otherwise hold the
// connection open, and a rate-limit or restart mid-import would lose progress.
// The worker resumes from the run's cursor. Admin-gated at the router.
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

	// Resolve the repo up front (fast, single call) so the source is keyed on a
	// stable external ID and owner/repo is proven to belong to this
	// installation before we queue any work. The token is discarded here; the
	// worker mints its own per-claim from the stored installation credential.
	token, revoke, err := mintGitHubIssuesReadToken(r.Context(), h.GHApp, row.InstallationID)
	if err != nil {
		if errors.Is(err, errGitHubIssuesPermission) {
			writeErrorCode(w, http.StatusForbidden, "github_issues_permission",
				"this GitHub installation has not granted Issues read access; re-authorize the app to enable issue import")
			return
		}
		writeError(w, http.StatusBadGateway, "failed to authenticate with github")
		return
	}
	provider, ok := externalissue.For("github")
	if !ok {
		revoke()
		writeError(w, http.StatusInternalServerError, "github provider unavailable")
		return
	}
	repo, err := provider.ResolveRepository(r.Context(), externalissue.Credentials{Token: token},
		externalissue.RepositoryRef{FullPath: req.Owner + "/" + req.Repo})
	revoke()
	if err != nil {
		writeError(w, mapExternalIssueStatus(err), "failed to resolve repository")
		return
	}

	// Validate the target project up front so a bad project id fails the request
	// (400) instead of failing every issue inside the worker.
	if projectID.Valid {
		if _, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
			ID: projectID, WorkspaceID: workspaceUUID,
		}); err != nil {
			writeError(w, http.StatusBadRequest, "target project not found in this workspace")
			return
		}
	}

	// Create or reuse the source (dedup on stable identity). credential_id holds
	// the installation UUID so the worker can mint a fresh token per claim.
	source, err := h.Queries.UpsertExternalIssueSource(r.Context(), db.UpsertExternalIssueSourceParams{
		WorkspaceID:          workspaceUUID,
		Provider:             "github",
		InstanceKey:          repo.InstanceKey,
		CredentialID:         installationUUID,
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

	// Snapshot the execution inputs onto the run so a later import request that
	// mutates the source (credential / project / filter) cannot redirect this
	// run mid-flight; the worker reads only this snapshot. project/user ids are
	// stored as strings and empty when unset.
	inputSnapshot := buildRunInputSnapshot(installationUUID, repo, projectID, actingUserID(r), state)

	// A source allows only one active run (partial unique index). If one is
	// already queued/running, report it instead of failing the request.
	run, err := h.Queries.CreateExternalIssueSyncRun(r.Context(), db.CreateExternalIssueSyncRunParams{
		WorkspaceID:    workspaceUUID,
		SourceID:       source.ID,
		Kind:           "backfill",
		State:          "queued",
		FilterSnapshot: []byte(fmt.Sprintf(`{"state":%q}`, state)),
		InputSnapshot:  inputSnapshot,
	})
	if err != nil {
		if existing, ok := h.activeRunForSource(r.Context(), workspaceUUID, source.ID); ok {
			writeJSON(w, http.StatusAccepted, importGitHubIssuesResponse{
				SourceID: util.UUIDToString(source.ID),
				RunID:    util.UUIDToString(existing.ID),
				State:    existing.State,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create import run")
		return
	}

	if h.ExternalIssueSyncWorker != nil {
		h.ExternalIssueSyncWorker.Notify()
	}
	writeJSON(w, http.StatusAccepted, importGitHubIssuesResponse{
		SourceID: util.UUIDToString(source.ID),
		RunID:    util.UUIDToString(run.ID),
		State:    "queued",
	})
}

// activeRunForSource returns the source's single active (queued/running) run.
func (h *Handler) activeRunForSource(ctx context.Context, workspaceID, sourceID pgtype.UUID) (db.ExternalIssueSyncRun, bool) {
	runs, err := h.Queries.ListExternalIssueSyncRunsBySource(ctx, db.ListExternalIssueSyncRunsBySourceParams{
		SourceID: sourceID, Limit: 5,
	})
	if err != nil {
		return db.ExternalIssueSyncRun{}, false
	}
	for _, run := range runs {
		if run.WorkspaceID == workspaceID && (run.State == "queued" || run.State == "running") {
			return run, true
		}
	}
	return db.ExternalIssueSyncRun{}, false
}

type syncRunStatusResponse struct {
	RunID     string `json:"run_id"`
	SourceID  string `json:"source_id"`
	State     string `json:"state"`
	Imported  int64  `json:"imported"`
	Updated   int64  `json:"updated"`
	Conflicts int64  `json:"conflicts"`
	Skipped   int64  `json:"skipped"`
	Failed    int64  `json:"failed"`
	Total     int64  `json:"total"`
	Cancel    bool   `json:"cancel_requested"`
	// Errors is a bounded sample of per-issue failure diagnostics so a partial
	// run tells the user which issues failed and why, not just a count.
	Errors []string `json:"errors"`
}

// GetSyncRun returns one run's progress so the UI can poll created/updated/
// conflict/failed and the terminal state. Admin-gated at the router.
func (h *Handler) GetSyncRun(w http.ResponseWriter, r *http.Request) {
	workspaceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	runUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runId"), "runId")
	if !ok {
		return
	}
	run, err := h.Queries.GetExternalIssueSyncRun(r.Context(), db.GetExternalIssueSyncRunParams{
		ID: runUUID, WorkspaceID: workspaceUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "sync run not found")
		return
	}
	writeJSON(w, http.StatusOK, syncRunStatusResponse{
		RunID:     util.UUIDToString(run.ID),
		SourceID:  util.UUIDToString(run.SourceID),
		State:     run.State,
		Imported:  run.ImportedCount,
		Updated:   run.UpdatedCount,
		Conflicts: run.ConflictCount,
		Skipped:   run.SkippedCount,
		Failed:    run.FailedCount,
		Total:     run.TotalSeen,
		Cancel:    run.CancelRequested,
		Errors:    decodeErrorSamples(run.ErrorSample),
	})
}

// CancelSyncRun requests cooperative cancellation; the worker stops at its next
// page boundary and finalizes the run as cancelled. Admin-gated at the router.
func (h *Handler) CancelSyncRun(w http.ResponseWriter, r *http.Request) {
	workspaceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	runUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runId"), "runId")
	if !ok {
		return
	}
	if err := h.Queries.RequestExternalIssueSyncRunCancel(r.Context(), db.RequestExternalIssueSyncRunCancelParams{
		ID: runUUID, WorkspaceID: workspaceUUID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to request cancellation")
		return
	}
	// 204 (empty body): the client treats any non-204 success as JSON and would
	// otherwise throw parsing an empty 202.
	w.WriteHeader(http.StatusNoContent)
}

// ResumeSyncRun re-queues a paused run (quota_blocked / needs_reauth / failed)
// from its saved cursor so counts and progress continue instead of starting a
// fresh run. Admin-gated at the router.
func (h *Handler) ResumeSyncRun(w http.ResponseWriter, r *http.Request) {
	workspaceUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	runUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runId"), "runId")
	if !ok {
		return
	}
	// Load the run + its (current) source so resume rebinds the credential and
	// target project from the re-validated source. A reconnect after a
	// needs_reauth stop mints a new installation UUID; carrying the old snapshot
	// credential forward would loop straight back to needs_reauth.
	existing, err := h.Queries.GetExternalIssueSyncRun(r.Context(), db.GetExternalIssueSyncRunParams{
		ID: runUUID, WorkspaceID: workspaceUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "sync run not found")
		return
	}
	source, err := h.Queries.GetExternalIssueSource(r.Context(), db.GetExternalIssueSourceParams{
		ID: existing.SourceID, WorkspaceID: workspaceUUID,
	})
	if err != nil {
		writeError(w, http.StatusConflict, "import source no longer exists")
		return
	}
	if !source.CredentialID.Valid {
		writeError(w, http.StatusConflict, "reconnect the GitHub installation before resuming")
		return
	}
	// Preserve the run's ORIGINAL execution semantics — target project, filter,
	// configured actor, repo identity — from its immutable snapshot. Only the
	// credential is refreshed (a reconnect after needs_reauth mints a new
	// installation UUID). We validate the refreshed credential still points at
	// the SAME stable repository the run was importing, so a source that was
	// repointed at a different repo cannot silently hijack the run.
	prevSnap, err := decodeRunInput(existing.InputSnapshot)
	if err != nil {
		writeError(w, http.StatusConflict, "run has no valid input snapshot")
		return
	}
	if source.RepositoryExternalID != prevSnap.RepositoryExternalID {
		writeError(w, http.StatusConflict,
			"the import source now points at a different repository; start a new import instead of resuming")
		return
	}
	// If the run's original target project was deleted, require an explicit
	// reselect rather than silently redirecting to wherever the source points now.
	if prevSnap.TargetProjectID.Valid {
		if _, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
			ID: prevSnap.TargetProjectID, WorkspaceID: workspaceUUID,
		}); err != nil {
			writeError(w, http.StatusConflict,
				"the run's target project no longer exists; reselect a project and start a new import")
			return
		}
	}
	repo := externalissue.Repository{
		InstanceKey: prevSnap.InstanceKey,
		ExternalID:  prevSnap.RepositoryExternalID,
		FullPath:    prevSnap.RepositoryFullPath,
	}
	// Rebuild the snapshot from the ORIGINAL project/filter/actor, swapping in
	// only the freshly validated credential.
	freshSnapshot := buildRunInputSnapshot(source.CredentialID, repo, prevSnap.TargetProjectID, prevSnap.ConfiguredByUserID, prevSnap.State)

	run, err := h.Queries.ResumeExternalIssueSyncRun(r.Context(), db.ResumeExternalIssueSyncRunParams{
		ID:            runUUID,
		WorkspaceID:   workspaceUUID,
		InputSnapshot: freshSnapshot,
	})
	if err != nil {
		// pgx.ErrNoRows here means the run is not in a resumable state (e.g.
		// already running or succeeded).
		writeError(w, http.StatusConflict, "run is not in a resumable state")
		return
	}
	if h.ExternalIssueSyncWorker != nil {
		h.ExternalIssueSyncWorker.Notify()
	}
	writeJSON(w, http.StatusAccepted, importGitHubIssuesResponse{
		SourceID: util.UUIDToString(run.SourceID),
		RunID:    util.UUIDToString(run.ID),
		State:    run.State,
	})
}

// buildRunInputSnapshot serializes the immutable execution inputs stored on a
// run (see migration 453 and the worker's runInputSnapshot).
func buildRunInputSnapshot(credentialID pgtype.UUID, repo externalissue.Repository, projectID, configuredBy pgtype.UUID, state string) []byte {
	m := map[string]string{
		"credential_id":          util.UUIDToString(credentialID),
		"provider":               "github",
		"instance_key":           repo.InstanceKey,
		"repository_external_id": repo.ExternalID,
		"repository_full_path":   repo.FullPath,
		"state":                  state,
	}
	if projectID.Valid {
		m["target_project_id"] = util.UUIDToString(projectID)
	}
	if configuredBy.Valid {
		m["configured_by_user_id"] = util.UUIDToString(configuredBy)
	}
	b, _ := json.Marshal(m)
	return b
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
