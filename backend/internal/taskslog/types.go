// Task lifecycle states, completion results, and repository mounts.

package taskslog

import (
	"encoding/json"
	"fmt"
	"time"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// State represents the lifecycle state of a task. The persisted and API
// representation is the string value itself, so a corrupt or forward-versioned
// value round-trips instead of aliasing an unrelated state.
//
// Transitions are written by message processing, lifecycle operations, and
// recovery. A few transitions are reachable from more than one source, so task
// code uses compare-and-swap helpers to keep them race-safe.
type State string

// Validate rejects unrecognized task states.
func (s State) Validate() error {
	switch s {
	case StatePending, StateBranching, StateProvisioning, StateStarting, StateRunning,
		StateWaiting, StateAsking, StateHasPlan, StatePulling, StatePushing,
		StateStopping, StateStopped, StatePurging, StateCrashed, StateFailed, StatePurged:
		return nil
	default:
		return fmt.Errorf("unsupported task state %q", string(s))
	}
}

// UnmarshalJSON rejects an unrecognized task state instead of decoding it silently.
func (s *State) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = State(raw)
	return s.Validate()
}

// String returns the API and log representation of the task state.
func (s State) String() string {
	if s.Validate() != nil {
		return "unknown"
	}
	return string(s)
}

// IsTerminal reports whether the task cannot be revived.
func (s State) IsTerminal() bool { return s == StateFailed || s == StatePurged }

// Task lifecycle states.
const (
	StatePending      State = "pending"
	StateBranching    State = "branching"
	StateProvisioning State = "provisioning"
	StateStarting     State = "starting"
	StateRunning      State = "running"
	StateWaiting      State = "waiting"
	StateAsking       State = "asking"
	StateHasPlan      State = "has_plan"
	StatePulling      State = "pulling"
	StatePushing      State = "pushing"
	StateStopping     State = "stopping"
	StateStopped      State = "stopped"
	StatePurging      State = "purging"
	StateCrashed      State = "crashed"
	StateFailed       State = "failed"
	StatePurged       State = "purged"
)

// Result holds the bounded durable outcome of a completed task. Completed-task
// restoration should read this summary without decoding message bodies; logs
// that predate required fields may fall back to a full history fold.
//
// Persistence uses explicit projections to versioned data schemas.
type Result struct {
	State    State
	DiffStat v3.DiffStat
	// DiskUsedBytes is the final measured writable-layer size. Nil means the
	// runtime could not provide a measurement.
	DiskUsedBytes  *int64
	CostUSD        float64
	Duration       time.Duration
	NumTurns       int
	Usage          agent.Usage
	AgentResult    string
	StartupFailure *v3.StartupFailure
	Err            error
}

// RepoMount describes one repository in a task.
//
// Persistence uses explicit projections to versioned data schemas.
type RepoMount struct {
	Name          string // relative path, e.g. "github/caic"
	BaseBranch    string // branch to fork from; empty = checkout default
	Branch        string // allocated branch, e.g. "caic-0"
	GitRoot       string // absolute host path; empty in purged-task entries
	ContainerPath string // path inside the runtime instance
}

// RepoMountFromMeta converts a log metadata repository to a RepoMount.
func RepoMountFromMeta(m v3.MetaRepo, gitRoot string) RepoMount {
	return RepoMount{Name: m.Name, BaseBranch: m.BaseBranch, Branch: m.Branch, ContainerPath: m.ContainerPath, GitRoot: gitRoot}
}

// ToRuntimeRepo converts a RepoMount to a runtime Repo.
func (r *RepoMount) ToRuntimeRepo() runtime.Repo {
	return runtime.Repo{GitRoot: r.GitRoot, ContainerPath: r.ContainerPath, Branch: r.Branch, BaseBranch: r.BaseBranch}
}

func runtimeCacheMountsFromMeta(in []v3.MetaCacheMount) []runtime.CacheMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.CacheMount, len(in))
	for i, m := range in {
		out[i] = runtime.CacheMount{Name: m.Name, Description: m.Description, HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly, Shallow: m.Shallow}
	}
	return out
}

func runtimeMountsFromMeta(in []v3.MetaMount) []runtime.Mount {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.Mount, len(in))
	for i, m := range in {
		out[i] = runtime.Mount{HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly}
	}
	return out
}
