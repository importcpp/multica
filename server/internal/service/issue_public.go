package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrIssueRevisionConflict means the caller based an update on a stale issue
// revision. Transports map it to their stable conflict response.
var ErrIssueRevisionConflict = errors.New("issue revision conflict")

// IssueContentPatch is the first shared Public API write primitive. It is
// deliberately transport- and credential-agnostic: App, PAT, and Plugin
// entrypoints authorize independently, then call the same business operation.
type IssueContentPatch struct {
	Title            *string
	Description      *string
	ExpectedRevision *int64
}

// UpdateContent updates only the low-risk issue content fields exposed in the
// first Public API slice. Assignment, status, project, and hierarchy changes
// remain separate operations because each has additional policy and side
// effects.
func (s *IssueService) UpdateContent(ctx context.Context, issue db.Issue, patch IssueContentPatch) (db.Issue, error) {
	return s.UpdateContentInTx(ctx, s.Queries, issue, patch)
}

// UpdateContentInTx is the transaction-aware core of UpdateContent: it applies
// the same content patch (title/description with optional optimistic-revision
// check) using the caller-supplied *db.Queries, so an operation that must be
// atomic with other writes — e.g. the external-issue sync applier advancing a
// link baseline in the same transaction — reuses the one update primitive
// instead of open-coding UpdateIssue. Pass s.Queries for the standalone path.
func (s *IssueService) UpdateContentInTx(ctx context.Context, q *db.Queries, issue db.Issue, patch IssueContentPatch) (db.Issue, error) {
	params := db.UpdateIssueParams{
		ID:            issue.ID,
		AssigneeType:  issue.AssigneeType,
		AssigneeID:    issue.AssigneeID,
		StartDate:     issue.StartDate,
		DueDate:       issue.DueDate,
		ParentIssueID: issue.ParentIssueID,
		ProjectID:     issue.ProjectID,
		Stage:         issue.Stage,
	}
	if patch.ExpectedRevision != nil {
		params.ExpectedRevision = pgtype.Int8{Int64: *patch.ExpectedRevision, Valid: true}
	}
	if patch.Title != nil {
		params.Title = pgtype.Text{String: *patch.Title, Valid: true}
	}
	if patch.Description != nil {
		params.Description = pgtype.Text{String: *patch.Description, Valid: true}
	}

	updated, err := q.UpdateIssue(ctx, params)
	if patch.ExpectedRevision != nil && errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, ErrIssueRevisionConflict
	}
	return updated, err
}
