// Adoption-time wiring of forge/CI monitoring for adopted tasks.

package app

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// adoptedTaskWiring connects adopted tasks back to forge and CI automation. It
// is an app-lifetime concern, driven during startup adoption.
type adoptedTaskWiring struct {
	ctx       context.Context
	authStore *auth.Store
	ciService *ci.Service
	forge     *forgemanager.Manager
	taskMgr   *tasks.Manager
	repos     *repos.Service
}

// WireCIMonitoring sets up CI monitoring for an adopted task that has a PR.
func (w *adoptedTaskWiring) WireCIMonitoring(ctx context.Context, at *tasks.AdoptedTask) {
	ri, ok := w.repos.InfoFor(at.RelPath)
	if !ok {
		return
	}
	f := w.forge.ForgeForInfo(ctx, &ri)
	if f == nil && w.authStore != nil {
		if u, ok := w.authStore.FindByProvider(forge.Kind(at.ForgeKind)); ok {
			f = w.forge.ForgeFor(auth.NewContext(ctx, &u), forge.Kind(at.ForgeKind))
		}
	}
	if f == nil {
		return
	}
	pr := at.Task.Snapshot().ForgePR
	if pr > 0 {
		sha, err := adoptedHeadSHA(ctx, f, at)
		if err != nil {
			slog.Info("adopt: skipping CI monitor; PR head SHA unavailable", "task", at.Task.ID, "branch", at.Branch, "repo", at.RelPath, "err", err)
			return
		}
		slog.Info("adopt: starting monitorCI", "task", at.Task.ID, "branch", at.Branch, "sha", sha)
		w.taskMgr.SetTaskMonitorBranch(at.Entry, at.Branch)
		go w.ciService.MonitorCI(w.ctx, at.Entry, f, at.ForgeOwner, at.ForgeRepo, sha) //nolint:contextcheck // CI monitoring must outlive startup
	}
}

// LookupExternalPRForTask queries the forge for a PR matching the task's branch.
func (w *adoptedTaskWiring) LookupExternalPRForTask(at *tasks.AdoptedTask) {
	ri, ok := w.repos.InfoFor(at.RelPath)
	if !ok {
		return
	}
	f := w.forge.ForgeForInfo(w.ctx, &ri)
	if f == nil && w.authStore != nil {
		if u, ok := w.authStore.FindByProvider(forge.Kind(at.ForgeKind)); ok {
			f = w.forge.ForgeFor(auth.NewContext(w.ctx, &u), forge.Kind(at.ForgeKind))
		}
	}
	if f == nil {
		return
	}
	pr, err := f.FindPRByBranch(w.ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
	if err != nil || pr.Number == 0 {
		return
	}
	slog.Info("adopt: found external PR", "repo", at.RelPath, "br", at.Branch, "pr", pr.Number)
	at.Task.SetPR(at.ForgeOwner, at.ForgeRepo, pr.Number)
	w.taskMgr.NotifyTaskChange()
	sha := pr.HeadSHA
	if sha == "" {
		var err error
		sha, err = f.GetDefaultBranchSHA(w.ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
		if err != nil {
			slog.Info("adopt: skipping CI monitor; PR head SHA unavailable", "task", at.Task.ID, "branch", at.Branch, "repo", at.RelPath, "err", err)
			return
		}
	}
	slog.Info("adopt: starting monitorCI", "task", at.Task.ID, "branch", at.Branch, "sha", sha)
	w.taskMgr.SetTaskMonitorBranch(at.Entry, at.Branch)
	w.ciService.MonitorCI(w.ctx, at.Entry, f, at.ForgeOwner, at.ForgeRepo, sha)
}

func adoptedHeadSHA(ctx context.Context, f forge.Forge, at *tasks.AdoptedTask) (string, error) {
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
