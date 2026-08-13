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
		Ctx:             ctx,
		AbsRoot:         absRoot,
		Repos:           func() []repo.Info { return watchedRepos(repoService) },
		RelPath:         repoService.RelPath,
		WorkspaceExists: repoService.WorkspaceRegistered,
		OnDiscovered: func(ctx context.Context, abs string) {
			registerDiscoveredRepo(ctx, repoService, repoStatus, abs)
		},
		OnRemoved: repoService.DeregisterWorkspace,
	})
}

func watchedRepos(repoService *repomgr.Service) []repo.Info {
	snap := repoService.Repos.Snapshot()
	out := make([]repo.Info, len(snap))
	for i := range snap {
		out[i] = repo.Info{
			RelPath:    snap[i].RelPath,
			AbsPath:    snap[i].AbsPath,
			BaseBranch: snap[i].BaseBranch,
		}
	}
	return out
}

func registerDiscoveredRepo(ctx context.Context, repoService *repomgr.Service, repoStatus *ci.RepoStatusStore, abs string) {
	result, err := repoService.DiscoverWorkspace(ctx, abs)
	if err != nil {
		slog.WarnContext(ctx, "new repo: discovery failed", "path", abs, "err", err)
		return
	}
	if repoService.WorkspaceRegistered(result.Info.RelPath) {
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
