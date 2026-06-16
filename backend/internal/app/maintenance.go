// Startup maintenance for md base images.

package app

import (
	"context"
	"log/slog"
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
		images := []preferences.ContainerImage{{BaseImage: md.DefaultBaseImage + ":latest"}}
		for _, img := range prefs.BaseImages() {
			if !slices.Contains(images, img) {
				images = append(images, img)
			}
		}
		for _, img := range images {
			w := &mdruntime.SlogWriter{Phase: "warmup"}
			built, err := client.Warmup(ctx, w, w, &md.WarmupOpts{
				BaseImage: img.BaseImage,
				Platform:  img.Platform,
				Quiet:     true,
			})
			if err != nil {
				slog.WarnContext(ctx, "warmup", "image", img.BaseImage, "platform", img.Platform, "err", err)
			} else if built {
				slog.InfoContext(ctx, "warmup", "image", img.BaseImage, "platform", img.Platform, "built", true)
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}
