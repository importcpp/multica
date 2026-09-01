package externalissue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// apiBase is the GitHub REST root. It is a package var so tests can point the
// adapter at an httptest server; production always uses the default.
var apiBase = "https://api.github.com"

// githubMaxPerPage is GitHub's hard cap for the issues list endpoint. It is a
// var (not const) only so tests can shrink it to force multi-page keyset
// pagination without seeding hundreds of fixture issues; production is always 100.
var githubMaxPerPage = 100

// SetMaxPerPageForTest overrides the page size and returns a restore func.
// Test-only.
func SetMaxPerPageForTest(n int) func() {
	prev := githubMaxPerPage
	githubMaxPerPage = n
	return func() { githubMaxPerPage = prev }
}

// githubProvider implements Provider against the GitHub REST API. It is
// stateless: the resolved token arrives per-call in Credentials, so this
// adapter never mints or caches a token. When the shared internal/integrations/
// githubapi client lands (PR1), the raw doJSON/newRequest calls here move behind
// it; the Provider surface does not change.
type githubProvider struct{}

func init() { register(githubProvider{}) }

func (githubProvider) Kind() Kind { return KindGitHub }

func (githubProvider) Capabilities() Capabilities {
	return Capabilities{
		FilterByLabel:        true,
		FilterByUpdatedAfter: true,
		Webhooks:             true,
	}
}

// httpClient is shared; the 20s timeout matches ghsnapshot's client.
var httpClient = &http.Client{Timeout: 20 * time.Second}

func setHeaders(req *http.Request, creds Credentials) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "multica-externalissue")
	if creds.Token != "" {
		req.Header.Set("Authorization", "Bearer "+creds.Token)
	}
}

// base returns the API root for these creds. GitHub App / github.com use the
// package default; a self-hosted override is validated by the caller before it
// reaches here, but we still only ever build request URLs from it via the
// helpers below.
func (githubProvider) base(creds Credentials) string {
	if creds.InstanceBaseURL != "" {
		return strings.TrimRight(creds.InstanceBaseURL, "/")
	}
	return strings.TrimRight(apiBase, "/")
}

func (p githubProvider) ResolveRepository(ctx context.Context, creds Credentials, ref RepositoryRef) (Repository, error) {
	owner, name, err := splitFullPath(ref.FullPath)
	if err != nil {
		return Repository{}, &Error{Kind: ErrPermanent, Err: err}
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s", p.base(creds), url.PathEscape(owner), url.PathEscape(name))
	var repo struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if _, err := p.doJSON(ctx, creds, endpoint, &repo); err != nil {
		return Repository{}, err
	}
	full := repo.FullName
	if full == "" {
		full = owner + "/" + name
	}
	return Repository{
		InstanceKey: instanceKey(creds),
		ExternalID:  strconv.FormatInt(repo.ID, 10),
		FullPath:    full,
	}, nil
}

// githubIssue is the wire shape we care about. A non-nil PullRequest marks the
// entry as a PR the issues endpoint mixes in — those are dropped.
type githubIssue struct {
	ID          int64                   `json:"id"`
	Number      int64                   `json:"number"`
	Title       string                  `json:"title"`
	Body        string                  `json:"body"`
	State       string                  `json:"state"`
	HTMLURL     string                  `json:"html_url"`
	User        *struct{ Login string } `json:"user"`
	Labels      []struct{ Name string } `json:"labels"`
	CreatedAt   string                  `json:"created_at"`
	UpdatedAt   string                  `json:"updated_at"`
	ClosedAt    string                  `json:"closed_at"`
	PullRequest *json.RawMessage        `json:"pull_request"`
}

func (p githubProvider) ListIssues(ctx context.Context, creds Credentials, repo Repository, filter IssueFilter, cursor Cursor) (IssuePage, error) {
	since, err := parseKeysetCursor(cursor, filter)
	if err != nil {
		return IssuePage{}, err
	}
	endpoint, err := p.listURL(creds, repo, filter, since)
	if err != nil {
		return IssuePage{}, err
	}
	var raw []githubIssue
	if _, err := p.doJSON(ctx, creds, endpoint, &raw); err != nil {
		return IssuePage{}, err
	}
	page := IssuePage{Issues: make([]Issue, 0, len(raw))}
	for _, gi := range raw {
		if gi.PullRequest != nil {
			continue // the issues endpoint mixes in PRs; drop them from OUTPUT only.
		}
		page.Issues = append(page.Issues, normalizeIssue(gi))
	}
	// The cursor is derived from the RAW feed (PRs included). GitHub paginates the
	// raw feed, so a PR-only page still consumes the timeline; deriving the
	// boundary from non-PR issues only would leave a PR-only page unable to advance
	// and the worker would requeue forever.
	page.NextCursor, page.IncompleteBucket = nextKeysetCursor(raw, since)
	return page, nil
}

// parseKeysetCursor decodes the value-anchored cursor: the RFC3339 EXCLUSIVE
// updated_at lower bound (empty = the filter's UpdatedAfter, or zero = whole
// history). Enumerating purely by the updated_at VALUE — never a page offset into
// a live list — is what makes the scan safe against a concurrent delete/transfer:
// those only shrink the result set; a surviving issue keeps its updated_at, so it
// still appears after the anchor we resume from.
func parseKeysetCursor(cursor Cursor, filter IssueFilter) (since time.Time, err error) {
	if cursor == "" {
		return filter.UpdatedAfter, nil
	}
	t, perr := time.Parse(time.RFC3339, string(cursor))
	if perr != nil {
		return time.Time{}, &Error{Kind: ErrPermanent, Err: fmt.Errorf("malformed cursor")}
	}
	return t, nil
}

// nextKeysetCursor computes the next `since` (or "" when the scan is complete)
// and whether the page was an incomplete same-second bucket. raw is the FULL page
// (PRs included), sorted updated_at asc.
//
//   - Short page (< per_page): scan reached the end → done, "".
//   - Full page spanning >1 second (firstSec < lastSec): re-anchor `since` to
//     lastSecond-1s. GitHub `since` is EXCLUSIVE, so -1s re-includes the whole
//     last-second bucket; every earlier second was fully returned (bounded above
//     by a present later second). Pure value advance → delete/transfer-safe; the
//     re-read prefix is deduped by the identity ledger.
//   - Full page entirely within ONE second S: a same-second bucket larger than a
//     page. A second-granular `since` cannot enumerate the overflow, and a page
//     offset walk would be corrupted by a concurrent delete. So advance PAST the
//     second (since = S, exclusive) to guarantee progress + delete-safety, and
//     flag IncompleteBucket so the run is marked partial, not succeeded. In
//     practice >per_page(=100) issues sharing one second is pathological.
func nextKeysetCursor(raw []githubIssue, since time.Time) (Cursor, bool) {
	if len(raw) < githubMaxPerPage {
		return "", false
	}
	firstSec := truncateToSecond(raw[0].UpdatedAt)
	lastSec := truncateToSecond(raw[len(raw)-1].UpdatedAt)
	if firstSec.IsZero() || lastSec.IsZero() {
		// Unparseable timestamps: advance one second past the anchor so we cannot
		// stall, and flag incomplete since we can't reason about the boundary.
		return Cursor(since.Add(time.Second).UTC().Format(time.RFC3339)), true
	}
	if lastSec.After(firstSec) {
		// Value transition present → advance by value to the last-second bucket.
		return Cursor(lastSec.Add(-time.Second).UTC().Format(time.RFC3339)), false
	}
	// firstSec == lastSec: the whole full page is one second. Only treat it as an
	// unenumerable OVERFLOW when the page actually holds ≥2 same-second items —
	// that is the evidence the bucket could exceed a page. (A per_page of 1 can
	// never provide that evidence; real GitHub per_page is 100, so this fires only
	// for the pathological >per_page-in-one-second case.) Advance past S (exclusive
	// since = S) to guarantee progress + delete-safety, and flag it incomplete.
	if len(raw) >= 2 {
		return Cursor(firstSec.UTC().Format(time.RFC3339)), true
	}
	// A full page of a single item in one second: advance past that second by
	// value (exclusive), no overflow evidence.
	return Cursor(firstSec.UTC().Format(time.RFC3339)), false
}

// truncateToSecond parses an RFC3339 timestamp to second precision (GitHub
// timestamps are second-granular). Returns the zero time on parse failure.
func truncateToSecond(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.Truncate(time.Second)
}

func (p githubProvider) GetIssue(ctx context.Context, creds Credentials, repo Repository, ref IssueRef) (Issue, error) {
	owner, name, err := splitFullPath(repo.FullPath)
	if err != nil {
		return Issue{}, &Error{Kind: ErrPermanent, Err: err}
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d", p.base(creds), url.PathEscape(owner), url.PathEscape(name), ref.Number)
	var gi githubIssue
	if _, err := p.doJSON(ctx, creds, endpoint, &gi); err != nil {
		return Issue{}, err
	}
	if gi.PullRequest != nil {
		return Issue{}, &Error{Kind: ErrNotFound, Err: fmt.Errorf("issue #%d is a pull request", ref.Number)}
	}
	return normalizeIssue(gi), nil
}

// listURL builds the issues list URL for a value-anchored keyset page. Every URL
// is constructed here from the trusted API base + repo path and the cursor's
// (since, page) VALUES — the cursor is never a server-minted URL, so there is no
// Link-header SSRF surface to validate. We always sort by updated_at asc so the
// `since` lower bound advances monotonically by value; deletions/transfers only
// remove rows, they never reorder survivors past the anchor.
func (p githubProvider) listURL(creds Credentials, repo Repository, filter IssueFilter, since time.Time) (string, error) {
	owner, name, err := splitFullPath(repo.FullPath)
	if err != nil {
		return "", &Error{Kind: ErrPermanent, Err: err}
	}
	q := url.Values{}
	state := filter.State
	if state == "" {
		state = "open"
	}
	q.Set("state", state)
	q.Set("per_page", strconv.Itoa(githubMaxPerPage))
	q.Set("sort", "updated")
	q.Set("direction", "asc")
	if len(filter.Labels) > 0 {
		q.Set("labels", strings.Join(filter.Labels, ","))
	}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s/repos/%s/%s/issues?%s", p.base(creds), url.PathEscape(owner), url.PathEscape(name), q.Encode()), nil
}

// doJSON performs a GET and decodes a 2xx body into out. Non-2xx responses are
// mapped onto the normalized error taxonomy. The response is returned so the
// caller can read pagination headers.
func (p githubProvider) doJSON(ctx context.Context, creds Credentials, endpoint string, out any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &Error{Kind: ErrPermanent, Err: err}
	}
	setHeaders(req, creds)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrTransient, Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, classifyStatus(resp, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp, &Error{Kind: ErrPermanent, Err: fmt.Errorf("decode response: %w", err)}
		}
	}
	return resp, nil
}

// classifyStatus maps an HTTP error response onto the normalized taxonomy. A 403
// with an exhausted rate-limit budget is treated as ErrRateLimited (with a
// RetryAt) rather than a plain forbidden, because GitHub signals throttling that
// way as well as via 429.
func classifyStatus(resp *http.Response, body []byte) *Error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return &Error{Kind: ErrUnauthorized, Err: apiError(resp, body)}
	case http.StatusForbidden:
		if retryAt, ok := rateLimitReset(resp); ok {
			return &Error{Kind: ErrRateLimited, RetryAt: retryAt, Err: apiError(resp, body)}
		}
		return &Error{Kind: ErrForbidden, Err: apiError(resp, body)}
	case http.StatusTooManyRequests:
		retryAt, _ := rateLimitReset(resp)
		return &Error{Kind: ErrRateLimited, RetryAt: retryAt, Err: apiError(resp, body)}
	case http.StatusNotFound:
		return &Error{Kind: ErrNotFound, Err: apiError(resp, body)}
	}
	if resp.StatusCode >= 500 {
		return &Error{Kind: ErrTransient, Err: apiError(resp, body)}
	}
	return &Error{Kind: ErrPermanent, Err: apiError(resp, body)}
}

func apiError(resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return fmt.Errorf("github api %d: %s", resp.StatusCode, msg)
}

// rateLimitReset derives a resume time from Retry-After (seconds) or
// X-RateLimit-Reset (unix seconds) when the remaining budget is zero.
func rateLimitReset(resp *http.Response) (time.Time, bool) {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
			return time.Now().Add(time.Duration(secs) * time.Second), true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if unix, err := strconv.ParseInt(strings.TrimSpace(reset), 10, 64); err == nil {
				return time.Unix(unix, 0), true
			}
		}
	}
	return time.Time{}, false
}

func normalizeIssue(gi githubIssue) Issue {
	state := StateOpen
	if strings.EqualFold(gi.State, "closed") {
		state = StateClosed
	}
	labels := make([]string, 0, len(gi.Labels))
	for _, l := range gi.Labels {
		labels = append(labels, l.Name)
	}
	author := ""
	if gi.User != nil {
		author = gi.User.Login
	}
	return Issue{
		ExternalID:      strconv.FormatInt(gi.ID, 10),
		Number:          gi.Number,
		Title:           gi.Title,
		Body:            gi.Body,
		State:           state,
		HTMLURL:         gi.HTMLURL,
		AuthorLogin:     author,
		Labels:          labels,
		CreatedAt:       gi.CreatedAt,
		RemoteUpdatedAt: gi.UpdatedAt,
		ClosedAt:        gi.ClosedAt,
	}
}

func splitFullPath(full string) (owner, name string, err error) {
	full = strings.Trim(strings.TrimSpace(full), "/")
	parts := strings.Split(full, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repository path %q, want owner/name", full)
	}
	return parts[0], parts[1], nil
}

// instanceKey normalizes the origin for a set of credentials. github.com is a
// constant; a self-hosted base collapses to its host.
func instanceKey(creds Credentials) string {
	if creds.InstanceBaseURL == "" {
		return "github.com"
	}
	if u, err := url.Parse(creds.InstanceBaseURL); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return "github.com"
}
