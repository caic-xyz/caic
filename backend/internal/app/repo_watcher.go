// Repository watcher assembly for app-managed repo injection.

package app

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/repowatch"
	"github.com/caic-xyz/caic/backend/internal/server"
)

func newRepoWatcher(ctx context.Context, absRoot string, s *server.Server) *repowatch.Watcher {
	return repowatch.New(&repowatch.Config{
		Ctx:          ctx,
		AbsRoot:      absRoot,
		Repos:        func() []repowatch.RepoInfo { return watchedRepos(s) },
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

func watchedRepos(s *server.Server) []repowatch.RepoInfo {
	repos := s.RepoSnapshot()
	out := make([]repowatch.RepoInfo, len(repos))
	for i := range repos {
		out[i] = repowatch.RepoInfo{
			RelPath:    repos[i].RelPath,
			AbsPath:    repos[i].AbsPath,
			BaseBranch: repos[i].BaseBranch,
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
