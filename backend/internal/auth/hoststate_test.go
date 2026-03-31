package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/auth"
)

func TestHostState(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		xfh  string // X-Forwarded-Host
		xfp  string // X-Forwarded-Proto
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
			state := &auth.HostState{}
			h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			r.Host = tc.host
			if tc.xfh != "" {
				r.Header.Set("X-Forwarded-Host", tc.xfh)
			}
			if tc.xfp != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xfp)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got := state.ExternalURL(); got != tc.want {
				t.Errorf("ExternalURL = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("RedirectURI resolves after lock", func(t *testing.T) {
		host := &auth.HostState{}
		ghOAuth := auth.GitHubConfig("id", "sec", host)
		glOAuth := auth.GitLabConfig("id", "sec", "", host)

		// Before lock, RedirectURI returns "".
		if got := ghOAuth.RedirectURI(); got != "" {
			t.Fatalf("GitHub RedirectURI before lock = %q, want empty", got)
		}
		if got := glOAuth.RedirectURI(); got != "" {
			t.Fatalf("GitLab RedirectURI before lock = %q, want empty", got)
		}

		// Lock via a request through the middleware.
		h := host.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.Host = "caic.example.com"
		r.Header.Set("X-Forwarded-Proto", "https")
		h.ServeHTTP(httptest.NewRecorder(), r)

		// After lock, RedirectURI returns the full URI.
		if want := "https://caic.example.com/api/v1/auth/github/callback"; ghOAuth.RedirectURI() != want {
			t.Errorf("GitHub RedirectURI = %q, want %q", ghOAuth.RedirectURI(), want)
		}
		if want := "https://caic.example.com/api/v1/auth/gitlab/callback"; glOAuth.RedirectURI() != want {
			t.Errorf("GitLab RedirectURI = %q, want %q", glOAuth.RedirectURI(), want)
		}
	})

	t.Run("static host state", func(t *testing.T) {
		host := auth.NewHostState("https://caic.example.com:8443")
		if got := host.ExternalURL(); got != "https://caic.example.com:8443" {
			t.Errorf("ExternalURL = %q, want %q", got, "https://caic.example.com:8443")
		}
		c := auth.GitHubConfig("id", "sec", host)
		if want := "https://caic.example.com:8443/api/v1/auth/github/callback"; c.RedirectURI() != want {
			t.Errorf("RedirectURI = %q, want %q", c.RedirectURI(), want)
		}
	})

	t.Run("middleware allows IP", func(t *testing.T) {
		state := &auth.HostState{}
		called := false
		h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.Host = "192.168.1.1:8080"
		h.ServeHTTP(httptest.NewRecorder(), r)
		if !called {
			t.Error("IP request should pass through")
		}
		if got := state.ExternalURL(); got != "" {
			t.Errorf("ExternalURL should be empty for IP, got %q", got)
		}
	})

	t.Run("middleware rejects different FQDN", func(t *testing.T) {
		state := &auth.HostState{}
		h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

		// Lock with first request.
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.Host = "caic.example.com"
		h.ServeHTTP(httptest.NewRecorder(), r)

		// Different FQDN is rejected.
		r = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.Host = "evil.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("different FQDN: status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("middleware rejects different port", func(t *testing.T) {
		state := &auth.HostState{}
		h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

		// Lock on port 8080.
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.Host = "caic.example.com:8080"
		h.ServeHTTP(httptest.NewRecorder(), r)

		// Same host, different port is rejected.
		r = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.Host = "caic.example.com:9090"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("different port: status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})
}
