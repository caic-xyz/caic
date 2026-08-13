// Repository watcher assembly for app-managed repo injection.

package app

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/repo/repomgr"
)

func newRepoWatcher(ctx context.Context, absRoot string, repoService *repomgr.Service, repoStatus *ci.RepoStatusStore) *repomgr.Watcher {
	return repomgr.NewWatcher(&repomgr.WatcherConfig{
		Ctx:     ctx,
		AbsRoot: absRoot,
		Repos:   repoService.Repos.Snapshot,
		RelPath: repoService.RelPath,
		WorkspaceExists: func(relPath string) bool {
			_, ok := repoService.Workspaces.Workspace(relPath)
			return ok
		},
		OnDiscovered: func(ctx context.Context, abs string) {
			registerDiscoveredRepo(ctx, repoService, repoStatus, abs)
		},
		OnRemoved: repoService.DeregisterWorkspace,
	})
}

func registerDiscoveredRepo(ctx context.Context, repoService *repomgr.Service, repoStatus *ci.RepoStatusStore, abs string) {
	result, err := repoService.DiscoverWorkspace(ctx, abs)
	if err != nil {
		slog.WarnContext(ctx, "new repo: discovery failed", "path", abs, "err", err)
		return
	}
	if _, ok := repoService.Workspaces.Workspace(result.Info.RelPath); ok {
		return
	}
	repoService.RegisterWorkspace(&result, moveRepoStatus(repoStatus))
	slog.InfoContext(ctx, "discovered new repo", "path", result.Info.RelPath, "br", result.Info.BaseBranch)
}

func moveRepoStatus(repoStatus *ci.RepoStatusStore) func(repo.Move) {
	return func(move repo.Move) {
		repoStatus.Move(move.OldRel, move.NewRel)
	}
}
