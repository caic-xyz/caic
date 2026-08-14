// Tests provider setup and auth-to-forge token adaptation.

package app

import (
	"slices"
	"testing"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
)

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
