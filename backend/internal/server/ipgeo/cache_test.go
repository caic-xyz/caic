// Tests for named IP origin cache behavior.

package ipgeo

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testOriginSource struct {
	name      string
	prefixes  []netip.Prefix
	err       error
	cacheable bool
	calls     int
}

func (s *testOriginSource) Name() string { return s.name }

func (s *testOriginSource) Cacheable() bool { return s.cacheable }

func (s *testOriginSource) Prefixes(context.Context) ([]netip.Prefix, error) {
	s.calls++
	return s.prefixes, s.err
}

func TestOriginCache(t *testing.T) {
	t.Parallel()
	t.Run("valid_fresh_cache_skips_fetch", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOriginCache(t, dir, "github", time.Now(), []string{"203.0.113.0/24"})

		c, err := NewChecker(t.Context(), "github", "", dir)
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		if got := originOf(c, "203.0.113.5"); got != "github" {
			t.Fatalf("CheckOrigin(cached prefix) origin = %q, want github", got)
		}
	})
	t.Run("valid_stale_cache_refreshes", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOriginCache(t, dir, "svc", time.Now().Add(-25*time.Hour), []string{"198.51.100.0/24"})
		source := &testOriginSource{name: "svc", prefixes: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, cacheable: true}
		prefixes := resolveTestOriginPrefixes(t, dir, source)
		c := &Checker{resolvers: []originResolver{namedPrefix{name: "svc", prefix: prefixes[0]}}}

		if source.calls != 1 {
			t.Fatalf("calls = %d, want 1", source.calls)
		}
		if got := originOf(c, "203.0.113.5"); got != "svc" {
			t.Fatalf("CheckOrigin(new prefix) origin = %q, want svc", got)
		}
		if got := originOf(c, "198.51.100.5"); got != "" {
			t.Fatalf("CheckOrigin(old prefix) origin = %q, want empty", got)
		}
	})
	t.Run("valid_stale_cache_used_on_refresh_failure", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOriginCache(t, dir, "svc", time.Now().Add(-25*time.Hour), []string{"203.0.113.0/24"})
		source := &testOriginSource{name: "svc", err: errors.New("offline"), cacheable: true}
		prefixes := resolveTestOriginPrefixes(t, dir, source)
		c := &Checker{resolvers: []originResolver{namedPrefix{name: "svc", prefix: prefixes[0]}}}

		if source.calls != 1 {
			t.Fatalf("calls = %d, want 1", source.calls)
		}
		if got := originOf(c, "203.0.113.5"); got != "svc" {
			t.Fatalf("CheckOrigin(stale prefix) origin = %q, want svc", got)
		}
	})
	t.Run("valid_static_source_bypasses_cache", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeOriginCache(t, dir, "svc", time.Now(), []string{"198.51.100.0/24"})
		source := &testOriginSource{name: "svc", prefixes: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}}
		prefixes := resolveTestOriginPrefixes(t, dir, source)
		c := &Checker{resolvers: []originResolver{namedPrefix{name: "svc", prefix: prefixes[0]}}}

		if source.calls != 1 {
			t.Fatalf("calls = %d, want 1", source.calls)
		}
		if got := originOf(c, "203.0.113.5"); got != "svc" {
			t.Fatalf("CheckOrigin(new prefix) origin = %q, want svc", got)
		}
		if got := originOf(c, "198.51.100.5"); got != "" {
			t.Fatalf("CheckOrigin(cached prefix) origin = %q, want empty", got)
		}
	})
}

func resolveTestOriginPrefixes(t *testing.T, dir string, source OriginSource) []netip.Prefix {
	cache, err := openOriginCache(filepath.Join(dir, "ip-origins.json"))
	if err != nil {
		t.Fatalf("openOriginCache: %v", err)
	}
	prefixes := resolveOriginPrefixes(t.Context(), cache, source)
	if len(prefixes) == 0 {
		t.Fatal("resolveOriginPrefixes returned no prefixes")
	}
	return prefixes
}

func writeOriginCache(t *testing.T, dir, name string, updated time.Time, prefixes []string) {
	path := filepath.Join(dir, "ip-origins.json")
	data, err := json.Marshal(map[string]originCacheEntry{name: {Updated: updated, Prefixes: prefixes}})
	if err != nil {
		t.Fatalf("marshal origin cache: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write origin cache: %v", err)
	}
}
