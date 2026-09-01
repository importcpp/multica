package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// issueExternalSourceResponse is the read-only provenance an issue detail page
// shows: which remote issue this Multica issue was imported from, its current
// remote state, and whether title/body are in conflict. The remote body is kept
// verbatim on the issue; provenance is surfaced here (a source badge), NOT by
// appending "Imported from ..." to the body.
type issueExternalSourceResponse struct {
	Provider      string `json:"provider"`
	InstanceKey   string `json:"instance_key"`
	DisplayNumber int64  `json:"display_number"`
	ExternalURL   string `json:"external_url"`
	RemoteState   string `json:"remote_state"`
	TitleConflict bool   `json:"title_conflict"`
	BodyConflict  bool   `json:"body_conflict"`
	TitleLocal    bool   `json:"title_local_owned"`
	BodyLocal     bool   `json:"body_local_owned"`
}

// GetIssueExternalSource returns the imported-from provenance for an issue, or
// 404 when the issue was not imported. Members who can see the issue can see its
// source badge (no credential detail is exposed).
func (h *Handler) GetIssueExternalSource(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	link, err := h.Queries.GetExternalIssueLinkByIssue(r.Context(), db.GetExternalIssueLinkByIssueParams{
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "issue has no external source")
		return
	}
	writeJSON(w, http.StatusOK, issueExternalSourceResponse{
		Provider:      link.Provider,
		InstanceKey:   link.InstanceKey,
		DisplayNumber: link.DisplayNumber,
		ExternalURL:   link.ExternalHtmlUrl,
		RemoteState:   link.RemoteState,
		TitleConflict: link.TitleConflict,
		BodyConflict:  link.BodyConflict,
		TitleLocal:    link.TitleLocalOwned,
		BodyLocal:     link.BodyLocalOwned,
	})
}

type resolveConflictRequest struct {
	// Action is one of: keep_local, resume_sync, use_remote.
	Action string `json:"action"`
	// Fields optionally scopes the action to "title" and/or "body"; empty means
	// both.
	Fields []string `json:"fields"`
}

// ResolveIssueExternalConflict applies a conflict decision to an imported issue.
// keep_local marks the field(s) local-owned (remote changes are surfaced but not
// applied); resume_sync clears local ownership so remote flows again; use_remote
// overwrites the local field(s) with the current remote content and clears the
// conflict. Admin/owner gated at the router.
func (h *Handler) ResolveIssueExternalConflict(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req resolveConflictRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	wantTitle, wantBody, ok := scopeFields(req.Fields)
	if !ok {
		writeError(w, http.StatusBadRequest, "fields must be a subset of [title, body]")
		return
	}
	link, err := h.Queries.GetExternalIssueLinkByIssue(r.Context(), db.GetExternalIssueLinkByIssueParams{
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "issue has no external source")
		return
	}

	switch req.Action {
	case "keep_local":
		// Local wins for the scoped fields; conflict cleared for those fields.
		if err := h.Queries.ResolveExternalIssueLinkField(r.Context(), db.ResolveExternalIssueLinkFieldParams{
			ID: link.ID, WorkspaceID: issue.WorkspaceID,
			TitleInScope: wantTitle, TitleOwned: true,
			BodyInScope: wantBody, BodyOwned: true,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to keep local")
			return
		}
	case "resume_sync":
		// Clear ownership so remote flows again, AND advance the baseline to the
		// current local content for the scoped fields. Without advancing, local
		// still differs from the stale baseline and the next sync re-raises the
		// same conflict; advancing makes local == baseline so a later remote change
		// applies cleanly. Hash must match the sync applier's (service.ContentHash).
		if err := h.Queries.ResumeExternalIssueLinkField(r.Context(), db.ResumeExternalIssueLinkFieldParams{
			ID: link.ID, WorkspaceID: issue.WorkspaceID,
			TitleInScope: wantTitle, TitleBaseline: service.ContentHash(issue.Title),
			BodyInScope: wantBody, BodyBaseline: service.ContentHash(issue.Description.String),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to resume sync")
			return
		}
	case "use_remote":
		// Fetch the CURRENT remote content and apply it through the shared,
		// event-emitting update path — a real overwrite, not just a baseline nudge.
		remote, status, err := h.fetchRemoteIssue(r, issue.WorkspaceID, link)
		if err != nil {
			writeError(w, status, "failed to fetch remote issue")
			return
		}
		var remoteUpdated time.Time
		if remote.RemoteUpdatedAt != "" {
			if t, perr := time.Parse(time.RFC3339, remote.RemoteUpdatedAt); perr == nil {
				remoteUpdated = t
			}
		}
		if err := h.ExternalIssueSync.UseRemoteFields(
			r.Context(), issue.WorkspaceID, issue.ID, remote.Title, remote.Body, remoteUpdated, wantTitle, wantBody,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to apply remote content")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "action must be keep_local, resume_sync, or use_remote")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fetchRemoteIssue resolves credentials for the link's provider and fetches the
// issue's current remote state, so "use remote" applies what is actually on the
// remote now (not a stale hash). Returns an HTTP status for the error path.
func (h *Handler) fetchRemoteIssue(r *http.Request, workspaceID pgtype.UUID, link db.ExternalIssueLink) (externalissue.Issue, int, error) {
	provider, ok := externalissue.For(link.Provider)
	if !ok {
		return externalissue.Issue{}, http.StatusInternalServerError, fmt.Errorf("provider unavailable")
	}
	source, err := h.Queries.GetExternalIssueSource(r.Context(), db.GetExternalIssueSourceParams{
		ID: link.SourceID, WorkspaceID: workspaceID,
	})
	if err != nil || !source.CredentialID.Valid {
		return externalissue.Issue{}, http.StatusConflict, fmt.Errorf("source unavailable")
	}
	resolver, ok := h.credentialResolver(link.Provider)
	if !ok {
		return externalissue.Issue{}, http.StatusInternalServerError, fmt.Errorf("no resolver")
	}
	creds, err := resolver.Resolve(r.Context(), externalissue.CredentialRef{
		Provider:    link.Provider,
		WorkspaceID: util.UUIDToString(workspaceID),
		ID:          util.UUIDToString(source.CredentialID),
	})
	if err != nil {
		return externalissue.Issue{}, http.StatusBadGateway, err
	}
	repo := externalissue.Repository{
		InstanceKey: source.InstanceKey,
		ExternalID:  source.RepositoryExternalID,
		FullPath:    source.RepositoryFullPath,
	}
	iss, err := provider.GetIssue(r.Context(), creds, repo, externalissue.IssueRef{
		ExternalID: link.ExternalIssueID, Number: link.DisplayNumber,
	})
	if err != nil {
		return externalissue.Issue{}, mapExternalIssueStatus(err), err
	}
	return iss, http.StatusOK, nil
}

// scopeFields interprets the optional fields list; empty means both. ok=false
// when any entry is not "title"/"body" so the caller can 400.
func scopeFields(fields []string) (title, body, ok bool) {
	if len(fields) == 0 {
		return true, true, true
	}
	for _, f := range fields {
		switch f {
		case "title":
			title = true
		case "body":
			body = true
		default:
			return false, false, false
		}
	}
	return title, body, true
}
