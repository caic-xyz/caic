// Repository watcher assembly for app-managed repo injection.

package app

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/repos"
)

func newRepoWatcher(ctx context.Context, absRoot string, repoService *repos.Service) *repos.Watcher {
	return repos.NewWatcher(&repos.WatcherConfig{
		Ctx:            ctx,
		AbsRoot:        absRoot,
		Repos:          func() []repos.Info { return watchedRepos(repoService) },
		RelPath:        repoService.RelPath,
		ExecutorExists: repoService.ExecutorRegistered,
		OnDiscovered: func(ctx context.Context, abs string) {
			registerDiscoveredRepo(ctx, repoService, abs)
		},
		OnRemoved: repoService.DeregisterExecutor,
	})
}

func watchedRepos(repoService *repos.Service) []repos.Info {
	snap := repoService.Snapshot()
	out := make([]repos.Info, len(snap))
	for i := range snap {
		out[i] = repos.Info{
			RelPath:    snap[i].RelPath,
			AbsPath:    snap[i].AbsPath,
			BaseBranch: snap[i].BaseBranch,
		}
	}
	return out
}

func registerDiscoveredRepo(ctx context.Context, repoService *repos.Service, abs string) {
	result, err := repoService.DiscoverExecutor(ctx, abs)
	if err != nil {
		slog.WarnContext(ctx, "new repo: discovery failed", "path", abs, "err", err)
		return
	}
	if result.InitErr != nil {
		slog.WarnContext(ctx, "new repo: executor init failed", "path", abs, "err", result.InitErr)
	}
	if repoService.ExecutorRegistered(result.Info.RelPath) {
		return
	}
	repoService.RegisterExecutor(&result)
	slog.InfoContext(ctx, "discovered new repo", "path", result.Info.RelPath, "br", result.Info.BaseBranch)
}
