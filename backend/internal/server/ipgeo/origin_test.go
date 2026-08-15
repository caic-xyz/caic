// Tests for OpenAI published IP range parsing and registration.

package ipgeo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchOpenAICIDRsFrom(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"creationTime": "2026-06-03T17:15:21.316989",
				"prefixes": []map[string]string{
					{"ipv4Prefix": "20.0.53.96/28"},
					{"ipv6Prefix": "2001:db8::/48"},
				},
			})
		}))
		t.Cleanup(srv.Close)

		prefixes, err := fetchOpenAICIDRsFrom(t.Context(), srv.URL)
		if err != nil {
			t.Fatalf("fetchOpenAICIDRsFrom: %v", err)
		}
		if len(prefixes) != 2 {
			t.Fatalf("len(prefixes) = %d, want 2", len(prefixes))
		}
		if got := prefixes[0].String(); got != "20.0.53.96/28" {
			t.Errorf("prefixes[0] = %q, want %q", got, "20.0.53.96/28")
		}
		if got := prefixes[1].String(); got != "2001:db8::/48" {
			t.Errorf("prefixes[1] = %q, want %q", got, "2001:db8::/48")
		}
	})
	t.Run("invalid", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"prefixes": []map[string]string{{"ipv4Prefix": "not-a-cidr"}},
			})
		}))
		t.Cleanup(srv.Close)

		if _, err := fetchOpenAICIDRsFrom(t.Context(), srv.URL); err == nil {
			t.Error("expected error for invalid CIDR")
		}
	})
}

func TestNewCheckerOpenAI(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeOriginCache(t, dir, "openai", time.Now(), []string{"20.0.53.96/28", "104.211.73.128/28"})

	c, err := NewChecker(t.Context(), testLogger(), "local,tailscale,openai", "", dir)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	for _, ip := range []string{"20.0.53.100", "104.211.73.130"} {
		if got := originOf(c, ip); got != "openai" {
			t.Errorf("CheckOrigin(%q) origin = %q, want %q", ip, got, "openai")
		}
	}
	if got := originOf(c, "8.8.8.8"); got != "" {
		t.Errorf("CheckOrigin(unregistered) origin = %q, want %q", got, "")
	}
}
