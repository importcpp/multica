package externalissue

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withAPIBase points the adapter at srv for the duration of the test.
func withAPIBase(t *testing.T, base string) {
	t.Helper()
	prev := apiBase
	apiBase = base
	t.Cleanup(func() { apiBase = prev })
}

func TestListIssuesFiltersPullRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/") || !strings.HasSuffix(r.URL.Path, "/issues") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
			{"id":1,"number":1,"title":"real issue","state":"open","html_url":"h1","user":{"login":"alice"},"labels":[{"name":"bug"}]},
			{"id":2,"number":2,"title":"a PR","state":"open","html_url":"h2","pull_request":{"url":"x"}},
			{"id":3,"number":3,"title":"closed one","state":"closed","html_url":"h3"}
		]`)
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	p := githubProvider{}
	repo := Repository{InstanceKey: "github.com", ExternalID: "10", FullPath: "o/r"}
	page, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{State: "all"}, "")
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(page.Issues) != 2 {
		t.Fatalf("want 2 issues after PR filter, got %d: %+v", len(page.Issues), page.Issues)
	}
	if page.Issues[0].Number != 1 || page.Issues[0].AuthorLogin != "alice" || page.Issues[0].State != StateOpen {
		t.Errorf("issue #1 mapped wrong: %+v", page.Issues[0])
	}
	if len(page.Issues[0].Labels) != 1 || page.Issues[0].Labels[0] != "bug" {
		t.Errorf("labels mapped wrong: %+v", page.Issues[0].Labels)
	}
	if page.Issues[1].State != StateClosed {
		t.Errorf("issue #3 should be closed, got %q", page.Issues[1].State)
	}
	if page.NextCursor != "" {
		t.Errorf("no Link header => empty cursor, got %q", page.NextCursor)
	}
}

// The keyset cursor advances by the updated_at VALUE (since=), not a page
// offset, so a full page yields a NextCursor anchored at that page's newest
// updated_at, and a short page ends the scan.
func TestListIssuesKeysetAdvancesBySinceValue(t *testing.T) {
	// Page 1: a full page (per_page) all sharing distinct updated_at; page 2:
	// short page ends the scan. Assert the second request carries since=<max of
	// page 1> and sort=updated&direction=asc.
	var sawSecondSince string
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("sort") != "updated" || r.URL.Query().Get("direction") != "asc" {
			t.Errorf("want sort=updated&direction=asc, got %q", r.URL.RawQuery)
		}
		if since := r.URL.Query().Get("since"); since != "" {
			sawSecondSince = since
			// Second page: one issue, short -> ends scan.
			fmt.Fprint(w, `[{"id":999,"number":999,"title":"tail","state":"open","updated_at":"2026-03-01T00:00:00Z"}]`)
			return
		}
		// First page: exactly per_page issues so the scan continues; newest is
		// 2026-02-02.
		fmt.Fprint(w, "[")
		for i := 0; i < githubMaxPerPage; i++ {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			day := 1
			if i == githubMaxPerPage-1 {
				day = 2
			}
			fmt.Fprintf(w, `{"id":%d,"number":%d,"title":"i%d","state":"open","updated_at":"2026-02-0%dT00:00:00Z"}`, i+1, i+1, i+1, day)
		}
		fmt.Fprint(w, "]")
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	p := githubProvider{}
	repo := Repository{FullPath: "o/r"}
	page1, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{State: "all"}, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if page1.NextCursor == "" {
		t.Fatal("full first page must yield a next cursor")
	}
	page2, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{State: "all"}, page1.NextCursor)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if page2.NextCursor != "" {
		t.Fatalf("short second page must end the scan, got cursor %q", page2.NextCursor)
	}
	// Page 1 spans two seconds (2026-02-01 x99, 2026-02-02 x1), so the value
	// transition lets us advance to just-before the last second. GitHub `since` is
	// EXCLUSIVE, so since = lastSecond-1s re-includes the whole last-second bucket.
	if sawSecondSince != "2026-02-01T23:59:59Z" {
		t.Fatalf("second request since = %q, want lastSecond-1s (exclusive re-include)", sawSecondSince)
	}
	if hits != 2 {
		t.Fatalf("want 2 requests, got %d", hits)
	}
}

// A malformed persisted cursor is rejected as permanent (not silently treated as
// a fresh scan), so a corrupted checkpoint fails loudly rather than re-importing.
func TestListIssuesRejectsMalformedCursor(t *testing.T) {
	withAPIBase(t, "https://api.github.com")
	p := githubProvider{}
	repo := Repository{FullPath: "o/r"}
	_, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{}, Cursor("not-a-cursor"))
	if err == nil {
		t.Fatal("expected malformed cursor to be rejected")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != ErrPermanent {
		t.Fatalf("want ErrPermanent, got %v", err)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		hdr    map[string]string
		want   ErrorKind
	}{
		{http.StatusUnauthorized, nil, ErrUnauthorized},
		{http.StatusForbidden, nil, ErrForbidden},
		{http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "4102444800"}, ErrRateLimited},
		{http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, ErrRateLimited},
		{http.StatusNotFound, nil, ErrNotFound},
		{http.StatusInternalServerError, nil, ErrTransient},
		{http.StatusBadRequest, nil, ErrPermanent},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status-%d", tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.hdr {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"message":"boom"}`)
			}))
			defer srv.Close()
			withAPIBase(t, srv.URL)

			p := githubProvider{}
			_, err := p.ListIssues(context.Background(), Credentials{}, Repository{FullPath: "o/r"}, IssueFilter{}, "")
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("want *Error, got %T %v", err, err)
			}
			if e.Kind != tc.want {
				t.Fatalf("status %d => want %v, got %v", tc.status, tc.want, e.Kind)
			}
			if tc.want == ErrRateLimited && e.RetryAt.IsZero() {
				t.Errorf("rate limited error should carry RetryAt")
			}
		})
	}
}

func TestGetIssueRejectsPR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":5,"number":5,"title":"pr","state":"open","pull_request":{"url":"x"}}`)
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	p := githubProvider{}
	_, err := p.GetIssue(context.Background(), Credentials{}, Repository{FullPath: "o/r"}, IssueRef{Number: 5})
	var e *Error
	if !errors.As(err, &e) || e.Kind != ErrNotFound {
		t.Fatalf("want ErrNotFound for PR, got %v", err)
	}
}

func TestAuthHeaderSetWhenTokenPresent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	p := githubProvider{}
	if _, err := p.ListIssues(context.Background(), Credentials{Token: "tok123"}, Repository{FullPath: "o/r"}, IssueFilter{}, ""); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("want bearer token header, got %q", gotAuth)
	}
}

func TestRegistryLookup(t *testing.T) {
	p, ok := For("github")
	if !ok {
		t.Fatal("github provider not registered")
	}
	if p.Kind() != KindGitHub {
		t.Fatalf("wrong kind %q", p.Kind())
	}
	if !KindGitHub.Valid() {
		t.Error("KindGitHub should be valid")
	}
	if _, ok := For("bogus"); ok {
		t.Error("unknown provider should not resolve")
	}
}

func TestListURLBuildsFilter(t *testing.T) {
	p := githubProvider{}
	withAPIBase(t, "https://api.github.com")
	u, err := p.listURL(Credentials{}, Repository{FullPath: "o/r"}, IssueFilter{
		State:  "all",
		Labels: []string{"bug", "p0"},
	}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state=all", "per_page=100", "sort=updated", "direction=asc", "labels=bug%2Cp0", "since=2026-01-02T03%3A04%3A05Z"} {
		if !strings.Contains(u, want) {
			t.Errorf("listURL missing %q in %s", want, u)
		}
	}
}

// A full page consisting ENTIRELY of PRs must still advance the cursor by VALUE.
// PRs are dropped from output but they occupy the raw GitHub feed, so the
// boundary is computed from the raw page; deriving it from non-PR issues only
// would leave a PR-only page unable to advance and the worker would requeue
// forever. Regression for codex56 round-7.
func TestListIssuesPROnlyPageStillAdvances(t *testing.T) {
	t.Cleanup(SetMaxPerPageForTest(2))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		since := r.URL.Query().Get("since")
		// First page (no since): two PRs spanning two seconds -> full page, advances
		// by value to the last second.
		if since == "" {
			fmt.Fprint(w, `[
				{"id":1,"number":1,"title":"pr-a","state":"open","updated_at":"2026-02-01T00:00:00Z","pull_request":{"url":"x"}},
				{"id":2,"number":2,"title":"pr-b","state":"open","updated_at":"2026-02-01T00:00:01Z","pull_request":{"url":"y"}}
			]`)
			return
		}
		// After advancing: one real issue, short page -> ends the scan.
		fmt.Fprint(w, `[{"id":3,"number":3,"title":"real","state":"open","updated_at":"2026-02-01T00:00:01Z"}]`)
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	p := githubProvider{}
	repo := Repository{FullPath: "o/r"}
	page1, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{State: "all"}, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Issues) != 0 {
		t.Fatalf("page1 issues = %d, want 0 (all PRs filtered)", len(page1.Issues))
	}
	if page1.NextCursor == "" {
		t.Fatal("PR-only full page must still yield a next cursor (value advance), not stall")
	}
	if page1.IncompleteBucket {
		t.Fatal("PR-only page spanning two seconds is a clean value advance, not incomplete")
	}
	page2, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{State: "all"}, page1.NextCursor)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Issues) != 1 || page2.Issues[0].Number != 3 {
		t.Fatalf("page2 issues = %+v, want the real issue #3", page2.Issues)
	}
	if page2.NextCursor != "" {
		t.Fatalf("short page must end scan, got %q", page2.NextCursor)
	}
}

// A full page entirely within ONE updated_at second is a bucket larger than a
// page that a second-granular `since` cannot enumerate safely. The adapter
// advances PAST the second (guaranteeing progress + delete-safety) and flags
// IncompleteBucket so the worker marks the run partial. Regression for codex56
// round-7 "same-second cross-page".
func TestListIssuesSameSecondOverflowFlagsIncomplete(t *testing.T) {
	t.Cleanup(SetMaxPerPageForTest(2))
	var sawSecondSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		since := r.URL.Query().Get("since")
		if since == "" {
			// Full page (per_page=2) both in the same second -> overflow bucket.
			fmt.Fprint(w, `[
				{"id":1,"number":1,"title":"a","state":"open","updated_at":"2026-02-01T00:00:00Z"},
				{"id":2,"number":2,"title":"b","state":"open","updated_at":"2026-02-01T00:00:00Z"}
			]`)
			return
		}
		sawSecondSince = since
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	p := githubProvider{}
	repo := Repository{FullPath: "o/r"}
	page1, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{State: "all"}, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if !page1.IncompleteBucket {
		t.Fatal("same-second full page must flag IncompleteBucket")
	}
	if page1.NextCursor == "" {
		t.Fatal("same-second overflow must still advance past the second, not stall")
	}
	page2, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{State: "all"}, page1.NextCursor)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if page2.NextCursor != "" {
		t.Fatalf("empty page must end scan, got %q", page2.NextCursor)
	}
	// It advanced PAST the second (exclusive since = S), guaranteeing progress.
	if sawSecondSince != "2026-02-01T00:00:00Z" {
		t.Fatalf("second request since = %q, want the overflow second (advance past)", sawSecondSince)
	}
}
