// Registry owns known repositories and their current local checkouts.

package repo

import (
	"iter"
	"slices"
	"strings"
	"sync"
)

// Registry owns the known repository set and the current checkout for each
// repository. Registration updates both collections while holding one lock, so
// callers cannot observe a known repository without its current checkout.
type Registry struct {
	mu           sync.Mutex
	repositories []Repository
	checkouts    map[string]*Checkout
}

// NewRegistry creates an empty repository registry.
func NewRegistry() *Registry {
	return &Registry{checkouts: make(map[string]*Checkout)}
}

// Repository returns immutable metadata for the checkout at relPath.
func (r *Registry) Repository(relPath string) (Repository, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.repositories {
		if r.repositories[i].RelPath == relPath {
			return r.repositories[i], true
		}
	}
	return Repository{}, false
}

// RepositoryByForge returns known metadata whose forge name matches owner/repo.
func (r *Registry) RepositoryByForge(owner, name string) (Repository, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.repositories {
		repo := r.repositories[i]
		if strings.EqualFold(repo.ForgeOwner, owner) && strings.EqualFold(repo.ForgeRepo, name) {
			return repo, true
		}
	}
	return Repository{}, false
}

// Repositories returns a snapshot of all known repositories.
func (r *Registry) Repositories() []Repository {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.repositories)
}

// Register adds or replaces a known repository and its current checkout.
func (r *Registry) Register(repository *Repository, checkout *Checkout) Move {
	checkout.RepoName = repository.RelPath
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.repositories {
		if r.repositories[i].RelPath != repository.RelPath && r.repositories[i].AbsPath != repository.AbsPath {
			continue
		}
		oldRel := r.repositories[i].RelPath
		if oldRel != repository.RelPath {
			delete(r.checkouts, oldRel)
		}
		r.repositories[i] = *repository
		r.checkouts[repository.RelPath] = checkout
		return Move{OldRel: oldRel, NewRel: repository.RelPath}
	}
	r.repositories = append(r.repositories, *repository)
	r.checkouts[repository.RelPath] = checkout
	return Move{}
}

// RegisterCheckout registers checkout-only state for tests. Managed
// repositories must use Register so identity and checkout state change together.
func (r *Registry) RegisterCheckout(relPath string, checkout *Checkout) {
	checkout.RepoName = relPath
	r.mu.Lock()
	r.checkouts[relPath] = checkout
	r.mu.Unlock()
}

// Remove removes the known repository and current checkout at relPath.
func (r *Registry) Remove(relPath string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.repositories {
		if r.repositories[i].RelPath != relPath {
			continue
		}
		r.repositories = slices.Delete(r.repositories, i, i+1)
		delete(r.checkouts, relPath)
		return true
	}
	return false
}

// UnregisterCheckout removes checkout-only state.
func (r *Registry) UnregisterCheckout(relPath string) {
	r.mu.Lock()
	delete(r.checkouts, relPath)
	r.mu.Unlock()
}

// Checkout returns the current checkout for relPath.
func (r *Registry) Checkout(relPath string) (*Checkout, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	checkout, ok := r.checkouts[relPath]
	return checkout, ok
}

// Checkouts returns a snapshot sequence of current checkouts. Iteration does
// not hold the registry lock.
func (r *Registry) Checkouts() iter.Seq[*Checkout] {
	return func(yield func(*Checkout) bool) {
		r.mu.Lock()
		checkouts := make([]*Checkout, 0, len(r.checkouts))
		for _, checkout := range r.checkouts {
			checkouts = append(checkouts, checkout)
		}
		r.mu.Unlock()
		for _, checkout := range checkouts {
			if !yield(checkout) {
				return
			}
		}
	}
}
