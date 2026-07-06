// Registry owns managed repository identity and cached CI status.

package repos

import (
	"slices"
	"strings"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/ci"
)

// Registry is the single owner of the managed-repository set and each repo's
// cached CI status. All access goes through its methods, which lock internally
// and return copies — callers never hold references into the underlying slice,
// so a concurrent add/remove can never tear a reader or leave a dangling
// interior pointer.
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
	mu       sync.Mutex
	repos    []Info
	ciStatus map[string]ci.RepoCIState // keyed by Info.RelPath
}

// InfoWithCI pairs a repo with its cached CI status snapshot.
type InfoWithCI struct {
	Info  Info
	CI    ci.RepoCIState
	HasCI bool
}

// NewRegistry creates a registry seeded with initial repos.
func NewRegistry(initial []Info) *Registry {
	return &Registry{repos: initial, ciStatus: make(map[string]ci.RepoCIState)}
}

// InfoFor returns a copy of the Info for rel.
func (r *Registry) InfoFor(rel string) (Info, bool) {
	if r == nil {
		return Info{}, false
	}
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
	if r == nil {
		return Info{}, false
	}
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
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.repos)
}

// SnapshotWithCI returns each repo paired with its CI status, captured
// atomically under a single lock acquisition.
func (r *Registry) SnapshotWithCI() []InfoWithCI {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]InfoWithCI, len(r.repos))
	for i := range r.repos {
		st, ok := r.ciStatus[r.repos[i].RelPath]
		out[i] = InfoWithCI{Info: r.repos[i], CI: st, HasCI: ok}
	}
	return out
}

// ForgePathsAtSHA returns the RelPaths of repos matching owner/repo whose
// cached CI HeadSHA equals sha.
func (r *Registry) ForgePathsAtSHA(owner, repo, sha string) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for i := range r.repos {
		info := &r.repos[i]
		if info.ForgeOwner == owner && info.ForgeRepo == repo && r.ciStatus[info.RelPath].HeadSHA == sha {
			out = append(out, info.RelPath)
		}
	}
	return out
}

// Add inserts or replaces a copy of info. RelPath and AbsPath are both stable
// identities for a repo, so adding either identity twice is idempotent. The
// caller registers the repowork.RepoWorkspace afterwards (see the ordering
// invariant on Registry).
func (r *Registry) Add(info *Info) {
	r.mu.Lock()
	for i := range r.repos {
		if r.repos[i].RelPath != info.RelPath && r.repos[i].AbsPath != info.AbsPath {
			continue
		}
		oldRel := r.repos[i].RelPath
		r.repos[i] = *info
		if oldRel != info.RelPath {
			if st, ok := r.ciStatus[oldRel]; ok {
				r.ciStatus[info.RelPath] = st
				delete(r.ciStatus, oldRel)
			}
		}
		r.mu.Unlock()
		return
	}
	r.repos = append(r.repos, *info)
	r.mu.Unlock()
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

// CIStatusFor returns the cached CI status for rel, or the zero value (Status
// == "") if none is recorded.
func (r *Registry) CIStatusFor(rel string) ci.RepoCIState {
	if r == nil {
		return ci.RepoCIState{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ciStatus[rel]
}

// SetCIStatusIfChanged stores next as the CI status for rel and reports whether
// the status field changed (so SSE subscribers can be notified).
func (r *Registry) SetCIStatusIfChanged(rel string, next ci.RepoCIState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.ciStatus[rel]
	r.ciStatus[rel] = next
	return prev.Status != next.Status
}
