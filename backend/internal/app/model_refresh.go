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
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// refreshHarnessModels checks if any harness caches are stale and refreshes
// them by launching a temporary runtime instance.
func refreshHarnessModels(ctx context.Context, cacheDir string, backend runtime.Backend, inventory runtime.Inventory, taskMgr *taskmgr.Manager, harnessEnv map[string][]string) {
	purgeStaleModelRefreshInstances(ctx, backend, inventory)
	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))

	fetchers := map[harness.Name]agent.ModelFetcher{}
	for h, b := range taskMgr.Backends() {
		if f, ok := b.(agent.ModelFetcher); ok {
			fetchers[h] = f
		}
	}
	for _, h := range slices.Sorted(maps.Keys(fetchers)) {
		env := harnessEnv[string(h)]
		if _, fresh := cache.Models(h, agent.APIKeyHash(env)); fresh {
			continue
		}
		refreshOneHarness(ctx, cache, backend, taskMgr, h, fetchers[h], env)
	}
}

// purgeStaleModelRefreshInstances removes temporary model-refresh runtimes left
// behind by a previous server exit.
func purgeStaleModelRefreshInstances(ctx context.Context, backend runtime.Backend, inventory runtime.Inventory) {
	if backend == nil || inventory == nil {
		return
	}
	instances, err := inventory.List(ctx)
	if err != nil {
		slog.WarnContext(ctx, "model refresh: stale instance scan failed", "err", err)
		return
	}
	for _, instance := range instances {
		value, err := inventory.Metadata(ctx, instance.ID, runtime.MetadataModelRefresh)
		if err != nil {
			slog.WarnContext(ctx, "model refresh: metadata read failed", "instance", instance.ID, "err", err)
			continue
		}
		if value != "true" {
			continue
		}
		purgeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err = backend.Purge(purgeCtx, instance.ID)
		cancel()
		if err != nil {
			slog.WarnContext(ctx, "model refresh: stale instance purge failed", "instance", instance.ID, "err", err)
			continue
		}
		slog.InfoContext(ctx, "model refresh: purged stale instance", "instance", instance.ID)
	}
}

// refreshOneHarness launches a temporary runtime instance, fetches models, and
// updates the cache and all workspace backends.
func refreshOneHarness(
	ctx context.Context,
	cache *agent.HarnessCache,
	backend runtime.Backend,
	taskMgr *taskmgr.Manager,
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

	if b, ok := taskMgr.Backends()[h]; ok {
		b.SetModels(models)
	}
}

type phaseLogWriter struct {
	phase string
}

func (w phaseLogWriter) Write(p []byte) (int, error) {
	slog.Info(w.phase, "out", string(p))
	return len(p), nil
}
