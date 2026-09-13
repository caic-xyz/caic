// Startup and scheduled maintenance for md images.

package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
)

// warmupInterval controls how often warmupImages re-checks for new base image
// versions. It also sets DigestCacheTTL so runtime starts between warmup cycles
// reuse the cached digest instead of hitting the registry.
const warmupInterval = 6 * time.Hour

func warmupImages(ctx context.Context, log *slog.Logger, client *md.Client, prefs *preferences.Store) error {
	ticker := time.NewTicker(warmupInterval)
	defer ticker.Stop()
	for {
		images := []preferences.ContainerImage{{BaseImage: md.DefaultBaseImage + ":latest"}}
		for _, img := range prefs.BaseImages() {
			if !slices.Contains(images, img) {
				images = append(images, img)
			}
		}
		for _, img := range images {
			w := &mdruntime.SlogWriter{Context: ctx, Logger: log, Phase: "warmup"}
			built, err := client.Warmup(ctx, w, w, &md.WarmupOpts{
				BaseImage: img.BaseImage,
				Platform:  img.Platform,
				Quiet:     true,
			})
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("warmup image %s platform %s: %w", img.BaseImage, img.Platform, err)
			} else if built {
				log.InfoContext(ctx, "image built", "image", img.BaseImage, "platform", img.Platform)
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil
		}
	}
}

// pruneImages removes unused md-built images one runtime at a time on sched
// until ctx is cancelled.
func pruneImages(ctx context.Context, log *slog.Logger, runtimes []mdRuntime, sched *autoupdate.Schedule) error {
	log.InfoContext(ctx, "image pruning enabled")
	for {
		now := time.Now()
		target := sched.Next(now)
		delay := target.Sub(now)
		log.DebugContext(ctx, "image pruning: next run", "in", delay.Round(time.Second))

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		for i := range runtimes {
			w := &mdruntime.SlogWriter{Context: ctx, Logger: log, Phase: "prune"}
			removed, err := runtimes[i].client.PruneImages(ctx, w, w)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				log.WarnContext(ctx, "prune unused images", "err", err)
				continue
			}
			log.InfoContext(ctx, "pruned unused images", "count", len(removed))
		}
	}
}
