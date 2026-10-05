// Tests for historical startup settings persistence and secret preservation.

package app

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSettingsPersistence(t *testing.T) {
	t.Parallel()
	const fixture = `{"sessionSecret":"fixture-session-secret","mcpOAuthPrivateKeyPEM":"fixture-key-pem","mcpOAuthKeyID":"fixture-key-id"}`
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := loadSettings(slog.Default(), p)
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionSecret != "fixture-session-secret" || s.OAuthPrivateKeyPEM != "fixture-key-pem" || s.OAuthKeyID != "fixture-key-id" {
		t.Fatalf("historical secrets changed: %#v", s)
	}
	if err := writeSettingsAtomic(p, s); err != nil {
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
		t.Fatalf("settings shape = %#v, want %#v", got, want)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %v", info.Mode().Perm())
	}
	again, err := loadSettings(slog.Default(), p)
	if err != nil {
		t.Fatal(err)
	}
	if *again != *s {
		t.Fatal("secrets changed on reload")
	}
}
