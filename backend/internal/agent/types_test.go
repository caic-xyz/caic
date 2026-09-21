// Tests for shared agent message and quota provider types.

package agent

import "testing"

func TestQuotaProvider(t *testing.T) {
	t.Parallel()
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		if !QuotaProviderClaudeCode.Valid() {
			t.Error("QuotaProviderClaudeCode.Valid() = false, want true")
		}
		if QuotaProvider("unknown").Valid() {
			t.Error("QuotaProvider(unknown).Valid() = true, want false")
		}
	})
	t.Run("String", func(t *testing.T) {
		t.Parallel()
		if got := QuotaProviderZai.String(); got != "Z.ai" {
			t.Errorf("QuotaProviderZai.String() = %q, want Z.ai", got)
		}
		if got := QuotaProviderCodex.String(); got != "Codex" {
			t.Errorf("QuotaProviderCodex.String() = %q, want Codex", got)
		}
		if got := QuotaProvider("unknown").String(); got != "unknown" {
			t.Errorf("unknown provider String() = %q, want the raw id", got)
		}
	})
	t.Run("QuotaProviderForModel", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			model string
			want  QuotaProvider
		}{
			{"zai/glm-5.3-flash", QuotaProviderZai},
			{"zai-coding-plan/glm-5.3-flash", QuotaProviderZai},
			{"openai-codex/gpt-5.6-terra", QuotaProviderCodex},
			{"openrouter/z-ai/glm-5.3-flash", QuotaProviderOpenRouter},
			{"deepseek/deepseek-flash", QuotaProviderDeepSeek},
			{"groq/llama-4", QuotaProviderGroq},
			{"claude-sonnet-4-5", ""},
			{"", ""},
		}
		for _, tc := range tests {
			if got := QuotaProviderForModel(tc.model); got != tc.want {
				t.Errorf("QuotaProviderForModel(%q) = %q, want %q", tc.model, got, tc.want)
			}
		}
	})
}

func TestRateLimitStatus(t *testing.T) {
	t.Parallel()
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		if !RateLimitStatusAllowedWarning.Valid() {
			t.Error("RateLimitStatusAllowedWarning.Valid() = false, want true")
		}
		if RateLimitStatus("unknown").Valid() {
			t.Error("RateLimitStatus(unknown).Valid() = true, want false")
		}
	})
}
