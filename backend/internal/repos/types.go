// Repository domain types.

package repos

import "github.com/caic-xyz/caic/backend/internal/forge"

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
