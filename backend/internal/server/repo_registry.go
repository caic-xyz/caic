// repoRegistry owns the set of managed repositories and their cached CI status.

package server

import (
	"slices"
	"strings"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/ci"
)

// repoRegistry is the single owner of the managed-repository set and each
// repo's cached CI status. All access goes through its methods, which lock
// internally and return copies — callers never hold references into the
// underlying slice, so a concurrent add/remove can never tear a reader or
// leave a dangling interior pointer.
//
// Ordering invariant with the Manager runner registry: a repo and its
// task.Runner live in two separate lock domains (this registry and the
// Manager's). Callers that add a repo register its runner *after* add(); callers
// that remove a repo unregister its runner *after* the remove. This leaves a
// brief, benign window where a repo is listed without a runner (just added) or a
// runner outlives its repo entry (just removed): in-flight tasks resolve their
// runner regardless, and newly-listed repos are not user-visible until the
// enclosing operation returns.
type repoRegistry struct {
	mu       sync.Mutex
	repos    []repoInfo
	ciStatus map[string]ci.RepoCIState // keyed by repoInfo.RelPath
}

// newRepoRegistry creates a registry seeded with initial (taken over verbatim).
func newRepoRegistry(initial []repoInfo) *repoRegistry {
	return &repoRegistry{repos: initial, ciStatus: make(map[string]ci.RepoCIState)}
}

// infoFor returns a copy of the repoInfo for rel.
func (r *repoRegistry) infoFor(rel string) (repoInfo, bool) {
	if r == nil {
		return repoInfo{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.repos {
		if r.repos[i].RelPath == rel {
			return r.repos[i], true
		}
	}
	return repoInfo{}, false
}

// byForge returns a copy of the repoInfo whose forge matches owner/repo
// (case-insensitive).
func (r *repoRegistry) byForge(owner, repo string) (repoInfo, bool) {
	if r == nil {
		return repoInfo{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.repos {
		if strings.EqualFold(r.repos[i].ForgeOwner, owner) && strings.EqualFold(r.repos[i].ForgeRepo, repo) {
			return r.repos[i], true
		}
	}
	return repoInfo{}, false
}

// snapshot returns a copy of all registered repos.
func (r *repoRegistry) snapshot() []repoInfo {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.repos)
}

// repoWithCI pairs a repo with its cached CI status snapshot.
type repoWithCI struct {
	info  repoInfo
	ci    ci.RepoCIState
	hasCI bool
}

// snapshotWithCI returns each repo paired with its CI status, captured
// atomically under a single lock acquisition.
func (r *repoRegistry) snapshotWithCI() []repoWithCI {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]repoWithCI, len(r.repos))
	for i := range r.repos {
		st, ok := r.ciStatus[r.repos[i].RelPath]
		out[i] = repoWithCI{info: r.repos[i], ci: st, hasCI: ok}
	}
	return out
}

// forgePathsAtSHA returns the RelPaths of repos matching owner/repo whose
// cached CI HeadSHA equals sha.
func (r *repoRegistry) forgePathsAtSHA(owner, repo, sha string) []string {
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

// add appends a copy of info. The caller registers the task.Runner afterwards
// (see the ordering invariant on repoRegistry).
func (r *repoRegistry) add(info *repoInfo) {
	r.mu.Lock()
	r.repos = append(r.repos, *info)
	r.mu.Unlock()
}

// removeMatching removes every repo for which pred reports true and returns
// their RelPaths. The caller unregisters the corresponding runners afterwards.
func (r *repoRegistry) removeMatching(pred func(repoInfo) bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var removed []string
	r.repos = slices.DeleteFunc(r.repos, func(ri repoInfo) bool {
		if pred(ri) {
			removed = append(removed, ri.RelPath)
			return true
		}
		return false
	})
	return removed
}

// absPathSet returns the set of absolute paths currently registered.
func (r *repoRegistry) absPathSet() map[string]struct{} {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]struct{}, len(r.repos))
	for i := range r.repos {
		out[r.repos[i].AbsPath] = struct{}{}
	}
	return out
}

// ciStatusFor returns the cached CI status for rel, or the zero value (Status
// == "") if none is recorded.
func (r *repoRegistry) ciStatusFor(rel string) ci.RepoCIState {
	if r == nil {
		return ci.RepoCIState{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ciStatus[rel]
}

// setCIStatusIfChanged stores next as the CI status for rel and reports whether
// the status field changed (so SSE subscribers can be notified).
func (r *repoRegistry) setCIStatusIfChanged(rel string, next ci.RepoCIState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.ciStatus[rel]
	r.ciStatus[rel] = next
	return prev.Status != next.Status
}
