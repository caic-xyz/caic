// CI service adapter and warning store.

package server

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// serverWarning is a timestamped warning message stored for SSE clients.
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

type warningStore struct {
	taskMgr *tasks.Manager

	mu       sync.Mutex
	warnings []serverWarning
}

func newWarningStore(taskMgr *tasks.Manager) *warningStore {
	return &warningStore{taskMgr: taskMgr}
}

// Emit delivers a CI warning to connected SSE clients.
func (s *warningStore) Emit(msg string) {
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
	if s.taskMgr != nil {
		s.taskMgr.NotifyTaskChange()
	}
}

// Since returns all warnings with a timestamp after t.
func (s *warningStore) Since(t time.Time) []serverWarning {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []serverWarning
	for _, w := range s.warnings {
		if w.ts.After(t) {
			out = append(out, w)
		}
	}
	return out
}

type ciTaskCreator interface {
	CreateTask(ctx context.Context, req bot.TaskRequest) (string, error)
}

type ciAdapterDeps struct {
	repoReg      *repoRegistry
	taskMgr      *tasks.Manager
	forge        *ForgeManager
	prefs        *preferences.Store
	warnings     *warningStore
	taskCreator  ciTaskCreator
	infoForRepo  func(relPath string) (RepoInfo, bool)
	notifyChange func()
}

// CIAdapter adapts server stores and managers to ci.Backend.
type CIAdapter struct {
	repoReg      *repoRegistry
	taskMgr      *tasks.Manager
	forge        *ForgeManager
	prefs        *preferences.Store
	warnings     *warningStore
	taskCreator  ciTaskCreator
	infoForRepo  func(relPath string) (RepoInfo, bool)
	notifyChange func()
}

func newCIAdapter(d ciAdapterDeps) *CIAdapter {
	return &CIAdapter{
		repoReg:      d.repoReg,
		taskMgr:      d.taskMgr,
		forge:        d.forge,
		prefs:        d.prefs,
		warnings:     d.warnings,
		taskCreator:  d.taskCreator,
		infoForRepo:  d.infoForRepo,
		notifyChange: d.notifyChange,
	}
}

// NotifyTaskChange wakes SSE subscribers after a task state change.
func (a *CIAdapter) NotifyTaskChange() {
	if a.notifyChange != nil {
		a.notifyChange()
	}
}

// EmitWarning delivers a CI warning to connected SSE clients.
func (a *CIAdapter) EmitWarning(msg string) {
	if a.warnings != nil {
		a.warnings.Emit(msg)
	}
}

// GitHubApp returns the GitHub App client for forge operations.
func (a *CIAdapter) GitHubApp() ci.GitHubAppClient { return a.forge.githubApp }

// ForgeForInfo returns a forge client for the given RepoInfo.
func (a *CIAdapter) ForgeForInfo(ctx context.Context, info *ci.RepoInfo) forge.Forge {
	r := &RepoInfo{
		ForgeKind:  info.ForgeKind,
		ForgeOwner: info.ForgeOwner,
		ForgeRepo:  info.ForgeRepo,
	}
	return a.forge.forgeForInfo(ctx, r)
}

// CreateTask creates a bot-style task for CI auto-fix.
func (a *CIAdapter) CreateTask(ctx context.Context, req bot.TaskRequest) (string, error) {
	return a.taskCreator.CreateTask(ctx, req)
}

// GetRunner returns the task runner for relPath.
func (a *CIAdapter) GetRunner(relPath string) (*task.Runner, bool) {
	return a.taskMgr.Runner(relPath)
}

// SetTaskMonitorBranch sets the CI monitor branch on a task entry.
func (a *CIAdapter) SetTaskMonitorBranch(entry ci.TaskEntry, branch string) {
	entry.SetMonitorBranch(branch)
}

// RepoInfoFor returns CI-level repo info for relPath.
func (a *CIAdapter) RepoInfoFor(relPath string) ci.RepoInfo {
	r, ok := a.infoForRepo(relPath)
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
func (a *CIAdapter) ListActiveRepos() []ci.RepoInfo {
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
	snap := a.repoReg.snapshot()
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
func (a *CIAdapter) SetRepoCIStatusIfChanged(relPath, sha string, result forgecache.Result) bool {
	checks := make([]forge.Check, len(result.Checks))
	copy(checks, result.Checks)
	next := ci.RepoCIState{Status: result.Status, Checks: checks, HeadSHA: sha}
	return a.repoReg.setCIStatusIfChanged(relPath, next)
}

// Prefs returns the user preferences store.
func (a *CIAdapter) Prefs() *preferences.Store { return a.prefs }

// repoInfoFor returns a copy of the RepoInfo for relPath. Callers needing a
// *RepoInfo (e.g. forgeForInfo) should take the address of the returned copy.
func (s *Server) repoInfoFor(relPath string) (RepoInfo, bool) {
	return s.repoReg.infoFor(relPath)
}
