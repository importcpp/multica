package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
)

// TestLiveImportMulticaRepoIntoDB drives the FULL pipeline end to end against
// real infrastructure: the live GitHub adapter fetches issues from the public
// multica-ai/multica repo, and each one flows through the atomic Apply into a
// real Postgres workspace. It proves "issue import" works, not just that the
// provider can read — the acceptance the user asked for.
//
// Gated on EXTERNALISSUE_LIVE=1 (needs network) AND DATABASE_URL (needs DB):
//
//	EXTERNALISSUE_LIVE=1 DATABASE_URL=... go test ./internal/service/ -run TestLiveImportMulticaRepoIntoDB -v
func TestLiveImportMulticaRepoIntoDB(t *testing.T) {
	if os.Getenv("EXTERNALISSUE_LIVE") != "1" {
		t.Skip("set EXTERNALISSUE_LIVE=1 to run the live import test")
	}
	pool := newExternalIssuePool(t)
	f := seedApplyFixture(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	provider, ok := externalissue.For("github")
	if !ok {
		t.Fatal("github provider not registered")
	}
	creds := externalissue.Credentials{Token: os.Getenv("GITHUB_TOKEN")}
	repo, err := provider.ResolveRepository(ctx, creds, externalissue.RepositoryRef{FullPath: "multica-ai/multica"})
	if err != nil {
		t.Fatalf("resolve repo: %v", err)
	}

	// First page of open issues is enough to prove the round trip.
	page, err := provider.ListIssues(ctx, creds, repo, externalissue.IssueFilter{State: "open"}, "")
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	if len(page.Issues) == 0 {
		t.Skip("no open issues on the repo right now")
	}

	var imported int
	for _, iss := range page.Issues {
		var updated time.Time
		if iss.RemoteUpdatedAt != "" {
			updated, _ = time.Parse(time.RFC3339, iss.RemoteUpdatedAt)
		}
		out, err := f.svc.Apply(ctx, f.params(RemoteIssue{
			ExternalID:  iss.ExternalID,
			Provider:    "github",
			InstanceKey: repo.InstanceKey,
			Number:      iss.Number,
			Title:       iss.Title,
			Body:        iss.Body,
			State:       string(iss.State),
			HTMLURL:     iss.HTMLURL,
			UpdatedAt:   updated,
		}))
		if err != nil {
			t.Fatalf("apply issue #%d: %v", iss.Number, err)
		}
		if out == OutcomeImported {
			imported++
		}
	}
	if imported == 0 {
		t.Fatal("expected to import at least one issue")
	}
	if n := f.countIssues(t); n != imported {
		t.Fatalf("issue rows = %d, want %d", n, imported)
	}
	t.Logf("imported %d real multica-ai/multica issues into the DB", imported)

	// Re-import the same page: idempotent, no duplicates.
	for _, iss := range page.Issues {
		var updated time.Time
		if iss.RemoteUpdatedAt != "" {
			updated, _ = time.Parse(time.RFC3339, iss.RemoteUpdatedAt)
		}
		if _, err := f.svc.Apply(ctx, f.params(RemoteIssue{
			ExternalID: iss.ExternalID, Provider: "github", InstanceKey: repo.InstanceKey,
			Number: iss.Number, Title: iss.Title, Body: iss.Body, State: string(iss.State),
			HTMLURL: iss.HTMLURL, UpdatedAt: updated,
		})); err != nil {
			t.Fatalf("re-apply issue #%d: %v", iss.Number, err)
		}
	}
	if n := f.countIssues(t); n != imported {
		t.Fatalf("after re-import, issue rows = %d, want %d (no duplicates)", n, imported)
	}
	t.Logf("re-import produced no duplicates: still %d issues", imported)
}
