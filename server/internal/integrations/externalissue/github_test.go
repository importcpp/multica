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

func TestListIssuesPaginationStops(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Query().Get("page") == "2" {
			// last page: no Link header
			fmt.Fprint(w, `[{"id":9,"number":9,"title":"last","state":"open"}]`)
			return
		}
		// first page advertises a same-origin next
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="next"`, apiBaseForLink(r)))
		fmt.Fprint(w, `[{"id":8,"number":8,"title":"first","state":"open"}]`)
	}))
	defer srv.Close()
	withAPIBase(t, srv.URL)

	p := githubProvider{}
	repo := Repository{FullPath: "o/r"}
	var cursor Cursor
	var all []Issue
	for {
		page, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{}, cursor)
		if err != nil {
			t.Fatalf("ListIssues: %v", err)
		}
		all = append(all, page.Issues...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(all) != 2 {
		t.Fatalf("want 2 issues across 2 pages, got %d", len(all))
	}
	if hits != 2 {
		t.Fatalf("want exactly 2 requests, got %d", hits)
	}
}

// apiBaseForLink returns the server origin the test handler is being hit on, so
// the injected Link stays same-origin (the SSRF guard requires it).
func apiBaseForLink(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func TestNextCursorRejectsForeignHost(t *testing.T) {
	withAPIBase(t, "https://api.github.com")
	p := githubProvider{}
	_, err := p.nextCursor(Credentials{}, `<https://evil.example.com/repos/o/r/issues?page=2>; rel="next"`)
	if err == nil {
		t.Fatal("expected rejection of cross-host cursor")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != ErrPermanent {
		t.Fatalf("want ErrPermanent, got %v", err)
	}
}

func TestListIssuesRejectsMaliciousCursor(t *testing.T) {
	withAPIBase(t, "https://api.github.com")
	p := githubProvider{}
	repo := Repository{FullPath: "o/r"}
	_, err := p.ListIssues(context.Background(), Credentials{}, repo, IssueFilter{}, Cursor("https://evil.example.com/steal?token=1"))
	if err == nil {
		t.Fatal("expected malicious cursor to be rejected before any request")
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
		State:        "all",
		Labels:       []string{"bug", "p0"},
		UpdatedAfter: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state=all", "per_page=100", "labels=bug%2Cp0", "since=2026-01-02T03%3A04%3A05Z"} {
		if !strings.Contains(u, want) {
			t.Errorf("listURL missing %q in %s", want, u)
		}
	}
}
