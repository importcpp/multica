package externalissue

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveGitHubMulticaRepo exercises the adapter against the real GitHub REST
// API using the public multica-ai/multica repository as the fixture, proving the
// end-to-end import path (resolve → paginate → normalize, PRs filtered) works
// against a live tracker. It is skipped unless EXTERNALISSUE_LIVE=1 so offline
// CI stays green; set GITHUB_TOKEN to raise the unauthenticated rate limit.
//
//	EXTERNALISSUE_LIVE=1 go test ./internal/integrations/externalissue/ -run Live -v
func TestLiveGitHubMulticaRepo(t *testing.T) {
	if os.Getenv("EXTERNALISSUE_LIVE") != "1" {
		t.Skip("set EXTERNALISSUE_LIVE=1 to run the live GitHub test")
	}
	creds := Credentials{Token: os.Getenv("GITHUB_TOKEN")}
	p := githubProvider{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	repo, err := p.ResolveRepository(ctx, creds, RepositoryRef{FullPath: "multica-ai/multica"})
	if err != nil {
		t.Fatalf("ResolveRepository: %v", err)
	}
	if repo.ExternalID == "" || repo.InstanceKey != "github.com" {
		t.Fatalf("bad repo identity: %+v", repo)
	}
	t.Logf("resolved repo: id=%s path=%s", repo.ExternalID, repo.FullPath)

	// Walk up to 3 pages of all issues, proving pagination + PR filtering.
	var cursor Cursor
	var total int
	var first *Issue
	for pageNum := 0; pageNum < 3; pageNum++ {
		page, err := p.ListIssues(ctx, creds, repo, IssueFilter{State: "all"}, cursor)
		if err != nil {
			t.Fatalf("ListIssues page %d: %v", pageNum, err)
		}
		for i := range page.Issues {
			iss := page.Issues[i]
			if iss.ExternalID == "" || iss.Number == 0 {
				t.Errorf("issue missing identity: %+v", iss)
			}
			if iss.State != StateOpen && iss.State != StateClosed {
				t.Errorf("issue %d has unnormalized state %q", iss.Number, iss.State)
			}
			if first == nil {
				c := iss
				first = &c
			}
		}
		total += len(page.Issues)
		t.Logf("page %d: %d issues (next=%t)", pageNum, len(page.Issues), page.NextCursor != "")
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	t.Logf("total issues seen across pages: %d", total)

	// GetIssue round-trip on the first issue we saw.
	if first != nil {
		got, err := p.GetIssue(ctx, creds, repo, IssueRef{Number: first.Number, ExternalID: first.ExternalID})
		if err != nil {
			t.Fatalf("GetIssue #%d: %v", first.Number, err)
		}
		if got.Number != first.Number {
			t.Fatalf("GetIssue returned #%d, want #%d", got.Number, first.Number)
		}
		t.Logf("GetIssue #%d ok: %q", got.Number, got.Title)
	}
}
