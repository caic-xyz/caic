// Repository watcher assembly for app-managed repo injection.

package app

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/server"
)

func newRepoWatcher(ctx context.Context, absRoot string, s *server.Server) *repos.Watcher {
	return repos.NewWatcher(&repos.WatcherConfig{
		Ctx:          ctx,
		AbsRoot:      absRoot,
		Repos:        func() []repos.Info { return watchedRepos(s) },
		RelPath:      s.RepoRelPath,
		RunnerExists: s.RunnerRegistered,
		OnDiscovered: func(ctx context.Context, abs string) {
			registerDiscoveredRepo(ctx, s, abs)
		},
		OnRemoved: func(rel string) {
			s.DeregisterRepoRunner(rel)
		},
	})
}

func watchedRepos(s *server.Server) []repos.Info {
	snap := s.RepoSnapshot()
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

func registerDiscoveredRepo(ctx context.Context, s *server.Server, abs string) {
	result, err := s.DiscoverRepoRunner(ctx, abs)
	if err != nil {
		slog.WarnContext(ctx, "new repo: discovery failed", "path", abs, "err", err)
		return
	}
	if result.InitErr != nil {
		slog.WarnContext(ctx, "new repo: runner init failed", "path", abs, "err", result.InitErr)
	}
	if s.RunnerRegistered(result.Info.RelPath) {
		return
	}
	s.RegisterRepoRunner(&result)
	slog.InfoContext(ctx, "discovered new repo", "path", result.Info.RelPath, "br", result.Info.BaseBranch)
}
