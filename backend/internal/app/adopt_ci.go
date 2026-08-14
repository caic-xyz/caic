// Adoption-time wiring of forge/CI monitoring for adopted tasks.

package app

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// adoptedTaskWiring connects adopted tasks back to forge and CI automation. It
// is an app-lifetime concern, driven during startup adoption.
type adoptedTaskWiring struct {
	authStore *auth.Store
	ciService *ci.Service
	forgeMgr  *forgemgr.Manager
	taskMgr   *taskmgr.Manager
	repoSvc   *repo.Service
}

// WireCIMonitoring sets up CI monitoring for an adopted task that has a PR.
func (w *adoptedTaskWiring) WireCIMonitoring(ctx context.Context, at *taskmgr.AdoptedTask) {
	ri, ok := w.repoSvc.Repositories.Repository(at.RelPath)
	if !ok {
		return
	}
	f := w.forgeMgr.ForgeForInfo(ctx, &ri)
	if f == nil && w.authStore != nil {
		if u, ok := w.authStore.FindByProvider(auth.Provider(at.ForgeKind)); ok {
			f = w.forgeMgr.ForgeFor(auth.NewContext(ctx, &u), forge.Kind(at.ForgeKind))
		}
	}
	if f == nil {
		return
	}
	pr := at.Task.Snapshot().ForgePR
	if pr > 0 {
		sha, err := adoptedHeadSHA(ctx, f, at)
		if err != nil {
			slog.InfoContext(ctx, "adopt: skipping CI monitor; PR head SHA unavailable", "task", at.Task.ID, "branch", at.Branch, "repo", at.RelPath, "err", err)
			return
		}
		slog.InfoContext(ctx, "adopt: starting monitorCI", "task", at.Task.ID, "branch", at.Branch, "sha", sha)
		w.taskMgr.SetTaskMonitorBranch(at.Entry, at.Branch)
		w.ciService.MonitorCI(ctx, at.Entry, f, at.ForgeOwner, at.ForgeRepo, sha)
	}
}

// LookupExternalPRForTask queries the forge for a PR matching the task's branch.
func (w *adoptedTaskWiring) LookupExternalPRForTask(ctx context.Context, at *taskmgr.AdoptedTask) {
	ri, ok := w.repoSvc.Repositories.Repository(at.RelPath)
	if !ok {
		return
	}
	f := w.forgeMgr.ForgeForInfo(ctx, &ri)
	if f == nil && w.authStore != nil {
		if u, ok := w.authStore.FindByProvider(auth.Provider(at.ForgeKind)); ok {
			f = w.forgeMgr.ForgeFor(auth.NewContext(ctx, &u), forge.Kind(at.ForgeKind))
		}
	}
	if f == nil {
		return
	}
	pr, err := f.FindPRByBranch(ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
	if err != nil || pr.Number == 0 {
		return
	}
	slog.InfoContext(ctx, "adopt: found external PR", "repo", at.RelPath, "br", at.Branch, "pr", pr.Number)
	at.Task.SetPR(at.ForgeOwner, at.ForgeRepo, pr.Number)
	w.taskMgr.NotifyTaskChange()
	sha := pr.HeadSHA
	if sha == "" {
		var err error
		sha, err = f.GetDefaultBranchSHA(ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
		if err != nil {
			slog.InfoContext(ctx, "adopt: skipping CI monitor; PR head SHA unavailable", "task", at.Task.ID, "branch", at.Branch, "repo", at.RelPath, "err", err)
			return
		}
	}
	slog.InfoContext(ctx, "adopt: starting monitorCI", "task", at.Task.ID, "branch", at.Branch, "sha", sha)
	w.taskMgr.SetTaskMonitorBranch(at.Entry, at.Branch)
	w.ciService.MonitorCI(ctx, at.Entry, f, at.ForgeOwner, at.ForgeRepo, sha)
}

func adoptedHeadSHA(ctx context.Context, f forge.Forge, at *taskmgr.AdoptedTask) (string, error) {
	pr, err := f.FindPRByBranch(ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
	if err == nil && pr.HeadSHA != "" {
		if at.Task != nil && pr.Number > 0 {
			at.Task.SetPR(at.ForgeOwner, at.ForgeRepo, pr.Number)
		}
		return pr.HeadSHA, nil
	}
	sha, err := f.GetDefaultBranchSHA(ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
	if err != nil {
		return "", err
	}
	return sha, nil
}
