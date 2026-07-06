// Registry owns registered repo workspaces independently from repo identity.

package repowork

import (
	"context"
	"log/slog"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// Registry maps repository relative paths to task workspaces.
//
// It is deliberately separate from repo.Registry: repo identity and workspace
// lifetimes are coordinated by callers using the ordering documented on
// repo.Registry, but each registry keeps its own lock domain.
type Registry struct {
	serverCtx context.Context
	backend   runtime.Backend

	mu         sync.Mutex
	workspaces map[string]*Workspace
}

// NewRegistry creates an empty workspace registry.
func NewRegistry(ctx context.Context, backend runtime.Backend) *Registry {
	return &Registry{
		serverCtx:  ctx,
		backend:    backend,
		workspaces: make(map[string]*Workspace),
	}
}

// RegisterWorkspace registers a task repo workspace keyed by relPath.
// "" registers the no-repo workspace.
func (r *Registry) RegisterWorkspace(relPath string, w *Workspace) {
	w.Runtime = r.backend
	if err := w.Init(r.serverCtx); err != nil {
		slog.WarnContext(r.serverCtx, "repo workspace init failed", "repo", relPath, "err", err)
	}
	r.mu.Lock()
	r.workspaces[relPath] = w
	r.mu.Unlock()
}

// Workspace returns the repo workspace for relPath, or nil.
func (r *Registry) Workspace(relPath string) (*Workspace, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workspaces[relPath]
	return w, ok
}

// RangeWorkspaces iterates over every registered repo workspace. It snapshots
// the registry under lock and invokes fn unlocked, so callbacks may safely call
// back into code that also needs the workspace registry. The workspace set is a
// point-in-time snapshot. Stops iteration if fn returns false.
func (r *Registry) RangeWorkspaces(fn func(relPath string, w *Workspace) bool) {
	r.mu.Lock()
	type kv struct {
		relPath string
		w       *Workspace
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
func (r *Registry) UnregisterWorkspace(relPath string) {
	r.mu.Lock()
	delete(r.workspaces, relPath)
	r.mu.Unlock()
}
