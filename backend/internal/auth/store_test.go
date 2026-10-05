// Tests for historical users persistence, token preservation, and empty-file shape.

package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStorePersistence(t *testing.T) {
	t.Parallel()
	t.Run("historical", func(t *testing.T) {
		t.Parallel()
		const fixture = `{"version":1,"users":[{"id":"usr_fixture","provider":"google","providerID":"123","username":"fixture","avatarURL":"https://example.invalid/avatar","accessToken":"fixture-access","refreshToken":"fixture-refresh","tokenExpiry":"2026-01-01T00:00:00Z","createdAt":"2025-01-01T00:00:00Z","lastSeenAt":"2025-02-01T00:00:00Z"}]}`
		p := filepath.Join(t.TempDir(), "users.json")
		if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		u, ok := s.FindByProviderID(ProviderGoogle, "123")
		if !ok {
			t.Fatal("historical user not found")
		}
		if u.ID != "usr_fixture" || u.AccessToken != "fixture-access" || u.RefreshToken != "fixture-refresh" || u.TokenExpiry.Year() != 2026 || u.CreatedAt.Year() != 2025 {
			t.Fatalf("historical user changed: %#v", u)
		}
		if err := saveUsersFile(&s.file, p); err != nil {
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
			t.Fatalf("users shape = %#v, want %#v", got, want)
		}
		again, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		u2, ok := again.FindByID("usr_fixture")
		if !ok || u2 != u {
			t.Fatal("user changed on reload")
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions = %v", info.Mode().Perm())
		}
	})
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		for _, fixture := range []string{`{"version":1,"users":null}`, `{"version":1,"users":[]}`} {
			p := filepath.Join(t.TempDir(), "users.json")
			if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := saveUsersFile(&s.file, p); err != nil {
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
				t.Fatalf("empty shape = %#v, want %#v", got, want)
			}
		}
	})
}
