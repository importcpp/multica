package ghsnapshot

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/githubapi"
)

// newTestClient builds a snapshot Client backed by a githubapi client pointed at
// a fake API base. App auth internals are covered by githubapi's own tests; here
// we only need the graphQL path.
func newTestClient(t *testing.T, apiBase string) *Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	api := githubapi.NewClientForTest("123", key)
	api.SetAPIBaseForTest(apiBase)
	return &Client{api: api, now: time.Now}
}

// TestGraphQLRateLimited maps a 403 with Retry-After to a *RateLimitError so the
// refresh manager can back off (acceptance criterion 3).
func TestGraphQLRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` +
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.graphQL(context.Background(), 1, "query{}", nil)
	rl, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("err = %T (%v), want *RateLimitError", err, err)
	}
	if rl.RetryAfter != 42*time.Second {
		t.Fatalf("RetryAfter = %s, want 42s", rl.RetryAfter)
	}
}
