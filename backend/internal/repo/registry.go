// Repository identity values managed by the checkout registry.

package repo

import "github.com/caic-xyz/caic/backend/internal/forge"

// Move reports a checkout path replacement for an existing repository.
type Move struct {
	OldRel string
	NewRel string
}

// Moved reports whether the checkout path changed.
func (m Move) Moved() bool {
	return m.OldRel != m.NewRel
}

// Repository is immutable metadata identifying a repository known to caic.
// A repository is separate from its live Checkout, which owns local git and
// runtime state.
type Repository struct {
	RelPath          string // e.g. "github/caic" — used as API ID.
	AbsPath          string
	BaseBranch       string
	BaseBranchRemote string     // Git remote name (e.g. "origin") used to determine BaseBranch.
	Remote           string     // Raw git remote URL (origin).
	ForgeKind        forge.Kind // empty if remote is not a recognized forge
	ForgeOwner       string     // empty if remote is not a recognized forge
	ForgeRepo        string     // empty if remote is not a recognized forge
}
