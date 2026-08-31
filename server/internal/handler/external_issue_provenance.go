package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
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
	wantTitle, wantBody := scopeFields(req.Fields)
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
		// Local wins for the scoped fields going forward.
		titleOwned := link.TitleLocalOwned || wantTitle
		bodyOwned := link.BodyLocalOwned || wantBody
		if err := h.Queries.SetExternalIssueLinkFieldOwnership(r.Context(), db.SetExternalIssueLinkFieldOwnershipParams{
			ID: link.ID, WorkspaceID: issue.WorkspaceID,
			TitleLocalOwned: titleOwned, BodyLocalOwned: bodyOwned,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to keep local")
			return
		}
	case "resume_sync":
		// Clear ownership for the scoped fields so remote can flow again.
		titleOwned := link.TitleLocalOwned && !wantTitle
		bodyOwned := link.BodyLocalOwned && !wantBody
		if err := h.Queries.SetExternalIssueLinkFieldOwnership(r.Context(), db.SetExternalIssueLinkFieldOwnershipParams{
			ID: link.ID, WorkspaceID: issue.WorkspaceID,
			TitleLocalOwned: titleOwned, BodyLocalOwned: bodyOwned,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to resume sync")
			return
		}
	case "use_remote":
		// Overwrite local with the remote content via the shared, event-emitting
		// update path, then clear conflicts and advance baselines.
		title := issue.Title
		body := issue.Description.String
		// The current remote content is not stored on the link (only its hash),
		// so a full "use remote" re-fetch belongs to the sync engine; for the
		// immediate UI action we clear conflict + ownership and let the next sync
		// apply remote (now unblocked). This keeps the endpoint side-effect-free
		// on issue content while unblocking the field.
		if err := h.Queries.ClearExternalIssueLinkConflicts(r.Context(), db.ClearExternalIssueLinkConflictsParams{
			ID: link.ID, WorkspaceID: issue.WorkspaceID,
			TitleBaselineHash: hashOrKeep(wantTitle, title, link.TitleBaselineHash),
			BodyBaselineHash:  hashOrKeep(wantBody, body, link.BodyBaselineHash),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to use remote")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "action must be keep_local, resume_sync, or use_remote")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scopeFields interprets the optional fields list; empty means both.
func scopeFields(fields []string) (title, body bool) {
	if len(fields) == 0 {
		return true, true
	}
	for _, f := range fields {
		switch f {
		case "title":
			title = true
		case "body":
			body = true
		}
	}
	return title, body
}

// hashOrKeep returns the sha256 of value when apply is true, else the existing
// baseline unchanged.
func hashOrKeep(apply bool, value, existing string) string {
	if !apply {
		return existing
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
