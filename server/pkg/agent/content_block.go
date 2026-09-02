package agent

import (
	"bytes"
	"encoding/json"
)

// containsImageContentBlock reports whether raw is an Anthropic-style content
// payload carrying at least one image block.
//
// Adapters call this while they still hold the decoded provider payload. By the
// time a tool result reaches the daemon it is a plain string, and guessing from
// the string — "does it contain \"image\"" — misfires on ordinary output that
// merely mentions the word. The distinction matters because a consumer applying
// a byte budget must keep an image whole or drop it whole; a half-sliced base64
// blob renders as neither a picture nor readable text.
func containsImageContentBlock(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	// Cheap reject before paying for a full decode: tool results are the
	// largest payloads in the system, and almost none of them are images.
	if !bytes.Contains(raw, []byte(`"image"`)) {
		return false
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	blocks, ok := value.([]any)
	if !ok {
		// A single block may arrive unwrapped.
		blocks = []any{value}
	}
	for _, block := range blocks {
		entry, ok := block.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := entry["type"].(string); kind == "image" {
			return true
		}
	}
	return false
}
