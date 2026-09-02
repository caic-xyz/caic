// Tests server configuration, API contract parity, preference handlers, and runtime settings mappings.

package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestServerHandlers(t *testing.T) {
	t.Parallel()
	t.Run("list_harnesses", func(t *testing.T) {
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
	})

	t.Run("preferences", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)

		got, err := s.serverHandlers.getPreferences(t.Context(), &api.EmptyReq{})
		if err != nil {
			t.Fatalf("getPreferences: %v", err)
		}
		if got.Settings.PurgeDelay != 15*time.Second {
			t.Fatalf("default purge delay = %s, want %s", got.Settings.PurgeDelay, 15*time.Second)
		}

		want := 91 * time.Second
		got, err = s.serverHandlers.updatePreferences(t.Context(), &v1.UpdatePreferencesReq{
			Settings: v1.UserSettings{PurgeDelay: want},
		})
		if err != nil {
			t.Fatalf("updatePreferences: %v", err)
		}
		if got.Settings.PurgeDelay != want {
			t.Errorf("updated purge delay = %s, want %s", got.Settings.PurgeDelay, want)
		}
	})
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

func TestValidatePreferenceSettings(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		settings v1.UserSettings
		want     string
	}{
		{
			name:     "unknown cache",
			settings: v1.UserSettings{WellKnownCaches: map[string]bool{"bogus": true}},
			want:     "unknown cache: bogus",
		},
		{
			name:     "invalid cache mapping",
			settings: v1.UserSettings{CacheMappings: []v1.CacheMappingResp{{HostPath: "", ContainerPath: "/cache"}}},
			want:     "cacheMappings[0]: host path is required",
		},
		{
			name:     "invalid custom mount",
			settings: v1.UserSettings{CustomMounts: []v1.MountMappingResp{{HostPath: "/host", ContainerPath: ""}}},
			want:     "customMounts[0]: container path is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validatePreferenceSettings(&tc.settings)
			apiErr, ok := errors.AsType[*api.Error](err)
			if !ok {
				t.Fatalf("error = %T, want *api.Error", err)
			}
			if apiErr.Error() != tc.want {
				t.Errorf("error = %q, want %q", apiErr, tc.want)
			}
		})
	}
}

func TestServerAPIV1EnumParity(t *testing.T) {
	t.Parallel()

	t.Run("harnesses", func(t *testing.T) {
		t.Parallel()
		want := map[v1.Harness]harness.Name{
			v1.HarnessClaude:   harness.Claude,
			v1.HarnessCodex:    harness.Codex,
			v1.HarnessOpenCode: harness.OpenCode,
			v1.HarnessPi:       harness.Pi,
		}
		got := make(map[harness.Name]struct{}, len(want))
		for dtoHarness, agentHarness := range want {
			if string(dtoHarness) != string(agentHarness) {
				t.Errorf("DTO harness %q does not match agent harness %q", dtoHarness, agentHarness)
			}
			got[agentHarness] = struct{}{}
		}
		for _, agentHarness := range []harness.Name{harness.Claude, harness.Codex, harness.OpenCode, harness.Pi} {
			if _, ok := got[agentHarness]; !ok {
				t.Errorf("agent harness %q is missing from the v1 API", agentHarness)
			}
		}
	})

	t.Run("forges", func(t *testing.T) {
		t.Parallel()
		want := map[v1.Forge]forge.Kind{
			v1.ForgeGitHub: forge.KindGitHub,
			v1.ForgeGitLab: forge.KindGitLab,
		}
		got := make(map[forge.Kind]struct{}, len(want))
		for dtoForge, forgeKind := range want {
			if string(dtoForge) != string(forgeKind) {
				t.Errorf("DTO forge %q does not match forge kind %q", dtoForge, forgeKind)
			}
			got[forgeKind] = struct{}{}
		}
		for _, forgeKind := range []forge.Kind{forge.KindGitHub, forge.KindGitLab} {
			if _, ok := got[forgeKind]; !ok {
				t.Errorf("forge kind %q is missing from the v1 API", forgeKind)
			}
		}
	})
}
