// Repository identity values managed by the checkout registry.

package repo

import (
	"strings"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// Repository identifies a remote repository known to caic.
type Repository struct {
	// Immutable.
	Remote     string
	ForgeKind  forge.Kind
	ForgeOwner string
	ForgeRepo  string
}

func (r Repository) key() string {
	if r.ForgeKind != "" {
		return string(r.ForgeKind) + "\x00" + strings.ToLower(r.ForgeOwner) + "\x00" + strings.ToLower(r.ForgeRepo)
	}
	return r.Remote
}
