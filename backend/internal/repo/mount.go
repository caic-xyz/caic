// Repository mounts connect task branches to host checkouts and runtime paths.

package repo

import "github.com/caic-xyz/caic/backend/internal/runtime"

// Mount describes one repository in a task.
//
// Persistence uses explicit projections to versioned data schemas.
type Mount struct {
	Name          string // relative path, e.g. "github/caic"
	BaseBranch    string // branch to fork from; empty = checkout default
	Branch        string // allocated branch, e.g. "caic-0"
	GitRoot       string // absolute host path; empty in purged-task entries
	ContainerPath string // path inside the runtime instance
}

// ToRuntimeRepo converts a Mount to a runtime Repo.
func (r *Mount) ToRuntimeRepo() runtime.Repo {
	return runtime.Repo{GitRoot: r.GitRoot, ContainerPath: r.ContainerPath, Branch: r.Branch, BaseBranch: r.BaseBranch}
}
