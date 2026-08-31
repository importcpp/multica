// Package githubapi is the shared GitHub App authentication layer: it turns a
// configured App private key into short-lived, per-installation access tokens,
// caching them per installation with early renewal and collapsing concurrent
// mints via singleflight. It also normalizes GitHub's throttling headers into a
// RateLimitError.
//
// It exists so the three GitHub call sites in the server — the PR-card snapshot
// pipeline (ghsnapshot), the repository-browse handler, and the external-issue
// import worker — share ONE auth/token implementation instead of each keeping
// its own. The App private key and every minted token are opaque secrets: they
// are never logged or embedded in an error message.
package githubapi

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
)

const (
	// DefaultAPIBase is the public GitHub REST/GraphQL root.
	DefaultAPIBase = "https://api.github.com"
	// tokenRenewSkew renews an installation token this long before it expires so
	// an in-flight request never races the expiry boundary (GitHub tokens live
	// one hour).
	tokenRenewSkew = 5 * time.Minute
)

// RateLimitError signals GitHub asked us to back off. RetryAfter is derived from
// Retry-After, then X-RateLimit-Reset, then a conservative default; it is always
// clamped to [1s, 5m].
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github rate limited, retry after %s", e.RetryAfter)
}

// TokenPermissions optionally narrows a minted installation token. A GitHub
// installation token can be requested with a subset of the App's granted
// permissions; the issue-import path asks for issues:read + metadata:read so a
// leaked token is least-privileged. The zero value requests the App's full
// granted set (used by the PR snapshot pipeline).
type TokenPermissions map[string]string

// Client is an installation-token-authenticated GitHub App client. A nil
// *Client is a valid "App not configured" value: Enabled reports false and
// callers degrade the feature off, so a deployment without a private key runs
// cleanly.
type Client struct {
	appID      string
	privateKey *rsa.PrivateKey
	apiBase    string
	httpClient *http.Client
	now        func() time.Time

	mu     sync.Mutex
	tokens map[tokenKey]cachedToken
	sf     singleflight.Group
}

// tokenKey caches per (installation, permission scope) so a full-scope token and
// a narrowed issues:read token for the same installation don't evict each other.
type tokenKey struct {
	installationID int64
	scope          string
}

type cachedToken struct {
	token  string
	expiry time.Time
}

// NewFromEnv builds a Client from GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY.
//   - Both unset → (nil, nil): App API not configured; caller degrades off.
//   - Key present but malformed → (nil, err): operator-actionable.
func NewFromEnv() (*Client, error) {
	appID := strings.TrimSpace(os.Getenv("GITHUB_APP_ID"))
	pemKey := strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY"))
	if appID == "" || pemKey == "" {
		return nil, nil
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pemKey))
	if err != nil {
		// Never include the key material in the error.
		return nil, fmt.Errorf("parse GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	return &Client{
		appID:      appID,
		privateKey: key,
		apiBase:    DefaultAPIBase,
		httpClient: &http.Client{Timeout: 20 * time.Second},
		now:        time.Now,
		tokens:     map[tokenKey]cachedToken{},
	}, nil
}

// Enabled reports whether the App API is configured. A nil client is disabled.
func (c *Client) Enabled() bool { return c != nil && c.privateKey != nil }

// APIBase returns the configured REST/GraphQL root (no trailing slash).
func (c *Client) APIBase() string {
	if c == nil || c.apiBase == "" {
		return DefaultAPIBase
	}
	return strings.TrimRight(c.apiBase, "/")
}

// HTTPClient exposes the shared http.Client so call sites reuse one transport.
func (c *Client) HTTPClient() *http.Client {
	if c == nil || c.httpClient == nil {
		return http.DefaultClient
	}
	return c.httpClient
}

// SetAPIBaseForTest points the client at a fake server; test-only.
func (c *Client) SetAPIBaseForTest(base string) { c.apiBase = base }

// NewClientForTest builds an enabled client with a caller-supplied key so other
// packages' tests can exercise call sites that need a working App client without
// reaching real GitHub. Test-only.
func NewClientForTest(appID string, key *rsa.PrivateKey) *Client {
	return &Client{
		appID:      appID,
		privateKey: key,
		apiBase:    DefaultAPIBase,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		now:        time.Now,
		tokens:     map[tokenKey]cachedToken{},
	}
}

// SignAppJWT mints the short-lived RS256 JWT GitHub requires for App-level
// calls (iat back-dated 60s for skew, exp capped at 9m under GitHub's 10m limit).
func (c *Client) SignAppJWT(now time.Time) (string, error) {
	if !c.Enabled() {
		return "", errors.New("github app not configured")
	}
	claims := jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": c.appID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(c.privateKey)
	if err != nil {
		return "", errors.New("sign App JWT failed")
	}
	return signed, nil
}

// InstallationToken returns a cached installation access token for the App's full
// granted scope, minting one when the cache is cold or within the renew skew.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	return c.InstallationTokenScoped(ctx, installationID, nil)
}

// InstallationTokenScoped returns a cached installation token narrowed to perms
// (nil = full granted scope). Concurrent callers for the same (installation,
// scope) are collapsed by singleflight so a cold cache mints once.
func (c *Client) InstallationTokenScoped(ctx context.Context, installationID int64, perms TokenPermissions) (string, error) {
	if !c.Enabled() {
		return "", errors.New("github app not configured")
	}
	key := tokenKey{installationID: installationID, scope: permScope(perms)}
	if tok, ok := c.cachedToken(key); ok {
		return tok, nil
	}
	v, err, _ := c.sf.Do(fmt.Sprintf("%d|%s", key.installationID, key.scope), func() (any, error) {
		if tok, ok := c.cachedToken(key); ok {
			return tok, nil
		}
		return c.mintInstallationToken(ctx, key, perms)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func permScope(perms TokenPermissions) string {
	if len(perms) == 0 {
		return ""
	}
	keys := make([]string, 0, len(perms))
	for k := range perms {
		keys = append(keys, k+"="+perms[k])
	}
	// Stable order so the cache key is deterministic regardless of map order.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return strings.Join(keys, ",")
}

func (c *Client) cachedToken(key tokenKey) (string, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tokens[key]; ok && now.Add(tokenRenewSkew).Before(t.expiry) {
		return t.token, true
	}
	return "", false
}

func (c *Client) mintInstallationToken(ctx context.Context, key tokenKey, perms TokenPermissions) (string, error) {
	now := c.now()
	appJWT, err := c.SignAppJWT(now)
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.APIBase(), key.installationID)
	var bodyReader io.Reader
	if len(perms) > 0 {
		payload, err := json.Marshal(map[string]any{"permissions": perms})
		if err != nil {
			return "", err
		}
		bodyReader = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bodyReader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+appJWT)
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// A 403/429 is a rate limit ONLY when GitHub actually signals throttling
	// (Retry-After, or X-RateLimit-Remaining: 0). A plain 403 — e.g. the
	// installation has not granted the requested permission — must surface as a
	// StatusError so callers can prompt a re-authorize instead of retrying
	// forever as if throttled.
	if resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode == http.StatusForbidden && isRateLimited(resp)) {
		return "", RateLimitFromResponse(resp, c.now())
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		// Never echo the body — a token-mint failure body can carry sensitive
		// hints; the status code is enough to diagnose.
		return "", &StatusError{StatusCode: resp.StatusCode}
	}
	var parsed struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", errors.New("github installation token: malformed response")
	}
	if parsed.Token == "" {
		return "", errors.New("github installation token: empty token")
	}
	expiry := now.Add(time.Hour)
	if parsed.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, parsed.ExpiresAt); err == nil {
			expiry = t
		}
	}
	c.mu.Lock()
	c.tokens[key] = cachedToken{token: parsed.Token, expiry: expiry}
	c.mu.Unlock()
	return parsed.Token, nil
}

// StatusError is a non-2xx GitHub response with the body deliberately withheld.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("github: unexpected status %d", e.StatusCode)
}

// isRateLimited reports whether a response actually carries a GitHub throttling
// signal — a Retry-After header, or X-RateLimit-Remaining: 0. Used to tell a
// throttled 403 apart from a permission/other 403.
func isRateLimited(resp *http.Response) bool {
	if strings.TrimSpace(resp.Header.Get("Retry-After")) != "" {
		return true
	}
	return strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0"
}

// RateLimitFromResponse builds a RateLimitError from GitHub's throttling headers.
// Retry-After (seconds) wins; then X-RateLimit-Reset (unix seconds); otherwise
// 60s. Clamped to [1s, 5m].
func RateLimitFromResponse(resp *http.Response, now time.Time) *RateLimitError {
	wait := time.Minute
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
			wait = time.Duration(secs) * time.Second
		}
	} else if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if unix, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			if d := time.Unix(unix, 0).Sub(now); d > 0 {
				wait = d
			}
		}
	}
	if wait < time.Second {
		wait = time.Second
	}
	if wait > 5*time.Minute {
		wait = 5 * time.Minute
	}
	return &RateLimitError{RetryAfter: wait}
}
