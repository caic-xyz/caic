// Tests default Antigravity registration and fresh relay wire construction.

package backends

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/agents/antigravity"
)

func TestDefault(t *testing.T) {
	t.Parallel()

	b := Default(t.TempDir(), nil)
	if _, ok := b[harness.Antigravity].(*antigravity.Backend); !ok {
		t.Fatalf("Antigravity backend = %T, want native implementation", b[harness.Antigravity])
	}
	first, err := b.ResolveWire(harness.Antigravity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.ResolveWire(harness.Antigravity)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("Antigravity tasks share a wire")
	}
}
