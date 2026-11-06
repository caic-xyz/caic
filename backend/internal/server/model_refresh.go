// Harness model inventory refresh validates catalogs, owns temporary-runtime cleanup, and preserves cached models on failure.

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// HarnessModels serializes model-inventory refreshes from startup,
// cache deletion, and user-triggered refreshes.
type HarnessModels struct {
	// Log records model refresh progress.
	Log *slog.Logger
	// CacheDir stores the harness model inventory cache.
	CacheDir string
	// Router creates temporary runtimes for model discovery.
	Router *runtime.Router
	// TaskManager owns the configured harness backends.
	TaskManager *taskmgr.Manager
	// HarnessEnv supplies environment variables to each harness refresh.
	HarnessEnv map[string][]string

	mu sync.Mutex
}

// RefreshAll updates harness model inventories. force bypasses the 24-hour cache.
func (r *HarnessModels) RefreshAll(ctx context.Context, force bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	refreshHarnessModels(ctx, r.Log, r.CacheDir, r.Router, r.TaskManager, r.HarnessEnv, force)
}

// Refresh bypasses the cache and updates one harness model inventory.
func (r *HarnessModels) Refresh(ctx context.Context, h harness.Name) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	fetcher, ok := r.TaskManager.Backends[h].(agent.ModelFetcher)
	if !ok {
		return fmt.Errorf("harness %q does not support model refresh", h)
	}
	purgeStaleModelRefreshInstances(ctx, r.Log, r.Router)
	cache := agent.OpenHarnessCache(filepath.Join(r.CacheDir, "harnesses.json"))
	return refreshOneHarness(ctx, r.Log, cache, r.Router, r.TaskManager, h, fetcher, r.HarnessEnv[string(h)])
}

// Watch refreshes stale model caches at startup, then watches harnesses.json
// for deletion and regenerates it on demand.
func (r *HarnessModels) Watch(ctx context.Context) error {
	r.RefreshAll(ctx, false)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create model cache watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(r.CacheDir); err != nil {
		return fmt.Errorf("watch model cache directory: %w", err)
	}

	cachePath := filepath.Clean(filepath.Join(r.CacheDir, "harnesses.json"))
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
			r.Log.InfoContext(ctx, "cache deleted, regenerating", "path", cachePath)
			r.RefreshAll(ctx, false)
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			r.Log.WarnContext(ctx, "cache watcher error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}

// refreshHarnessModels refreshes stale harness caches, or every cache when
// force is true, by launching a temporary runtime instance.
func refreshHarnessModels(ctx context.Context, log *slog.Logger, cacheDir string, router *runtime.Router, taskMgr *taskmgr.Manager, harnessEnv map[string][]string, force bool) {
	purgeStaleModelRefreshInstances(ctx, log, router)
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
		if fresh && !force {
			continue
		}
		if err := refreshOneHarness(ctx, log, cache, router, taskMgr, h, fetchers[h], env); err != nil {
			log.WarnContext(ctx, "model refresh failed", "harness", h, "err", err)
		}
	}
}

// purgeStaleModelRefreshInstances removes temporary model-refresh runtimes left
// behind by a previous server exit.
func purgeStaleModelRefreshInstances(ctx context.Context, log *slog.Logger, router *runtime.Router) {
	instances, err := router.List(ctx)
	if err != nil {
		log.WarnContext(ctx, "stale instance scan incomplete", "err", err)
	}
	for i := range instances {
		id := instances[i].ID
		value, err := router.Metadata(ctx, id, runtime.MetadataModelRefresh)
		if err != nil {
			log.WarnContext(ctx, "metadata read failed", "instance", id, "err", err)
			continue
		}
		if value != "true" {
			continue
		}
		purgeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err = router.Purge(purgeCtx, id)
		cancel()
		if err != nil {
			log.WarnContext(ctx, "stale instance purge failed", "instance", id, "err", err)
			continue
		}
		log.InfoContext(ctx, "purged stale instance", "instance", id)
	}
}

// refreshOneHarness launches a temporary runtime instance, fetches an
// inventory, and updates the cache and all checkout backends.
func refreshOneHarness(
	ctx context.Context,
	log *slog.Logger,
	cache *agent.HarnessCache,
	router *runtime.Router,
	taskMgr *taskmgr.Manager,
	h harness.Name,
	fetcher agent.ModelFetcher,
	env []string,
) error {
	log.InfoContext(ctx, "model cache stale, fetching", "harness", h)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	w := phaseLogWriter{log: log}
	name, err := router.Launch(ctx, nil, &runtime.StartOptions{
		RuntimeName: router.Runtimes[0].Name(),
		Metadata: runtime.Metadata{
			runtime.MetadataModelRefresh: "true",
		},
		Harness:   h,
		LogWriter: w,
	})
	if err != nil {
		return fmt.Errorf("launch model discovery: %w", err)
	}
	// Finish cleanup before publishing a new catalog. A failed refresh keeps
	// the previously usable models both in memory and on disk.
	var inventory agent.ModelInventory
	conn, err := router.Connect(ctx, name, &runtime.StartOptions{Harness: h, LogWriter: w})
	if err != nil {
		err = fmt.Errorf("connect model discovery: %w", err)
	} else {
		inventory, err = fetcher.FetchModelInventory(ctx, conn.AgentTarget, env)
		if err != nil {
			err = fmt.Errorf("fetch model inventory: %w", err)
		} else if len(inventory.Models) == 0 {
			err = errors.New("model discovery returned no models")
		}
	}
	cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	purgeErr := router.Purge(cleanup, name)
	cancelCleanup()
	if purgeErr != nil {
		err = errors.Join(err, fmt.Errorf("purge model discovery instance: %w", purgeErr))
	}
	if err != nil {
		return err
	}
	if b, ok := taskMgr.Backends[h]; ok {
		b.SetModelInventory(inventory)
	}
	cache.SetModelInventory(h, inventory, agent.APIKeyHash(env))
	log.InfoContext(ctx, "model cache refreshed", "harness", h, "count", len(inventory.Models))
	return nil
}

type phaseLogWriter struct {
	log *slog.Logger
}

func (w phaseLogWriter) Write(p []byte) (int, error) {
	w.log.Info("runtime output", "out", string(p))
	return len(p), nil
}
