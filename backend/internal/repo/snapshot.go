// Git snapshots carry process-local read order for safe deferred publication.

package repo

import (
	"sync/atomic"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// gitReadSequence spans checkouts so replacing a Checkout cannot reuse an order
// already applied by a task. Neither the sequence nor cache stamps persist.
var gitReadSequence atomic.Uint64

// GitRead identifies the runtime and completion order of a Git snapshot.
// Treat it as immutable. The zero value does not identify a completed read.
type GitRead struct {
	InstanceID runtime.ID
	sequence   uint64
}

// NewGitRead stamps a completed probe or an authoritative local cache update.
// A Checkout stamps probes under branchMu after reading; a Task stamps local
// updates under its own lock so already completed probes cannot overwrite them.
func NewGitRead(id runtime.ID) GitRead {
	return GitRead{InstanceID: id, sequence: gitReadSequence.Add(1)}
}

// NewerThan reports whether this read completed after the previously applied
// read. A zero read is never newer, including when the previous read is zero.
func (r GitRead) NewerThan(previous GitRead) bool {
	return r.sequence > previous.sequence
}

// GitSnapshot contains owned, immutable data from one full, compact, or numstat
// probe. Full probes include Statuses; compact and full probes include RepoStates.
// Partial probe failures can return populated data together with an error.
type GitSnapshot struct {
	Read       GitRead
	DiffStat   agent.DiffStat
	RepoStates []agent.RepoState
	Statuses   []runtime.RepositoryStatus
}
