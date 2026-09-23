// Tests provider setup, historical cost estimation, and auth-to-forge token adaptation.

package app

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

func TestEstimateUsageRowCost(t *testing.T) {
	t.Parallel()
	pricer := usage.NewPricer(nil)
	at := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		harness string
		model   string
		tokens  usagedb.TokenBuckets
		want    float64
		priced  bool
	}{
		{"Claude unprefixed and one-hour cache", "claude", "claude-opus-5-5", usagedb.TokenBuckets{Input: 1_000_000, CacheWrite1h: 1_000_000, Output: 1_000_000}, 32, true},
		{"Codex unprefixed", "codex", "gpt-6-sol", usagedb.TokenBuckets{Input: 1_000_000, CacheRead: 1_000_000, Output: 1_000_000}, 12.2, true},
		{"OpenCode prefixed", "opencode", "openai/gpt-6-luna", usagedb.TokenBuckets{Input: 1_000_000, Output: 1_000_000}, 0.6, true},
		{"Pi prefixed", "pi", "anthropic/claude-opus-5-5", usagedb.TokenBuckets{CacheRead: 1_000_000}, 0.2, true},
		{"unknown harness", "new", "gpt-6-sol", usagedb.TokenBuckets{Input: 1_000_000}, 0, false},
		{"unknown model", "codex", "unpriced", usagedb.TokenBuckets{Input: 1_000_000}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := usagedb.UsageRow{Harness: tc.harness, Model: tc.model, TokenBuckets: tc.tokens}
			got, ok := estimateUsageRowCost(pricer, &row, at)
			if ok != tc.priced || math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("estimated cost = %v/%v, want %v/%v", got, ok, tc.want, tc.priced)
			}
		})
	}
}

func TestAuthForgeTokenSource(t *testing.T) {
	t.Parallel()
	source := authForgeTokenSource{}
	t.Run("matching provider", func(t *testing.T) {
		t.Parallel()
		ctx := auth.NewContext(t.Context(), &auth.User{ID: "user", Provider: auth.ProviderGitHub, AccessToken: t.Name()})
		token, ok := source.TokenFor(ctx, forge.KindGitHub)
		if !ok {
			t.Fatal("TokenFor returned no token")
		}
		if token.AccessToken != t.Name() || token.UserID != "user" {
			t.Errorf("token = %#v, want request user token", token)
		}
	})

	t.Run("mismatched provider", func(t *testing.T) {
		t.Parallel()
		ctx := auth.NewContext(t.Context(), &auth.User{Provider: auth.ProviderGitHub, AccessToken: t.Name()})
		if _, ok := source.TokenFor(ctx, forge.KindGitLab); ok {
			t.Fatal("TokenFor returned a token for mismatched forge")
		}
	})

	t.Run("GitLab provider", func(t *testing.T) {
		t.Parallel()
		ctx := auth.NewContext(t.Context(), &auth.User{ID: "gitlab-user", Provider: auth.ProviderGitLab, AccessToken: t.Name()})
		token, ok := source.TokenFor(ctx, forge.KindGitLab)
		if !ok {
			t.Fatal("TokenFor returned no GitLab token")
		}
		if token.AccessToken != t.Name() || token.UserID != "gitlab-user" {
			t.Errorf("token = %#v, want request user token", token)
		}
	})
}

func TestAppendProviderAPIKey(t *testing.T) {
	t.Parallel()
	t.Run("deepseek from core env", func(t *testing.T) {
		t.Parallel()
		opts := appendProviderAPIKey(nil, "deepseek", map[string]string{
			"DEEPSEEK_API_KEY": "sk_from_core_env",
		})

		if !slices.ContainsFunc(opts, func(o genai.ProviderOption) bool {
			v, ok := o.(genai.ProviderOptionAPIKey)
			return ok && string(v) == "sk_from_core_env"
		}) {
			t.Fatalf("opts = %#v, want DEEPSEEK_API_KEY from core env", opts)
		}
	})

	t.Run("gemini from environment", func(t *testing.T) {
		t.Parallel()
		opts := appendProviderAPIKeyWithEnv(nil, "gemini", nil, func(name string) string {
			if name == "GEMINI_API_KEY" {
				return "AIza_env"
			}
			return ""
		})

		if !slices.ContainsFunc(opts, func(o genai.ProviderOption) bool {
			v, ok := o.(genai.ProviderOptionAPIKey)
			return ok && string(v) == "AIza_env"
		}) {
			t.Fatalf("opts = %#v, want Gemini API key from environment", opts)
		}
	})
}

func TestUsageFetcherKey(t *testing.T) {
	t.Run("explicit env var precedence", func(t *testing.T) {
		t.Setenv("DASHSCOPE_API_KEY_US", "us-key")
		t.Setenv("DASHSCOPE_API_KEY", "fallback")
		key := usageFetcherKey([]string{"DASHSCOPE_API_KEY_US", "DASHSCOPE_API_KEY"}, "alibaba", nil, nil)
		if key != "us-key" {
			t.Fatalf("key = %q, want DASHSCOPE_API_KEY_US", key)
		}
	})

	t.Run("core env wins over process environment", func(t *testing.T) {
		t.Setenv("ZAI_API_KEY", "process")
		key := usageFetcherKey([]string{"ZAI_API_KEY"}, "zai", map[string]string{"ZAI_API_KEY": "core"}, nil)
		if key != "core" {
			t.Fatalf("key = %q, want core env value", key)
		}
	})

	t.Run("falls back to provider registry", func(t *testing.T) {
		t.Setenv("CEREBRAS_API_KEY", "registry")
		key := usageFetcherKey(nil, "cerebras", nil, nil)
		if key != "registry" {
			t.Fatalf("key = %q, want CEREBRAS_API_KEY from registry", key)
		}
	})
}
