// Tests historical preferences schemas, omission defaults, and exact write contracts.

package preferences

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPreferencesPersistence(t *testing.T) {
	t.Parallel()
	t.Run("settings_defaults", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, settings string
			valid          bool
		}{
			{"missing", "", false},
			{"null", `,"settings":null`, true},
			{"empty", `,"settings":{}`, true},
			{"null_delay", `,"settings":{"purgeDelay":null}`, true},
			{"zero_delay", `,"settings":{"purgeDelay":0}`, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				p := filepath.Join(t.TempDir(), "preferences.json")
				if err := os.WriteFile(p, []byte(`{"users":{"fixture":{"version":1`+tc.settings+`}}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				s, err := Open(p)
				if !tc.valid {
					if err == nil {
						t.Fatal("expected invalid purge delay")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := s.Get("fixture").Settings.PurgeDelay; got != 15*time.Second {
					t.Fatalf("purge delay = %s", got)
				}
			})
		}
	})
	t.Run("historical_round_trip", func(t *testing.T) {
		t.Parallel()
		const fixture = `{"users":{"fixture":{"version":1,"repositories":[{"path":"github/project","baseBranch":"develop","harness":"codex","model":"fixture-model","lastUsed":123}],"harness":"codex","models":{"codex":"fixture-model"},"efforts":{"codex":{"fixture-model":"high","":"low"}},"settings":{"autoFixOnCIFailure":true,"autoFixOnPROpen":false,"baseImage":"fixture-image","runtimeSettings":{"docker":{"containerPlatform":"linux/arm64","maxCPUs":4},"podman":{}},"purgeDelay":15000000000,"wellKnownCaches":{"go":true,"npm":false},"cacheMappings":[{"hostPath":"~/cache","containerPath":"","enabled":true}],"customMounts":[{"hostPath":"~/src","containerPath":"/src","enabled":false,"readOnly":true}],"runtimeName":"docker"}}}}`
		p := filepath.Join(t.TempDir(), "preferences.json")
		if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		before := s.Get("fixture")
		if before.Settings.RuntimeSettings["docker"].ContainerPlatform != "linux/arm64" || before.Efforts["codex"][""] != "low" {
			t.Fatalf("historical nested preferences changed: %#v", before)
		}
		if err := s.Update("fixture", func(*Preferences) {}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(p) //nolint:gosec // Test-owned file in t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		var got, want any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(fixture), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("preferences shape = %#v, want %#v", got, want)
		}
		again, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if after := again.Get("fixture"); !reflect.DeepEqual(after, before) {
			t.Fatal("preferences changed on reload")
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions = %v", info.Mode().Perm())
		}
	})
	t.Run("empty_collections", func(t *testing.T) {
		t.Parallel()
		f, err := decodeUsersFile([]byte(`{"users":{"fixture":{"version":1,"repositories":[],"models":{},"efforts":{"codex":null},"settings":{"cacheMappings":[],"customMounts":[],"runtimeSettings":{},"wellKnownCaches":{}}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		p := f.Users["fixture"]
		if p.Repositories == nil || p.Models == nil || p.Settings.CacheMappings == nil || p.Settings.CustomMounts == nil || p.Settings.RuntimeSettings == nil || p.Settings.WellKnownCaches == nil || p.Efforts["codex"] != nil {
			t.Fatal("empty and null collection distinctions changed")
		}
	})
}
