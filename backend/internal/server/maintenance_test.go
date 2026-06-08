// Tests for backend maintenance tasks: image warmup and harness model refresh.

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

func TestRefreshHarnessModels(t *testing.T) {
	t.Parallel()

	t.Run("valid_fetches_stale_model_fetchers", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		harness := agent.Harness("fetch")
		env := map[string][]string{string(harness): {"FETCH_API_KEY=secret"}}
		runtimeBackend := &modelRefreshRuntime{}
		fetcher := &modelFetchBackend{harness: harness, models: []string{"z-model", "a-model"}}
		s := newModelRefreshTestServer(t.Context(), cacheDir, env, runtimeBackend, map[agent.Harness]agent.Backend{
			harness: fetcher,
			"plain": stubBackend{},
		})

		s.refreshHarnessModels()

		if runtimeBackend.launches != 1 || runtimeBackend.connects != 1 || runtimeBackend.purges != 1 {
			t.Fatalf("runtime calls = launch %d connect %d purge %d, want 1 each", runtimeBackend.launches, runtimeBackend.connects, runtimeBackend.purges)
		}
		if runtimeBackend.harness != harness {
			t.Fatalf("launched harness = %q, want %q", runtimeBackend.harness, harness)
		}
		if fetcher.instance != "refresh-fetch" {
			t.Fatalf("fetch instance = %q, want refresh-fetch", fetcher.instance)
		}
		if !slices.Equal(fetcher.env, env[string(harness)]) {
			t.Fatalf("fetch env = %v, want %v", fetcher.env, env[string(harness)])
		}
		if !slices.Equal(fetcher.setModels, []string{"z-model", "a-model"}) {
			t.Fatalf("SetModels = %v, want fetched models", fetcher.setModels)
		}
		models, fresh := agent.OpenHarnessCache(cacheDir+"/harnesses.json").Models(harness, agent.APIKeyHash(env[string(harness)]))
		if !fresh || !slices.Equal(models, []string{"z-model", "a-model"}) {
			t.Fatalf("cache models = %v fresh=%t, want fetched fresh models", models, fresh)
		}
	})

	t.Run("valid_skips_fresh_cache", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		harness := agent.Harness("fetch")
		cache := agent.OpenHarnessCache(cacheDir + "/harnesses.json")
		cache.SetModels(harness, []string{"cached-model"}, "")
		runtimeBackend := &modelRefreshRuntime{}
		fetcher := &modelFetchBackend{harness: harness, models: []string{"new-model"}}
		s := newModelRefreshTestServer(t.Context(), cacheDir, nil, runtimeBackend, map[agent.Harness]agent.Backend{
			harness: fetcher,
		})

		s.refreshHarnessModels()

		if runtimeBackend.launches != 0 {
			t.Fatalf("launches = %d, want 0 for fresh cache", runtimeBackend.launches)
		}
		if fetcher.instance != "" {
			t.Fatalf("fetch instance = %q, want no fetch", fetcher.instance)
		}
	})
}

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
		store := &cacheSizeStore{
			sizes: map[string]v1.CacheSize{
				"npm": {Name: "npm", SizeBytes: 12},
			},
		}
		h := &serverConfigHandlers{cacheSizes: store}

		resp, err := h.getCacheSizes(t.Context(), &api.EmptyReq{})
		if err != nil {
			t.Fatalf("getCacheSizes: %v", err)
		}
		if len(resp.WellKnown) != 1 || resp.WellKnown[0].Name != "npm" || resp.WellKnown[0].SizeBytes != 12 {
			t.Fatalf("resp = %+v, want npm size 12", resp)
		}

		w := httptest.NewRecorder()
		handle(h.getCacheSizes)(w, httptest.NewRequestWithContext(t.Context(), "GET", "/api/caic/v1/server/cache-sizes", http.NoBody))
		if w.Code != 200 {
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

func newModelRefreshTestServer(ctx context.Context, cacheDir string, env map[string][]string, runtimeBackend runtime.Backend, backends map[agent.Harness]agent.Backend) *Server {
	s := &Server{
		ctx:            ctx,
		cacheDir:       cacheDir,
		runtimeBackend: runtimeBackend,
		harnessEnv:     env,
	}
	s.taskMgr = tasks.New(tasks.Config{
		ServerCtx: ctx,
		Backend:   runtimeBackend,
		Backends:  backends,
	})
	return s
}

type modelFetchBackend struct {
	stubBackend

	harness   agent.Harness
	models    []string
	instance  string
	env       []string
	setModels []string
}

func (b *modelFetchBackend) Harness() agent.Harness { return b.harness }

func (b *modelFetchBackend) FetchModels(_ context.Context, instance string, env []string) ([]string, error) {
	b.instance = instance
	b.env = append([]string(nil), env...)
	return b.models, nil
}

func (b *modelFetchBackend) SetModels(models []string) {
	b.setModels = append([]string(nil), models...)
}

type modelRefreshRuntime struct {
	launches int
	connects int
	purges   int
	harness  agent.Harness
}

var _ runtime.Backend = (*modelRefreshRuntime)(nil)

func (r *modelRefreshRuntime) Launch(_ context.Context, _ []runtime.Repo, opts *runtime.StartOptions) (runtime.InstanceID, error) {
	r.launches++
	r.harness = opts.Harness
	return runtime.InstanceID("refresh-" + string(opts.Harness)), nil
}

func (r *modelRefreshRuntime) Connect(_ context.Context, _ runtime.InstanceID, _ *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	r.connects++
	return runtime.ConnectionInfo{}, nil
}

func (*modelRefreshRuntime) Diff(_ context.Context, _ runtime.InstanceID, _ int, _ ...string) (string, error) {
	return "", nil
}

func (*modelRefreshRuntime) Fetch(_ context.Context, _ runtime.InstanceID) error { return nil }

func (*modelRefreshRuntime) Stop(_ context.Context, _ runtime.InstanceID) error { return nil }

func (r *modelRefreshRuntime) Purge(_ context.Context, _ runtime.InstanceID) error {
	r.purges++
	return nil
}

func (*modelRefreshRuntime) Revive(_ context.Context, _ runtime.InstanceID) error { return nil }

func (*modelRefreshRuntime) Fork(_ context.Context, _ runtime.InstanceID, _ []runtime.Repo, _ *runtime.ForkOptions) (runtime.InstanceID, []runtime.Repo, error) {
	return "", nil, errors.New("fork not implemented")
}

func (*modelRefreshRuntime) VNCPort(_ context.Context, _ runtime.InstanceID) int { return 0 }

func (*modelRefreshRuntime) Processes(_ context.Context, _ runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	return nil, nil
}

func (*modelRefreshRuntime) Signal(_ context.Context, _ runtime.InstanceID, _ int, _ string) error {
	return nil
}
