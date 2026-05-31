// Background watcher that registers and deregisters repos as they appear and disappear under the repos root.

package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md/gitutil"
)

// repoInitResult holds the outcome of initialising a single newly-discovered
// repository.
type repoInitResult struct {
	info   repoInfo
	runner *task.Runner
}

// watchNewRepos polls absRoot and its subdirectories every 30 seconds for
// new or removed git repositories, registering and deregistering them without
// requiring a server restart.
func (s *Server) watchNewRepos() {
	mtimes := make(map[string]time.Time)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.pollRepoChanges(s.ctx, mtimes)
		case <-s.ctx.Done():
			return
		}
	}
}

// pollRepoChanges enumerates all directories at depth 0..repoDiscoveryDepth-1
// from absRoot, stats each, and for those whose mtime has advanced calls
// syncReposInDir. Directories that have disappeared since the last tick
// trigger deregisterReposUnder.
func (s *Server) pollRepoChanges(ctx context.Context, mtimes map[string]time.Time) {
	dirs := collectWatchDirs(ctx, s.absRoot, repoDiscoveryDepth-1)
	dirSet := make(map[string]struct{}, len(dirs))
	for _, d := range dirs {
		dirSet[d] = struct{}{}
	}
	// Deregister repos under any directory that disappeared since last tick.
	for dir := range mtimes {
		if _, ok := dirSet[dir]; !ok {
			delete(mtimes, dir)
			s.deregisterReposUnder(ctx, dir)
		}
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil {
			delete(mtimes, dir)
			s.deregisterReposUnder(ctx, dir)
			continue
		}
		if !info.ModTime().After(mtimes[dir]) {
			continue
		}
		mtimes[dir] = info.ModTime()
		s.syncReposInDir(ctx, dir)
	}
}

// syncReposInDir discovers repositories at depth 1 within dir and reconciles
// the registered set: new repos are initialised and removed repos are
// deregistered.
func (s *Server) syncReposInDir(ctx context.Context, dir string) {
	paths, err := gitutil.DiscoverRepos(dir, 1)
	if err != nil {
		slog.WarnContext(ctx, "discover repos: scan failed", "dir", dir, "err", err)
		return
	}
	currentSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		currentSet[p] = struct{}{}
	}

	// Deregister repos directly under dir that are no longer present.
	s.mu.Lock()
	var removed []string
	s.repos = slices.DeleteFunc(s.repos, func(r repoInfo) bool {
		if filepath.Dir(r.AbsPath) != dir {
			return false
		}
		if _, ok := currentSet[r.AbsPath]; ok {
			return false
		}
		removed = append(removed, r.RelPath)
		delete(s.runners, r.RelPath)
		return true
	})
	registered := make(map[string]struct{}, len(s.repos))
	for i := range s.repos {
		registered[s.repos[i].AbsPath] = struct{}{}
	}
	s.mu.Unlock()
	for _, rel := range removed {
		slog.InfoContext(ctx, "deregistered removed repo", "path", rel)
	}

	// Initialise newly discovered repos in parallel.
	var newPaths []string
	for _, p := range paths {
		if _, ok := registered[p]; !ok {
			newPaths = append(newPaths, p)
		}
	}
	if len(newPaths) == 0 {
		return
	}
	results := make([]repoInitResult, len(newPaths))
	var wg sync.WaitGroup
	for i, abs := range newPaths {
		wg.Go(func() {
			rel, err := filepath.Rel(s.absRoot, abs)
			if err != nil {
				rel = filepath.Base(abs)
			}
			// Guard against a concurrent clone adding the same path.
			s.mu.Lock()
			_, exists := s.runners[rel]
			s.mu.Unlock()
			if exists {
				return
			}
			remoteName, err := gitutil.DefaultRemote(ctx, abs)
			if err != nil {
				slog.DebugContext(ctx, "new repo: no remote, skipping", "path", abs, "err", err)
				return
			}
			branch, err := gitutil.DefaultBranch(ctx, abs, remoteName)
			if err != nil {
				slog.WarnContext(ctx, "new repo: cannot determine default branch", "path", abs, "err", err)
				return
			}
			remote := gitutil.RemoteOriginURL(ctx, abs)
			runner := &task.Runner{
				BaseBranch: branch,
				Dir:        abs,
				RepoName:   rel,
				LogDir:     s.logDir,
				CacheDir:   s.cacheDir,
				HarnessEnv: s.backend.HarnessEnv,
				Container:  s.backend,
			}
			if err := runner.Init(ctx); err != nil {
				slog.WarnContext(ctx, "new repo: runner init failed", "path", abs, "err", err)
			}
			var forgeKind forge.Kind
			var forgeOwner, forgeRepo string
			if rawURL, err := forge.RemoteURL(ctx, abs); err == nil {
				forgeKind, forgeOwner, forgeRepo, _ = forge.ParseRemoteURL(rawURL)
			}
			results[i] = repoInitResult{
				info: repoInfo{
					RelPath: rel, AbsPath: abs, BaseBranch: branch, BaseBranchRemote: remoteName, Remote: remote,
					ForgeKind: forgeKind, ForgeOwner: forgeOwner, ForgeRepo: forgeRepo,
				},
				runner: runner,
			}
			slog.InfoContext(ctx, "discovered new repo", "path", rel, "br", branch)
		})
	}
	wg.Wait()

	s.mu.Lock()
	for i := range results {
		if results[i].runner == nil {
			continue
		}
		rel := results[i].info.RelPath
		if _, exists := s.runners[rel]; exists {
			continue
		}
		s.repos = append(s.repos, results[i].info)
		s.runners[rel] = results[i].runner
	}
	s.mu.Unlock()
}

// deregisterReposUnder removes all registered repositories whose absolute
// path is within dir (used when dir itself has been deleted).
func (s *Server) deregisterReposUnder(ctx context.Context, dir string) {
	prefix := dir + string(filepath.Separator)
	s.mu.Lock()
	var removed []string
	s.repos = slices.DeleteFunc(s.repos, func(r repoInfo) bool {
		if !strings.HasPrefix(r.AbsPath, prefix) {
			return false
		}
		removed = append(removed, r.RelPath)
		delete(s.runners, r.RelPath)
		return true
	})
	s.mu.Unlock()
	for _, rel := range removed {
		slog.InfoContext(ctx, "deregistered removed repo", "path", rel)
	}
}

// collectWatchDirs returns root and all of its subdirectories down to
// maxDepth levels, for use as mtime-watch targets. Subdirectories that
// cannot be read are silently skipped. Dot-prefixed entries (e.g. ".git")
// are skipped to match gitutil.DiscoverRepos's recursion behaviour and to
// avoid descending into a repo's internal .git directory, which itself
// looks like a bare repo to the discoverer.
func collectWatchDirs(ctx context.Context, root string, maxDepth int) []string {
	dirs := []string{root}
	if maxDepth <= 0 {
		return dirs
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		slog.DebugContext(ctx, "watch dirs: read dir failed", "path", root, "err", err)
		return dirs
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sub := filepath.Join(root, e.Name())
		dirs = append(dirs, collectWatchDirs(ctx, sub, maxDepth-1)...)
	}
	return dirs
}
