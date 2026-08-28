package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// TestWriteMeasuredJSONByteIdenticalToWriteJSON locks the load-bearing assumption
// behind the F2 claim-observability patch: swapping writeJSON for writeMeasuredJSON
// at the /tasks/claim response sites must not change a single byte on the wire.
//
// writeJSON and writeMeasuredJSON both encode via json.NewEncoder(buf).Encode
// through a pooled buffer (which appends a trailing newline and HTML-escapes by
// default), so the emitted bytes must match for every input. This table-driven
// test fails closed if that invariant ever drifts, so the "no wire-behavior
// change" claim is provable rather than reasoned.
func TestWriteMeasuredJSONByteIdenticalToWriteJSON(t *testing.T) {
	type skill struct {
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Files       map[string]string `json:"files"`
	}
	type claimResp struct {
		ID     string   `json:"id"`
		Name   string   `json:"name"`
		Skills []skill  `json:"skills"`
		Args   []string `json:"args"`
	}

	cases := []struct {
		name string
		v    any
	}{
		{"nil", nil},
		{"no_task", map[string]any{"task": nil}},
		{"empty_map", map[string]any{}},
		{"empty_slice", []string{}},
		{"scalar_string", "plain string"},
		{"scalar_bool", true},
		{"numbers", map[string]any{"i": 42, "f": 3.5, "neg": -17, "big": 1234567890123}},
		{"html_escapable", map[string]any{"s": `a<b> & "c" 'd' <script>`}},
		{"ampersand_lt_gt", map[string]any{"raw": "1 < 2 && 3 > 2"}},
		{"unicode_and_separators", map[string]any{"s": "héllo 世界 🚀   "}},
		{"nested", map[string]any{"a": []any{1, "two", true, nil}, "b": map[string]any{"c": []int{1, 2, 3}}}},
		{"large_claim_with_skills", map[string]any{"task": claimResp{
			ID:   "11111111-2222-3333-4444-555555555555",
			Name: "agent <CC> & friends",
			Skills: []skill{
				{Name: "multica-working-on-issues", Description: "do work <safely> & well", Files: map[string]string{"SKILL.md": "# Title\n<b>x</b> & y"}},
				{Name: "multica-mentioning", Description: "ping people", Files: map[string]string{"SKILL.md": "line1\nline2"}},
			},
			Args: []string{"--flag", "a<b", "c&d"},
		}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recEnc := httptest.NewRecorder()
			writeJSON(recEnc, http.StatusOK, tc.v)

			recMeasured := httptest.NewRecorder()
			n, err := writeMeasuredJSON(recMeasured, http.StatusOK, tc.v)
			if err != nil {
				t.Fatalf("writeMeasuredJSON returned error: %v", err)
			}

			encBody := recEnc.Body.Bytes()
			measuredBody := recMeasured.Body.Bytes()

			if !bytes.Equal(encBody, measuredBody) {
				t.Fatalf("wire bytes differ:\n writeJSON         = %q\n writeMeasuredJSON = %q", encBody, measuredBody)
			}
			if n != len(measuredBody) {
				t.Fatalf("reported payload bytes %d != actual body length %d", n, len(measuredBody))
			}
			if n != len(encBody) {
				t.Fatalf("reported payload bytes %d != writeJSON body length %d", n, len(encBody))
			}
			if recEnc.Code != recMeasured.Code {
				t.Fatalf("status code differs: writeJSON=%d writeMeasuredJSON=%d", recEnc.Code, recMeasured.Code)
			}
			if got, want := recMeasured.Header().Get("Content-Type"), recEnc.Header().Get("Content-Type"); got != want {
				t.Fatalf("Content-Type differs: writeMeasuredJSON=%q writeJSON=%q", got, want)
			}
		})
	}

	// Sanity guard: both encoders HTML-escape by default, so a literal '<' rune must
	// not survive into the body (it is emitted as the escaped form). This documents
	// the escaping behaviour that makes the byte-identity comparison meaningful,
	// without depending on the escaped literal appearing in source.
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"x": "<&>"})
	if bytes.ContainsRune(rec.Body.Bytes(), '<') {
		t.Fatalf("expected '<' to be HTML-escaped out of the body, got %q", rec.Body.String())
	}
}

// TestWriteJSONSetsContentLength verifies that the JSON response writers advertise
// an accurate Content-Length header. Encoding straight into the ResponseWriter after
// WriteHeader forces net/http into chunked transfer encoding (no Content-Length), so
// both writeJSON and writeMeasuredJSON buffer the body first and set the header
// explicitly. The value must equal the exact number of bytes written on the wire.
func TestWriteJSONSetsContentLength(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"empty_map", map[string]any{}},
		{"simple", map[string]string{"hello": "world"}},
		{"html_escapable", map[string]any{"s": `a<b> & "c" <script>`}},
		{"unicode", map[string]any{"s": "héllo 世界 🚀"}},
		{"nested", map[string]any{"a": []any{1, "two", true, nil}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeJSON(rec, http.StatusOK, tc.v)

			got := rec.Header().Get("Content-Length")
			if got == "" {
				t.Fatalf("writeJSON did not set Content-Length header")
			}
			cl, err := strconv.Atoi(got)
			if err != nil {
				t.Fatalf("Content-Length %q is not an integer: %v", got, err)
			}
			if cl != rec.Body.Len() {
				t.Fatalf("Content-Length = %d, want %d (actual body length)", cl, rec.Body.Len())
			}

			// writeMeasuredJSON must set the same, accurate Content-Length.
			recMeasured := httptest.NewRecorder()
			n, err := writeMeasuredJSON(recMeasured, http.StatusOK, tc.v)
			if err != nil {
				t.Fatalf("writeMeasuredJSON returned error: %v", err)
			}
			gotMeasured := recMeasured.Header().Get("Content-Length")
			clMeasured, err := strconv.Atoi(gotMeasured)
			if err != nil {
				t.Fatalf("writeMeasuredJSON Content-Length %q is not an integer: %v", gotMeasured, err)
			}
			if clMeasured != recMeasured.Body.Len() || clMeasured != n {
				t.Fatalf("writeMeasuredJSON Content-Length = %d, body = %d, reported = %d; all must match", clMeasured, recMeasured.Body.Len(), n)
			}
			if got != gotMeasured {
				t.Fatalf("Content-Length differs: writeJSON=%q writeMeasuredJSON=%q", got, gotMeasured)
			}
		})
	}
}

// TestWriteJSONTrailingNewline pins the trailing newline that json.Encoder.Encode
// emits. writeJSON used to append it by hand; after switching to a pooled
// bytes.Buffer + json.Encoder the newline must still be there, byte-for-byte, so
// existing clients and golden-file tests see no change.
func TestWriteJSONTrailingNewline(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"hello": "world"})

	body := rec.Body.Bytes()
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Fatalf("writeJSON body must end in a newline, got %q", body)
	}
	// Exactly one trailing newline, not two.
	if len(body) >= 2 && body[len(body)-2] == '\n' {
		t.Fatalf("writeJSON emitted a double trailing newline: %q", body)
	}
	cl, _ := strconv.Atoi(rec.Header().Get("Content-Length"))
	if cl != len(body) {
		t.Fatalf("Content-Length %d excludes/miscounts the newline (body %d)", cl, len(body))
	}
}

// TestWriteJSONEncodeErrorFallback covers the branch where json encoding fails
// (a channel is unmarshalable). The response must be the self-describing error
// body with a 500 and an accurate Content-Length, never a half-written payload.
func TestWriteJSONEncodeErrorFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"bad": make(chan int)})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got, want := rec.Body.String(), `{"error":"failed to encode response"}`+"\n"; got != want {
		t.Fatalf("fallback body = %q, want %q", got, want)
	}
	cl, err := strconv.Atoi(rec.Header().Get("Content-Length"))
	if err != nil || cl != rec.Body.Len() {
		t.Fatalf("Content-Length = %q, want %d", rec.Header().Get("Content-Length"), rec.Body.Len())
	}

	// writeMeasuredJSON reports zero bytes and surfaces the error on the same input.
	recMeasured := httptest.NewRecorder()
	n, err := writeMeasuredJSON(recMeasured, http.StatusOK, map[string]any{"bad": make(chan int)})
	if err == nil {
		t.Fatalf("writeMeasuredJSON should return an error for an unmarshalable value")
	}
	if n != 0 {
		t.Fatalf("writeMeasuredJSON reported %d bytes on error, want 0", n)
	}
}

// TestWriteJSONPoolReuseNoCorruption runs many concurrent writes with distinct
// payloads through the shared buffer pool: each response must contain exactly its
// own body, proving buffers are Reset on Get and never aliased across calls.
func TestWriteJSONPoolReuseNoCorruption(t *testing.T) {
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			writeJSON(rec, http.StatusOK, map[string]int{"seq": i})

			var got map[string]int
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Errorf("iter %d: body not valid JSON: %v (%q)", i, err, rec.Body.String())
				return
			}
			if got["seq"] != i {
				t.Errorf("iter %d: got seq=%d, buffer was reused without isolation", i, got["seq"])
			}
		}(i)
	}
	wg.Wait()
}
