// Tests for forge Manager.

package forgemgr

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
)

func TestManager(t *testing.T) {
	t.Parallel()
	t.Run("ForgeFor", func(t *testing.T) {
		t.Parallel()
		t.Run("PAT", func(t *testing.T) {
			t.Parallel()
			m := New("pat-token", "", nil)
			if f := m.ForgeFor(t.Context(), forge.KindGitHub); f == nil {
				t.Fatal("ForgeFor returned nil with PAT set")
			}
		})

		t.Run("no token returns nil", func(t *testing.T) {
			t.Parallel()
			m := New("", "", nil)
			if f := m.ForgeFor(t.Context(), forge.KindGitHub); f != nil {
				t.Fatal("ForgeFor should return nil when no tokens available")
			}
		})

		t.Run("OAuth provider must match forge", func(t *testing.T) {
			t.Parallel()
			cases := []struct {
				provider auth.Provider
				kind     forge.Kind
				other    forge.Kind
			}{
				{provider: auth.ProviderGitHub, kind: forge.KindGitHub, other: forge.KindGitLab},
				{provider: auth.ProviderGitLab, kind: forge.KindGitLab, other: forge.KindGitHub},
			}
			for _, tc := range cases {
				t.Run(string(tc.provider), func(t *testing.T) {
					t.Parallel()
					m := New("", "", nil)
					ctx := auth.NewContext(t.Context(), &auth.User{Provider: tc.provider, AccessToken: t.Name()})
					if f := m.ForgeFor(ctx, tc.kind); f == nil {
						t.Fatal("ForgeFor returned nil for matching OAuth provider")
					}
					if f := m.ForgeFor(ctx, tc.other); f != nil {
						t.Fatal("ForgeFor returned a client for mismatched OAuth provider")
					}
				})
			}
		})
	})
}
