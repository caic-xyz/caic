// Tests account and session persistence, revocation, suspension lifetimes, administrator safety, and publication failures.

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	t.Run("configuration owns super administrator", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "users.json")
		store, err := Open(path, "github:root")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("opening configured authority created identity files")
		}
		other, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "other", Username: "root", Role: RoleSuperAdmin})
		if err != nil || other.Role != RoleMember {
			t.Fatalf("nonmatching account = %+v, %v", other, err)
		}
		root, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "root", Role: RoleAdmin, AccessToken: "provider-token"})
		if err != nil || root.Role != RoleSuperAdmin {
			t.Fatalf("configured account = %+v, %v", root, err)
		}
		for _, u := range store.Users() {
			if u.ID == root.ID && u.Role != RoleSuperAdmin {
				t.Fatal("account listing lost effective authority")
			}
		}
		providerUser, ok := store.FindByProvider(ProviderGitHub)
		if !ok || providerUser.Role != RoleSuperAdmin {
			t.Fatal("provider lookup lost effective authority")
		}
		refreshed, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "root", Role: RoleMember})
		if err != nil || refreshed.Role != RoleSuperAdmin {
			t.Fatal("provider refresh changed configured authority")
		}
		_, session, err := store.IssueToken(&root, []byte("secret"), time.Hour, SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		approved, ok := store.SessionUser(root.ID, session.ID)
		if !ok || approved.Role != RoleSuperAdmin {
			t.Fatal("session lost effective authority")
		}
		if err := store.SetRole(root.ID, RoleMember); err == nil {
			t.Fatal("application demoted configured account")
		}
		if err := store.SetRole(other.ID, RoleSuperAdmin); err == nil {
			t.Fatal("application granted configuration role")
		}
		if err := store.SetDisabled(root.ID, true); err == nil {
			t.Fatal("application disabled configured account")
		}
		if err := store.SetRole(other.ID, RoleAdmin); err != nil {
			t.Fatal(err)
		}
		if err := store.SetRole(other.ID, RoleMember); err != nil {
			t.Fatalf("ignored enabled super administrator: %v", err)
		}
		raw, err := os.ReadFile(path) //nolint:gosec // Test-owned users file in t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("super_admin")) {
			t.Fatal("configuration authority persisted as an application role")
		}
		// Changing configuration changes effective roles on existing sessions after restart.
		moved, err := Open(path, "github:other")
		if err != nil {
			t.Fatal(err)
		}
		old, ok := moved.SessionUser(root.ID, session.ID)
		if !ok || old.Role != RoleMember || old.AccountEpoch != root.AccountEpoch {
			t.Fatal("restart retained super authority or changed session generation")
		}
		current, ok := moved.FindByID(other.ID)
		if !ok || current.Role != RoleSuperAdmin {
			t.Fatal("new configured identity did not acquire authority")
		}
		if err := store.SetDisabled(other.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, "github:other"); err == nil {
			t.Fatal("disabled identity silently became super administrator")
		}
		recovered, err := Open(path, "github:recovery")
		if err != nil {
			t.Fatal(err)
		}
		stillDisabled, _ := recovered.FindByID(other.ID)
		if !stillDisabled.Disabled || stillDisabled.AccountEpoch == 0 {
			t.Fatal("recovery removed account fence")
		}
	})

	t.Run("super administrator selector validation", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, provider := range []Provider{ProviderGitHub, ProviderGitLab, ProviderGoogle} {
				store, err := Open(filepath.Join(t.TempDir(), "users.json"), string(provider)+":123")
				if err != nil {
					t.Fatal(err)
				}
				u, err := store.UpsertUser(&User{Provider: provider, ProviderID: "123"})
				if err != nil || u.Role != RoleSuperAdmin {
					t.Fatalf("provider %s: %+v %v", provider, u, err)
				}
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, selector := range []string{"GitHub:1", "github", "github:", "github: 1", "github:a:b", "google:1\n", "unknown:1"} {
				if _, err := Open(filepath.Join(t.TempDir(), "users.json"), selector); err == nil {
					t.Errorf("accepted %q", selector)
				}
			}
			path := filepath.Join(t.TempDir(), "users.json")
			if err := os.WriteFile(path, []byte(`{"version":1,"users":[{"id":"injected","role":"super_admin"}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, ""); err == nil {
				t.Fatal("persisted super_admin injected authority")
			}
		})
	})

	t.Run("pairing validates approver at publication", func(t *testing.T) {
		t.Parallel()
		store, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "approver"})
		if err != nil {
			t.Fatal(err)
		}
		secret := []byte("pair signing key")
		_, session, err := store.IssueToken(&user, secret, time.Hour, SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		captured, ok := store.SessionUser(user.ID, session.ID)
		if !ok {
			t.Fatal("fresh approver unavailable")
		}
		if err := store.RevokeSession(user.ID, session.ID); err != nil {
			t.Fatal(err)
		}
		// Simulate revocation after eligibility lookup but before session publication.
		if _, _, err := store.issueToken(&captured, secret, time.Hour, SessionInfo{}, session.ID); !errors.Is(err, ErrAccountDisabled) {
			t.Fatalf("revoked approval published session: %v", err)
		}
		if len(store.Sessions(user.ID)) != 0 {
			t.Fatal("revoked approval persisted a derived session")
		}
	})

	t.Run("detached owner retains original generation", func(t *testing.T) {
		t.Parallel()
		store, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "detached"})
		if err != nil {
			t.Fatal(err)
		}
		request := NewContext(t.Context(), &user)
		admitted, release, err := store.BindOwner(request, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		if err := store.SetDisabled(user.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := store.SetDisabled(user.ID, false); err != nil {
			t.Fatal(err)
		}
		for _, ctx := range []context.Context{request, admitted} {
			_, fresh, err := store.BindOwner(context.WithoutCancel(ctx), user.ID)
			if err == nil {
				fresh()
				t.Error("detached old generation regained owner authority")
			}
		}
	})

	t.Run("refresh cannot restore suspended session", func(t *testing.T) {
		t.Parallel()
		store, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "refresh", RefreshToken: "refresh-token", TokenExpiry: time.Now().Add(-time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		secret := []byte("refresh signing secret")
		token, _, err := store.IssueToken(&user, secret, time.Hour, SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		refresh := func(work context.Context, _ *User) *User {
			if err := store.SetDisabled(user.ID, true); err != nil {
				t.Fatal(err)
			}
			if work.Err() == nil {
				t.Error("provider refresh lifetime survived disablement")
			}
			if err := store.SetDisabled(user.ID, false); err != nil {
				t.Fatal(err)
			}
			current, _ := store.FindByID(user.ID)
			return &current
		}
		handler := Middleware(store, secret, refresh)(RequireUser(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/private", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("refreshed old session regained authority: %d", response.Code)
		}
	})

	t.Run("AccountDisablement", func(t *testing.T) {
		t.Parallel()
		t.Run("sessions lifetimes and restart", func(t *testing.T) {
			t.Parallel()
			s, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
			if err != nil {
				t.Fatal(err)
			}
			u, err := s.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "target"})
			if err != nil {
				t.Fatal(err)
			}
			secret := []byte("test secret")
			token, _, err := s.IssueToken(&u, secret, time.Hour, SessionInfo{})
			if err != nil {
				t.Fatal(err)
			}
			claims, err := validateToken(token, secret)
			if err != nil {
				t.Fatal(err)
			}
			work, release, err := s.BindOwner(t.Context(), u.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			if err := s.SetDisabled(u.ID, true); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(work.Err(), context.Canceled) {
				t.Fatal("existing work was not cancelled")
			}
			if _, valid := s.validateSession(claims, token); valid {
				t.Fatal("disabled session accepted")
			}
			if _, _, err := s.BindOwner(t.Context(), u.ID); !errors.Is(err, ErrAccountDisabled) {
				t.Fatalf("new work: %v", err)
			}
			if _, _, err := s.IssueToken(&u, secret, time.Hour, SessionInfo{}); !errors.Is(err, ErrAccountDisabled) {
				t.Fatalf("new login: %v", err)
			}
			if _, err := s.UpsertUser(&u); !errors.Is(err, ErrAccountDisabled) {
				t.Fatalf("provider login: %v", err)
			}
			again, err := Open(s.path, "")
			if err != nil {
				t.Fatal(err)
			}
			persisted, ok := again.FindByID(u.ID)
			if !ok || !persisted.Disabled || persisted.AccountEpoch != 1 || persisted.ProviderID != u.ProviderID {
				t.Fatalf("restart lost account fence: %+v", persisted)
			}
			if err := again.SetDisabled(u.ID, false); err != nil {
				t.Fatal(err)
			}
			if _, _, err := again.BindUser(t.Context(), &u); !errors.Is(err, ErrAccountDisabled) {
				t.Fatalf("stale request generation rebound: %v", err)
			}
			if _, valid := again.validateSession(claims, token); valid {
				t.Fatal("re-enable restored old session")
			}
			if sessions := again.Sessions(u.ID); len(sessions) != 0 {
				t.Fatalf("revoked sessions listed active: %v", sessions)
			}
			currentUser, _ := again.FindByID(u.ID)
			fresh, _, err := again.IssueToken(&currentUser, secret, time.Hour, SessionInfo{})
			if err != nil {
				t.Fatal(err)
			}
			freshClaims, err := validateToken(fresh, secret)
			if err != nil {
				t.Fatal(err)
			}
			if _, valid := again.validateSession(freshClaims, fresh); !valid {
				t.Fatal("fresh login rejected")
			}
			if work.Err() == nil {
				t.Fatal("re-enable restored an old lifetime")
			}
		})
		t.Run("authenticated HTTP stream closes", func(t *testing.T) {
			t.Parallel()
			store, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
			if err != nil {
				t.Fatal(err)
			}
			user, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "stream"})
			if err != nil {
				t.Fatal(err)
			}
			secret := []byte("stream signing secret")
			token, _, err := store.IssueToken(&user, secret, time.Hour, SessionInfo{})
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			done := make(chan struct{})
			handler := Middleware(store, secret, nil)(RequireUser(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
			})))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/stream", http.NoBody)
			req.Header.Set("Authorization", "Bearer "+token)
			var workers sync.WaitGroup
			workers.Go(func() { handler.ServeHTTP(httptest.NewRecorder(), req); close(done) })
			t.Cleanup(workers.Wait)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("stream did not start")
			}
			if err := store.SetDisabled(user.ID, true); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("HTTP stream survived disablement")
			}
		})

		t.Run("persistence failure does not publish", func(t *testing.T) {
			t.Parallel()
			s, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
			if err != nil {
				t.Fatal(err)
			}
			u, err := s.UpsertUser(&User{Provider: ProviderGoogle, ProviderID: "target"})
			if err != nil {
				t.Fatal(err)
			}
			work, release, err := s.BindOwner(t.Context(), u.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			if err := os.Mkdir(s.path+".tmp", 0o700); err != nil {
				t.Fatal(err)
			}
			if err := s.SetDisabled(u.ID, true); err == nil {
				t.Fatal("save unexpectedly succeeded")
			}
			if work.Err() != nil {
				t.Fatal("failed save cancelled work")
			}
			current, ok := s.ActiveUser(u.ID)
			if !ok || current.AccountEpoch != 0 {
				t.Fatalf("failed save changed authority: %+v", current)
			}
			again, err := Open(s.path, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, active := again.ActiveUser(u.ID); !active {
				t.Fatal("failed save changed persisted authority")
			}
		})
		t.Run("last enabled administrator", func(t *testing.T) {
			t.Parallel()
			s, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
			if err != nil {
				t.Fatal(err)
			}
			a, err := s.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "a"})
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "b"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetRole(a.ID, RoleAdmin); err != nil {
				t.Fatal(err)
			}
			if err := s.SetDisabled(a.ID, true); err == nil {
				t.Fatal("disabled last administrator")
			}
			if err := s.SetRole(b.ID, RoleAdmin); err != nil {
				t.Fatal(err)
			}
			if err := s.SetDisabled(b.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := s.SetRole(a.ID, RoleMember); err == nil {
				t.Fatal("disabled account counted as enabled administrator")
			}
			if err := s.SetRole(b.ID, RoleMember); err != nil {
				t.Fatalf("demoting disabled administrator removed no enabled authority: %v", err)
			}
		})
	})
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
			s, err := Open(p, "")
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
			if u.Role != RoleMember {
				t.Fatalf("historical role = %q", u.Role)
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
			again, err := Open(p, "")
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
		t.Run("unsupported version", func(t *testing.T) {
			t.Parallel()
			for _, fixture := range []string{`{"users":[]}`, `{"version":2,"users":[]}`} {
				p := filepath.Join(t.TempDir(), "users.json")
				if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := Open(p, ""); err == nil || !strings.Contains(err.Error(), "unsupported users version") {
					t.Fatalf("Open(%s) error = %v, want unsupported users version", fixture, err)
				}
			}
		})
		t.Run("empty", func(t *testing.T) {
			t.Parallel()
			for _, fixture := range []string{`{"version":1,"users":null}`, `{"version":1,"users":[]}`} {
				p := filepath.Join(t.TempDir(), "users.json")
				if err := os.WriteFile(p, []byte(fixture), 0o600); err != nil {
					t.Fatal(err)
				}
				s, err := Open(p, "")
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
	t.Run("SetRole", func(t *testing.T) {
		t.Parallel()
		s, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "a", Role: RoleAdmin})
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "b"})
		if err != nil {
			t.Fatal(err)
		}
		if a.Role != RoleMember || b.Role != RoleMember {
			t.Fatal("login assigned administrator authority")
		}
		if err := s.SetRole(a.ID, RoleAdmin); err != nil {
			t.Fatal(err)
		}
		if err := s.SetRole(a.ID, RoleMember); err == nil {
			t.Fatal("demoted last administrator")
		}
		if err := s.SetRole(b.ID, RoleAdmin); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		start := make(chan struct{})
		for _, id := range []string{a.ID, b.ID} {
			wg.Go(func() { <-start; errs <- s.SetRole(id, RoleMember) })
		}
		close(start)
		wg.Wait()
		close(errs)
		success := 0
		for err := range errs {
			if err == nil {
				success++
			}
		}
		if success != 1 {
			t.Fatalf("successful demotions = %d", success)
		}
		again, err := Open(s.path, "")
		if err != nil {
			t.Fatal(err)
		}
		admins := 0
		for _, u := range again.Users() {
			if u.Role == RoleAdmin {
				admins++
				u.AccessToken = "refreshed"
				updated, err := again.UpsertUser(&u)
				if err != nil || updated.Role != RoleAdmin {
					t.Fatalf("refresh lost role: %v", err)
				}
			}
		}
		if admins != 1 {
			t.Fatalf("administrators after restart = %d", admins)
		}
		if err := s.SetRole(a.ID, "owner"); err == nil {
			t.Fatal("accepted unknown role")
		}
		if err := s.SetRole("missing", RoleAdmin); !os.IsNotExist(err) {
			t.Fatalf("missing user: %v", err)
		}
	})
	t.Run("failed publication", func(t *testing.T) {
		t.Parallel()
		s, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		u, err := s.UpsertUser(&User{Provider: ProviderGoogle, ProviderID: "a", Username: "before"})
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(s.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(s.path+".tmp", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := s.SetRole(u.ID, RoleAdmin); err == nil {
			t.Fatal("role save unexpectedly succeeded")
		}
		if _, err := s.UpsertUser(&User{Provider: ProviderGoogle, ProviderID: "a", Username: "after"}); err == nil {
			t.Fatal("update unexpectedly succeeded")
		}
		if _, err := s.UpsertUser(&User{Provider: ProviderGoogle, ProviderID: "b"}); err == nil {
			t.Fatal("creation unexpectedly succeeded")
		}
		got, ok := s.FindByID(u.ID)
		if !ok || got != u || len(s.Users()) != 1 {
			t.Fatalf("failed save changed active users: %#v", s.Users())
		}
		after, err := os.ReadFile(s.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatal("failed save changed persisted users")
		}
	})
	t.Run("unknown stored role", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "users.json")
		if err := os.WriteFile(p, []byte(`{"version":1,"users":[{"id":"a","role":"owner"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(p, ""); err == nil {
			t.Fatal("opened invalid authority")
		}
	})
	t.Run("WithAccountAuthority", func(t *testing.T) {
		t.Parallel()
		store, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
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
				err := store.WithAccountAuthority(user.ID, user.AccountEpoch, func() error {
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
				err := store.WithAccountAuthority("missing", 0, func() error { calls++; return nil })
				if !errors.Is(err, ErrAccountDisabled) || calls != 0 {
					t.Fatalf("unknown account = %v, calls %d", err, calls)
				}
			})
			t.Run("callback", func(t *testing.T) {
				t.Parallel()
				failure := errors.New("publication failed")
				calls := 0
				err := store.WithAccountAuthority(user.ID, user.AccountEpoch, func() error { calls++; return failure })
				if !errors.Is(err, failure) || calls != 1 {
					t.Fatalf("publication = %v, calls %d", err, calls)
				}
			})
		})
	})
}
