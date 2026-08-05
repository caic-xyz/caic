// Tests server configuration, preference handlers, and runtime settings mappings.

package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/api"
)

func TestServerHandlersListHarnesses(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, map[harness.Name]agent.Backend{
		harness.Codex: &agenttest.FakeBackend{
			Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "gpt-5", EffortOptions: []string{"low", "ultra"}}}},
		},
	})

	got, err := s.serverHandlers.listHarnesses(t.Context(), &api.EmptyReq{})
	if err != nil {
		t.Fatalf("listHarnesses: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("len(harnesses) = %d, want 1", len(*got))
	}
	models := (*got)[0].Models
	if len(models) != 1 || models[0].ID != "gpt-5" || len(models[0].EffortOptions) != 2 {
		t.Fatalf("models = %#v, want gpt-5 with [low ultra]", models)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal harnesses: %v", err)
	}
	if strings.Contains(string(data), "null") {
		t.Fatalf("harnesses JSON = %s, want arrays instead of null", data)
	}
	if !strings.Contains(string(data), `"models":[{"id":"gpt-5"`) {
		t.Fatalf("harnesses JSON = %s, want gpt-5 in models", data)
	}
	if strings.Contains(string(data), `"modelCapabilities":`) || strings.Contains(string(data), `"modes":`) {
		t.Fatalf("harnesses JSON = %s, want models as the only model configuration", data)
	}
}

func TestSettingsMountsResolveContainerPaths(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		settings := &preferences.Settings{
			CacheMappings: []preferences.CacheMapping{{HostPath: "~/.cache/tool", Enabled: true}},
			CustomMounts:  []preferences.MountMapping{{HostPath: "~/.claude", Enabled: true}},
		}
		caches, err := cacheMountsFromSettings(settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(caches) != 1 || caches[0].ContainerPath != "/home/user/.cache/tool" {
			t.Fatalf("caches = %+v, want md-resolved container path", caches)
		}
		mounts, err := mountsFromSettings(settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(mounts) != 1 || mounts[0].ContainerPath != "/home/user/.claude" {
			t.Fatalf("mounts = %+v, want md-resolved container path", mounts)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		settings := &preferences.Settings{CacheMappings: []preferences.CacheMapping{{HostPath: "cache", ContainerPath: "/cache", Enabled: true}}}
		if _, err := cacheMountsFromSettings(settings); err == nil || !strings.Contains(err.Error(), "host path must be absolute or home-relative") {
			t.Fatalf("cacheMountsFromSettings() error = %v, want invalid host path", err)
		}
		settings = &preferences.Settings{CustomMounts: []preferences.MountMapping{{HostPath: "cache", ContainerPath: "/cache", Enabled: true}}}
		if _, err := mountsFromSettings(settings); err == nil || !strings.Contains(err.Error(), "custom mount 0: host path must be absolute or home-relative") {
			t.Fatalf("mountsFromSettings() error = %v, want indexed invalid host path", err)
		}
	})
}
