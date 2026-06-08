// Tests for forge client resolution.

package forgemanager

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

func TestForgeFor(t *testing.T) {
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
}
