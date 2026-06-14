// Tests for MCP tool and resource authorization policy.

package server

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
)

func TestCaicToolRegistryAuthorizeTool(t *testing.T) {
	t.Parallel()

	t.Run("remote forge tools require linked forge identity", func(t *testing.T) {
		t.Parallel()

		c := &caicToolRegistry{}
		tests := []struct {
			name string
			user *auth.User
		}{
			{name: "no user"},
			{name: "GitLab without token", user: &auth.User{Provider: forge.KindGitLab, Username: "alice"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				for name := range mcpForgeTools {
					ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{
						Scopes: map[string]struct{}{
							mcpScopeReposWrite: {},
						},
						Remote: true,
					})
					if tt.user != nil {
						ctx = auth.NewContext(ctx, tt.user)
					}

					reason, ok := c.authorizeTool(ctx, name)
					if ok {
						t.Fatalf("authorizeTool(%q) ok = true, want false", name)
					}
					if reason != "linked GitHub identity or GitLab token is required for forge MCP tools" {
						t.Fatalf("authorizeTool(%q) reason = %q", name, reason)
					}
				}
			})
		}
	})

	t.Run("remote forge tools allow linked GitHub server authority", func(t *testing.T) {
		t.Parallel()

		c := &caicToolRegistry{}
		for name := range mcpForgeTools {
			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{
				Scopes: map[string]struct{}{
					mcpScopeReposWrite: {},
				},
				Remote: true,
			})
			ctx = auth.NewContext(ctx, &auth.User{Provider: forge.KindGitHub, Username: "alice"})

			reason, ok := c.authorizeTool(ctx, name)
			if !ok {
				t.Fatalf("authorizeTool(%q) ok = false, reason = %q", name, reason)
			}
			if reason != "allow" {
				t.Fatalf("authorizeTool(%q) reason = %q", name, reason)
			}
		}
	})

	t.Run("remote forge tools allow linked user authority", func(t *testing.T) {
		t.Parallel()

		c := &caicToolRegistry{}
		for name := range mcpForgeTools {
			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{
				Scopes: map[string]struct{}{
					mcpScopeReposWrite: {},
				},
				Remote: true,
			})
			ctx = auth.NewContext(ctx, &auth.User{Provider: forge.KindGitLab, Username: "alice", AccessToken: "forge-token"})

			reason, ok := c.authorizeTool(ctx, name)
			if !ok {
				t.Fatalf("authorizeTool(%q) ok = false, reason = %q", name, reason)
			}
			if reason != "allow" {
				t.Fatalf("authorizeTool(%q) reason = %q", name, reason)
			}
		}
	})
}
