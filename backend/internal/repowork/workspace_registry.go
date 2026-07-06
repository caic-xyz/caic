// WorkspaceRegistry owns registered repo workspaces independently from repo identity.

package repowork

import (
	"context"
	"log/slog"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// WorkspaceRegistry maps repository relative paths to task workspaces.
//
// It is deliberately separate from Registry: repo identity and workspace
// lifetimes are coordinated by callers using the ordering documented on
// Registry, but each registry keeps its own lock domain.
type WorkspaceRegistry struct {
	serverCtx context.Context
	backend   runtime.Backend

	mu         sync.Mutex
	workspaces map[string]*RepoWorkspace
}

// NewWorkspaceRegistry creates an empty workspace registry.
func NewWorkspaceRegistry(ctx context.Context, backend runtime.Backend) *WorkspaceRegistry {
	return &WorkspaceRegistry{
		serverCtx:  ctx,
		backend:    backend,
		workspaces: make(map[string]*RepoWorkspace),
	}
}

// RegisterWorkspace registers a task repo workspace keyed by relPath.
// "" registers the no-repo workspace.
func (r *WorkspaceRegistry) RegisterWorkspace(relPath string, w *RepoWorkspace) {
	w.Runtime = r.backend
	if err := w.Init(r.serverCtx); err != nil {
		slog.WarnContext(r.serverCtx, "repo workspace init failed", "repo", relPath, "err", err)
	}
	r.mu.Lock()
	r.workspaces[relPath] = w
	r.mu.Unlock()
}

// Workspace returns the repo workspace for relPath, or nil.
func (r *WorkspaceRegistry) Workspace(relPath string) (*RepoWorkspace, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workspaces[relPath]
	return w, ok
}

// RangeWorkspaces iterates over every registered repo workspace. It snapshots
// the registry under lock and invokes fn unlocked, so callbacks may safely call
// back into code that also needs the workspace registry. The workspace set is a
// point-in-time snapshot. Stops iteration if fn returns false.
func (r *WorkspaceRegistry) RangeWorkspaces(fn func(relPath string, w *RepoWorkspace) bool) {
	r.mu.Lock()
	type kv struct {
		relPath string
		w       *RepoWorkspace
	}
	snap := make([]kv, 0, len(r.workspaces))
	for relPath, w := range r.workspaces {
		snap = append(snap, kv{relPath, w})
	}
	r.mu.Unlock()
	for _, it := range snap {
		if !fn(it.relPath, it.w) {
			return
		}
	}
}

// UnregisterWorkspace removes the repo workspace registered for relPath.
func (r *WorkspaceRegistry) UnregisterWorkspace(relPath string) {
	r.mu.Lock()
	delete(r.workspaces, relPath)
	r.mu.Unlock()
}
