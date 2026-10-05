// Tests for named IP origin cache behavior.

package ipgeo

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/server/ipgeo/data"
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

		c, err := NewChecker(t.Context(), testLogger(), "github", "", dir)
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
	prefixes := resolveOriginPrefixes(t.Context(), testLogger(), cache, source)
	if len(prefixes) == 0 {
		t.Fatal("resolveOriginPrefixes returned no prefixes")
	}
	return prefixes
}

func writeOriginCache(t *testing.T, dir, name string, updated time.Time, prefixes []string) {
	path := filepath.Join(dir, "ip-origins.json")
	raw, err := json.Marshal(map[string]data.Entry{name: {Updated: updated, Prefixes: prefixes}})
	if err != nil {
		t.Fatalf("marshal origin cache: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write origin cache: %v", err)
	}
}

func TestOriginCacheHistorical(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ip-origins.json")
	fixture := `{"github":{"updated":"2020-01-01T00:00:00.123456789Z","prefixes":["203.0.113.5/24"]},"empty":{"updated":"2020-01-01T00:00:00Z","prefixes":[]},"nil":{"updated":"2020-01-01T00:00:00Z","prefixes":null}}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := openOriginCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.fresh("github"); ok {
		t.Fatal("historical entry unexpectedly fresh")
	}
	prefixes, ok := c.stale("github")
	if !ok || len(prefixes) != 1 || prefixes[0].String() != "203.0.113.0/24" {
		t.Fatalf("stale prefixes: %v, hit=%t", prefixes, ok)
	}
	if err := c.set("github", prefixes); err != nil {
		t.Fatal(err)
	}
	reopened, err := openOriginCache(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.fresh("github")
	if !ok || !reflect.DeepEqual(got, prefixes) {
		t.Fatalf("fresh reload: %v, hit=%t", got, ok)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // Test-owned path inside t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"prefixes": []`) || !strings.Contains(string(raw), `"prefixes": null`) {
		t.Fatalf("disk shape: %s", raw)
	}
}

func TestOriginCacheRecovery(t *testing.T) {
	t.Parallel()
	for name, fixture := range map[string]string{
		"partial decode": `{"github":{"updated":"2099-01-01T00:00:00Z","prefixes":["198.51.100.0/24"]},"bad":{"prefixes":42}}`,
		"null":           `null`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "ip-origins.json")
			if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
				t.Fatal(err)
			}
			// Use the production loader: malformed caches must remain writable.
			c := loadOriginCache(t.Context(), testLogger(), dir)
			if c == nil {
				t.Fatal("malformed file disabled cache")
			}
			if _, ok := c.stale("github"); ok {
				t.Fatal("retained partially decoded prefixes")
			}
			source := &testOriginSource{name: "github", prefixes: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, cacheable: true}
			prefixes := resolveOriginPrefixes(t.Context(), testLogger(), c, source)
			if source.calls != 1 {
				t.Fatal("did not refetch discarded cache")
			}
			reopened, err := openOriginCache(path)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := reopened.fresh("github")
			if !ok || !reflect.DeepEqual(got, prefixes) {
				t.Fatalf("recovery: %v, hit=%t", got, ok)
			}
		})
	}
	t.Run("read error", func(t *testing.T) {
		t.Parallel()
		if _, err := openOriginCache(t.TempDir()); err == nil {
			t.Fatal("directory read did not return error")
		}
	})
}
