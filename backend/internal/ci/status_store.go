// Repository CI status cache and matching helpers.

package ci

import (
	"sync"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
)

// RepoCIState is an in-memory CI status snapshot for a repository's default branch.
type RepoCIState struct {
	Status  forge.CIStatus
	Checks  []forge.Check
	HeadSHA string
}

// RepoRef identifies the repository fields needed to match cached CI status.
type RepoRef struct {
	RelPath    string
	ForgeOwner string
	ForgeRepo  string
}

// RepoStatusStore owns in-memory default-branch CI status snapshots for repos.
type RepoStatusStore struct {
	mu      sync.RWMutex
	status  map[string]RepoCIState
	changed chan struct{}
}

// NewRepoStatusStore returns an empty CI status store.
func NewRepoStatusStore() *RepoStatusStore {
	return &RepoStatusStore{status: map[string]RepoCIState{}, changed: make(chan struct{})}
}

// Changed returns a channel closed when a stored repo status changes.
func (s *RepoStatusStore) Changed() <-chan struct{} {
	s.mu.RLock()
	ch := s.changed
	s.mu.RUnlock()
	return ch
}

// StatusFor returns a copy of the cached CI status for rel.
func (s *RepoStatusStore) StatusFor(rel string) (RepoCIState, bool) {
	s.mu.RLock()
	st, ok := s.status[rel]
	s.mu.RUnlock()
	if !ok {
		return RepoCIState{}, false
	}
	st.Checks = append([]forge.Check(nil), st.Checks...)
	return st, true
}

// SetResultIfChanged stores result for rel and reports whether the status changed.
func (s *RepoStatusStore) SetResultIfChanged(rel, sha string, result forgecache.Result) bool {
	next := RepoCIState{Status: result.Status, Checks: append([]forge.Check(nil), result.Checks...), HeadSHA: sha}

	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.status[rel]
	s.status[rel] = next
	if prev.Status == next.Status {
		return false
	}
	s.notifyChangedLocked()
	return true
}

// Move migrates cached status from oldRel to newRel when a repo relpath changes.
func (s *RepoStatusStore) Move(oldRel, newRel string) {
	if oldRel == newRel {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.status[oldRel]
	if !ok {
		return
	}
	delete(s.status, oldRel)
	s.status[newRel] = st
	s.notifyChangedLocked()
}

// PathsAtSHA returns repo paths whose cached default-branch status has sha.
func (s *RepoStatusStore) PathsAtSHA(repos []RepoRef, owner, repo, sha string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, info := range repos {
		if info.ForgeOwner != owner || info.ForgeRepo != repo {
			continue
		}
		st, ok := s.status[info.RelPath]
		if ok && st.HeadSHA == sha {
			out = append(out, info.RelPath)
		}
	}
	return out
}

func (s *RepoStatusStore) notifyChangedLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}
