// Tests model inventories and their shared cache behavior.

package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

func TestHarnessCacheHistorical(t *testing.T) {
	t.Parallel()
	fixture := `{"codex":{"inventory":{"models":[{"id":"gpt-5","effortOptions":["low","high"],"contextWindow":200000},{"id":"nil-effort","effortOptions":null},{"id":"empty-effort","effortOptions":[]}]},"updated":"2020-01-01T00:00:00.123456789Z","env_hash":"key-hash"},"claude":null}`
	path := filepath.Join(t.TempDir(), "harnesses.json")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	c := OpenHarnessCache(path)
	inv, fresh := c.ModelInventory(harness.Codex, "key-hash")
	if fresh || len(inv.Models) != 3 || inv.Models[0].ID != "gpt-5" || inv.Models[0].ContextWindow != 200000 || !slices.Equal(inv.Models[0].EffortOptions, []string{"low", "high"}) || inv.Models[1].EffortOptions != nil || inv.Models[2].EffortOptions == nil {
		t.Fatalf("historical inventory: %#v, fresh=%t", inv, fresh)
	}
	if wrong, fresh := c.ModelInventory(harness.Codex, "wrong"); fresh || len(wrong.Models) != 0 {
		t.Fatal("API key mismatch hit cache")
	}
	c.SetModelInventory(harness.Codex, inv, "key-hash")
	reopened := OpenHarnessCache(path)
	got, fresh := reopened.ModelInventory(harness.Codex, "key-hash")
	if !fresh || !reflect.DeepEqual(got, inv) {
		t.Fatalf("reload: %#v, fresh=%t", got, fresh)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // Test-owned path inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"contextWindow": 200000`) || !strings.Contains(string(raw), `"effortOptions": null`) || !strings.Contains(string(raw), `"effortOptions": []`) || !strings.Contains(string(raw), `"claude": null`) {
		t.Fatalf("disk shape: %s", raw)
	}
}

func TestHarnessCacheRecovery(t *testing.T) {
	t.Parallel()
	for name, fixture := range map[string]string{
		"partial decode": `{"codex":{"inventory":{"models":[{"id":"old","effortOptions":[]}]},"updated":"2099-01-01T00:00:00Z"},"claude":{"inventory":{"models":42}}}`,
		"null":           `null`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "harnesses.json")
			if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
				t.Fatal(err)
			}
			c := OpenHarnessCache(path)
			if inv, fresh := c.ModelInventory(harness.Codex, ""); fresh || len(inv.Models) != 0 {
				t.Fatal("retained partial inventory")
			}
			c.SetModelInventory(harness.Codex, ModelInventory{Models: []Model{{ID: "new"}}}, "")
			got, fresh := OpenHarnessCache(path).ModelInventory(harness.Codex, "")
			if !fresh || len(got.Models) != 1 || got.Models[0].ID != "new" {
				t.Fatalf("recovery: %#v, fresh=%t", got, fresh)
			}
		})
	}
	t.Run("read error", func(t *testing.T) {
		t.Parallel()
		c := OpenHarnessCache(t.TempDir())
		if inv, fresh := c.ModelInventory(harness.Codex, ""); fresh || len(inv.Models) != 0 {
			t.Fatal("read failure produced inventory")
		}
	})
}
