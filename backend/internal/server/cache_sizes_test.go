// Tests for well-known cache size snapshots.

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestRefreshCacheSizes(t *testing.T) {
	t.Parallel()

	t.Run("valid_calculates_well_known_cache_sizes", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		writeSizedFile(t, filepath.Join(home, ".cache", "tool", "a.bin"), 7)
		writeSizedFile(t, filepath.Join(home, ".cache", "tool", "nested", "b.bin"), 11)
		writeSizedFile(t, filepath.Join(home, ".other", "c.bin"), 5)
		caches := map[string][]md.CacheMount{
			"tool": {
				{Name: "tool", HostPath: "~/.cache/tool", ContainerPath: "/home/user/.cache/tool"},
				{Name: "tool-dup", HostPath: "~/.cache/tool", ContainerPath: "/home/user/.cache/tool"},
			},
			"combo": {
				{Name: "tool", HostPath: "~/.cache/tool", ContainerPath: "/home/user/.cache/tool"},
				{Name: "other", HostPath: "~/.other", ContainerPath: "/home/user/.other"},
			},
			"missing": {
				{Name: "missing", HostPath: "~/.missing", ContainerPath: "/home/user/.missing"},
			},
		}

		sizes := calculateWellKnownCacheSizes(t.Context(), home, caches)

		if sizes["tool"].SizeBytes != 18 {
			t.Fatalf("tool size = %d, want 18", sizes["tool"].SizeBytes)
		}
		if sizes["combo"].SizeBytes != 23 {
			t.Fatalf("combo size = %d, want 23", sizes["combo"].SizeBytes)
		}
		if sizes["missing"].SizeBytes != 0 || sizes["missing"].Error != "" {
			t.Fatalf("missing = %+v, want zero without error", sizes["missing"])
		}
		if sizes["tool"].CalculatedAt.IsZero() {
			t.Fatal("CalculatedAt is zero")
		}
	})

	t.Run("valid_reports_unresolved_home", func(t *testing.T) {
		t.Parallel()
		sizes := calculateWellKnownCacheSizes(t.Context(), "", map[string][]md.CacheMount{
			"tool": {{Name: "tool", HostPath: "~/.cache/tool", ContainerPath: "/home/user/.cache/tool"}},
		})

		if sizes["tool"].Error == "" {
			t.Fatal("Error is empty for unresolved home")
		}
	})

	t.Run("valid_handler_returns_snapshot", func(t *testing.T) {
		t.Parallel()
		store := &CacheSizeStore{
			sizes: map[string]v1.CacheSize{
				"npm": {Name: "npm", SizeBytes: 12},
			},
		}
		h := &serverHandlers{cacheSizes: store}

		resp, err := h.getCacheSizes(t.Context(), &api.EmptyReq{})
		if err != nil {
			t.Fatalf("getCacheSizes: %v", err)
		}
		if len(resp.WellKnown) != 1 || resp.WellKnown[0].Name != "npm" || resp.WellKnown[0].SizeBytes != 12 {
			t.Fatalf("resp = %+v, want npm size 12", resp)
		}

		w := httptest.NewRecorder()
		handle(h.getCacheSizes)(w, httptest.NewRequestWithContext(t.Context(), "GET", "/api/caic/v1/server/cache-sizes", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
	})
}

func writeSizedFile(t *testing.T, path string, size int) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
