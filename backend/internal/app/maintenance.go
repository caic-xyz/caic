// Scheduled repository repacking and image pruning, plus the image warmup interval.

package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
)

// warmupInterval controls how often scheduled warmup re-checks for new base image
// versions. It also sets DigestCacheTTL so runtime starts between warmup cycles
// reuse the cached digest instead of hitting the registry.
const warmupInterval = 6 * time.Hour

const minRepackObjectBytes = 1 << 30
const minRepackLooseObjects = 1000
const minRepackLooseBytes = 64 << 20

// repackRepositories packs large checkouts serially on the configured schedule.
func repackRepositories(ctx context.Context, log *slog.Logger, checkouts *repo.Registry, sched *autoupdate.Schedule) error {
	for {
		now := time.Now()
		timer := time.NewTimer(sched.Next(now).Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		for checkout := range checkouts.Checkouts() {
			if ctx.Err() != nil {
				return nil
			}
			stats, err := (&git.Checkout{Root: checkout.Dir}).ObjectStats(ctx)
			if err != nil {
				log.WarnContext(ctx, "measure repository objects", "repo", checkout.Dir, "err", err)
				continue
			}
			if stats.LooseBytes+stats.PackBytes < minRepackObjectBytes ||
				stats.LooseCount <= minRepackLooseObjects || stats.LooseBytes <= minRepackLooseBytes {
				continue
			}
			start := time.Now()
			if err := repackRepository(ctx, checkout.Dir); err != nil {
				log.WarnContext(ctx, "repack repository", "repo", checkout.Dir, "err", err)
				continue
			}
			log.InfoContext(ctx, "repacked repository", "repo", checkout.Dir,
				"loose_objects", stats.LooseCount, "loose_bytes", stats.LooseBytes, "duration", time.Since(start))
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
