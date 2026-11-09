// Tests explicit local authority, current role resolution, and invalid credentials.

package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestMiddleware(t *testing.T) {
	t.Parallel()
	t.Run("disabled with retained credentials", func(t *testing.T) {
		t.Parallel()
		store, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		u, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "1", Username: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		secret := []byte("0123456789abcdef0123456789abcdef")
		token, _, err := store.IssueToken(&u, secret, time.Hour, SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		for _, transport := range []string{"bearer", "cookie"} {
			t.Run(transport, func(t *testing.T) {
				t.Parallel()
				h := Middleware(nil, secret, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, ok := UserFromContext(r.Context()); ok || SessionIDFromContext(r.Context()) != "" {
						t.Fatal("disabled authentication injected a session identity")
					}
					if !LocalOperatorFromContext(r.Context()) {
						t.Fatal("local authority missing")
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
				if transport == "cookie" {
					req.AddCookie(&http.Cookie{Name: "caic_session", Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
				} else {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
				}
			})
		}
	})
	t.Run("current role and missing credentials", func(t *testing.T) {
		t.Parallel()
		s, err := Open(filepath.Join(t.TempDir(), "users.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		u, err := s.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "a"})
		if err != nil {
			t.Fatal(err)
		}
		secret := []byte("session signing secret")
		token, _, err := s.IssueToken(&u, secret, time.Hour, SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		var role Role
		h := Middleware(s, secret, nil)(RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if LocalOperatorFromContext(r.Context()) {
				t.Fatal("authenticated request became local")
			}
			u, _ := UserFromContext(r.Context())
			role = u.Role
			w.WriteHeader(http.StatusNoContent)
		})))
		request := func(token string) int {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/", http.NoBody)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w.Code
		}
		if status := request(token); status != http.StatusNoContent || role != RoleMember {
			t.Fatalf("initial status=%d role=%s", status, role)
		}
		if err := s.SetRole(u.ID, RoleAdmin); err != nil {
			t.Fatal(err)
		}
		if status := request(token); status != http.StatusNoContent || role != RoleAdmin {
			t.Fatalf("updated status=%d role=%s", status, role)
		}
		for _, token := range []string{"", "invalid"} {
			if status := request(token); status != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status=%d", status)
			}
		}
		unsigned, _, err := s.IssueToken(&u, nil, time.Hour, SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		h = Middleware(s, nil, nil)(RequireUser(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("missing secret granted access") })))
		if status := request(unsigned); status != http.StatusUnauthorized {
			t.Fatalf("missing secret status=%d", status)
		}
	})
}
