// Repository watcher assembly for app-managed repo injection.

package app

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/repo"
)

func newRepoWatcher(ctx context.Context, absRoot string, repoService *repo.Service, repoStatus *ci.RepoStatusStore) *repo.Watcher {
	return repo.NewWatcher(&repo.WatcherConfig{
		Ctx:     ctx,
		AbsRoot: absRoot,
		Repos:   repoService.Repositories.Repositories,
		RelPath: repoService.RelPath,
		CheckoutExists: func(relPath string) bool {
			_, ok := repoService.Repositories.Checkout(relPath)
			return ok
		},
		OnDiscovered: func(ctx context.Context, abs string) {
			registerDiscoveredRepo(ctx, repoService, repoStatus, abs)
		},
		OnRemoved: repoService.DeregisterCheckout,
	})
}

func registerDiscoveredRepo(ctx context.Context, repoService *repo.Service, repoStatus *ci.RepoStatusStore, abs string) {
	result, err := repoService.DiscoverCheckout(ctx, abs)
	if err != nil {
		slog.WarnContext(ctx, "new repo: discovery failed", "path", abs, "err", err)
		return
	}
	if _, ok := repoService.Repositories.Checkout(result.Repository.RelPath); ok {
		return
	}
	repoService.RegisterCheckout(&result, moveRepoStatus(repoStatus))
	slog.InfoContext(ctx, "discovered new repo", "path", result.Repository.RelPath, "br", result.Repository.BaseBranch)
}

func moveRepoStatus(repoStatus *ci.RepoStatusStore) func(repo.Move) {
	return func(move repo.Move) {
		repoStatus.Move(move.OldRel, move.NewRel)
	}
}
