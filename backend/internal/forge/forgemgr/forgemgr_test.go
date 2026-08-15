// Tests for forge Manager.

package forgemgr

import (
	"context"
	"log/slog"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type fakeOAuthTokenSource map[forge.Kind]OAuthToken

func (s fakeOAuthTokenSource) TokenFor(_ context.Context, kind forge.Kind) (OAuthToken, bool) {
	token, ok := s[kind]
	return token, ok
}

func TestManager(t *testing.T) {
	t.Parallel()
	t.Run("ForgeFor", func(t *testing.T) {
		t.Parallel()
		t.Run("PAT", func(t *testing.T) {
			t.Parallel()
			m := New(testLogger(), "pat-token", "", nil, NoOAuthTokenSource())
			if f := m.ForgeFor(t.Context(), forge.KindGitHub); f == nil {
				t.Fatal("ForgeFor returned nil with PAT set")
			}
		})

		t.Run("no token returns nil", func(t *testing.T) {
			t.Parallel()
			m := New(testLogger(), "", "", nil, NoOAuthTokenSource())
			if f := m.ForgeFor(t.Context(), forge.KindGitHub); f != nil {
				t.Fatal("ForgeFor should return nil when no tokens available")
			}
		})

		t.Run("OAuth token source", func(t *testing.T) {
			t.Parallel()
			m := New(testLogger(), "", "", nil, fakeOAuthTokenSource{
				forge.KindGitHub: {AccessToken: t.Name(), UserID: "github-user"},
			})
			if f := m.ForgeFor(t.Context(), forge.KindGitHub); f == nil {
				t.Fatal("ForgeFor returned nil for OAuth token")
			}
			if f := m.ForgeFor(t.Context(), forge.KindGitLab); f != nil {
				t.Fatal("ForgeFor returned a client without an OAuth token")
			}
		})
	})
}
