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
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// NotifyTaskChange wakes SSE subscribers after a task state change.
func (s *Server) NotifyTaskChange() { s.notifyTaskChange() }

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
	defer s.mu.Unlock()
	now := time.Now()
	// Deduplicate: skip if the same message was emitted recently.
	for _, w := range slices.Backward(s.warnings) {
		if now.Sub(w.ts) > warningDedup {
			break
		}
		if w.msg == msg {
			return
		}
	}
	s.warnings = append(s.warnings, serverWarning{msg: msg, ts: now})
	// Trim to cap.
	if len(s.warnings) > maxWarnings {
		s.warnings = s.warnings[len(s.warnings)-maxWarnings:]
	}
	s.taskChanged()
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
	r, ok := s.runners[relPath]
	return r, ok
}

// SetTaskMonitorBranch sets the CI monitor branch on a task entry, serialized
// with other server state mutations via s.mu.
func (s *Server) SetTaskMonitorBranch(entry ci.TaskEntry, branch string) {
	s.mu.Lock()
	entry.SetMonitorBranch(branch)
	s.mu.Unlock()
}

// ---- Repos -----------------------------------------------------------

// RepoInfoFor returns CI-level repo info for relPath.
func (s *Server) RepoInfoFor(relPath string) ci.RepoInfo {
	r := s.repoInfoFor(relPath)
	if r == nil {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ci.RepoInfo
	for i := range s.repos {
		if s.repos[i].ForgeOwner == "" {
			continue
		}
		if !s.repoHasActiveTasksLocked(s.repos[i].RelPath) {
			continue
		}
		r := &s.repos[i]
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

// repoHasActiveTasksLocked reports whether relPath has at least one non-terminal
// task. Must be called with s.mu held.
func (s *Server) repoHasActiveTasksLocked(relPath string) bool {
	for _, e := range s.tasks {
		if e.task.GetState() != task.StateWaiting {
			continue
		}
		for _, m := range e.task.Repos {
			if s.runners[m.Name] != nil && s.runners[m.Name].Dir == relPath {
				return true
			}
		}
	}
	return false
}

// SetRepoCIStatusIfChanged updates the cached CI status for relPath.
// Returns true if the CI status changed (SSE subscribers should be notified).
func (s *Server) SetRepoCIStatusIfChanged(relPath, sha string, result forgecache.Result) bool {
	checks := make([]forge.Check, len(result.Checks))
	copy(checks, result.Checks)
	next := ci.RepoCIState{Status: result.Status, Checks: checks, HeadSHA: sha}
	s.mu.Lock()
	prev := s.repoCIStatus[relPath]
	changed := prev.Status != next.Status
	s.repoCIStatus[relPath] = next
	s.mu.Unlock()
	return changed
}

// ---- Preferences -----------------------------------------------------

// Prefs returns the user preferences store.
func (s *Server) Prefs() *preferences.Store { return s.prefs }

// ---- taskEntry implements ci.TaskEntry ---------------------------------------

// GetTask returns the underlying task.
func (e *taskEntry) GetTask() *task.Task { return e.task }

// GetMonitorBranch returns the branch being monitored for CI.
func (e *taskEntry) GetMonitorBranch() string { return e.monitorBranch }

// SetMonitorBranch sets the branch to monitor for CI.
func (e *taskEntry) SetMonitorBranch(b string) { e.monitorBranch = b }

// GetResult returns the task completion result, or nil.
func (e *taskEntry) GetResult() *task.Result { return e.result }

// SetResult sets the task completion result.
func (e *taskEntry) SetResult(r *task.Result) { e.result = r }

// CloseDone closes the task's completion channel.
func (e *taskEntry) CloseDone() { close(e.done) }

// ---- helpers -----------------------------------------------------------------

// checkToDTO converts a forge.Check to a v1.ForgeCheck for API responses.
func checkToDTO(c *forge.Check) v1.ForgeCheck {
	return v1.ForgeCheck{
		Name:        c.Name,
		Owner:       c.Owner,
		Repo:        c.Repo,
		RunID:       c.RunID,
		JobID:       c.JobID,
		Status:      v1.CheckStatus(c.Status),
		Conclusion:  v1.CheckConclusion(c.Conclusion),
		QueuedAt:    c.QueuedAt,
		StartedAt:   c.StartedAt,
		CompletedAt: c.CompletedAt,
	}
}

func (s *Server) repoInfoFor(relPath string) *repoInfo {
	for i := range s.repos {
		if s.repos[i].RelPath == relPath {
			return &s.repos[i]
		}
	}
	return nil
}
