// Tests model inventories and their shared cache behavior.

package agent

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestCachedModelInventory(t *testing.T) {
	t.Parallel()

	t.Run("empty cache directory", func(t *testing.T) {
		t.Parallel()

		if got := CachedModelInventory("", harness.Codex, nil); len(got.Models) != 0 {
			t.Fatalf("CachedModelInventory() = %#v, want empty inventory", got)
		}
	})
	t.Run("loads harness inventory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		envVars := []string{"OPENAI_API_KEY=secret"}
		cache := OpenHarnessCache(filepath.Join(dir, "harnesses.json"))
		inventory := ModelInventory{Models: []Model{{ID: "gpt-5", EffortOptions: []string{"low", "high"}}}}
		cache.SetModelInventory(harness.Codex, inventory, APIKeyHash(envVars))

		got := CachedModelInventory(dir, harness.Codex, envVars)
		if !slices.Equal(got.Models[0].EffortOptions, []string{"low", "high"}) {
			t.Fatalf("CachedModelInventory() = %#v, want cached inventory", got)
		}
	})
}

func TestHarnessCache(t *testing.T) {
	t.Parallel()

	cache := OpenHarnessCache(filepath.Join(t.TempDir(), "harnesses.json"))
	inventory := ModelInventory{Models: []Model{{ID: "gpt-5", EffortOptions: []string{"low", "high"}}}}
	cache.SetModelInventory(harness.Codex, inventory, "key-hash")

	got, fresh := cache.ModelInventory(harness.Codex, "key-hash")
	if !fresh || !slices.Equal(got.Models[0].EffortOptions, []string{"low", "high"}) {
		t.Fatalf("ModelInventory() = %#v fresh=%t, want cached inventory", got, fresh)
	}
}
