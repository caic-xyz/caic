// Periodic well-known cache size snapshots for settings and diagnostics.

package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

const cacheSizeRefreshInterval = 24 * time.Hour

// CacheSizeStore holds periodic well-known cache size snapshots. It is
// constructed and refreshed by internal/app; HTTP handlers only read snapshots.
type CacheSizeStore struct {
	mu    sync.RWMutex
	home  string
	sizes map[string]v1.CacheSize
}

// NewCacheSizeStore creates an empty cache size store.
func NewCacheSizeStore() *CacheSizeStore {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("cache size: cannot resolve user home", "err", err)
	}
	return &CacheSizeStore{
		home:  home,
		sizes: make(map[string]v1.CacheSize, len(md.WellKnownCaches)),
	}
}

// Refresh recomputes the well-known cache size snapshot once.
func (c *CacheSizeStore) Refresh(ctx context.Context) {
	sizes := calculateWellKnownCacheSizes(ctx, c.home, md.WellKnownCaches)

	c.mu.Lock()
	c.sizes = sizes
	c.mu.Unlock()
}

// Snapshot returns the current well-known cache sizes, sorted by name.
func (c *CacheSizeStore) Snapshot() []v1.CacheSize {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]v1.CacheSize, 0, len(c.sizes))
	for _, size := range c.sizes {
		out = append(out, size)
	}
	slices.SortFunc(out, func(a, b v1.CacheSize) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

func calculateWellKnownCacheSizes(ctx context.Context, home string, caches map[string][]md.CacheMount) map[string]v1.CacheSize {
	now := time.Now().UTC()
	out := make(map[string]v1.CacheSize, len(caches))
	for name, mounts := range caches {
		size, err := cacheMountsSize(ctx, home, mounts)
		snapshot := v1.CacheSize{
			Name:         name,
			SizeBytes:    size,
			CalculatedAt: now,
		}
		if err != nil {
			snapshot.Error = err.Error()
		}
		out[name] = snapshot
	}
	return out
}

func cacheMountsSize(ctx context.Context, home string, mounts []md.CacheMount) (int64, error) {
	seen := make(map[string]struct{}, len(mounts))
	var total int64
	var errs []error
	for _, m := range mounts {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		hostPath := md.ResolveHostPath(m.HostPath, home)
		if home == "" && md.IsHomeRelativeHostPath(m.HostPath) {
			hostPath = ""
		}
		if hostPath == "" {
			errs = append(errs, fmt.Errorf("%s: empty host path", m.Name))
			continue
		}
		if _, ok := seen[hostPath]; ok {
			continue
		}
		seen[hostPath] = struct{}{}
		size, err := directorySize(ctx, hostPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", hostPath, err))
		}
		total += size
	}
	return total, errors.Join(errs...)
}

func directorySize(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// RefreshLoop refreshes well-known cache sizes until ctx is cancelled. It is
// owned by internal/app, which starts it as a background maintenance loop.
func (c *CacheSizeStore) RefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(cacheSizeRefreshInterval)
	defer ticker.Stop()
	for {
		c.Refresh(ctx)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}
