// Tests login eligibility, current account roles, session cookies, and browser metadata bounds.

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/gomodedev/gomode/oauth/oauthclient"
)

func TestAuthHandlers(t *testing.T) {
	t.Parallel()
	t.Run("handleGetMe/current role", func(t *testing.T) {
		t.Parallel()
		s, err := auth.Open(filepath.Join(t.TempDir(), "users.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		u, err := s.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "role-test"})
		if err != nil {
			t.Fatal(err)
		}
		router := newTestOAuthRouter(t, s)
		token, _, err := s.IssueToken(&u, router.sessionSecret, time.Hour, auth.SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		h, err := router.buildHandler()
		if err != nil {
			t.Fatal(err)
		}
		for _, role := range []auth.Role{auth.RoleMember, auth.RoleAdmin} {
			if err := s.SetRole(u.ID, role); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://caic.example.com/auth/me", http.NoBody)
			r.AddCookie(&http.Cookie{Name: "caic_session", Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var me v1.UserResp
			if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || me.ID != u.ID || me.Role != v1.Role(role) {
				t.Fatalf("me=%+v status=%d", me, w.Code)
			}
		}
	})
	t.Run("checkLogin", func(t *testing.T) {
		t.Parallel()
		for _, provider := range []auth.Provider{auth.ProviderGitHub, auth.ProviderGitLab, auth.ProviderGoogle} {
			t.Run(string(provider), func(t *testing.T) {
				t.Parallel()
				h := &authHandlers{
					githubOAuth: &oauthclient.ProviderConfig{}, gitlabOAuth: &oauthclient.ProviderConfig{}, googleOAuth: &oauthclient.ProviderConfig{},
					githubAllowedUsers: []string{"alice"}, gitlabAllowedUsers: []string{"alice"}, googleAllowedUsers: []string{"alice"},
				}
				t.Run("valid", func(t *testing.T) {
					t.Parallel()
					if err := h.checkLogin(provider, "Alice"); err != nil {
						t.Fatal(err)
					}
				})
				t.Run("error", func(t *testing.T) {
					t.Parallel()
					if err := h.checkLogin(provider, "Bob"); err == nil {
						t.Fatal("user outside allowlist accepted")
					}
				})
			})
		}
		t.Run("unrestricted", func(t *testing.T) {
			t.Parallel()
			h := &authHandlers{githubOAuth: &oauthclient.ProviderConfig{}}
			if err := h.checkLogin(auth.ProviderGitHub, "Anyone"); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("empty allowlist", func(t *testing.T) {
			t.Parallel()
			h := &authHandlers{githubOAuth: &oauthclient.ProviderConfig{}, githubAllowedUsers: []string{}}
			if err := h.checkLogin(auth.ProviderGitHub, "Anyone"); err == nil {
				t.Fatal("empty allowlist accepted a user")
			}
		})
		t.Run("provider unavailable", func(t *testing.T) {
			t.Parallel()
			h := &authHandlers{}
			if err := h.checkLogin(auth.ProviderGitHub, "Alice"); err == nil {
				t.Fatal("unconfigured provider accepted")
			}
		})
	})
	t.Run("writeSessionCookie", func(t *testing.T) {
		t.Parallel()
		h := &authHandlers{}
		w := httptest.NewRecorder()
		expires := time.Now().UTC().Truncate(time.Second).Add(90 * time.Second)
		h.writeSessionCookie(w, "credential", expires)
		cookies := w.Result().Cookies()
		if len(cookies) != 1 || !cookies[0].Expires.Equal(expires) || cookies[0].MaxAge < 88 || cookies[0].MaxAge > 90 {
			t.Fatalf("cookie does not follow issued expiry: %v", cookies)
		}
	})
	t.Run("sessionInfo", func(t *testing.T) {
		t.Parallel()
		h := &authHandlers{}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/auth/device/poll", nil)
		r.Header.Set("User-Agent", strings.Repeat("é", 129))
		if got := h.sessionInfo(r, "provider").UserAgent; got != strings.Repeat("é", 128) {
			t.Fatalf("session user agent = %q", got)
		}
	})
}
