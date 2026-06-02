// Background maintenance: base-image warmup and harness model-cache refresh.

package server

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// warmupInterval controls how often warmupImages re-checks for new base image
// versions. It also sets DigestCacheTTL so that container starts between
// warmup cycles reuse the cached digest instead of hitting the registry.
const warmupInterval = 6 * time.Hour

// warmupImages periodically calls md.Client.Warmup for the default base image
// and any custom images configured in user preferences. This ensures the image
// is pulled and the md-user layer is built before a task needs it.
func (s *Server) warmupImages() {
	// Run immediately on startup, then every warmupInterval.
	ticker := time.NewTicker(warmupInterval)
	defer ticker.Stop()
	for {
		images := []string{md.DefaultBaseImage + ":latest"}
		for _, img := range s.prefs.BaseImages() {
			if !slices.Contains(images, img) {
				images = append(images, img)
			}
		}
		for _, img := range images {
			w := &container.SlogWriter{Phase: "warmup"}
			built, err := s.mdClient.Warmup(s.ctx, w, w, &md.WarmupOpts{
				BaseImage: img,
				Quiet:     true,
			})
			if err != nil {
				slog.Warn("warmup", "image", img, "err", err)
			} else if built {
				slog.Info("warmup", "image", img, "built", true)
			}
		}
		select {
		case <-ticker.C:
		case <-s.ctx.Done():
			return
		}
	}
}

// refreshHarnessModels checks if any harness caches are stale and refreshes
// them by launching a temporary container. Runs once at startup.
func (s *Server) refreshHarnessModels() {
	cache := agent.OpenHarnessCache(filepath.Join(s.cacheDir, "harnesses.json"))

	type fetchFunc func(ctx context.Context, container string, env []string) ([]string, error)
	harnesses := []struct {
		h     agent.Harness
		fetch fetchFunc
	}{
		{agent.Codex, codex.FetchModels},
		{agent.Pi, pi.FetchModels},
		{agent.OpenCode, opencode.FetchModels},
	}
	for _, entry := range harnesses {
		if _, fresh := cache.Models(entry.h, agent.APIKeyHash(s.backend.HarnessEnv[string(entry.h)])); fresh {
			continue
		}
		s.refreshOneHarness(cache, entry.h, entry.fetch)
	}
}

// refreshOneHarness launches a temporary container, fetches models, and
// updates the cache and all runner backends.
func (s *Server) refreshOneHarness(cache *agent.HarnessCache, h agent.Harness, fetch func(ctx context.Context, container string, env []string) ([]string, error)) {
	slog.Info("model cache stale, fetching", "rt", s.mdClient.Runtime, "harness", h)
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()

	w := &container.SlogWriter{Phase: "model-refresh"}
	name, err := s.backend.Launch(ctx, nil, []string{"model-refresh"}, &task.StartOptions{
		Harness:   h,
		LogWriter: w,
	})
	if err != nil {
		slog.Warn("model refresh: launch failed", "harness", h, "err", err)
		return
	}
	defer func() {
		_ = s.backend.Purge(context.WithoutCancel(ctx), name, nil)
	}()
	if _, err := s.backend.Connect(ctx, name, nil, &task.StartOptions{Harness: h, LogWriter: w}); err != nil {
		slog.Warn("model refresh: connect failed", "harness", h, "err", err)
		return
	}
	models, err := fetch(ctx, string(name), s.backend.HarnessEnv[string(h)])
	if err != nil {
		slog.Warn("model refresh: fetch failed", "harness", h, "err", err)
		return
	}
	cache.SetModels(h, models, agent.APIKeyHash(s.backend.HarnessEnv[string(h)]))
	slog.Info("model cache refreshed", "harness", h, "count", len(models))

	s.taskMgr.RangeRunners(func(_ string, r *task.Runner) bool {
		if b, ok := r.Backends[h]; ok {
			b.SetModels(models)
		}
		return true
	})
}
