// Tests for host authentication state management.

package auth_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
		name    string
		host    string
		xfh     string
		xfp     string
		trusted bool
		want    string
	}{
		{"non-default HTTP port", "caic.example.com:8080", "", "", false, "http://caic.example.com:8080"},
		{"default HTTP port omitted", "caic.example.com:80", "", "", false, "http://caic.example.com"},
		{"default HTTPS port omitted", "caic.example.com:443", "", "", false, "http://caic.example.com:443"},
		{"non-default HTTPS port", "quick.giraffe-cobra.ts.net:8443", "", "", false, "http://quick.giraffe-cobra.ts.net:8443"},
		{"HTTP without port", "caic.example.com", "", "", false, "http://caic.example.com"},
		{"untrusted X-Forwarded headers ignored", "caic.example.com", "evil.example.com", "https", false, "http://caic.example.com"},
		{"trusted X-Forwarded-Host overrides r.Host", "127.0.0.1:8080", "caic.example.com", "https", true, "https://caic.example.com"},
		{"trusted X-Forwarded-Host with port", "127.0.0.1:8080", "caic.example.com:8443", "https", true, "https://caic.example.com:8443"},
	} {
		t.Run("lock "+tc.name, func(t *testing.T) {
			t.Parallel()
			state := &auth.HostState{}
			if tc.trusted {
				state = auth.NewHostState("", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
			}
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
		fqdnReq.TLS = &tls.ConnectionState{}
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
		host := auth.NewHostState("https://caic.example.com:8443", nil)
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

	t.Run("trusted standardized Forwarded uses nearest proxy value", func(t *testing.T) {
		t.Parallel()
		state := auth.NewHostState("", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
		r := dummyReq(t, "127.0.0.1:2242", "", "")
		r.Header.Set("Forwarded", `for=198.51.100.1;host=poison.example;proto=http, for=203.0.113.2;host=caic.example.com;proto=https`)
		if got := state.ExternalURL(r); got != "https://caic.example.com" {
			t.Fatalf("ExternalURL = %q, want trusted nearest Forwarded origin", got)
		}
	})

	t.Run("trusted repeated Forwarded fields use final proxy field", func(t *testing.T) {
		t.Parallel()
		state := auth.NewHostState("", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
		r := dummyReq(t, "127.0.0.1:2242", "", "")
		r.Header.Add("Forwarded", "host=poison.example;proto=http")
		r.Header.Add("Forwarded", "for=203.0.113.2;host=caic.example.com;proto=https")
		if got := state.ExternalURL(r); got != "https://caic.example.com" {
			t.Fatalf("ExternalURL = %q, want final repeated Forwarded origin", got)
		}
	})

	t.Run("trusted repeated X-Forwarded fields use final proxy values", func(t *testing.T) {
		t.Parallel()
		state := auth.NewHostState("", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
		r := dummyReq(t, "127.0.0.1:2242", "", "")
		r.Header.Add("X-Forwarded-Host", "poison.example")
		r.Header.Add("X-Forwarded-Host", "old.example, caic.example.com")
		r.Header.Add("X-Forwarded-Proto", "http")
		r.Header.Add("X-Forwarded-Proto", "http, https")
		if got := state.ExternalURL(r); got != "https://caic.example.com" {
			t.Fatalf("ExternalURL = %q, want final repeated X-Forwarded origin", got)
		}
	})

	t.Run("trusted inconsistent forwarding header sets are rejected", func(t *testing.T) {
		t.Parallel()
		state := auth.NewHostState("", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
		h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		r := dummyReq(t, "127.0.0.1:2242", "poison.example", "https")
		r.Header.Set("Forwarded", "host=caic.example.com;proto=https")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("trusted malformed final forwarding value is rejected", func(t *testing.T) {
		t.Parallel()
		state := auth.NewHostState("", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
		h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		r := dummyReq(t, "127.0.0.1:2242", "", "")
		r.Header.Add("Forwarded", "host=caic.example.com;proto=https")
		r.Header.Add("Forwarded", `host="unterminated;proto=https`)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("trusted duplicate empty Forwarded parameters are rejected", func(t *testing.T) {
		t.Parallel()
		for _, forwarded := range []string{
			"host=;host=caic.example.com;proto=https",
			"host=caic.example.com;proto=;proto=https",
		} {
			state := auth.NewHostState("", []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")})
			h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			r := dummyReq(t, "127.0.0.1:2242", "", "")
			r.Header.Set("Forwarded", forwarded)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("Forwarded %q status = %d, want %d", forwarded, w.Code, http.StatusForbidden)
			}
		}
	})

	t.Run("malformed and bare proxy peers are untrusted", func(t *testing.T) {
		t.Parallel()
		trusted := []netip.Prefix{
			netip.MustParsePrefix("192.0.2.0/24"),
			netip.MustParsePrefix("2001:db8::/32"),
		}
		for _, remoteAddr := range []string{
			"192.0.2.10",
			"192.0.2.10:notaport",
			"2001:db8::10",
			"[2001:db8::10]",
			"[2001:db8::10]:notaport",
		} {
			state := auth.NewHostState("", trusted)
			r := dummyReq(t, "direct.example.com", "poison.example.com", "https")
			r.RemoteAddr = remoteAddr
			if got := state.ExternalURL(r); got != "http://direct.example.com" {
				t.Errorf("RemoteAddr %q ExternalURL = %q, want direct origin", remoteAddr, got)
			}
		}
	})

	t.Run("untrusted Forwarded cannot satisfy static host check", func(t *testing.T) {
		t.Parallel()
		state := auth.NewHostState("https://caic.example.com", nil)
		h := state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		r := dummyReq(t, "evil.example.com", "caic.example.com", "https")
		r.Header.Set("Forwarded", "host=caic.example.com;proto=https")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
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

type benchmarkResponseWriter struct {
	header http.Header
}

func (w *benchmarkResponseWriter) Header() http.Header         { return w.header }
func (w *benchmarkResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *benchmarkResponseWriter) WriteHeader(int)             {}

func BenchmarkHostStateMiddleware(b *testing.B) {
	trustedPrefix := netip.MustParsePrefix("192.0.2.0/24")
	for _, tc := range []struct {
		name  string
		state *auth.HostState
		req   *http.Request
	}{
		{
			name:  "direct",
			state: auth.NewHostState("https://caic.example.com", nil),
			req:   httptest.NewRequestWithContext(b.Context(), http.MethodGet, "https://caic.example.com/", http.NoBody),
		},
		{
			name:  "trusted forwarded",
			state: auth.NewHostState("https://caic.example.com", []netip.Prefix{trustedPrefix}),
			req:   httptest.NewRequestWithContext(b.Context(), http.MethodGet, "http://127.0.0.1:2242/", http.NoBody),
		},
	} {
		b.Run(tc.name, func(b *testing.B) {
			tc.req.RemoteAddr = "192.0.2.10:1234"
			if tc.name == "trusted forwarded" {
				tc.req.Header.Set("Forwarded", "for=198.51.100.2;host=caic.example.com;proto=https")
			}
			h := tc.state.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			w := &benchmarkResponseWriter{header: http.Header{}}
			b.ReportAllocs()
			for b.Loop() {
				h.ServeHTTP(w, tc.req)
			}
		})
	}
}
