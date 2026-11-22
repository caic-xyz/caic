// Discovers and reconciles topmost app-managed repositories below the checkout root.

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
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

const repoWatcherInterval = 30 * time.Second

// repoWatcher reconciles local checkouts below the app's repository root.
type repoWatcher struct {
	log        *slog.Logger
	ctx        context.Context
	absRoot    string
	checkouts  *repo.Registry
	repoStatus *ci.RepoStatusStore
	runtimes   *runtime.Router
}

func newRepoWatcher(ctx context.Context, log *slog.Logger, absRoot string, checkouts *repo.Registry, repoStatus *ci.RepoStatusStore, runtimes *runtime.Router) *repoWatcher {
	return &repoWatcher{
		log:        log.With("root", absRoot),
		ctx:        ctx,
		absRoot:    absRoot,
		checkouts:  checkouts,
		repoStatus: repoStatus,
		runtimes:   runtimes,
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
	instances, err := w.runtimes.List(ctx)
	if err != nil {
		w.log.WarnContext(ctx, "runtime instance list incomplete; branch numbering may collide with a running container", "path", abs, "err", err)
	}
	liveBranches := repo.LiveBranchesByRoot(instances)[abs]
	checkout, err := repo.DiscoverCheckout(ctx, w.log.With("path", abs), abs, liveBranches)
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
	rootRepos, err := git.DiscoverCheckouts(w.absRoot, 0)
	if err != nil {
		w.log.DebugContext(ctx, "root checkout scan failed", "err", err)
	}
	// A directory can become a repository after nested checkouts were registered.
	// Remove those checkouts even on the first poll after server startup.
	for checkout := range w.checkouts.Checkouts() {
		if checkout.Dir == w.absRoot || !strings.HasPrefix(checkout.Dir, w.absRoot+string(filepath.Separator)) {
			continue
		}
		if _, ok := dirSet[filepath.Dir(checkout.Dir)]; !ok || len(rootRepos) != 0 {
			w.checkouts.UnregisterCheckout(checkout.RelPath)
			w.log.InfoContext(ctx, "unregistered nested checkout", "path", checkout.RelPath)
		}
	}
	for dir := range mtimes {
		if _, ok := dirSet[dir]; !ok {
			delete(mtimes, dir)
			// A new .git changes the repository directory's mtime, not its
			// parent's. Rescan the parent to discover this new boundary.
			delete(mtimes, filepath.Dir(dir))
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
	var dirs []string
	var walk func(string, int)
	walk = func(dir string, depth int) {
		// Match startup discovery's repository boundary, including gitfiles and
		// bare repositories. Watching inside it would discover its submodules
		// and nested repositories as independent checkouts on later polls.
		repos, err := git.DiscoverCheckouts(dir, 0)
		if err != nil {
			log.DebugContext(ctx, "watch dirs: discover checkout failed", "path", dir, "err", err)
			return
		}
		if len(repos) != 0 {
			if dir == root {
				dirs = append(dirs, dir)
			}
			return
		}
		dirs = append(dirs, dir)
		if depth <= 0 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			log.DebugContext(ctx, "watch dirs: read dir failed", "path", dir, "err", err)
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			walk(filepath.Join(dir, entry.Name()), depth-1)
		}
	}
	walk(root, maxDepth)
	return dirs
}
