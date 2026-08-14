// Tool-output formatting for API event conversion.

package apiconv

import (
	"encoding/json"
	"strings"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// FormatToolOutput analyzes a tool output string and returns its content type
// along with an optional formatted version.
func FormatToolOutput(raw string) (contentType v1.ToolOutputContentType, formatted string) {
	if raw == "" {
		return v1.ToolOutputText, ""
	}
	if ct, formatted := formatAsJSON(raw); ct != "" {
		return ct, formatted
	}
	if looksLikeMarkdown(raw) {
		return v1.ToolOutputMarkdown, ""
	}
	return v1.ToolOutputText, ""
}

func formatAsJSON(raw string) (ct v1.ToolOutputContentType, formatted string) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
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

func looksLikeMarkdown(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "- ") ||
		strings.HasPrefix(trimmed, "* ") ||
		strings.HasPrefix(trimmed, "+ ") ||
		strings.Contains(trimmed, "```") ||
		strings.Contains(trimmed, "\n")
}
