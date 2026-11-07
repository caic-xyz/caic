// Tests disabled authentication with retained cookie and bearer credentials.

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
		store, err := Open(filepath.Join(t.TempDir(), "users.json"))
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
}
