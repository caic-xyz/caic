// CI service adapter bridging caic stores to the ci.Backend interface.

package app

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/repo/repomgr"
	"github.com/caic-xyz/caic/backend/internal/repo/repowork"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

type ciTaskCreator interface {
	CreateTask(ctx context.Context, req bot.TaskRequest) (string, error)
}

// ciAdapter adapts caic stores and managers to ci.Backend.
type ciAdapter struct {
	repoMgr     *repomgr.Service
	repoStatus  *ci.RepoStatusStore
	taskMgr     *taskmgr.Manager
	forgeMgr    *forgemgr.Manager
	prefs       *preferences.Store
	warnings    *server.WarningStore
	taskCreator ciTaskCreator
}

// NotifyTaskChange wakes SSE subscribers after a task state change.
func (a *ciAdapter) NotifyTaskChange() {
	a.taskMgr.NotifyTaskChange()
}

// EmitWarning delivers a CI warning to connected SSE clients.
func (a *ciAdapter) EmitWarning(msg string) {
	if a.warnings != nil {
		a.warnings.Emit(msg)
	}
}

// GitHubApp returns the GitHub App client for forge operations.
func (a *ciAdapter) GitHubApp() ci.GitHubAppClient { return a.forgeMgr.GitHubApp() }

// ForgeForInfo returns a forge client for the given RepoInfo.
func (a *ciAdapter) ForgeForInfo(ctx context.Context, info *ci.RepoInfo) forge.Forge {
	r := &repo.Info{
		ForgeKind:  info.ForgeKind,
		ForgeOwner: info.ForgeOwner,
		ForgeRepo:  info.ForgeRepo,
	}
	return a.forgeMgr.ForgeForInfo(ctx, r)
}

// CreateTask creates a bot-style task for CI auto-fix.
func (a *ciAdapter) CreateTask(ctx context.Context, req bot.TaskRequest) (string, error) {
	return a.taskCreator.CreateTask(ctx, req)
}

// GetWorkspace returns the task workspace for relPath.
func (a *ciAdapter) GetWorkspace(relPath string) (*repowork.Workspace, bool) {
	return a.taskMgr.Workspace(relPath)
}

// SetTaskMonitorBranch sets the CI monitor branch on a task entry.
func (a *ciAdapter) SetTaskMonitorBranch(entry ci.TaskEntry, branch string) {
	entry.SetMonitorBranch(branch)
}

// RepoInfoFor returns CI-level repo info for relPath.
func (a *ciAdapter) RepoInfoFor(relPath string) ci.RepoInfo {
	r, ok := a.repoMgr.InfoFor(relPath)
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
func (a *ciAdapter) ListActiveRepos() []ci.RepoInfo {
	active := make(map[string]struct{})
	a.taskMgr.Range(func(_ string, e *taskmgr.Entry) bool {
		if e.Result() != nil {
			return true
		}
		for _, m := range e.Task().ReposSnapshot() {
			active[m.Name] = struct{}{}
		}
		return true
	})
	var out []ci.RepoInfo
	snap := a.repoMgr.Snapshot()
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
func (a *ciAdapter) SetRepoCIStatusIfChanged(relPath, sha string, result forgecache.Result) bool {
	return a.repoStatus.SetResultIfChanged(relPath, sha, result)
}

// Prefs returns the user preferences store.
func (a *ciAdapter) Prefs() *preferences.Store { return a.prefs }
