// Tests shared login eligibility, issued-session cookie expiry, and browser metadata bounds.

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/maruel/gomode/oauth/oauthclient"
)

func TestAuthHandlers(t *testing.T) {
	t.Parallel()
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
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/auth/device/poll", nil)
		w := httptest.NewRecorder()
		expires := time.Now().UTC().Truncate(time.Second).Add(90 * time.Second)
		h.writeSessionCookie(w, r, "credential", expires)
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
