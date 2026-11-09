// Tests user persistence, account publication fencing, token preservation, and empty-file shape.

package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStore(t *testing.T) {
	t.Parallel()
	t.Run("Persistence", func(t *testing.T) {
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
	})
	t.Run("WithUser", func(t *testing.T) {
		t.Parallel()
		store, err := Open(filepath.Join(t.TempDir(), "users.json"))
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.UpsertUser(&User{Provider: ProviderGoogle, ProviderID: "authority", Username: "Before"})
		if err != nil {
			t.Fatal(err)
		}
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			entered := make(chan bool, 1)
			release := make(chan struct{})
			published := make(chan error, 1)
			go func() {
				found, err := store.WithUser(user.ID, func() error {
					locked := !store.mu.TryLock()
					if !locked {
						store.mu.Unlock()
					}
					entered <- locked
					select {
					case <-release:
					case <-t.Context().Done():
					}
					return nil
				})
				if !found && err == nil {
					err = errors.New("known user missing")
				}
				published <- err
			}()
			if !<-entered {
				t.Fatal("publication does not hold the account lock")
			}
			started := make(chan struct{})
			mutated := make(chan error, 1)
			go func() {
				close(started)
				next := user
				next.Username = "After"
				_, err := store.UpsertUser(&next)
				mutated <- err
			}()
			<-started
			select {
			case err := <-mutated:
				t.Fatalf("account mutation passed held publication: %v", err)
			default:
			}
			close(release)
			if err := <-published; err != nil {
				t.Fatal(err)
			}
			if err := <-mutated; err != nil {
				t.Fatal(err)
			}
			updated, ok := store.FindByID(user.ID)
			if !ok || updated.Username != "After" {
				t.Fatalf("updated account = %#v, found %v", updated, ok)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("unknown", func(t *testing.T) {
				t.Parallel()
				calls := 0
				found, err := store.WithUser("missing", func() error { calls++; return nil })
				if found || err != nil || calls != 0 {
					t.Fatalf("unknown account = %v, %v, calls %d", found, err, calls)
				}
			})
			t.Run("callback", func(t *testing.T) {
				t.Parallel()
				failure := errors.New("publication failed")
				calls := 0
				found, err := store.WithUser(user.ID, func() error { calls++; return failure })
				if !found || !errors.Is(err, failure) || calls != 1 {
					t.Fatalf("publication = %v, %v, calls %d", found, err, calls)
				}
			})
		})
	})
}
