// Tests for model list sorting.

package agent

import (
	"slices"
	"testing"
)

func TestSortModels(t *testing.T) {
	t.Run("dedup", func(t *testing.T) {
		input := []string{
			"openai/gpt-5",
			"openai/gpt-5.5",
			"openai/gpt-5.5-pro",
			"z-ai/glm-4.6",
			"z-ai/glm-5",
			"openrouter/x-ai/grok-3",
			"openrouter/x-ai/grok-4.3",
			"mistralai/devstral-medium",
		}
		got := SortModels(input)
		want := []string{
			"mistralai/devstral-medium",
			"openai/gpt-5.5",
			"openai/gpt-5.5-pro",
			"openrouter/x-ai/grok-4.3",
			"z-ai/glm-5",
		}
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("empty", func(t *testing.T) {
		got := SortModels(nil)
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

func TestParseModelVersion(t *testing.T) {
	t.Run("cases", func(t *testing.T) {
		tests := []struct {
			id       string
			wantProv string
			wantKey  string
			wantVer  float64
			wantOK   bool
		}{
			{"openai/gpt-5.3-codex", "openai", "openai/gpt-*", 5.3, true},
			{"anthropic/claude-opus-4.6", "anthropic", "anthropic/claude-opus-*", 4.6, true},
			{"google/gemini-3.1-pro-preview", "google", "google/gemini-*", 3.1, true},
			{"mistralai/devstral-2512", "mistralai", "mistralai/devstral-*", 2512, true},
			{"mistralai/devstral-medium", "mistralai", "mistralai/devstral-medium", 0, false},
			{"noprefix", "", "noprefix", 0, false},
			{"openrouter/x-ai/grok-4.3", "x-ai", "x-ai/grok-*", 4.3, true},
			{"openrouter/anthropic/claude-opus-4.6", "anthropic", "anthropic/claude-opus-*", 4.6, true},
		}
		for _, tt := range tests {
			prov, key, ver, ok := parseModelVersion(tt.id)
			if prov != tt.wantProv || key != tt.wantKey || ver != tt.wantVer || ok != tt.wantOK {
				t.Errorf("parseModelVersion(%q) = (%q, %q, %v, %v), want (%q, %q, %v, %v)",
					tt.id, prov, key, ver, ok, tt.wantProv, tt.wantKey, tt.wantVer, tt.wantOK)
			}
		}
	})
}
