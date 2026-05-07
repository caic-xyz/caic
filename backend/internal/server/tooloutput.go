// Tool output formatting utilities for the SSE event stream.

package server

import (
	"encoding/json"
	"strings"

	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

// FormatToolOutput analyzes a tool output string and returns its content type
// along with an optional formatted version. The formatted version is non-empty
// when the content can be meaningfully transformed (e.g. JSON pretty-printing).
//
// Detection order: JSON first (strict parse), then markdown (pattern matching),
// then plain text (fallback).
func FormatToolOutput(raw string) (contentType v1.ToolOutputContentType, formatted string) {
	if raw == "" {
		return v1.ToolOutputText, ""
	}

	if ct, fmt := formatAsJSON(raw); ct != "" {
		return ct, fmt
	}
	if looksLikeMarkdown(raw) {
		return v1.ToolOutputMarkdown, ""
	}
	return v1.ToolOutputText, ""
}

// formatAsJSON attempts to parse raw as JSON. On success it returns the
// pretty-printed version; on failure it returns empty strings.
func formatAsJSON(raw string) (ct v1.ToolOutputContentType, formatted string) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		// Not a JSON object; check for JSON array.
		if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
			return "", ""
		}
	}

	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", ""
	}

	formattedBytes, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", ""
	}
	return v1.ToolOutputJSON, string(formattedBytes)
}

// looksLikeMarkdown detects common markdown patterns in the text.
func looksLikeMarkdown(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "- ") ||
		strings.HasPrefix(trimmed, "* ") ||
		strings.HasPrefix(trimmed, "+ ") ||
		strings.Contains(trimmed, "```") ||
		strings.Contains(trimmed, "\n") // multi-line text is likely structured
}
