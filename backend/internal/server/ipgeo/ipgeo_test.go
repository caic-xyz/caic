package ipgeo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestGetClientIP(t *testing.T) {
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
			r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
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

func TestCountryCode(t *testing.T) {
	t.Run("special addresses", func(t *testing.T) {
		// A nil-reader Checker handles all special cases; public IPs return "".
		c := &Checker{}
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
				if got := c.CountryCode(tt.ip); got != tt.want {
					t.Errorf("CountryCode(%q) = %q, want %q", tt.ip, got, tt.want)
				}
			})
		}
	})
	t.Run("named CIDR groups", func(t *testing.T) {
		c := &Checker{namedCIDRs: []namedPrefix{
			{name: "github", prefix: netip.MustParsePrefix("192.30.252.0/22")},
			{name: "github", prefix: netip.MustParsePrefix("185.199.108.0/22")},
		}}
		for _, ip := range []string{"192.30.252.1", "192.30.255.255", "185.199.108.0", "185.199.111.255"} {
			if got := c.CountryCode(ip); got != "github" {
				t.Errorf("CountryCode(%q) = %q, want %q", ip, got, "github")
			}
		}
		// Local/tailscale take priority over named CIDRs (they wouldn't overlap in practice).
		if got := c.CountryCode("127.0.0.1"); got != "local" {
			t.Errorf("CountryCode(loopback) = %q, want %q", got, "local")
		}
		// Outside registered ranges returns "".
		if got := c.CountryCode("8.8.8.8"); got != "" {
			t.Errorf("CountryCode(unregistered public) = %q, want %q", got, "")
		}
	})
	t.Run("multiple named groups", func(t *testing.T) {
		c := &Checker{namedCIDRs: []namedPrefix{
			{name: "github", prefix: netip.MustParsePrefix("192.30.252.0/22")},
			{name: "myservice", prefix: netip.MustParsePrefix("203.0.113.0/24")},
		}}
		if got := c.CountryCode("192.30.252.1"); got != "github" {
			t.Errorf("CountryCode(github ip) = %q, want %q", got, "github")
		}
		if got := c.CountryCode("203.0.113.5"); got != "myservice" {
			t.Errorf("CountryCode(myservice ip) = %q, want %q", got, "myservice")
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
	t.Run("error on empty", func(t *testing.T) {
		if _, err := parseAllowlist(""); err == nil {
			t.Error("expected error for empty string")
		}
	})
	t.Run("error on whitespace only", func(t *testing.T) {
		if _, err := parseAllowlist("  ,  "); err == nil {
			t.Error("expected error for whitespace-only")
		}
	})
	t.Run("allows listed country codes", func(t *testing.T) {
		a := mustParseAllowlist(t, "CA,US,tailscale")
		for _, cc := range []string{"CA", "US", "TAILSCALE", "tailscale", "ca"} {
			if !a.allowed(cc) {
				t.Errorf("allowed(%q) = false, want true", cc)
			}
		}
	})
	t.Run("blocks unlisted", func(t *testing.T) {
		a := mustParseAllowlist(t, "CA")
		for _, cc := range []string{"US", "GB", "local", "tailscale", ""} {
			if a.allowed(cc) {
				t.Errorf("allowed(%q) = true, want false", cc)
			}
		}
	})
	t.Run("0.0.0.0/0 and ::/0 allows all IPs", func(t *testing.T) {
		a := mustParseAllowlist(t, "0.0.0.0/0,::/0")
		for _, ip := range []string{"1.2.3.4", "8.8.8.8", "192.168.1.1", "::1", "2001:db8::1"} {
			if !a.containsIP(ip) {
				t.Errorf("containsIP(%q) = false, want true", ip)
			}
		}
	})
	t.Run("CIDR entries matched by containsIP", func(t *testing.T) {
		a := mustParseAllowlist(t, "CA,34.74.90.64/28,34.74.226.0/24")
		if !a.allowed("CA") {
			t.Error("CA should be allowed")
		}
		if !a.containsIP("34.74.90.65") {
			t.Error("34.74.90.65 should be in 34.74.90.64/28")
		}
		if !a.containsIP("34.74.226.100") {
			t.Error("34.74.226.100 should be in 34.74.226.0/24")
		}
		if a.containsIP("8.8.8.8") {
			t.Error("8.8.8.8 should not be in any CIDR")
		}
	})
	t.Run("CIDR-only allowlist does not affect allowed", func(t *testing.T) {
		a := mustParseAllowlist(t, "34.74.90.64/28")
		if a.allowed("US") {
			t.Error("US should not be allowed in CIDR-only list")
		}
		if !a.containsIP("34.74.90.70") {
			t.Error("34.74.90.70 should be in CIDR")
		}
	})
	t.Run("invalid CIDR returns error", func(t *testing.T) {
		if _, err := parseAllowlist("CA,not-a-cidr/bad"); err == nil {
			t.Error("expected error for invalid CIDR")
		}
	})
}

func TestNeedsDB(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"local", false},
		{"tailscale", false},
		{"local,tailscale", false},
		{"github", false}, // named origin — no DB needed
		{"CA", true},
		{"local,CA", true},
		{"tailscale,US", true},
		{"github,CA", true},       // named origin + country code — DB needed
		{"34.74.90.64/28", false}, // CIDR only — no DB needed
		{"34.74.90.64/28,local", false},
		{"34.74.90.64/28,CA", true},
	}
	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			a := mustParseAllowlist(t, tt.s)
			if got := a.needsDB(); got != tt.want {
				t.Errorf("needsDB() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewChecker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hooks": []string{"192.30.252.0/22", "185.199.108.0/22"},
		})
	}))
	defer srv.Close()
	origURL := githubMetaURL
	githubMetaURL = srv.URL + "/meta"
	defer func() { githubMetaURL = origURL }()

	t.Run("github in allowlist fetches CIDRs", func(t *testing.T) {
		c, err := NewChecker(context.Background(), "local,tailscale,github", "")
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		if got := c.CountryCode("192.30.252.1"); got != "github" {
			t.Errorf("CountryCode(github ip) = %q, want %q", got, "github")
		}
		if got := c.CountryCode("8.8.8.8"); got != "" {
			t.Errorf("CountryCode(unregistered) = %q, want %q", got, "")
		}
	})
	t.Run("github not in allowlist skips fetch", func(t *testing.T) {
		c, err := NewChecker(context.Background(), "local,tailscale", "")
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		// No github CIDRs registered; IP returns "".
		if got := c.CountryCode("192.30.252.1"); got != "" {
			t.Errorf("CountryCode(github ip without registration) = %q, want %q", got, "")
		}
	})
	t.Run("fetch failure is non-fatal", func(t *testing.T) {
		githubMetaURL = "http://127.0.0.1:0/meta" // unreachable
		defer func() { githubMetaURL = srv.URL + "/meta" }()
		if _, err := NewChecker(context.Background(), "local,tailscale,github", ""); err != nil {
			t.Errorf("NewChecker should not fail on fetch error: %v", err)
		}
	})
	t.Run("country code without DB returns error", func(t *testing.T) {
		if _, err := NewChecker(context.Background(), "CA", ""); err == nil {
			t.Error("expected error when country code given without DB path")
		}
	})
}
