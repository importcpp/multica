package githubapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, apiBase string) *Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{
		appID:      "123",
		privateKey: key,
		apiBase:    apiBase,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		now:        time.Now,
		tokens:     map[tokenKey]cachedToken{},
	}
}

// TestInstallationTokenCaches proves the token is minted once and reused within
// the renew skew — the cache the whole server relies on to stay under GitHub's
// App-JWT budget.
func TestInstallationTokenCaches(t *testing.T) {
	var mints int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			atomic.AddInt32(&mints, 1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` +
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tok, err := c.InstallationToken(ctx, 42)
		if err != nil {
			t.Fatalf("InstallationToken: %v", err)
		}
		if tok != "ghs_secret" {
			t.Fatalf("token = %q", tok)
		}
	}
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Fatalf("minted %d times, want 1 (cache miss)", got)
	}
}

// TestScopedTokenSeparateCache proves a narrowed (issues:read) token and a
// full-scope token for the same installation are cached separately — the import
// path's least-privilege token must not be served to the snapshot pipeline.
func TestScopedTokenSeparateCache(t *testing.T) {
	var mints int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			n := atomic.AddInt32(&mints, 1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_` + string(rune('A'+n)) + `","expires_at":"` +
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ctx := context.Background()
	if _, err := c.InstallationToken(ctx, 7); err != nil {
		t.Fatalf("full: %v", err)
	}
	if _, err := c.InstallationTokenScoped(ctx, 7, TokenPermissions{"issues": "read", "metadata": "read"}); err != nil {
		t.Fatalf("scoped: %v", err)
	}
	// Each scope mints once; a repeat of each must hit cache.
	if _, err := c.InstallationToken(ctx, 7); err != nil {
		t.Fatalf("full repeat: %v", err)
	}
	if _, err := c.InstallationTokenScoped(ctx, 7, TokenPermissions{"metadata": "read", "issues": "read"}); err != nil {
		t.Fatalf("scoped repeat (reordered perms): %v", err)
	}
	if got := atomic.LoadInt32(&mints); got != 2 {
		t.Fatalf("minted %d times, want 2 (one per scope, reordered perms hit cache)", got)
	}
}

// TestInstallationTokenSingleflight proves concurrent callers for the same
// installation on a cold cache collapse into a single mint.
func TestInstallationTokenSingleflight(t *testing.T) {
	var mints int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			atomic.AddInt32(&mints, 1)
			<-release
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` +
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	const n = 16
	var wg sync.WaitGroup
	toks := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			toks[i], errs[i] = c.InstallationToken(context.Background(), 42)
		}(i)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Fatalf("minted %d times under %d concurrent callers, want 1", got, n)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil || toks[i] != "ghs_secret" {
			t.Fatalf("caller %d: token=%q err=%v", i, toks[i], errs[i])
		}
	}
}

// TestMintRateLimited maps a 403 with Retry-After from the token endpoint to a
// *RateLimitError.
func TestMintRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.InstallationToken(context.Background(), 1)
	rl, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("err = %T (%v), want *RateLimitError", err, err)
	}
	if rl.RetryAfter != 42*time.Second {
		t.Fatalf("RetryAfter = %s, want 42s", rl.RetryAfter)
	}
}

func TestRateLimitFromResponse(t *testing.T) {
	now := time.Unix(1000, 0)
	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{"retry-after wins", map[string]string{"Retry-After": "30", "X-RateLimit-Reset": "5000"}, 30 * time.Second},
		{"reset fallback", map[string]string{"X-RateLimit-Reset": "1090"}, 90 * time.Second},
		{"default", map[string]string{}, time.Minute},
		{"clamped to 5m", map[string]string{"Retry-After": "99999"}, 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			for k, v := range tc.headers {
				resp.Header.Set(k, v)
			}
			if got := RateLimitFromResponse(resp, now).RetryAfter; got != tc.want {
				t.Fatalf("wait = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestNewFromEnv covers the three configuration outcomes, including clean
// degradation: no key → nil client, no error.
func TestNewFromEnv(t *testing.T) {
	t.Run("unconfigured yields nil client no error", func(t *testing.T) {
		t.Setenv("GITHUB_APP_ID", "")
		t.Setenv("GITHUB_APP_PRIVATE_KEY", "")
		c, err := NewFromEnv()
		if err != nil || c != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", c, err)
		}
		if c.Enabled() {
			t.Fatal("nil client must report disabled")
		}
	})
	t.Run("malformed key is an error", func(t *testing.T) {
		t.Setenv("GITHUB_APP_ID", "1")
		t.Setenv("GITHUB_APP_PRIVATE_KEY", "-----BEGIN RSA PRIVATE KEY-----\nnope\n-----END RSA PRIVATE KEY-----")
		c, err := NewFromEnv()
		if err == nil {
			t.Fatal("want error for malformed key")
		}
		if strings.Contains(err.Error(), "nope") {
			t.Fatal("error leaked key material")
		}
		if c != nil {
			t.Fatal("want nil client on error")
		}
	})
	t.Run("valid key enables the client", func(t *testing.T) {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		t.Setenv("GITHUB_APP_ID", "1")
		t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pemBytes))
		c, err := NewFromEnv()
		if err != nil || !c.Enabled() {
			t.Fatalf("got (%v, %v), want enabled client", c, err)
		}
	})
}
