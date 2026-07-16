// Package repomgr manages discovered git repositories and their task workspaces.
package repomgr

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/repo"
)

// DiscoveryDepth is the maximum directory levels below AbsRoot that
// DiscoverCheckouts scans for git repositories, both at startup and during
// background polling.
const DiscoveryDepth = 3

// WatcherConfig contains watcher dependencies.
type WatcherConfig struct {
	Ctx     context.Context
	AbsRoot string
	// TODO: I'm unhappy with this design.
	Repos           func() []repo.Info
	RelPath         func(string) string
	WorkspaceExists func(string) bool
	OnDiscovered    func(context.Context, string)
	OnRemoved       func(string)
	Interval        time.Duration
	MaxDepth        int
}

// Watcher polls the repository root and reconciles the workspace registry.
type Watcher struct {
	// Immutable.
	log     *slog.Logger
	ctx     context.Context
	absRoot string
	// TODO: I'm unhappy with this design.
	repos           func() []repo.Info
	relPath         func(string) string
	workspaceExists func(string) bool
	onDiscovered    func(context.Context, string)
	onRemoved       func(string)
	interval        time.Duration
	maxDepth        int
}

// NewWatcher creates a repository watcher.
func NewWatcher(c *WatcherConfig) *Watcher {
	interval := c.Interval
	if interval == 0 {
		interval = 30 * time.Second
	}
	maxDepth := c.MaxDepth
	if maxDepth == 0 {
		maxDepth = DiscoveryDepth
	}
	workspaceExists := c.WorkspaceExists
	if workspaceExists == nil {
		workspaceExists = func(string) bool { return false }
	}
	onDiscovered := c.OnDiscovered
	if onDiscovered == nil {
		onDiscovered = func(context.Context, string) {}
	}
	onRemoved := c.OnRemoved
	if onRemoved == nil {
		onRemoved = func(string) {}
	}
	return &Watcher{
		log:             slog.Default().With(slog.String("cmp", "repo-watcher"), slog.String("root", c.AbsRoot)),
		ctx:             c.Ctx,
		absRoot:         c.AbsRoot,
		repos:           c.Repos,
		relPath:         c.RelPath,
		workspaceExists: workspaceExists,
		onDiscovered:    onDiscovered,
		onRemoved:       onRemoved,
		interval:        interval,
		maxDepth:        maxDepth,
	}
}

// Watch polls AbsRoot and its subdirectories for new or removed git
// repositories until the watcher context is canceled.
func (w *Watcher) Watch() {
	mtimes := make(map[string]time.Time)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.pollRepoChanges(w.ctx, mtimes)
		case <-w.ctx.Done():
			return
		}
	}
}

// SyncReposInDir discovers repositories at depth 1 within dir and reconciles
// the registered set.
func (w *Watcher) SyncReposInDir(ctx context.Context, dir string) {
	paths, err := git.DiscoverCheckouts(dir, 1)
	if err != nil {
		w.log.WarnContext(ctx, "repo scan failed", "dir", dir, "err", err)
		return
	}
	currentSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		currentSet[p] = struct{}{}
	}

	registered := w.registeredAbsPathSet()
	registeredRepos := w.repos()
	for i := range registeredRepos {
		r := &registeredRepos[i]
		if filepath.Dir(r.AbsPath) != dir {
			continue
		}
		_, present := currentSet[r.AbsPath]
		if present {
			continue
		}
		w.onRemoved(r.RelPath)
		w.log.InfoContext(ctx, "deregistered removed repo", "path", r.RelPath)
	}

	var newPaths []string
	for _, p := range paths {
		if _, ok := registered[p]; !ok {
			newPaths = append(newPaths, p)
		}
	}
	if len(newPaths) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, abs := range newPaths {
		wg.Go(func() {
			rel := w.relPath(abs)
			if w.workspaceExists(rel) {
				return
			}
			w.onDiscovered(ctx, abs)
		})
	}
	wg.Wait()
}

func (w *Watcher) pollRepoChanges(ctx context.Context, mtimes map[string]time.Time) {
	dirs := collectWatchDirs(ctx, w.log, w.absRoot, w.maxDepth-1)
	dirSet := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		dirSet[d] = struct{}{}
	}
	for dir := range mtimes {
		if _, ok := dirSet[dir]; !ok {
			delete(mtimes, dir)
			w.deregisterReposUnder(ctx, dir)
		}
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			delete(mtimes, dir)
			w.deregisterReposUnder(ctx, dir)
			continue
		}
		if !info.ModTime().After(mtimes[dir]) {
			continue
		}
		mtimes[dir] = info.ModTime()
		w.SyncReposInDir(ctx, dir)
	}
}

func (w *Watcher) deregisterReposUnder(ctx context.Context, dir string) {
	prefix := dir + string(filepath.Separator)
	registeredRepos := w.repos()
	for i := range registeredRepos {
		r := &registeredRepos[i]
		if !strings.HasPrefix(r.AbsPath, prefix) {
			continue
		}
		w.onRemoved(r.RelPath)
		w.log.InfoContext(ctx, "deregistered removed repo", "path", r.RelPath)
	}
}

func (w *Watcher) registeredAbsPathSet() map[string]struct{} {
	repos := w.repos()
	out := make(map[string]struct{}, len(repos))
	for i := range repos {
		out[repos[i].AbsPath] = struct{}{}
	}
	return out
}

func collectWatchDirs(ctx context.Context, log *slog.Logger, root string, maxDepth int) []string {
	dirs := []string{root}
	if maxDepth <= 0 {
		return dirs
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		log.DebugContext(ctx, "watch dirs: read dir failed", "path", root, "err", err)
		return dirs
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sub := filepath.Join(root, e.Name())
		dirs = append(dirs, collectWatchDirs(ctx, log, sub, maxDepth-1)...)
	}
	return dirs
}
