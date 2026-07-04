// Harness model-cache refresh during app startup.

package app

import (
	"context"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// refreshHarnessModels checks if any harness caches are stale and refreshes
// them by launching a temporary runtime instance.
func refreshHarnessModels(ctx context.Context, cacheDir string, backend runtime.Backend, taskMgr *tasks.Manager, harnessEnv map[string][]string) {
	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))

	fetchers := map[harness.Name]agent.ModelFetcher{}
	taskMgr.RangeExecutors(func(_ string, r *task.RepoExecutor) bool {
		for h, b := range r.Backends {
			if f, ok := b.(agent.ModelFetcher); ok {
				fetchers[h] = f
			}
		}
		return true
	})
	for _, h := range slices.Sorted(maps.Keys(fetchers)) {
		env := harnessEnv[string(h)]
		if _, fresh := cache.Models(h, agent.APIKeyHash(env)); fresh {
			continue
		}
		refreshOneHarness(ctx, cache, backend, taskMgr, h, fetchers[h], env)
	}
}

// refreshOneHarness launches a temporary runtime instance, fetches models, and
// updates the cache and all runner backends.
func refreshOneHarness(
	ctx context.Context,
	cache *agent.HarnessCache,
	backend runtime.Backend,
	taskMgr *tasks.Manager,
	h harness.Name,
	fetcher agent.ModelFetcher,
	env []string,
) {
	if backend == nil {
		return
	}
	slog.InfoContext(ctx, "model cache stale, fetching", "harness", h)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
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
		slog.WarnContext(ctx, "model refresh: launch failed", "harness", h, "err", err)
		return
	}
	defer func() {
		_ = backend.Purge(context.WithoutCancel(ctx), name)
	}()
	conn, err := backend.Connect(ctx, name, &runtime.StartOptions{Harness: h, LogWriter: w})
	if err != nil {
		slog.WarnContext(ctx, "model refresh: connect failed", "harness", h, "err", err)
		return
	}
	models, err := fetcher.FetchModels(ctx, conn.AgentTarget, env)
	if err != nil {
		slog.WarnContext(ctx, "model refresh: fetch failed", "harness", h, "err", err)
		return
	}
	cache.SetModels(h, models, agent.APIKeyHash(env))
	slog.InfoContext(ctx, "model cache refreshed", "harness", h, "count", len(models))

	taskMgr.RangeExecutors(func(_ string, r *task.RepoExecutor) bool {
		if b, ok := r.Backends[h]; ok {
			b.SetModels(models)
		}
		return true
	})
}

type phaseLogWriter struct {
	phase string
}

func (w phaseLogWriter) Write(p []byte) (int, error) {
	slog.Info(w.phase, "out", string(p))
	return len(p), nil
}
