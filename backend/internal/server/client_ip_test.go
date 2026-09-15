// Tests for HTTP client-IP extraction behind trusted reverse proxies.

package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	t.Parallel()
	trusted := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8:1::/48")}
	for _, tc := range []struct {
		name       string
		remoteAddr string
		xff        []string
		xri        []string
		prefixes   []netip.Prefix
		want       string
	}{
		{name: "IPv4 direct peer", remoteAddr: "198.51.100.1:443", want: "198.51.100.1"},
		{name: "IPv4 bare direct peer", remoteAddr: "198.51.100.1", want: "198.51.100.1"},
		{name: "IPv6 direct peer", remoteAddr: "[2001:db8:2::1]:443", want: "2001:db8:2::1"},
		{name: "IPv6 bare direct peer", remoteAddr: "2001:db8:2::1", want: "2001:db8:2::1"},
		{name: "untrusted peer ignores X-Forwarded-For", remoteAddr: "198.51.100.1:443", xff: []string{"127.0.0.1"}, prefixes: trusted, want: "198.51.100.1"},
		{name: "untrusted peer ignores X-Real-IP", remoteAddr: "198.51.100.1:443", xri: []string{"127.0.0.1"}, prefixes: trusted, want: "198.51.100.1"},
		{name: "trusted peer accepts X-Forwarded-For", remoteAddr: "192.0.2.10:443", xff: []string{"198.51.100.1"}, prefixes: trusted, want: "198.51.100.1"},
		{name: "trusted peer accepts X-Real-IP", remoteAddr: "192.0.2.10:443", xri: []string{"198.51.100.1"}, prefixes: trusted, want: "198.51.100.1"},
		{name: "X-Forwarded-For uses rightmost untrusted hop", remoteAddr: "192.0.2.10:443", xff: []string{"203.0.113.1, 198.51.100.1, 192.0.2.20"}, prefixes: trusted, want: "198.51.100.1"},
		{name: "X-Forwarded-For permits trusted IPv6 hops", remoteAddr: "[2001:db8:1::10]:443", xff: []string{"203.0.113.1, 2001:db8:1::20"}, prefixes: trusted, want: "203.0.113.1"},
		{name: "malformed X-Forwarded-For uses direct peer", remoteAddr: "192.0.2.10:443", xff: []string{"198.51.100.1, not-an-ip"}, prefixes: trusted, want: "192.0.2.10"},
		{name: "empty X-Forwarded-For does not fall back to X-Real-IP", remoteAddr: "192.0.2.10:443", xff: []string{""}, xri: []string{"127.0.0.1"}, prefixes: trusted, want: "192.0.2.10"},
		{name: "malformed X-Real-IP uses direct peer", remoteAddr: "192.0.2.10:443", xri: []string{"not-an-ip"}, prefixes: trusted, want: "192.0.2.10"},
		{name: "repeated X-Real-IP uses direct peer", remoteAddr: "192.0.2.10:443", xri: []string{"198.51.100.1", "127.0.0.1"}, prefixes: trusted, want: "192.0.2.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
			r.RemoteAddr = tc.remoteAddr
			for _, value := range tc.xff {
				r.Header.Add("X-Forwarded-For", value)
			}
			for _, value := range tc.xri {
				r.Header.Add("X-Real-IP", value)
			}
			if got := clientIP(r, tc.prefixes); got != tc.want {
				t.Errorf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}
