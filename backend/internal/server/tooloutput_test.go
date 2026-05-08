// Tests for tooloutput.go

package server

import (
	"encoding/json"
	"testing"

	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

func TestFormatToolOutput(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		ct, formatted := FormatToolOutput("")
		if ct != v1.ToolOutputText {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputText)
		}
		if formatted != "" {
			t.Errorf("formatted = %q, want empty", formatted)
		}
	})

	t.Run("valid JSON object", func(t *testing.T) {
		t.Parallel()
		input := `{"name":"test","value":42}`
		ct, formatted := FormatToolOutput(input)
		if ct != v1.ToolOutputJSON {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputJSON)
		}
		if formatted == "" {
			t.Fatal("formatted is empty, want pretty-printed JSON")
		}
		// Verify the formatted output is valid JSON.
		var v any
		if err := json.Unmarshal([]byte(formatted), &v); err != nil {
			t.Errorf("formatted is not valid JSON: %v", err)
		}
		// Verify it's pretty-printed (contains newlines and indentation).
		if !contains(formatted, "\n") || !contains(formatted, "  ") {
			t.Errorf("formatted is not pretty-printed: %q", formatted)
		}
	})

	t.Run("valid JSON array", func(t *testing.T) {
		t.Parallel()
		input := `[1, 2, 3]`
		ct, formatted := FormatToolOutput(input)
		if ct != v1.ToolOutputJSON {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputJSON)
		}
		if formatted == "" {
			t.Fatal("formatted is empty, want pretty-printed JSON")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		t.Parallel()
		input := `{"name":"test",invalid}`
		ct, formatted := FormatToolOutput(input)
		if ct != v1.ToolOutputText {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputText)
		}
		if formatted != "" {
			t.Errorf("formatted = %q, want empty", formatted)
		}
	})

	t.Run("markdown heading", func(t *testing.T) {
		t.Parallel()
		input := "# Hello World\n\nThis is markdown."
		ct, formatted := FormatToolOutput(input)
		if ct != v1.ToolOutputMarkdown {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputMarkdown)
		}
		if formatted != "" {
			t.Errorf("formatted = %q, want empty", formatted)
		}
	})

	t.Run("markdown list", func(t *testing.T) {
		t.Parallel()
		input := "- Item 1\n- Item 2\n- Item 3"
		ct, _ := FormatToolOutput(input)
		if ct != v1.ToolOutputMarkdown {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputMarkdown)
		}
	})

	t.Run("markdown code fence", func(t *testing.T) {
		t.Parallel()
		input := "```\nsome code\n```"
		ct, _ := FormatToolOutput(input)
		if ct != v1.ToolOutputMarkdown {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputMarkdown)
		}
	})

	t.Run("plain text single line", func(t *testing.T) {
		t.Parallel()
		input := "Just some plain text output from a command."
		ct, formatted := FormatToolOutput(input)
		if ct != v1.ToolOutputText {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputText)
		}
		if formatted != "" {
			t.Errorf("formatted = %q, want empty", formatted)
		}
	})

	t.Run("plain text multi-line", func(t *testing.T) {
		t.Parallel()
		input := "Line one\nLine two\nLine three"
		ct, _ := FormatToolOutput(input)
		// Multi-line text is detected as markdown (structured).
		if ct != v1.ToolOutputMarkdown {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputMarkdown)
		}
	})

	t.Run("JSON with whitespace", func(t *testing.T) {
		t.Parallel()
		input := `  {"key": "value"}  `
		ct, _ := FormatToolOutput(input)
		if ct != v1.ToolOutputJSON {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputJSON)
		}
	})

	t.Run("JSON nested object", func(t *testing.T) {
		t.Parallel()
		input := `{"user":{"name":"Alice","age":30}}`
		ct, formatted := FormatToolOutput(input)
		if ct != v1.ToolOutputJSON {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputJSON)
		}
		if formatted == "" {
			t.Fatal("formatted is empty, want pretty-printed JSON")
		}
	})
}

func TestFormatAsJSON(t *testing.T) {
	t.Parallel()
	t.Run("simple object", func(t *testing.T) {
		t.Parallel()
		ct, formatted := formatAsJSON(`{"a":1}`)
		if ct != v1.ToolOutputJSON {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputJSON)
		}
		if formatted != "{\n  \"a\": 1\n}" {
			t.Errorf("formatted = %q, want %q", formatted, "{\n  \"a\": 1\n}")
		}
	})

	t.Run("simple array", func(t *testing.T) {
		t.Parallel()
		ct, formatted := formatAsJSON(`[1,2,3]`)
		if ct != v1.ToolOutputJSON {
			t.Errorf("contentType = %q, want %q", ct, v1.ToolOutputJSON)
		}
		if formatted != "[\n  1,\n  2,\n  3\n]" {
			t.Errorf("formatted = %q, want %q", formatted, "[\n  1,\n  2,\n  3\n]")
		}
	})

	t.Run("not JSON - starts with text", func(t *testing.T) {
		t.Parallel()
		ct, formatted := formatAsJSON("hello world")
		if ct != "" {
			t.Errorf("contentType = %q, want empty", ct)
		}
		if formatted != "" {
			t.Errorf("formatted = %q, want empty", formatted)
		}
	})

	t.Run("not JSON - invalid syntax", func(t *testing.T) {
		t.Parallel()
		ct, formatted := formatAsJSON(`{"key": "value",}`)
		if ct != "" {
			t.Errorf("contentType = %q, want empty", ct)
		}
		if formatted != "" {
			t.Errorf("formatted = %q, want empty", formatted)
		}
	})
}

func TestLooksLikeMarkdown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"heading", "# Title", true},
		{"unordered list dash", "- item", true},
		{"unordered list star", "* item", true},
		{"unordered list plus", "+ item", true},
		{"code fence", "```code```", true},
		{"multi-line", "line1\nline2", true},
		{"single line plain", "hello world", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := looksLikeMarkdown(tt.input)
			if got != tt.want {
				t.Errorf("looksLikeMarkdown(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// contains is a helper to check if a string contains a substring.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || contains(s[1:], substr)))
}
