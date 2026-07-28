// Tests for fake runtime and repository fixtures used by smoke and e2e tests.

package smoketest

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestInitHarnessCache(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()
	if err := InitHarnessCache(cacheDir); err != nil {
		t.Fatalf("InitHarnessCache: %v", err)
	}

	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
	for _, h := range []harness.Name{harness.Codex, harness.Pi, harness.OpenCode} {
		inventory, fresh := cache.ModelInventory(h, "")
		if !fresh {
			t.Errorf("ModelInventory(%q) = %#v fresh=%t, want fresh inventory", h, inventory, fresh)
		}
		if !slices.Equal(inventory.IDs(), []string{"fake-model"}) {
			t.Errorf("ModelInventory(%q).IDs() = %v, want [fake-model]", h, inventory.IDs())
		}
	}
}
