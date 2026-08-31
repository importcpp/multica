// Package ghsnapshot fetches a GitHub pull request's CI + mergeability state
// from the GitHub API and treats that response as the single source of truth
// for the PR card (MUL-5265, Plan C). Webhooks and page visits only trigger a
// refresh; nothing here infers state incrementally from webhook payloads.
//
// The package has three layers:
//   - Client (this file): a thin wrapper over the shared internal/integrations/
//     githubapi App-auth client (App JWT → cached installation token) that adds
//     the single GraphQL call this pipeline needs.
//   - snapshot.go: the single GraphQL query, contexts pagination, and
//     normalization into a flat per-check snapshot.
//   - refresh.go: the outbound work queue (dedup, single in-flight per PR,
//     bounded concurrency, Retry-After backoff), the head-SHA-guarded atomic
//     write, and the trigger surfaces (webhook / page visit / TTL sweep).
//
// Credential hygiene (acceptance criterion 6): the App private key and every
// installation token are treated as opaque secrets and are NEVER written to a
// log or embedded in an error message.
package ghsnapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/githubapi"
)

// RateLimitError signals that GitHub asked us to back off. It aliases the shared
// githubapi type so the refresh queue's backoff logic and the auth layer speak
// the same error.
type RateLimitError = githubapi.RateLimitError

// Client is an installation-token-authenticated GitHub API client for the PR
// snapshot GraphQL call. It delegates App auth + token caching to the shared
// githubapi.Client; a nil *Client is a valid "feature disabled" value.
type Client struct {
	api *githubapi.Client
	now func() time.Time
}

// NewClientFromEnv builds a Client from the shared githubapi env config. Both
// unset → (nil, nil): the App API is not configured; the caller degrades the
// whole feature off. Malformed key → (nil, err): operator-actionable.
func NewClientFromEnv() (*Client, error) {
	api, err := githubapi.NewFromEnv()
	if err != nil {
		return nil, err
	}
	if api == nil {
		return nil, nil
	}
	return &Client{api: api, now: time.Now}, nil
}

// NewClientFromAPI wraps an already-constructed shared client so the server can
// build githubapi once and hand it to every GitHub call site.
func NewClientFromAPI(api *githubapi.Client) *Client {
	if api == nil || !api.Enabled() {
		return nil
	}
	return &Client{api: api, now: time.Now}
}

// Enabled reports whether the App API is configured. A nil client is disabled.
func (c *Client) Enabled() bool { return c != nil && c.api.Enabled() }

// graphQL runs a single GraphQL query as the given installation and returns the
// raw `data` object. GitHub returns HTTP 200 even for query-level errors, so we
// inspect the `errors` array too, mapping a RATE_LIMITED error type to a
// RateLimitError.
func (c *Client) graphQL(ctx context.Context, installationID int64, query string, variables map[string]any) (json.RawMessage, error) {
	token, err := c.api.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	endpoint := c.api.APIBase() + "/graphql"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.api.HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, githubapi.RateLimitFromResponse(resp, c.now())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github graphql: unexpected status %d", resp.StatusCode)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, errors.New("github graphql: malformed response")
	}
	if len(envelope.Errors) > 0 {
		for _, e := range envelope.Errors {
			if e.Type == "RATE_LIMITED" {
				return nil, &RateLimitError{RetryAfter: time.Minute}
			}
		}
		// Surface the message but nothing else; GraphQL error messages do not
		// contain credentials.
		return nil, fmt.Errorf("github graphql error: %s", envelope.Errors[0].Message)
	}
	if len(envelope.Data) == 0 {
		return nil, errors.New("github graphql: empty data")
	}
	return envelope.Data, nil
}
