// Import-time wiring of forge/CI monitoring for restored tasks.

package app

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// importedTaskWiring connects restored tasks back to forge and CI automation.
// It is an app-lifetime concern, driven during startup import.
type importedTaskWiring struct {
	log       *slog.Logger
	authStore *auth.Store
	ciService *ci.Service
	forgeMgr  *forgemgr.Manager
	taskMgr   *taskmgr.Manager
}

// WireCIMonitoring sets up CI monitoring for an imported task that has a PR.
func (w *importedTaskWiring) WireCIMonitoring(ctx context.Context, entry *taskmgr.Entry, checkout *repo.Checkout) {
	t := entry.Task()
	log := w.log.With("task", t.ID)
	primary := t.Primary()
	if primary == nil || checkout.Repository == nil {
		return
	}
	f := w.forgeMgr.ForgeForInfo(ctx, checkout.Repository)
	if f == nil && w.authStore != nil {
		if u, ok := w.authStore.FindByProvider(auth.Provider(checkout.Repository.ForgeKind)); ok {
			f = w.forgeMgr.ForgeFor(auth.NewContext(ctx, &u), checkout.Repository.ForgeKind)
		}
	}
	if f == nil {
		return
	}
	pr := t.Snapshot().ForgePR
	if pr > 0 {
		sha, err := importedHeadSHA(ctx, f, t, checkout.Repository, primary.Branch)
		if err != nil {
			log.InfoContext(ctx, "skipping CI monitor; PR head SHA unavailable", "branch", primary.Branch, "repo", primary.Name, "err", err)
			return
		}
		log.InfoContext(ctx, "starting CI monitor", "branch", primary.Branch, "sha", sha)
		w.taskMgr.SetTaskMonitorBranch(entry, primary.Branch)
		w.ciService.MonitorCI(ctx, entry, f, checkout.Repository.ForgeOwner, checkout.Repository.ForgeRepo, sha)
	}
}

// LookupExternalPRForTask queries the forge for a PR matching the task's branch.
func (w *importedTaskWiring) LookupExternalPRForTask(ctx context.Context, entry *taskmgr.Entry, checkout *repo.Checkout) {
	t := entry.Task()
	log := w.log.With("task", t.ID)
	primary := t.Primary()
	if primary == nil || checkout.Repository == nil {
		return
	}
	f := w.forgeMgr.ForgeForInfo(ctx, checkout.Repository)
	if f == nil && w.authStore != nil {
		if u, ok := w.authStore.FindByProvider(auth.Provider(checkout.Repository.ForgeKind)); ok {
			f = w.forgeMgr.ForgeFor(auth.NewContext(ctx, &u), checkout.Repository.ForgeKind)
		}
	}
	if f == nil {
		return
	}
	pr, err := f.FindPRByBranch(ctx, checkout.Repository.ForgeOwner, checkout.Repository.ForgeRepo, primary.Branch)
	if err != nil || pr.Number == 0 {
		return
	}
	log.InfoContext(ctx, "found external PR", "repo", primary.Name, "br", primary.Branch, "pr", pr.Number)
	t.SetPR(checkout.Repository.ForgeOwner, checkout.Repository.ForgeRepo, pr.Number)
	w.taskMgr.NotifyTaskChange()
	sha := pr.HeadSHA
	if sha == "" {
		var err error
		sha, err = f.GetDefaultBranchSHA(ctx, checkout.Repository.ForgeOwner, checkout.Repository.ForgeRepo, primary.Branch)
		if err != nil {
			log.InfoContext(ctx, "skipping CI monitor; PR head SHA unavailable", "branch", primary.Branch, "repo", primary.Name, "err", err)
			return
		}
	}
	log.InfoContext(ctx, "starting CI monitor", "branch", primary.Branch, "sha", sha)
	w.taskMgr.SetTaskMonitorBranch(entry, primary.Branch)
	w.ciService.MonitorCI(ctx, entry, f, checkout.Repository.ForgeOwner, checkout.Repository.ForgeRepo, sha)
}

func importedHeadSHA(ctx context.Context, f forge.Forge, t *task.Task, repository *repo.Repository, branch string) (string, error) {
	pr, err := f.FindPRByBranch(ctx, repository.ForgeOwner, repository.ForgeRepo, branch)
	if err == nil && pr.HeadSHA != "" {
		if pr.Number > 0 {
			t.SetPR(repository.ForgeOwner, repository.ForgeRepo, pr.Number)
		}
		return pr.HeadSHA, nil
	}
	sha, err := f.GetDefaultBranchSHA(ctx, repository.ForgeOwner, repository.ForgeRepo, branch)
	if err != nil {
		return "", err
	}
	return sha, nil
}
