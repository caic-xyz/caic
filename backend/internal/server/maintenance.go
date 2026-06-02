// Background maintenance: base-image warmup and harness model-cache refresh.

package server

import (
	"context"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// refreshHarnessModels checks if any harness caches are stale and refreshes
// them by launching a temporary runtime instance. Runs once at startup.
func (s *Server) refreshHarnessModels() {
	cache := agent.OpenHarnessCache(filepath.Join(s.cacheDir, "harnesses.json"))

	fetchers := map[agent.Harness]agent.ModelFetcher{}
	s.taskMgr.RangeRunners(func(_ string, r *task.Runner) bool {
		for h, b := range r.Backends {
			if f, ok := b.(agent.ModelFetcher); ok {
				fetchers[h] = f
			}
		}
		return true
	})
	for _, h := range slices.Sorted(maps.Keys(fetchers)) {
		if _, fresh := cache.Models(h, agent.APIKeyHash(s.harnessEnvFor(h))); fresh {
			continue
		}
		s.refreshOneHarness(cache, h, fetchers[h])
	}
}

// refreshOneHarness launches a temporary runtime instance, fetches models, and
// updates the cache and all runner backends.
func (s *Server) refreshOneHarness(cache *agent.HarnessCache, h agent.Harness, fetcher agent.ModelFetcher) {
	backend := s.runtimeBackend
	if backend == nil {
		return
	}
	slog.Info("model cache stale, fetching", "harness", h)
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()

	w := phaseLogWriter{phase: "model-refresh"}
	name, err := backend.Launch(ctx, nil, &runtime.StartOptions{
		Metadata: runtime.Metadata{
			runtime.MetadataModelRefresh: "true",
		},
		Harness:   h,
		LogWriter: w,
	})
	if err != nil {
		slog.Warn("model refresh: launch failed", "harness", h, "err", err)
		return
	}
	defer func() {
		_ = backend.Purge(context.WithoutCancel(ctx), name)
	}()
	if _, err := backend.Connect(ctx, name, &runtime.StartOptions{Harness: h, LogWriter: w}); err != nil {
		slog.Warn("model refresh: connect failed", "harness", h, "err", err)
		return
	}
	env := s.harnessEnvFor(h)
	models, err := fetcher.FetchModels(ctx, string(name), env)
	if err != nil {
		slog.Warn("model refresh: fetch failed", "harness", h, "err", err)
		return
	}
	cache.SetModels(h, models, agent.APIKeyHash(env))
	slog.Info("model cache refreshed", "harness", h, "count", len(models))

	s.taskMgr.RangeRunners(func(_ string, r *task.Runner) bool {
		if b, ok := r.Backends[h]; ok {
			b.SetModels(models)
		}
		return true
	})
}

func (s *Server) harnessEnvFor(h agent.Harness) []string {
	return s.harnessEnv[string(h)]
}

// RefreshHarnessModels refreshes stale harness model caches.
func (s *Server) RefreshHarnessModels() {
	s.refreshHarnessModels()
}

type phaseLogWriter struct {
	phase string
}

func (w phaseLogWriter) Write(p []byte) (int, error) {
	slog.Info(w.phase, "out", string(p))
	return len(p), nil
}
