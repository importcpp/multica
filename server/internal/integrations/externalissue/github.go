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

// githubMaxPerPage is GitHub's hard cap for the issues list endpoint.
const githubMaxPerPage = 100

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
	endpoint, err := p.listURL(creds, repo, filter, cursor)
	if err != nil {
		return IssuePage{}, err
	}
	var raw []githubIssue
	resp, err := p.doJSON(ctx, creds, endpoint, &raw)
	if err != nil {
		return IssuePage{}, err
	}
	page := IssuePage{Issues: make([]Issue, 0, len(raw))}
	for _, gi := range raw {
		if gi.PullRequest != nil {
			continue // the issues endpoint mixes in PRs; drop them.
		}
		page.Issues = append(page.Issues, normalizeIssue(gi))
	}
	next, err := p.nextCursor(creds, resp.Header.Get("Link"))
	if err != nil {
		return IssuePage{}, err
	}
	page.NextCursor = next
	return page, nil
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

// listURL builds the first-page URL from the filter, or reuses the opaque cursor
// for subsequent pages. The cursor is a full GitHub-minted next URL; before
// trusting it we confirm it stays on the same approved origin and path — a
// server that returned a cross-host Link must not redirect our bearer token.
func (p githubProvider) listURL(creds Credentials, repo Repository, filter IssueFilter, cursor Cursor) (string, error) {
	if cursor != "" {
		if err := p.assertSameEndpoint(creds, repo, string(cursor)); err != nil {
			return "", err
		}
		return string(cursor), nil
	}
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
	q.Set("sort", "created")
	q.Set("direction", "asc")
	if len(filter.Labels) > 0 {
		q.Set("labels", strings.Join(filter.Labels, ","))
	}
	if !filter.UpdatedAfter.IsZero() {
		q.Set("since", filter.UpdatedAfter.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s/repos/%s/%s/issues?%s", p.base(creds), url.PathEscape(owner), url.PathEscape(name), q.Encode()), nil
}

// nextCursor extracts the rel="next" URL from a Link header, returning "" when
// there is no next page. The returned URL is validated for same-origin/path on
// the way back IN (listURL), not here, so a stored cursor is re-checked even if
// it was persisted between runs.
func (p githubProvider) nextCursor(creds Credentials, link string) (Cursor, error) {
	next := parseLinkNext(link)
	if next == "" {
		return "", nil
	}
	if err := p.assertSameOriginPath(creds, next); err != nil {
		return "", err
	}
	return Cursor(next), nil
}

// assertSameEndpoint validates a cursor URL points at the issues LIST endpoint
// of THIS repository on the approved origin — not merely some path containing
// "/issues". GitHub's own next link is one of:
//   - /repos/{owner}/{name}/issues
//   - /repositories/{externalID}/issues
//
// so we accept exactly those two shapes for this repo and reject anything else
// (a same-host link to another repo, another resource, or a deeper path).
func (p githubProvider) assertSameEndpoint(creds Credentials, repo Repository, raw string) error {
	u, err := p.parseSameOrigin(creds, raw)
	if err != nil {
		return err
	}
	path := strings.TrimRight(u.Path, "/")
	byFullPath := "/repos/" + repo.FullPath + "/issues"
	byID := "/repositories/" + repo.ExternalID + "/issues"
	if !strings.EqualFold(path, byFullPath) && path != byID {
		return &Error{Kind: ErrPermanent, Err: fmt.Errorf("pagination URL path %q is not this repo's issues endpoint", u.Path)}
	}
	return nil
}

// assertSameOriginPath rejects any URL whose scheme/host differs from the
// approved API base, or whose path is not an issues endpoint. Used for the
// rel="next" URL when the concrete repo endpoint shapes are already known to
// listURL; the stricter per-repo check is assertSameEndpoint.
func (p githubProvider) assertSameOriginPath(creds Credentials, raw string) error {
	u, err := p.parseSameOrigin(creds, raw)
	if err != nil {
		return err
	}
	// GitHub's own next link may point at /repositories/{id}/issues rather than
	// /repos/{owner}/{name}/issues; both are legitimate REST issue paths.
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/issues") {
		return &Error{Kind: ErrPermanent, Err: fmt.Errorf("pagination URL path %q is not an issues endpoint", u.Path)}
	}
	return nil
}

// parseSameOrigin parses raw and confirms it shares the approved API base's
// scheme+host, blocking a malicious Link from redirecting an authenticated
// request to an attacker host (SSRF / token exfiltration).
func (p githubProvider) parseSameOrigin(creds Credentials, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &Error{Kind: ErrPermanent, Err: fmt.Errorf("invalid pagination URL: %w", err)}
	}
	baseU, err := url.Parse(p.base(creds))
	if err != nil {
		return nil, &Error{Kind: ErrPermanent, Err: fmt.Errorf("invalid api base: %w", err)}
	}
	if !strings.EqualFold(u.Scheme, baseU.Scheme) || !strings.EqualFold(u.Host, baseU.Host) {
		return nil, &Error{Kind: ErrPermanent, Err: fmt.Errorf("pagination URL host %q not on approved origin %q", u.Host, baseU.Host)}
	}
	return u, nil
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

// parseLinkNext extracts the URL whose rel is "next" from an RFC 5988 Link
// header, or "" when absent.
func parseLinkNext(link string) string {
	if link == "" {
		return ""
	}
	for _, part := range strings.Split(link, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		rawURL := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(rawURL, "<") || !strings.HasSuffix(rawURL, ">") {
			continue
		}
		rawURL = rawURL[1 : len(rawURL)-1]
		for _, attr := range segs[1:] {
			attr = strings.TrimSpace(attr)
			if attr == `rel="next"` || attr == "rel=next" {
				return rawURL
			}
		}
	}
	return ""
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
