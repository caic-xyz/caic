// Git targets and ordered snapshots preserve repository summaries across partial failures.

package repo

import (
	"slices"
	"strings"
	"sync/atomic"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

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

// GitTarget captures a runtime instance and its repository mapping together.
// Task constructs it under its lock; query callers must treat it as immutable.
type GitTarget struct {
	InstanceID runtime.ID
	Repos      []runtime.Repo
}

// GitSnapshot contains owned, immutable data from one full, compact, or numstat
// probe. Full probes include Statuses; compact and full probes include RepoStates.
// Partial probe failures can return populated data together with an error.
type GitSnapshot struct {
	Read        GitRead
	Target      GitTarget
	FailedRepos []int // Repositories whose previous summary must be retained and marked stale.
	DiffStat    v3.DiffStat
	RepoStates  []v3.RepoState
	Statuses    []runtime.RepositoryStatus
}

// Summary retains the last known files and state for failed repositories.
// Successful repositories replace their data, including newly clean states.
func (s *GitSnapshot) Summary(previous v3.DiffStat, states []v3.RepoState) (v3.DiffStat, []v3.RepoState) {
	if len(s.FailedRepos) == 0 {
		return s.DiffStat, s.RepoStates
	}
	var stats v3.DiffStat
	var merged []v3.RepoState
	for i := range s.Target.Repos {
		r := &s.Target.Repos[i]
		prefix := ""
		if len(s.Target.Repos) > 1 {
			prefix = diffRepoPrefix(r) + "/"
		}
		failed := slices.Contains(s.FailedRepos, i)
		source := s.DiffStat
		if failed {
			source = previous
		}
		start := len(stats)
		for _, f := range source {
			if prefix == "" || strings.HasPrefix(f.Path, prefix) {
				stats = append(stats, f)
			}
		}
		state := v3.RepoState{RepoIndex: i, Branch: r.Branch}
		candidates := s.RepoStates
		if failed || len(s.RepoStates) == 0 {
			candidates = states
		}
		for _, old := range candidates {
			if old.RepoIndex == i {
				state = old
				break
			}
		}
		if !failed && len(s.RepoStates) == 0 {
			state.ChangedFiles = len(stats) - start
			state.LinesAdded = 0
			state.LinesDeleted = 0
			for _, f := range stats[start:] {
				state.LinesAdded += f.LinesAdded
				state.LinesDeleted += f.LinesDeleted
			}
		}
		state.Stale = failed || state.Stale && len(s.RepoStates) == 0
		merged = append(merged, state)
	}
	return stats, merged
}
