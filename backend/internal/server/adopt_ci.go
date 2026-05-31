// Adoption-time wiring of forge/CI monitoring for adopted tasks.

package server

import (
	"context"
	"log/slog"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// wireAdoptedCIMonitoring sets up CI monitoring for an adopted task that has a PR.
func (s *Server) wireAdoptedCIMonitoring(ctx context.Context, at *tasks.AdoptedTask) {
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
		sha, err := f.GetDefaultBranchSHA(ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
		if err != nil {
			slog.Warn("adopt: GetDefaultBranchSHA failed", "task", at.Task.ID, "branch", at.Branch, "err", err)
			return
		}
		slog.Info("adopt: starting monitorCI", "task", at.Task.ID, "branch", at.Branch, "sha", sha)
		s.taskMgr.SetTaskMonitorBranch(at.Entry, at.Branch)
		go s.ciService.MonitorCI(s.ctx, at.Entry, f, at.ForgeOwner, at.ForgeRepo, sha) //nolint:contextcheck // CI monitoring must outlive the request
	}
}

// lookupExternalPRForTask queries the forge for a PR matching the task's branch.
func (s *Server) lookupExternalPRForTask(at *tasks.AdoptedTask) {
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
	sha, err := f.GetDefaultBranchSHA(s.ctx, at.ForgeOwner, at.ForgeRepo, at.Branch)
	if err != nil {
		slog.Warn("adopt: GetDefaultBranchSHA failed", "task", at.Task.ID, "branch", at.Branch, "err", err)
		return
	}
	slog.Info("adopt: starting monitorCI", "task", at.Task.ID, "branch", at.Branch, "sha", sha)
	s.taskMgr.SetTaskMonitorBranch(at.Entry, at.Branch)
	s.ciService.MonitorCI(s.ctx, at.Entry, f, at.ForgeOwner, at.ForgeRepo, sha)
}
