// Registry owns managed repository identity.

// Package reporeg owns managed repository identity.
package reporeg

import (
	"slices"
	"strings"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// RelPathMove reports a relpath replacement for an existing repo identity.
type RelPathMove struct {
	OldRel string
	NewRel string
}

// Moved reports whether the relpath changed.
func (m RelPathMove) Moved() bool {
	return m.OldRel != m.NewRel
}

// Registry is the single owner of the managed-repository set. All access goes
// through its methods, which lock internally and return copies — callers never
// hold references into the underlying slice, so a concurrent add/remove can
// never tear a reader or leave a dangling interior pointer.
//
// Ordering invariant with WorkspaceRegistry: a repo and its
// repowork.RepoWorkspace live in two separate lock domains (this registry and
// the WorkspaceRegistry). Callers that add a repo register its workspace *after*
// add(); callers that remove a repo unregister its workspace *after* the
// remove. This leaves a brief, benign window where a repo is listed without a
// workspace (just added) or a workspace outlives its repo entry (just removed):
// in-flight tasks resolve their workspace regardless, and newly-listed repos are
// not user-visible until the enclosing operation returns.
type Registry struct {
	mu    sync.Mutex
	repos []Info
}

// New creates a registry seeded with initial repos.
func New(initial []Info) *Registry {
	return &Registry{repos: initial}
}

// InfoFor returns a copy of the Info for rel.
func (r *Registry) InfoFor(rel string) (Info, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.repos {
		if r.repos[i].RelPath == rel {
			return r.repos[i], true
		}
	}
	return Info{}, false
}

// ByForge returns a copy of the Info whose forge matches owner/repo
// (case-insensitive).
func (r *Registry) ByForge(owner, repo string) (Info, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.repos {
		if strings.EqualFold(r.repos[i].ForgeOwner, owner) && strings.EqualFold(r.repos[i].ForgeRepo, repo) {
			return r.repos[i], true
		}
	}
	return Info{}, false
}

// Snapshot returns a copy of all registered repos.
func (r *Registry) Snapshot() []Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.repos)
}

// Add inserts or replaces a copy of info. RelPath and AbsPath are both stable
// identities for a repo, so adding either identity twice is idempotent. The
// caller registers the repowork.RepoWorkspace afterwards (see the ordering
// invariant on Registry). It returns any relpath replacement for external
// per-repo caches to migrate explicitly.
func (r *Registry) Add(info *Info) RelPathMove {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.repos {
		if r.repos[i].RelPath != info.RelPath && r.repos[i].AbsPath != info.AbsPath {
			continue
		}
		oldRel := r.repos[i].RelPath
		r.repos[i] = *info
		return RelPathMove{OldRel: oldRel, NewRel: info.RelPath}
	}
	r.repos = append(r.repos, *info)
	return RelPathMove{}
}

// RemoveMatching removes every repo for which pred reports true and returns
// their RelPaths. The caller unregisters the corresponding workspaces afterwards.
func (r *Registry) RemoveMatching(pred func(Info) bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed []string
	r.repos = slices.DeleteFunc(r.repos, func(ri Info) bool {
		if pred(ri) {
			removed = append(removed, ri.RelPath)
			return true
		}
		return false
	})
	return removed
}

// Info describes a repository managed by caic.
type Info struct {
	RelPath          string // e.g. "github/caic" — used as API ID.
	AbsPath          string
	BaseBranch       string
	BaseBranchRemote string     // Git remote name (e.g. "origin") used to determine BaseBranch.
	Remote           string     // Raw git remote URL (origin).
	ForgeKind        forge.Kind // empty if remote is not a recognized forge
	ForgeOwner       string     // empty if remote is not a recognized forge
	ForgeRepo        string     // empty if remote is not a recognized forge
}
