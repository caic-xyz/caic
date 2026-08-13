// Harness model inventory cache refresh and deletion watcher.

package app

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// watchHarnessModelCache refreshes stale model caches at startup, then watches
// harnesses.json for deletion and regenerates it on demand.
func watchHarnessModelCache(ctx context.Context, cacheDir string, router *runtime.Router, taskMgr *taskmgr.Manager, harnessEnv map[string][]string) error {
	refreshHarnessModels(ctx, cacheDir, router, taskMgr, harnessEnv)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create model cache watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(cacheDir); err != nil {
		return fmt.Errorf("watch model cache directory: %w", err)
	}

	cachePath := filepath.Clean(filepath.Join(cacheDir, "harnesses.json"))
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if filepath.Clean(event.Name) != cachePath {
				continue
			}
			if !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
				continue
			}
			slog.InfoContext(ctx, "model cache deleted, regenerating", "path", cachePath)
			refreshHarnessModels(ctx, cacheDir, router, taskMgr, harnessEnv)
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.WarnContext(ctx, "model refresh: cache watcher error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}

// refreshHarnessModels checks if any harness caches are stale and refreshes
// them by launching a temporary runtime instance.
func refreshHarnessModels(ctx context.Context, cacheDir string, router *runtime.Router, taskMgr *taskmgr.Manager, harnessEnv map[string][]string) {
	purgeStaleModelRefreshInstances(ctx, router)
	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))

	fetchers := map[harness.Name]agent.ModelFetcher{}
	for h, b := range taskMgr.Backends {
		if f, ok := b.(agent.ModelFetcher); ok {
			fetchers[h] = f
		}
	}
	for _, h := range slices.Sorted(maps.Keys(fetchers)) {
		env := harnessEnv[string(h)]
		envHash := agent.APIKeyHash(env)
		_, fresh := cache.ModelInventory(h, envHash)
		if fresh {
			continue
		}
		refreshOneHarness(ctx, cache, router, taskMgr, h, fetchers[h], env)
	}
}

// purgeStaleModelRefreshInstances removes temporary model-refresh runtimes left
// behind by a previous server exit.
func purgeStaleModelRefreshInstances(ctx context.Context, router *runtime.Router) {
	instances, err := router.List(ctx)
	if err != nil {
		slog.WarnContext(ctx, "model refresh: stale instance scan failed", "err", err)
		return
	}
	for i := range instances {
		id := instances[i].ID
		value, err := router.Metadata(ctx, id, runtime.MetadataModelRefresh)
		if err != nil {
			slog.WarnContext(ctx, "model refresh: metadata read failed", "instance", id, "err", err)
			continue
		}
		if value != "true" {
			continue
		}
		purgeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err = router.Purge(purgeCtx, id)
		cancel()
		if err != nil {
			slog.WarnContext(ctx, "model refresh: stale instance purge failed", "instance", id, "err", err)
			continue
		}
		slog.InfoContext(ctx, "model refresh: purged stale instance", "instance", id)
	}
}

// refreshOneHarness launches a temporary runtime instance, fetches an
// inventory, and updates the cache and all workspace backends.
func refreshOneHarness(
	ctx context.Context,
	cache *agent.HarnessCache,
	router *runtime.Router,
	taskMgr *taskmgr.Manager,
	h harness.Name,
	fetcher agent.ModelFetcher,
	env []string,
) {
	slog.InfoContext(ctx, "model cache stale, fetching", "harness", h)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	w := phaseLogWriter{phase: "model-refresh"}
	name, err := router.Launch(ctx, nil, &runtime.StartOptions{
		RuntimeName: router.Runtimes[0].Name(),
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
		if err := router.Purge(context.WithoutCancel(ctx), name); err != nil {
			slog.WarnContext(ctx, "model refresh: purge failed", "harness", h, "instance", name, "err", err)
		}
	}()
	conn, err := router.Connect(ctx, name, &runtime.StartOptions{Harness: h, LogWriter: w})
	if err != nil {
		slog.WarnContext(ctx, "model refresh: connect failed", "harness", h, "err", err)
		return
	}
	inventory, err := fetcher.FetchModelInventory(ctx, conn.AgentTarget, env)
	if err != nil {
		slog.WarnContext(ctx, "model refresh: fetch failed", "harness", h, "err", err)
		return
	}
	if b, ok := taskMgr.Backends[h]; ok {
		b.SetModelInventory(inventory)
	}
	cache.SetModelInventory(h, inventory, agent.APIKeyHash(env))
	slog.InfoContext(ctx, "model cache refreshed", "harness", h, "count", len(inventory.Models))
}

type phaseLogWriter struct {
	phase string
}

func (w phaseLogWriter) Write(p []byte) (int, error) {
	slog.Info(w.phase, "out", string(p))
	return len(p), nil
}
