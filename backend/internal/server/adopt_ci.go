// Adoption-time wiring of forge/CI monitoring for adopted tasks.

package server

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// WireAdoptedCIMonitoring sets up CI monitoring for an adopted task that has a PR.
func (s *Server) WireAdoptedCIMonitoring(ctx context.Context, at *tasks.AdoptedTask) {
	ri, ok := s.repoInfoFor(at.RelPath)
	if !ok {
		return
	}
	f := s.forge.forgeForInfo(ctx, &ri)
	if f == nil && s.authStore != nil {
		if u, ok := s.authStore.FindByProvider(forge.Kind(at.ForgeKind)); ok {
			f = s.forge.forgeFor(auth.NewContext(ctx, &u), forge.Kind(at.ForgeKind))
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
		s.taskMgr.SetTaskMonitorBranch(at.Entry, at.Branch)
		go s.ciService.MonitorCI(s.ctx, at.Entry, f, at.ForgeOwner, at.ForgeRepo, sha) //nolint:contextcheck // CI monitoring must outlive the request
	}
}

// LookupExternalPRForTask queries the forge for a PR matching the task's branch.
func (s *Server) LookupExternalPRForTask(at *tasks.AdoptedTask) {
	ri, ok := s.repoInfoFor(at.RelPath)
	if !ok {
		return
	}
	f := s.forge.forgeForInfo(s.ctx, &ri)
	if f == nil && s.authStore != nil {
		if u, ok := s.authStore.FindByProvider(forge.Kind(at.ForgeKind)); ok {
			f = s.forge.forgeFor(auth.NewContext(s.ctx, &u), forge.Kind(at.ForgeKind))
		}
	}
	if f == nil {
		return
	}
	pr, err := f.FindPRByBranch(s.ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
	if err != nil || pr.Number == 0 {
		return
	}
	slog.Info("adopt: found external PR", "repo", at.RelPath, "br", at.Branch, "pr", pr.Number)
	at.Task.SetPR(at.ForgeOwner, at.ForgeRepo, pr.Number)
	s.taskMgr.NotifyTaskChange()
	sha := pr.HeadSHA
	if sha == "" {
		var err error
		sha, err = f.GetDefaultBranchSHA(s.ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
		if err != nil {
			slog.Info("adopt: skipping CI monitor; PR head SHA unavailable", "task", at.Task.ID, "branch", at.Branch, "repo", at.RelPath, "err", err)
			return
		}
	}
	slog.Info("adopt: starting monitorCI", "task", at.Task.ID, "branch", at.Branch, "sha", sha)
	s.taskMgr.SetTaskMonitorBranch(at.Entry, at.Branch)
	s.ciService.MonitorCI(s.ctx, at.Entry, f, at.ForgeOwner, at.ForgeRepo, sha)
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
