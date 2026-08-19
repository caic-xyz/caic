// Task-log data types persist task state, results, and repository mounts.

package taskslog

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
type State string //nolint:recvcheck // UnmarshalJSON requires a pointer receiver; Validate/String/IsTerminal use value receivers so they work on the untyped State constants

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

// Result holds the outcome of a completed task.
//
// Is serialized as task metadata to disk. Is not used for HTTP wire protocol.
type Result struct {
	State       State          `json:"state"`
	DiffStat    agent.DiffStat `json:"diff_stat"`
	CostUSD     float64        `json:"cost_usd"`
	Duration    time.Duration  `json:"duration"`
	NumTurns    int            `json:"num_turns"`
	Usage       agent.Usage    `json:"usage"`
	AgentResult string         `json:"agent_result"`
	Err         error          `json:"-"`
}

type persistedResult Result

// MarshalJSON preserves Result's error text in rebuildable task metadata.
func (r *Result) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	errText := ""
	if r.Err != nil {
		errText = r.Err.Error()
	}
	return json.Marshal(struct {
		persistedResult

		Error string `json:"error,omitempty"`
	}{persistedResult: persistedResult(*r), Error: errText})
}

// UnmarshalJSON restores Result's persisted error text from task metadata.
func (r *Result) UnmarshalJSON(data []byte) error {
	var decoded struct {
		persistedResult

		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = Result(decoded.persistedResult)
	if decoded.Error != "" {
		r.Err = errors.New(decoded.Error)
	}
	return nil
}

// RepoMount describes one repository in a task.
//
// Is serialized as task metadata to disk. Is not used for HTTP wire protocol.
type RepoMount struct {
	Name          string `json:"name"`           // relative path, e.g. "github/caic"
	BaseBranch    string `json:"base_branch"`    // branch to fork from; empty = checkout default
	Branch        string `json:"branch"`         // allocated branch, e.g. "caic-0"
	GitRoot       string `json:"git_root"`       // absolute host path; empty in purged-task entries
	ContainerPath string `json:"container_path"` // path inside the runtime instance
}

// ToRuntimeRepo converts a RepoMount to a runtime Repo.
func (r *RepoMount) ToRuntimeRepo() runtime.Repo {
	return runtime.Repo{GitRoot: r.GitRoot, ContainerPath: r.ContainerPath, Branch: r.Branch, BaseBranch: r.BaseBranch}
}

// RepoMountFromMeta converts a log metadata repository to a RepoMount.
func RepoMountFromMeta(m agent.MetaRepo, gitRoot string) RepoMount {
	return RepoMount{Name: m.Name, BaseBranch: m.BaseBranch, Branch: m.Branch, ContainerPath: m.ContainerPath, GitRoot: gitRoot}
}

func runtimeCacheMountsFromMeta(in []agent.MetaCacheMount) []runtime.CacheMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.CacheMount, len(in))
	for i, m := range in {
		out[i] = runtime.CacheMount{Name: m.Name, Description: m.Description, HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly, Shallow: m.Shallow}
	}
	return out
}

func runtimeMountsFromMeta(in []agent.MetaMount) []runtime.Mount {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.Mount, len(in))
	for i, m := range in {
		out[i] = runtime.Mount{HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly}
	}
	return out
}
