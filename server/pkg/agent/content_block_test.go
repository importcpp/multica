package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContainsImageContentBlock(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"empty", "", false},
		{"plain text result", `"command finished with exit code 0"`, false},
		{"text content block", `[{"type":"text","text":"hello"}]`, false},
		{"image block in array", `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`, true},
		{"bare image block", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`, true},
		{"image alongside text", `[{"type":"text","text":"see below"},{"type":"image","source":{"data":"AAAA"}}]`, true},
		// The word appearing in ordinary output must not trip the detector:
		// misreading a log line as an image would replace it with a
		// placeholder and destroy the very content the log exists to show.
		{"text merely mentioning images", `"failed to load \"image\" from disk"`, false},
		{"json with image-like key", `{"type":"text","text":"{\"image\":\"foo\"}"}`, false},
		{"malformed json", `[{"type":"image"`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsImageContentBlock(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("containsImageContentBlock(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestContainsImageContentBlockLargePayload(t *testing.T) {
	block := `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		strings.Repeat("A", 60000) + `"}}]`
	if !containsImageContentBlock(json.RawMessage(block)) {
		t.Error("a real screenshot payload was not recognised as an image")
	}
}
