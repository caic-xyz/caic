// Tests for fake runtime and repository fixtures used by smoke and e2e tests.

package smoketest

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestInitHarnessCache(t *testing.T) {
	t.Parallel()

	cacheDir := t.TempDir()
	if err := InitHarnessCache(cacheDir); err != nil {
		t.Fatalf("InitHarnessCache: %v", err)
	}

	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
	for _, h := range []agent.Harness{agent.Codex, agent.Pi, agent.OpenCode} {
		models, fresh := cache.Models(h, "")
		if !fresh {
			t.Errorf("Models(%q) fresh = false, want true", h)
		}
		if !slices.Equal(models, []string{"fake-model"}) {
			t.Errorf("Models(%q) = %v, want [fake-model]", h, models)
		}
	}
}
