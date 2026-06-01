// Tests for provider detection and title-generation LLM provider setup.

package server

import (
	"slices"
	"testing"

	"github.com/maruel/genai"
)

func TestAppendProviderAPIKey(t *testing.T) {
	t.Parallel()
	t.Run("deepseek from core env", func(t *testing.T) {
		t.Parallel()
		opts := appendProviderAPIKey(nil, "deepseek", map[string]string{
			"DEEPSEEK_API_KEY": "sk_from_core_env",
		}, "")

		if !slices.ContainsFunc(opts, func(o genai.ProviderOption) bool {
			v, ok := o.(genai.ProviderOptionAPIKey)
			return ok && string(v) == "sk_from_core_env"
		}) {
			t.Fatalf("opts = %#v, want DEEPSEEK_API_KEY from core env", opts)
		}
	})

	t.Run("gemini compatibility key", func(t *testing.T) {
		t.Parallel()
		opts := appendProviderAPIKeyWithEnv(nil, "gemini", nil, "AIza_compat", func(name string) string {
			if name == "GEMINI_API_KEY" {
				return "AIza_env"
			}
			return ""
		})

		if !slices.ContainsFunc(opts, func(o genai.ProviderOption) bool {
			v, ok := o.(genai.ProviderOptionAPIKey)
			return ok && string(v) == "AIza_compat"
		}) {
			t.Fatalf("opts = %#v, want compatibility Gemini API key", opts)
		}
	})
}
