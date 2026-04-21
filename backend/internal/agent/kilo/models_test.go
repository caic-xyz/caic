// Tests for Kilo model list sorting.

package kilo

import (
	"slices"
	"testing"
)

func TestSortModels(t *testing.T) {
	t.Run("AnthropicStale", func(t *testing.T) {
		input := []string{
			"anthropic/claude-3-haiku",
			"anthropic/claude-3.5-sonnet",
			"anthropic/claude-opus-4.6",
			"anthropic/claude-sonnet-4.6",
		}
		got := SortModels(input)
		wantRecent := []string{
			"anthropic/claude-opus-4.6",
			"anthropic/claude-sonnet-4.6",
		}
		wantOld := []string{
			"anthropic/claude-3-haiku",
			"anthropic/claude-3.5-sonnet",
		}
		want := slices.Concat(wantRecent, wantOld)
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("DeepSeekOnlyBareV", func(t *testing.T) {
		input := []string{
			"deepseek/deepseek-chat",
			"deepseek/deepseek-r1",
			"deepseek/deepseek-v3.1-terminus",
			"deepseek/deepseek-v3.2",
			"deepseek/deepseek-v3.2-exp",
		}
		got := SortModels(input)
		wantRecent := []string{"deepseek/deepseek-v3.2"}
		wantOld := []string{
			"deepseek/deepseek-chat",
			"deepseek/deepseek-r1",
			"deepseek/deepseek-v3.1-terminus",
			"deepseek/deepseek-v3.2-exp",
		}
		want := slices.Concat(wantRecent, wantOld)
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("GoogleVariantGrouping", func(t *testing.T) {
		input := []string{
			"google/gemini-2.5-pro",
			"google/gemini-3.1-pro-preview",
			"google/gemini-3.1-pro-preview-customtools",
			"google/gemini-2.5-flash",
			"google/gemini-3.1-flash-preview",
			"google/gemini-3.1-flash-image-preview",
			"google/gemini-2.0-flash-lite-001",
			"google/gemini-3.1-flash-lite-preview",
		}
		got := SortModels(input)
		wantRecent := []string{
			"google/gemini-3.1-flash-lite-preview",
			"google/gemini-3.1-flash-preview",
			"google/gemini-3.1-pro-preview",
		}
		wantOld := []string{
			"google/gemini-2.0-flash-lite-001",
			"google/gemini-2.5-flash",
			"google/gemini-2.5-pro",
			"google/gemini-3.1-flash-image-preview",
			"google/gemini-3.1-pro-preview-customtools",
		}
		want := slices.Concat(wantRecent, wantOld)
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("MinimaxOnlyBareM", func(t *testing.T) {
		input := []string{
			"minimax/minimax-01",
			"minimax/minimax-m1",
			"minimax/minimax-m2-her",
			"minimax/minimax-m2.5",
		}
		got := SortModels(input)
		wantRecent := []string{"minimax/minimax-m2.5"}
		wantOld := []string{
			"minimax/minimax-01",
			"minimax/minimax-m1",
			"minimax/minimax-m2-her",
		}
		want := slices.Concat(wantRecent, wantOld)
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("OpenAICodexLatest", func(t *testing.T) {
		input := []string{
			"openai/gpt-5",
			"openai/gpt-5-codex",
			"openai/gpt-5.1-codex-max",
			"openai/gpt-5.3-codex",
			"openai/o3",
		}
		got := SortModels(input)
		wantRecent := []string{"openai/gpt-5.3-codex"}
		wantOld := []string{
			"openai/gpt-5",
			"openai/gpt-5-codex",
			"openai/gpt-5.1-codex-max",
			"openai/o3",
		}
		want := slices.Concat(wantRecent, wantOld)
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("QwenLatestVersion", func(t *testing.T) {
		input := []string{
			"qwen/qwen-2.5-72b-instruct",
			"qwen/qwen-max",
			"qwen/qwen3-coder",
			"qwen/qwen3-coder:free",
			"qwen/qwen3.5-27b",
			"qwen/qwq-32b",
		}
		got := SortModels(input)
		wantRecent := []string{"qwen/qwen3.5-27b"}
		wantOld := []string{
			"qwen/qwen-2.5-72b-instruct",
			"qwen/qwen-max",
			"qwen/qwen3-coder",
			"qwen/qwen3-coder:free",
			"qwen/qwq-32b",
		}
		want := slices.Concat(wantRecent, wantOld)
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("XAIOnlyCode", func(t *testing.T) {
		input := []string{
			"x-ai/grok-3",
			"x-ai/grok-4",
			"x-ai/grok-code-fast-1",
		}
		got := SortModels(input)
		wantRecent := []string{"x-ai/grok-code-fast-1"}
		wantOld := []string{
			"x-ai/grok-3",
			"x-ai/grok-4",
		}
		want := slices.Concat(wantRecent, wantOld)
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("ZAILatestVersion", func(t *testing.T) {
		input := []string{
			"z-ai/glm-4.5v",
			"z-ai/glm-4.6",
			"z-ai/glm-4.6v",
			"z-ai/glm-4.7",
			"z-ai/glm-4.7-flash",
			"z-ai/glm-5",
		}
		got := SortModels(input)
		wantRecent := []string{"z-ai/glm-5"}
		wantOld := []string{
			"z-ai/glm-4.5v",
			"z-ai/glm-4.6",
			"z-ai/glm-4.6v",
			"z-ai/glm-4.7",
			"z-ai/glm-4.7-flash",
		}
		want := slices.Concat(wantRecent, wantOld)
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("NonTopAtEnd", func(t *testing.T) {
		input := []string{
			"mistralai/devstral-2512",
			"openai/gpt-5.3-codex",
			"cohere/command-r",
		}
		got := SortModels(input)
		want := []string{
			"openai/gpt-5.3-codex",
			"cohere/command-r",
			"mistralai/devstral-2512",
		}
		if !slices.Equal(got, want) {
			t.Errorf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("Empty", func(t *testing.T) {
		got := SortModels(nil)
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

func TestParseModelVersion(t *testing.T) {
	t.Run("Cases", func(t *testing.T) {
		tests := []struct {
			id      string
			wantKey string
			wantVer float64
			wantOK  bool
		}{
			{"openai/gpt-5.3-codex", "openai/gpt-*-codex", 5.3, true},
			{"anthropic/claude-opus-4.6", "anthropic/claude-opus-*", 4.6, true},
			{"google/gemini-3.1-pro-preview", "google/gemini-*-pro-preview", 3.1, true},
			{"mistralai/devstral-2512", "mistralai/devstral-*", 2512, true},
			{"mistralai/devstral-medium", "mistralai/devstral-medium", 0, false},
			{"noprefix", "noprefix", 0, false},
			{"anthropic/claude-3.7-sonnet:thinking", "anthropic/claude-*-sonnet:thinking", 3.7, true},
		}
		for _, tt := range tests {
			key, ver, ok := parseModelVersion(tt.id)
			if key != tt.wantKey || ver != tt.wantVer || ok != tt.wantOK {
				t.Errorf("parseModelVersion(%q) = (%q, %v, %v), want (%q, %v, %v)",
					tt.id, key, ver, ok, tt.wantKey, tt.wantVer, tt.wantOK)
			}
		}
	})
}

func TestIsProviderStale(t *testing.T) {
	t.Run("Cases", func(t *testing.T) {
		tests := []struct {
			provider string
			name     string
			want     bool
		}{
			// Anthropic.
			{"anthropic", "claude-3-haiku", true},
			{"anthropic", "claude-opus-4.6", false},
			// DeepSeek.
			{"deepseek", "deepseek-chat", true},
			{"deepseek", "deepseek-v3.2", false},
			{"deepseek", "deepseek-v3.2-exp", true},
			// Google.
			{"google", "gemini-3.1-pro-preview", false},
			{"google", "gemini-3.1-pro-preview-customtools", true},
			{"google", "gemini-3.1-flash-image-preview", true},
			{"google", "palm-2", true},
			// Minimax.
			{"minimax", "minimax-m2.5", false},
			{"minimax", "minimax-m2-her", true},
			{"minimax", "minimax-01", true},
			// OpenAI.
			{"openai", "gpt-5.3-codex", false},
			{"openai", "gpt-5-chat", true},
			{"openai", "o3", true},
			// Qwen.
			{"qwen", "qwen3.5-27b", false},
			{"qwen", "qwen3-coder", false},
			{"qwen", "qwen-2.5-72b-instruct", false},
			{"qwen", "qwen-max", true},
			{"qwen", "qwq-32b", true},
			// x-ai.
			{"x-ai", "grok-code-fast-1", false},
			{"x-ai", "grok-4", true},
			// Other.
			{"z-ai", "glm-5", false},
		}
		for _, tt := range tests {
			got := isProviderStale(tt.provider, tt.name)
			if got != tt.want {
				t.Errorf("isProviderStale(%q, %q) = %v, want %v",
					tt.provider, tt.name, got, tt.want)
			}
		}
	})
}

func TestQwenVersion(t *testing.T) {
	t.Run("Cases", func(t *testing.T) {
		tests := []struct {
			name    string
			wantVer float64
			wantOK  bool
		}{
			{"qwen3.5-27b", 3.5, true},
			{"qwen3-coder", 3, true},
			{"qwen-2.5-72b-instruct", 2.5, true},
			{"qwen2.5-coder-7b-instruct", 2.5, true},
			{"qwen-max", 0, false},
			{"qwen-plus", 0, false},
			{"qwq-32b", 0, false},
		}
		for _, tt := range tests {
			ver, ok := qwenVersion(tt.name)
			if ver != tt.wantVer || ok != tt.wantOK {
				t.Errorf("qwenVersion(%q) = (%v, %v), want (%v, %v)",
					tt.name, ver, ok, tt.wantVer, tt.wantOK)
			}
		}
	})
}
