// Tests for IP geolocation lookup functionality.

package ipgeo

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

type countingResolver struct {
	name   string
	prefix netip.Prefix
	calls  int
}

func (r *countingResolver) Resolve(addr netip.Addr) string {
	r.calls++
	if r.prefix.Contains(addr) {
		return r.name
	}
	return ""
}

func originOf(c *Checker, ip string) string {
	origin, _ := c.CheckOrigin(ip)
	return origin
}

func TestGetClientIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		remoteAddr    string
		xForwardedFor string
		xRealIP       string
		want          string
	}{
		{name: "remote addr ipv4", remoteAddr: "1.2.3.4:5678", want: "1.2.3.4"},
		{name: "remote addr ipv6", remoteAddr: "[::1]:8080", want: "::1"},
		{name: "x-forwarded-for single", xForwardedFor: "1.2.3.4", remoteAddr: "10.0.0.1:80", want: "1.2.3.4"},
		{name: "x-forwarded-for chain", xForwardedFor: "1.2.3.4, 10.0.0.1", remoteAddr: "10.0.0.2:80", want: "1.2.3.4"},
		{name: "x-real-ip", xRealIP: "5.6.7.8", remoteAddr: "10.0.0.1:80", want: "5.6.7.8"},
		{name: "x-forwarded-for beats x-real-ip", xForwardedFor: "1.2.3.4", xRealIP: "5.6.7.8", remoteAddr: "10.0.0.1:80", want: "1.2.3.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			r.RemoteAddr = tt.remoteAddr
			if tt.xForwardedFor != "" {
				r.Header.Set("X-Forwarded-For", tt.xForwardedFor)
			}
			if tt.xRealIP != "" {
				r.Header.Set("X-Real-IP", tt.xRealIP)
			}
			if got := GetClientIP(r); got != tt.want {
				t.Errorf("GetClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckOrigin(t *testing.T) {
	t.Parallel()
	t.Run("special addresses", func(t *testing.T) {
		t.Parallel()
		c, err := NewChecker(t.Context(), "local,tailscale", "", "")
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		tests := []struct {
			ip   string
			want string
		}{
			{"127.0.0.1", "local"},
			{"::1", "local"},
			{"10.0.0.1", "local"},
			{"192.168.1.1", "local"},
			{"172.16.0.1", "local"},
			{"0.0.0.0", "local"},
			{"::", "local"},
			{"169.254.1.1", "local"},
			{"fe80::1", "local"},
			{"100.64.0.1", "tailscale"},
			{"100.100.100.100", "tailscale"},
			{"100.127.255.254", "tailscale"},
			{"100.63.255.255", ""}, // just outside Tailscale range
			{"100.128.0.0", ""},    // just outside Tailscale range
			{"8.8.8.8", ""},        // public IP, no MMDB
			{"not-an-ip", ""},
		}
		for _, tt := range tests {
			t.Run(tt.ip, func(t *testing.T) {
				t.Parallel()
				if got := originOf(c, tt.ip); got != tt.want {
					t.Errorf("CheckOrigin(%q) origin = %q, want %q", tt.ip, got, tt.want)
				}
			})
		}
	})
	t.Run("named CIDR groups", func(t *testing.T) {
		t.Parallel()
		c, err := NewChecker(t.Context(), "local,tailscale,anthropic", "", "")
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		c.resolvers = append(c.resolvers,
			namedPrefix{name: "github", prefix: netip.MustParsePrefix("192.30.252.0/22")},
			namedPrefix{name: "github", prefix: netip.MustParsePrefix("185.199.108.0/22")},
			namedPrefix{name: "openai", prefix: netip.MustParsePrefix("20.0.53.96/28")},
		)
		for _, tt := range []struct {
			ip   string
			want string
		}{
			{ip: "160.79.104.1", want: "anthropic"},
			{ip: "192.30.252.1", want: "github"},
			{ip: "192.30.255.255", want: "github"},
			{ip: "185.199.108.0", want: "github"},
			{ip: "185.199.111.255", want: "github"},
			{ip: "20.0.53.100", want: "openai"},
		} {
			if got := originOf(c, tt.ip); got != tt.want {
				t.Errorf("CheckOrigin(%q) origin = %q, want %q", tt.ip, got, tt.want)
			}
		}
		// Local/tailscale take priority over named CIDRs (they wouldn't overlap in practice).
		if got := originOf(c, "127.0.0.1"); got != "local" {
			t.Errorf("CheckOrigin(loopback) origin = %q, want %q", got, "local")
		}
		// Outside registered ranges returns "".
		if got := originOf(c, "8.8.8.8"); got != "" {
			t.Errorf("CheckOrigin(unregistered public) origin = %q, want %q", got, "")
		}
	})
	t.Run("multiple named groups", func(t *testing.T) {
		t.Parallel()
		c := &Checker{resolvers: []originResolver{
			namedPrefix{name: "github", prefix: netip.MustParsePrefix("192.30.252.0/22")},
			namedPrefix{name: "myservice", prefix: netip.MustParsePrefix("203.0.113.0/24")},
		}}
		if got := originOf(c, "192.30.252.1"); got != "github" {
			t.Errorf("CheckOrigin(github ip) origin = %q, want %q", got, "github")
		}
		if got := originOf(c, "203.0.113.5"); got != "myservice" {
			t.Errorf("CheckOrigin(myservice ip) origin = %q, want %q", got, "myservice")
		}
	})
	t.Run("returns allowed origin", func(t *testing.T) {
		t.Parallel()
		c, err := NewChecker(t.Context(), "local", "", "")
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		origin, allowed := c.CheckOrigin("127.0.0.1")
		if origin != "local" || !allowed {
			t.Fatalf("CheckOrigin(loopback) = %q, %t; want local, true", origin, allowed)
		}
	})
	t.Run("returns blocked origin", func(t *testing.T) {
		t.Parallel()
		c, err := NewChecker(t.Context(), "tailscale", "", "")
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		origin, allowed := c.CheckOrigin("127.0.0.1")
		if origin != "local" || allowed {
			t.Fatalf("CheckOrigin(loopback) = %q, %t; want local, false", origin, allowed)
		}
	})
	t.Run("resolves origin once", func(t *testing.T) {
		t.Parallel()
		resolver := &countingResolver{name: "svc", prefix: netip.MustParsePrefix("203.0.113.0/24")}
		c := &Checker{
			resolvers: []originResolver{resolver},
			allowlist: mustParseAllowlist(t, "svc"),
		}
		origin, allowed := c.CheckOrigin("203.0.113.5")
		if origin != "svc" || !allowed {
			t.Fatalf("CheckOrigin(service IP) = %q, %t; want svc, true", origin, allowed)
		}
		if resolver.calls != 1 {
			t.Fatalf("resolver calls = %d, want 1", resolver.calls)
		}
	})
	t.Run("nil allowlist allows all", func(t *testing.T) {
		t.Parallel()
		c := &Checker{}
		for _, ip := range []string{"8.8.8.8", "127.0.0.1", "not-an-ip"} {
			_, allowed := c.CheckOrigin(ip)
			if !allowed {
				t.Errorf("CheckOrigin(%q) allowed = false, want true", ip)
			}
		}
	})
}

func mustParseAllowlist(t *testing.T, s string) *allowlist {
	a, err := parseAllowlist(s)
	if err != nil {
		t.Fatalf("parseAllowlist(%q): %v", s, err)
	}
	return a
}

func TestParseAllowlist(t *testing.T) {
	t.Parallel()
	t.Run("error on empty", func(t *testing.T) {
		t.Parallel()
		if _, err := parseAllowlist(""); err == nil {
			t.Error("expected error for empty string")
		}
	})
	t.Run("error on whitespace only", func(t *testing.T) {
		t.Parallel()
		if _, err := parseAllowlist("  ,  "); err == nil {
			t.Error("expected error for whitespace-only")
		}
	})
	t.Run("allows listed country codes", func(t *testing.T) {
		t.Parallel()
		a := mustParseAllowlist(t, "CA,US,tailscale")
		for _, cc := range []string{"CA", "US", "TAILSCALE", "tailscale", "ca"} {
			if !a.allowed(cc) {
				t.Errorf("allowed(%q) = false, want true", cc)
			}
		}
	})
	t.Run("blocks unlisted", func(t *testing.T) {
		t.Parallel()
		a := mustParseAllowlist(t, "CA")
		for _, cc := range []string{"US", "GB", "local", "tailscale", ""} {
			if a.allowed(cc) {
				t.Errorf("allowed(%q) = true, want false", cc)
			}
		}
	})
	t.Run("0.0.0.0/0 and ::/0 allows all IPs", func(t *testing.T) {
		t.Parallel()
		a := mustParseAllowlist(t, "0.0.0.0/0,::/0")
		for _, ip := range []string{"1.2.3.4", "8.8.8.8", "192.168.1.1", "::1", "2001:db8::1"} {
			if !a.contains(netip.MustParseAddr(ip)) {
				t.Errorf("contains(%q) = false, want true", ip)
			}
		}
	})
	t.Run("CIDR entries matched by containsIP", func(t *testing.T) {
		t.Parallel()
		a := mustParseAllowlist(t, "CA,34.74.90.64/28,34.74.226.0/24")
		if !a.allowed("CA") {
			t.Error("CA should be allowed")
		}
		if !a.contains(netip.MustParseAddr("34.74.90.65")) {
			t.Error("34.74.90.65 should be in 34.74.90.64/28")
		}
		if !a.contains(netip.MustParseAddr("34.74.226.100")) {
			t.Error("34.74.226.100 should be in 34.74.226.0/24")
		}
		if a.contains(netip.MustParseAddr("8.8.8.8")) {
			t.Error("8.8.8.8 should not be in any CIDR")
		}
	})
	t.Run("CIDR-only allowlist does not affect allowed", func(t *testing.T) {
		t.Parallel()
		a := mustParseAllowlist(t, "34.74.90.64/28")
		if a.allowed("US") {
			t.Error("US should not be allowed in CIDR-only list")
		}
		if !a.contains(netip.MustParseAddr("34.74.90.70")) {
			t.Error("34.74.90.70 should be in CIDR")
		}
	})
	t.Run("invalid CIDR returns error", func(t *testing.T) {
		t.Parallel()
		if _, err := parseAllowlist("CA,not-a-cidr/bad"); err == nil {
			t.Error("expected error for invalid CIDR")
		}
	})
}

func TestNeedsDB(t *testing.T) {
	t.Parallel()
	tests := []struct {
		s    string
		want bool
	}{
		{"local", false},
		{"tailscale", false},
		{"local,tailscale", false},
		{"anthropic", false}, // named origin — no DB needed
		{"github", false},    // named origin — no DB needed
		{"openai", false},    // named origin — no DB needed
		{"CA", true},
		{"local,CA", true},
		{"tailscale,US", true},
		{"anthropic,CA", true},    // named origin + country code — DB needed
		{"github,CA", true},       // named origin + country code — DB needed
		{"openai,CA", true},       // named origin + country code — DB needed
		{"34.74.90.64/28", false}, // CIDR only — no DB needed
		{"34.74.90.64/28,local", false},
		{"34.74.90.64/28,CA", true},
	}
	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			t.Parallel()
			a := mustParseAllowlist(t, tt.s)
			if got := a.needsDB(); got != tt.want {
				t.Errorf("needsDB() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewChecker(t *testing.T) {
	t.Parallel()

	t.Run("anthropic in allowlist uses static CIDR", func(t *testing.T) {
		t.Parallel()
		c, err := NewChecker(t.Context(), "local,tailscale,anthropic", "", "")
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		if got := originOf(c, "160.79.104.1"); got != "anthropic" {
			t.Errorf("CheckOrigin(anthropic ip) origin = %q, want %q", got, "anthropic")
		}
	})
	t.Run("github in allowlist uses cached CIDRs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOriginCache(t, dir, "github", time.Now(), []string{"192.30.252.0/22", "185.199.108.0/22"})
		c, err := NewChecker(t.Context(), "local,tailscale,github", "", dir)
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		if got := originOf(c, "192.30.252.1"); got != "github" {
			t.Errorf("CheckOrigin(github ip) origin = %q, want %q", got, "github")
		}
		if got := originOf(c, "8.8.8.8"); got != "" {
			t.Errorf("CheckOrigin(unregistered) origin = %q, want %q", got, "")
		}
	})
	t.Run("github not in allowlist skips fetch", func(t *testing.T) {
		t.Parallel()
		c, err := NewChecker(t.Context(), "local,tailscale", "", "")
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		// No github CIDRs registered; IP returns "".
		if got := originOf(c, "192.30.252.1"); got != "" {
			t.Errorf("CheckOrigin(github ip without registration) origin = %q, want %q", got, "")
		}
	})
	t.Run("fetch failure is non-fatal", func(t *testing.T) {
		t.Parallel()
		source := &testOriginSource{name: "github", err: errors.New("offline"), cacheable: true}
		if prefixes := resolveOriginPrefixes(t.Context(), nil, source); len(prefixes) != 0 {
			t.Errorf("resolveOriginPrefixes returned %v, want none", prefixes)
		}
	})
	t.Run("country code without DB returns error", func(t *testing.T) {
		t.Parallel()
		if _, err := NewChecker(t.Context(), "CA", "", ""); err == nil {
			t.Error("expected error when country code given without DB path")
		}
	})
}
