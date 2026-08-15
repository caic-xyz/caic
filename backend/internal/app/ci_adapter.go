// CI service adapter bridging caic stores to the ci.Backend interface.

package app

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

type ciTaskCreator interface {
	CreateTask(ctx context.Context, req task.CreateRequest) (string, error)
}

// ciAdapter adapts caic stores and managers to ci.Backend.
type ciAdapter struct {
	repoMgr     *repo.Service
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
	a.warnings.Emit(msg)
}

// GitHubApp returns the GitHub App client for forge operations.
func (a *ciAdapter) GitHubApp() ci.GitHubAppClient { return a.forgeMgr.GitHubApp() }

// ForgeForInfo returns a forge client for the given RepoInfo.
func (a *ciAdapter) ForgeForInfo(ctx context.Context, info *ci.RepoInfo) forge.Forge {
	r := &repo.Repository{
		ForgeKind:  info.ForgeKind,
		ForgeOwner: info.ForgeOwner,
		ForgeRepo:  info.ForgeRepo,
	}
	return a.forgeMgr.ForgeForInfo(ctx, r)
}

// CreateTask creates an automated task for CI auto-fix.
func (a *ciAdapter) CreateTask(ctx context.Context, req task.CreateRequest) (string, error) {
	return a.taskCreator.CreateTask(ctx, req)
}

// GetCheckout returns the task checkout for relPath.
func (a *ciAdapter) GetCheckout(relPath string) (*repo.Checkout, bool) {
	return a.taskMgr.Checkouts.Checkout(relPath)
}

// RuntimeRouter returns the task runtime router.
func (a *ciAdapter) RuntimeRouter() *runtime.Router { return a.taskMgr.Runtimes }

// SetTaskMonitorBranch sets the CI monitor branch on a task entry.
func (a *ciAdapter) SetTaskMonitorBranch(entry ci.TaskEntry, branch string) {
	entry.SetMonitorBranch(branch)
}

// RepoInfoFor returns CI-level repo info for relPath.
func (a *ciAdapter) RepoInfoFor(relPath string) ci.RepoInfo {
	checkout, ok := a.repoMgr.Repositories.Checkout(relPath)
	if !ok || checkout.Repository == nil {
		return ci.RepoInfo{}
	}
	return ci.RepoInfo{
		RelPath:    checkout.RelPath,
		BaseBranch: checkout.BaseBranch,
		ForgeKind:  checkout.Repository.ForgeKind,
		ForgeOwner: checkout.Repository.ForgeOwner,
		ForgeRepo:  checkout.Repository.ForgeRepo,
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
	for checkout := range a.repoMgr.Repositories.Checkouts() {
		if checkout.Repository == nil || checkout.Repository.ForgeOwner == "" {
			continue
		}
		if _, ok := active[checkout.RelPath]; !ok {
			continue
		}
		out = append(out, ci.RepoInfo{
			RelPath:    checkout.RelPath,
			BaseBranch: checkout.BaseBranch,
			ForgeKind:  checkout.Repository.ForgeKind,
			ForgeOwner: checkout.Repository.ForgeOwner,
			ForgeRepo:  checkout.Repository.ForgeRepo,
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
