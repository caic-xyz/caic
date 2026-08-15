// Repository watcher assembly for app-managed repo injection.

package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/repo"
)

const repoWatcherInterval = 30 * time.Second

// repoWatcher reconciles local checkouts below the app's repository root.
type repoWatcher struct {
	log        *slog.Logger
	ctx        context.Context
	absRoot    string
	checkouts  *repo.Registry
	repoStatus *ci.RepoStatusStore
}

func newRepoWatcher(ctx context.Context, absRoot string, checkouts *repo.Registry, repoStatus *ci.RepoStatusStore) *repoWatcher {
	return &repoWatcher{
		log:        slog.With("cmp", "repo-watcher", "root", absRoot),
		ctx:        ctx,
		absRoot:    absRoot,
		checkouts:  checkouts,
		repoStatus: repoStatus,
	}
}

func (w *repoWatcher) watch() {
	mtimes := make(map[string]time.Time)
	ticker := time.NewTicker(repoWatcherInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.poll(w.ctx, mtimes)
		case <-w.ctx.Done():
			return
		}
	}
}

func (w *repoWatcher) syncReposInDir(ctx context.Context, dir string) {
	paths, err := git.DiscoverCheckouts(dir, 1)
	if err != nil {
		w.log.WarnContext(ctx, "repo scan failed", "dir", dir, "err", err)
		return
	}
	current := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		current[path] = struct{}{}
	}

	registered := make(map[string]struct{})
	for checkout := range w.checkouts.Checkouts() {
		registered[checkout.Dir] = struct{}{}
		if filepath.Dir(checkout.Dir) != dir {
			continue
		}
		if _, ok := current[checkout.Dir]; ok {
			continue
		}
		w.checkouts.UnregisterCheckout(checkout.RelPath)
		w.log.InfoContext(ctx, "unregistered removed checkout", "path", checkout.RelPath)
	}

	var wg sync.WaitGroup
	for _, abs := range paths {
		if _, ok := registered[abs]; ok {
			continue
		}
		wg.Go(func() { w.register(ctx, abs) })
	}
	wg.Wait()
}

func (w *repoWatcher) register(ctx context.Context, abs string) {
	checkout, err := repo.DiscoverCheckout(ctx, w.log.With("path", abs), abs)
	if err != nil {
		w.log.WarnContext(ctx, "new repo: discovery failed", "path", abs, "err", err)
		return
	}
	checkout.RelPath = checkoutRelPath(w.absRoot, abs)
	if _, ok := w.checkouts.Checkout(checkout.RelPath); ok {
		return
	}
	if err := w.checkouts.RegisterCheckout(checkout); err != nil {
		w.log.WarnContext(ctx, "register checkout failed", "path", checkout.RelPath, "err", err)
		return
	}
	w.log.InfoContext(ctx, "discovered new repo", "path", checkout.RelPath, "br", checkout.BaseBranch)
}

func (w *repoWatcher) poll(ctx context.Context, mtimes map[string]time.Time) {
	dirs := collectRepoWatchDirs(ctx, w.log, w.absRoot, repoDiscoveryDepth-1)
	dirSet := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		dirSet[dir] = struct{}{}
	}
	for dir := range mtimes {
		if _, ok := dirSet[dir]; !ok {
			delete(mtimes, dir)
			w.unregisterUnder(ctx, dir)
		}
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			delete(mtimes, dir)
			w.unregisterUnder(ctx, dir)
			continue
		}
		if !info.ModTime().After(mtimes[dir]) {
			continue
		}
		mtimes[dir] = info.ModTime()
		w.syncReposInDir(ctx, dir)
	}
}

func (w *repoWatcher) unregisterUnder(ctx context.Context, dir string) {
	prefix := dir + string(filepath.Separator)
	for checkout := range w.checkouts.Checkouts() {
		if !strings.HasPrefix(checkout.Dir, prefix) {
			continue
		}
		w.checkouts.UnregisterCheckout(checkout.RelPath)
		w.log.InfoContext(ctx, "unregistered removed checkout", "path", checkout.RelPath)
	}
}

func collectRepoWatchDirs(ctx context.Context, log *slog.Logger, root string, maxDepth int) []string {
	dirs := []string{root}
	if maxDepth <= 0 {
		return dirs
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		log.DebugContext(ctx, "watch dirs: read dir failed", "path", root, "err", err)
		return dirs
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dirs = append(dirs, collectRepoWatchDirs(ctx, log, filepath.Join(root, entry.Name()), maxDepth-1)...)
	}
	return dirs
}
