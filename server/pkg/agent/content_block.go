package agent

import (
	"bytes"
	"encoding/json"
)

// ToolResultOutput converts a provider's raw tool_result content into the
// logical output a reader sees, plus whether that output is a structured image.
//
// Adapters call this while they still hold the decoded payload, because both
// answers are unrecoverable downstream. By the time the daemon sees a tool
// result it has only a string: it cannot tell a JSON-encoded string from a
// bare JSON document, and guessing at image-ness from the text would misfire on
// output that merely mentions the word.
//
// Unwrapping matters for more than tidiness. The byte count reported alongside
// a truncated preview is meant to be the size of the output the user saw; left
// wrapped, the same content measures differently depending on which provider
// produced it, because escaping inflates it. A wrapped string is also the shape
// that produces previews full of literal \n once cut.
func ToolResultOutput(raw json.RawMessage) (output string, isImage bool) {
	if len(raw) == 0 {
		return "", false
	}
	// A tool result delivered as a JSON string is transport encoding, not
	// content. Decode exactly one layer; anything else is passed through.
	var unwrapped string
	if json.Unmarshal(raw, &unwrapped) == nil {
		// A decoded string cannot be a structured content block.
		return unwrapped, false
	}
	return string(raw), containsImageContentBlock(raw)
}

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
