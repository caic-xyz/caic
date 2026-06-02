// Startup maintenance for task logs and md base images.

package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
)

// warmupInterval controls how often warmupImages re-checks for new base image
// versions. It also sets DigestCacheTTL so runtime starts between warmup cycles
// reuse the cached digest instead of hitting the registry.
const warmupInterval = 6 * time.Hour

func warmupImages(ctx context.Context, client *md.Client, prefs *preferences.Store) {
	ticker := time.NewTicker(warmupInterval)
	defer ticker.Stop()
	for {
		images := []string{md.DefaultBaseImage + ":latest"}
		for _, img := range prefs.BaseImages() {
			if !slices.Contains(images, img) {
				images = append(images, img)
			}
		}
		for _, img := range images {
			w := &mdruntime.SlogWriter{Phase: "warmup"}
			built, err := client.Warmup(ctx, w, w, &md.WarmupOpts{
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
		case <-ctx.Done():
			return
		}
	}
}

// migrateTaskLogs moves *.jsonl files from cacheDir into the tasks
// subdirectory. This is a one-time migration for installations that stored
// task logs directly in CacheDir.
// TODO: Remove after 2026-05-01.
func migrateTaskLogs(cacheDir, tasksDir string) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	var jsonlFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".jsonl" {
			jsonlFiles = append(jsonlFiles, e)
		}
	}
	if len(jsonlFiles) == 0 {
		return
	}
	if err := os.MkdirAll(tasksDir, 0o750); err != nil {
		slog.Warn("migrate: cannot create tasks dir", "path", tasksDir, "err", err)
		return
	}
	for _, e := range jsonlFiles {
		src := filepath.Join(cacheDir, e.Name())
		dst := filepath.Join(tasksDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			slog.Warn("migrate: cannot move log", "file", e.Name(), "err", err)
		}
	}
	slog.Info("migrated task logs", "n", len(jsonlFiles), "dst", tasksDir)
}
