// Implements the ci package role interfaces, adapting server state for the CI service.

package server

import (
	"context"
	"slices"
	"time"

	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// NotifyTaskChange wakes SSE subscribers after a task state change.
func (s *Server) NotifyTaskChange() { s.taskMgr.NotifyTaskChange() }

// serverWarning is a timestamped warning message stored in the Server's ring buffer.
type serverWarning struct {
	msg string
	ts  time.Time
}

const (
	// maxWarnings caps the warning ring buffer.
	maxWarnings = 100
	// warningDedup suppresses duplicate messages within this window.
	warningDedup = 5 * time.Minute
)

// EmitWarning delivers a CI warning to connected SSE clients.
func (s *Server) EmitWarning(msg string) {
	s.mu.Lock()
	now := time.Now()
	// Deduplicate: skip if the same message was emitted recently.
	for _, w := range slices.Backward(s.warnings) {
		if now.Sub(w.ts) > warningDedup {
			break
		}
		if w.msg == msg {
			s.mu.Unlock()
			return
		}
	}
	s.warnings = append(s.warnings, serverWarning{msg: msg, ts: now})
	if len(s.warnings) > maxWarnings {
		s.warnings = s.warnings[len(s.warnings)-maxWarnings:]
	}
	s.mu.Unlock()
	s.taskMgr.NotifyTaskChange()
}

// warningsSince returns all warnings with a timestamp after t.
func (s *Server) warningsSince(t time.Time) []serverWarning {
	var out []serverWarning
	for _, w := range s.warnings {
		if w.ts.After(t) {
			out = append(out, w)
		}
	}
	return out
}

// ---- Forge -----------------------------------------------------------

// GitHubApp returns the GitHub App client for forge operations.
func (s *Server) GitHubApp() ci.GitHubAppClient { return s.forge.githubApp }

// ForgeForInfo returns a forge client for the given RepoInfo.
func (s *Server) ForgeForInfo(ctx context.Context, info *ci.RepoInfo) forge.Forge {
	r := &repoInfo{
		ForgeKind:  info.ForgeKind,
		ForgeOwner: info.ForgeOwner,
		ForgeRepo:  info.ForgeRepo,
	}
	return s.forge.forgeForInfo(ctx, r)
}

// ---- Tasks -----------------------------------------------------------

// GetRunner returns the task runner for relPath.
func (s *Server) GetRunner(relPath string) (*task.Runner, bool) {
	return s.taskMgr.Runner(relPath)
}

// SetTaskMonitorBranch sets the CI monitor branch on a task entry.
func (s *Server) SetTaskMonitorBranch(entry ci.TaskEntry, branch string) {
	entry.SetMonitorBranch(branch)
}

// ---- Repos -----------------------------------------------------------

// RepoInfoFor returns CI-level repo info for relPath.
func (s *Server) RepoInfoFor(relPath string) ci.RepoInfo {
	r, ok := s.repoInfoFor(relPath)
	if !ok {
		return ci.RepoInfo{}
	}
	return ci.RepoInfo{
		RelPath:    r.RelPath,
		BaseBranch: r.BaseBranch,
		ForgeKind:  r.ForgeKind,
		ForgeOwner: r.ForgeOwner,
		ForgeRepo:  r.ForgeRepo,
	}
}

// ListActiveRepos returns repos with forge info that have active (non-terminal) tasks.
func (s *Server) ListActiveRepos() []ci.RepoInfo {
	active := make(map[string]struct{})
	s.taskMgr.Range(func(_ string, e *tasks.Entry) bool {
		if e.Result() != nil {
			return true
		}
		for _, m := range e.Task().ReposSnapshot() {
			active[m.Name] = struct{}{}
		}
		return true
	})
	var out []ci.RepoInfo
	snap := s.repoReg.snapshot()
	for i := range snap {
		r := &snap[i]
		if r.ForgeOwner == "" {
			continue
		}
		if _, ok := active[r.RelPath]; !ok {
			continue
		}
		out = append(out, ci.RepoInfo{
			RelPath:    r.RelPath,
			BaseBranch: r.BaseBranch,
			ForgeKind:  r.ForgeKind,
			ForgeOwner: r.ForgeOwner,
			ForgeRepo:  r.ForgeRepo,
		})
	}
	return out
}

// SetRepoCIStatusIfChanged updates the cached CI status for relPath.
// Returns true if the CI status changed (SSE subscribers should be notified).
func (s *Server) SetRepoCIStatusIfChanged(relPath, sha string, result forgecache.Result) bool {
	checks := make([]forge.Check, len(result.Checks))
	copy(checks, result.Checks)
	next := ci.RepoCIState{Status: result.Status, Checks: checks, HeadSHA: sha}
	return s.repoReg.setCIStatusIfChanged(relPath, next)
}

// ---- Preferences -----------------------------------------------------

// Prefs returns the user preferences store.
func (s *Server) Prefs() *preferences.Store { return s.prefs }

// repoInfoFor returns a copy of the repoInfo for relPath. Callers needing a
// *repoInfo (e.g. forgeForInfo) should take the address of the returned copy.
func (s *Server) repoInfoFor(relPath string) (repoInfo, bool) {
	return s.repoReg.infoFor(relPath)
}
