// Tests for host authentication state management.

package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/oauth/oauthclient"
)

// dummyReq returns a request with the given Host and optional X-Forwarded headers.
func dummyReq(t *testing.T, host, xfh, xfp string) *http.Request {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
	r.Host = host
	if xfh != "" {
		r.Header.Set("X-Forwarded-Host", xfh)
	}
	if xfp != "" {
		r.Header.Set("X-Forwarded-Proto", xfp)
	}
	return r
}

func TestHostState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		host string
		xfh  string
		xfp  string
		want string
	}{
		{"non-default HTTP port", "caic.example.com:8080", "", "", "http://caic.example.com:8080"},
		{"default HTTP port omitted", "caic.example.com:80", "", "", "http://caic.example.com"},
		{"default HTTPS port omitted", "caic.example.com:443", "", "https", "https://caic.example.com"},
		{"non-default HTTPS port", "quick.giraffe-cobra.ts.net:8443", "", "https", "https://quick.giraffe-cobra.ts.net:8443"},
		{"HTTPS without port", "caic.example.com", "", "https", "https://caic.example.com"},
		{"HTTP without port", "caic.example.com", "", "", "http://caic.example.com"},
		{"X-Forwarded-Host overrides r.Host", "127.0.0.1:8080", "caic.example.com", "https", "https://caic.example.com"},
		{"X-Forwarded-Host with port", "127.0.0.1:8080", "caic.example.com:8443", "https", "https://caic.example.com:8443"},
	} {
		t.Run("lock "+tc.name, func(t *testing.T) {
			t.Parallel()
			state := &auth.HostState{}
			r := dummyReq(t, tc.host, tc.xfh, tc.xfp)
			if got := state.ExternalURL(r); got != tc.want {
				t.Errorf("ExternalURL = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("RedirectURI resolves lazily", func(t *testing.T) {
		t.Parallel()
		host := &auth.HostState{}

		// Before lock, non-FQDN request: ExternalURL returns "".
		ipReq := dummyReq(t, "127.0.0.1:8080", "", "")
		gh := oauthclient.NewGitHubConfig("id", "sec", func(r *http.Request) string {
			u := host.ExternalURL(r)
			if u == "" {
				return ""
			}
			return u + "/api/caic/v1/auth/github/callback"
		})
		if got := gh.RedirectURI(ipReq); got != "" {
			t.Errorf("RedirectURI before lock = %q, want empty", got)
		}

		// Lock via a request through ExternalURL.
		fqdnReq := dummyReq(t, "caic.example.com", "", "https")
		_ = host.ExternalURL(fqdnReq)

		// After lock, RedirectURI resolves from the locked external URL.
		if want := "https://caic.example.com/api/caic/v1/auth/github/callback"; gh.RedirectURI(fqdnReq) != want {
			t.Errorf("GitHub RedirectURI = %q, want %q", gh.RedirectURI(fqdnReq), want)
		}
		gl := oauthclient.NewGitLabConfig("id", "sec", "", func(r *http.Request) string {
			u := host.ExternalURL(r)
			if u == "" {
				return ""
			}
			return u + "/api/caic/v1/auth/gitlab/callback"
		})
		if want := "https://caic.example.com/api/caic/v1/auth/gitlab/callback"; gl.RedirectURI(fqdnReq) != want {
			t.Errorf("GitLab RedirectURI = %q, want %q", gl.RedirectURI(fqdnReq), want)
		}
	})

	t.Run("static host state", func(t *testing.T) {
		t.Parallel()
		host := auth.NewHostState("https://caic.example.com:8443")
		r := dummyReq(t, "caic.example.com:8443", "", "https")
		if got := host.ExternalURL(r); got != "https://caic.example.com:8443" {
			t.Errorf("ExternalURL = %q, want %q", got, "https://caic.example.com:8443")
		}
		c := oauthclient.NewGitHubConfig("id", "sec", func(r *http.Request) string {
			u := host.ExternalURL(r)
			if u == "" {
				return ""
			}
			return u + "/api/caic/v1/auth/github/callback"
		})
		if want := "https://caic.example.com:8443/api/caic/v1/auth/github/callback"; c.RedirectURI(r) != want {
			t.Errorf("RedirectURI = %q, want %q", c.RedirectURI(r), want)
		}
	})

	t.Run("middleware allows IP", func(t *testing.T) {
		t.Parallel()
		state := &auth.HostState{}
		called := false
		h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		r := dummyReq(t, "192.168.1.1:8080", "", "")
		h.ServeHTTP(httptest.NewRecorder(), r)
		if !called {
			t.Error("IP request should pass through")
		}
		if got := state.ExternalURL(r); got != "" {
			t.Errorf("ExternalURL should be empty for IP, got %q", got)
		}
	})

	t.Run("middleware rejects different FQDN", func(t *testing.T) {
		t.Parallel()
		state := &auth.HostState{}
		h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

		// Lock with first request.
		r := dummyReq(t, "caic.example.com", "", "")
		h.ServeHTTP(httptest.NewRecorder(), r)

		// Different FQDN is rejected.
		r = dummyReq(t, "evil.example.com", "", "")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("different FQDN: status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("middleware rejects different port", func(t *testing.T) {
		t.Parallel()
		state := &auth.HostState{}
		h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

		// Lock on port 8080.
		r := dummyReq(t, "caic.example.com:8080", "", "")
		h.ServeHTTP(httptest.NewRecorder(), r)

		// Same host, different port is rejected.
		r = dummyReq(t, "caic.example.com:9090", "", "")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("different port: status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})
}
