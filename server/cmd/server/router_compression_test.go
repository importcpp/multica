package main

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
)

func TestMainRouterCompressesJSONWhenRequested(t *testing.T) {
	router := NewRouter(nil, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)

	t.Run("gzip accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", got)
		}
		if got := strings.Join(rec.Header().Values("Vary"), ","); !strings.Contains(got, "Accept-Encoding") {
			t.Fatalf("Vary = %q, want it to include Accept-Encoding", got)
		}
		if got := rec.Header().Get("Content-Length"); got != "" {
			t.Fatalf("Content-Length = %q, want empty for compressed response", got)
		}

		reader, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("open gzip response: %v", err)
		}
		defer reader.Close()

		var body liveResponse
		if err := json.NewDecoder(reader).Decode(&body); err != nil {
			t.Fatalf("decode gzip response: %v", err)
		}
		if body.Status != "ok" {
			t.Fatalf("status = %q, want ok", body.Status)
		}
	})

	t.Run("gzip not accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()

		router.ServeHTTP(rec, req)

		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("Content-Encoding = %q, want empty", got)
		}
		if got := rec.Header().Get("Content-Length"); got == "" {
			t.Fatal("Content-Length is empty for uncompressed response")
		}

		var body liveResponse
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body.Status != "ok" {
			t.Fatalf("status = %q, want ok", body.Status)
		}
	})
}
