// Registry owns known repositories and their current local checkouts.

package repo

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
)

// Registry owns known repositories and their current local checkouts.
type Registry struct {
	mu           sync.Mutex
	repositories map[string]*Repository
	checkouts    map[string]*Checkout
}

// NewRegistry creates an empty repository registry.
func NewRegistry() *Registry {
	return &Registry{
		repositories: make(map[string]*Repository),
		checkouts:    make(map[string]*Checkout),
	}
}

// RegisterRepository records a repository and returns its canonical identity.
func (r *Registry) RegisterRepository(repository Repository) *Repository {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registerRepositoryLocked(repository)
}

// RepositoryByForge returns the known repository whose forge name matches owner/repo.
func (r *Registry) RepositoryByForge(owner, name string) (*Repository, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, repository := range r.repositories {
		if strings.EqualFold(repository.ForgeOwner, owner) && strings.EqualFold(repository.ForgeRepo, name) {
			return repository, true
		}
	}
	return nil, false
}

// Repositories returns a snapshot of all known repositories.
func (r *Registry) Repositories() []*Repository {
	r.mu.Lock()
	defer r.mu.Unlock()
	repositories := make([]*Repository, 0, len(r.repositories))
	for _, repository := range r.repositories {
		repositories = append(repositories, repository)
	}
	slices.SortFunc(repositories, func(a, b *Repository) int {
		return strings.Compare(a.key(), b.key())
	})
	return repositories
}

// RegisterCheckout records a new local checkout.
//
// A checkout with a repository must point to the canonical value previously
// returned by RegisterRepository.
func (r *Registry) RegisterCheckout(checkout *Checkout) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if checkout.Repository != nil && r.repositories[checkout.Repository.key()] != checkout.Repository {
		return errors.New("checkout repository is not registered")
	}
	if _, ok := r.checkouts[checkout.RelPath]; ok {
		return fmt.Errorf("checkout already registered at %q", checkout.RelPath)
	}
	for _, registered := range r.checkouts {
		if registered.Dir == checkout.Dir {
			return fmt.Errorf("checkout directory already registered at %q", checkout.Dir)
		}
	}
	r.checkouts[checkout.RelPath] = checkout
	return nil
}

// UnregisterCheckout removes the current checkout at relPath from the registry.
// It does not remove files or the associated repository.
func (r *Registry) UnregisterCheckout(relPath string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.checkouts[relPath]; !ok {
		return false
	}
	delete(r.checkouts, relPath)
	return true
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
		slices.SortFunc(checkouts, func(a, b *Checkout) int {
			return strings.Compare(a.RelPath, b.RelPath)
		})
		for _, checkout := range checkouts {
			if !yield(checkout) {
				return
			}
		}
	}
}

func (r *Registry) registerRepositoryLocked(repository Repository) *Repository {
	key := repository.key()
	if existing, ok := r.repositories[key]; ok {
		return existing
	}
	r.repositories[key] = &repository
	return &repository
}
