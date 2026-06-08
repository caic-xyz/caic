// CI service adapter bridging caic stores to the ci.Backend interface.

package app

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

type ciTaskCreator interface {
	CreateTask(ctx context.Context, req bot.TaskRequest) (string, error)
}

// ciAdapter adapts caic stores and managers to ci.Backend.
type ciAdapter struct {
	repos        *repos.Service
	taskMgr      *tasks.Manager
	forge        *forgemanager.Manager
	prefs        *preferences.Store
	warnings     *server.WarningStore
	taskCreator  ciTaskCreator
	notifyChange func()
}

// NotifyTaskChange wakes SSE subscribers after a task state change.
func (a *ciAdapter) NotifyTaskChange() {
	if a.notifyChange != nil {
		a.notifyChange()
	}
}

// EmitWarning delivers a CI warning to connected SSE clients.
func (a *ciAdapter) EmitWarning(msg string) {
	if a.warnings != nil {
		a.warnings.Emit(msg)
	}
}

// GitHubApp returns the GitHub App client for forge operations.
func (a *ciAdapter) GitHubApp() ci.GitHubAppClient { return a.forge.GitHubApp() }

// ForgeForInfo returns a forge client for the given RepoInfo.
func (a *ciAdapter) ForgeForInfo(ctx context.Context, info *ci.RepoInfo) forge.Forge {
	r := &repos.Info{
		ForgeKind:  info.ForgeKind,
		ForgeOwner: info.ForgeOwner,
		ForgeRepo:  info.ForgeRepo,
	}
	return a.forge.ForgeForInfo(ctx, r)
}

// CreateTask creates a bot-style task for CI auto-fix.
func (a *ciAdapter) CreateTask(ctx context.Context, req bot.TaskRequest) (string, error) {
	return a.taskCreator.CreateTask(ctx, req)
}

// GetRunner returns the task runner for relPath.
func (a *ciAdapter) GetRunner(relPath string) (*task.Runner, bool) {
	return a.taskMgr.Runner(relPath)
}

// SetTaskMonitorBranch sets the CI monitor branch on a task entry.
func (a *ciAdapter) SetTaskMonitorBranch(entry ci.TaskEntry, branch string) {
	entry.SetMonitorBranch(branch)
}

// RepoInfoFor returns CI-level repo info for relPath.
func (a *ciAdapter) RepoInfoFor(relPath string) ci.RepoInfo {
	r, ok := a.repos.InfoFor(relPath)
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
	a.taskMgr.Range(func(_ string, e *tasks.Entry) bool {
		if e.Result() != nil {
			return true
		}
		for _, m := range e.Task().ReposSnapshot() {
			active[m.Name] = struct{}{}
		}
		return true
	})
	var out []ci.RepoInfo
	snap := a.repos.Snapshot()
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
	return a.repos.SetCIStatusIfChanged(relPath, sha, result)
}

// Prefs returns the user preferences store.
func (a *ciAdapter) Prefs() *preferences.Store { return a.prefs }
