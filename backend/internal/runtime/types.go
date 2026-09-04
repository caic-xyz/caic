// Package runtime defines caic-owned task execution runtime interfaces and types.
package runtime

import (
	"context"
	"io"
	"iter"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// MetadataKey identifies a caic runtime metadata field.
type MetadataKey string

// Runtime metadata keys used to recognize and restore caic-managed instances.
const (
	MetadataTaskID            MetadataKey = "caic.id"
	MetadataLegacyTaskID      MetadataKey = "caic"
	MetadataHarness           MetadataKey = "caic.harness"
	MetadataLegacyHarness     MetadataKey = "harness"
	MetadataGitHubToken       MetadataKey = "caic.githubToken" //nolint:gosec // Metadata key for a boolean flag, not a credential.
	MetadataModelRefresh      MetadataKey = "caic.modelRefresh"
	MetadataDisplayCapability MetadataKey = "md.display"
	MetadataSmokeRun          MetadataKey = "caic.smoke_run"
)

// Metadata stores runtime-neutral metadata for an instance.
//
// Runtime adapters choose the backing store. mdruntime uses container labels;
// other adapters can use disk metadata, cloud tags, or a local registry.
type Metadata map[MetadataKey]string

// EventFilter selects runtime lifecycle events.
type EventFilter struct {
	MetadataKey MetadataKey
}

// Name identifies a runtime backend, such as docker or podman.
type Name string

// ID identifies a runtime instance across runtime backends.
//
// It combines a runtime Name with a backend-local instance ID, encoded as
// "name:instance". Runtime systems receive and return the qualified form so
// different runtimes can own colliding backend-local instance IDs.
type ID string

// InstanceID identifies a backend-local runtime allocation.
//
// It is used only inside concrete runtime adapters when calling their backing
// runtime APIs. Application-facing runtime interfaces use qualified ID values.
type InstanceID string

// NewID returns a qualified ID from a runtime name and backend-local instance ID.
func NewID(runtimeName Name, instanceID InstanceID) ID {
	if instanceID == "" || runtimeName == "" {
		return ID(instanceID)
	}
	return ID(string(runtimeName) + ":" + string(instanceID))
}

// RuntimeName returns the runtime name part of the qualified ID.
//
// An empty return means the ID is unqualified and invalid outside a concrete
// runtime backend implementation.
func (id ID) RuntimeName() Name {
	runtimeName, _, ok := strings.Cut(string(id), ":")
	if !ok {
		return ""
	}
	return Name(runtimeName)
}

// InstanceID returns the backend-local instance ID part of the qualified ID.
func (id ID) InstanceID() InstanceID {
	_, instanceID, ok := strings.Cut(string(id), ":")
	if !ok {
		return InstanceID(id)
	}
	return InstanceID(instanceID)
}

// ConnectionTarget describes how agent relay operations reach a runtime.
//
// It is currently SSH-shaped because mdruntime is the only production adapter.
// Non-SSH adapters should replace direct agent SSH/file-copy operations with a
// runtime-owned execution and transfer contract instead of leaking adapter
// details into task orchestration.
type ConnectionTarget struct {
	SSHHost string
}

// Repo describes a git repository available to a runtime instance.
type Repo struct {
	GitRoot       string
	ContainerPath string
	Branch        string
	BaseBranch    string
	Remote        string
}

// RepositoryStatus describes the checked-out branch and its changes relative
// to the host checkout's original tracking branch.
type RepositoryStatus struct {
	Branch      string
	Upstream    string
	Ahead       int
	Behind      int
	Commits     []GitCommit
	Uncommitted []GitFileStatus
}

// GitCommit describes one commit that is ahead of the comparison branch.
type GitCommit struct {
	SHA          string
	Subject      string
	Decorations  string
	AuthoredDate string
	Stat         []GitFileStat
}

// GitFileStat describes one path's changes in a commit.
type GitFileStat struct {
	Path    string
	Added   int
	Deleted int
	Binary  bool
}

// GitFileStatus describes one uncommitted path from git status porcelain v2.
type GitFileStatus struct {
	Path           string
	OriginalPath   string
	IndexStatus    string
	WorktreeStatus string
}

// CacheMount describes a host cache directory made available to a runtime.
//
// Is serialized as task metadata to disk. Is not used for HTTP wire protocol.
type CacheMount struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"` // Resolved target path in the runtime container.
	ReadOnly      bool   `json:"read_only"`
	Shallow       bool   `json:"shallow"`
}

// Mount describes a host directory bind-mounted into a runtime.
//
// Is serialized as task metadata to disk. Is not used for HTTP wire protocol.
type Mount struct {
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"` // Resolved target path in the runtime container.
	ReadOnly      bool   `json:"read_only"`
}

// ConnectionInfo describes connection details returned by a runtime instance.
type ConnectionInfo struct {
	AgentTarget      ConnectionTarget
	TailscaleFQDN    string
	TailscaleAuthURL string
}

// Instance describes a known runtime instance.
type Instance struct {
	ID            ID
	AgentTarget   ConnectionTarget
	State         string
	Repos         []Repo
	Tailscale     bool
	TailscaleFQDN string
	USB           bool
	Display       bool
	Sudo          bool
	VNCPort       int
}

// InstanceInspect describes observed runtime configuration for an instance.
type InstanceInspect struct {
	Runtime         string
	ID              ID
	State           string
	ImageRef        string
	ImageID         string
	OS              string
	CPUArchitecture string
	CPULimit        int
	Mounts          []Mount
	Caches          []CacheMount
}

// ProcessInfo describes a single process running inside a runtime instance.
type ProcessInfo struct {
	// PID is the process ID.
	PID int
	// PPID is the parent process ID.
	PPID int
	// PGRP is the process group ID.
	PGRP int
	// User is the effective user name.
	User string
	// State is the process state and modifiers reported by the runtime.
	State string
	// Priority is the kernel scheduling priority.
	Priority int
	// Nice is the process niceness value.
	Nice int
	// Threads is the number of threads in the process.
	Threads int
	// CPU is the percentage of CPU capacity used at the time of inspection.
	CPU float64
	// Mem is the percentage of physical memory used at the time of inspection.
	Mem float64
	// RSSBytes is the resident set size in bytes.
	RSSBytes uint64
	// CPUTime is the cumulative user and system CPU time consumed by the process.
	CPUTime time.Duration
	// StartedAt is when the process started.
	StartedAt time.Time
	// Command is the full command line reported by the runtime.
	Command string
}

// Stats is a snapshot of runtime resource usage.
type Stats struct {
	Ts         time.Time
	CPUPerc    float64
	MemUsed    uint64
	MemLimit   uint64
	MemPerc    float64
	NetRx      uint64
	NetTx      uint64
	BlockRead  uint64
	BlockWrite uint64
	DiskUsed   int64
}

// StatsSample is a streamed runtime resource usage snapshot for one instance.
type StatsSample struct {
	InstanceID ID
	Stats      Stats
}

// EventKind identifies a runtime lifecycle event.
type EventKind string

const (
	// EventDestroy reports that an instance was removed and cannot be revived.
	EventDestroy EventKind = "destroy"
	// EventDie reports that an instance stopped.
	EventDie EventKind = "die"
	// EventOOM reports that the runtime killed an instance for exceeding memory.
	EventOOM EventKind = "oom"
	// EventRestart reports that a previously stopped instance restarted.
	EventRestart EventKind = "restart"
	// EventStart reports that a previously stopped instance started.
	EventStart EventKind = "start"
)

// Event describes one runtime lifecycle transition.
type Event struct {
	InstanceID ID
	Kind       EventKind
}

// StartOptions holds optional flags for runtime instance startup.
type StartOptions struct {
	RuntimeName       Name
	Metadata          Metadata
	BaseImage         string
	ContainerPlatform string
	Harness           harness.Name
	Caches            []CacheMount
	Mounts            []Mount
	Tailscale         bool
	USB               bool
	Display           bool
	Sudo              bool
	// MaxCPUs limits the number of CPU cores the runtime instance may use.
	// Zero means use the runtime adapter default.
	MaxCPUs int
	// GitHubToken is the resolved GitHub token to inject into the runtime
	// environment. Empty means no token is injected.
	GitHubToken string
	// LogWriter receives provisioning log lines from the runtime backend.
	// Must not be nil.
	LogWriter io.Writer
}

// ForkRepo describes one repository in a fork: its identity, the branches it
// carries, and the fork's destination primary branch.
type ForkRepo struct {
	// GitRoot is the absolute host git repository path (the runtime's GitRoot).
	GitRoot string
	// ContainerPath is the container path to mount a new repo at. Used only for repos
	// not already in the source instance; existing repos keep their mount.
	ContainerPath string
	// SourceBranches are the branches to carry, primary first. For a repo already
	// in the source instance these are its current branches; for a new repo they
	// are the host branches to push (empty defaults to the repo's upstream).
	SourceBranches []string
	// DestPrimary is the fork's primary branch. The caller owns its uniqueness;
	// the runtime uses it verbatim.
	DestPrimary string
}

// ForkOptions holds parameters for forking a runtime instance.
type ForkOptions struct {
	RuntimeName Name
	Metadata    Metadata
	// Repos is the full set of repositories the fork should contain: every repo
	// already in the source instance plus any new ones to add. Each names its
	// destination primary branch; the caller owns branch uniqueness.
	Repos     []ForkRepo
	Display   bool // Inherit or enable X11/VNC.
	Tailscale bool // Inherit or enable Tailscale.
	USB       bool // Inherit or enable USB.
	Sudo      bool // Inherit or enable root access (password-based sudo).
	Harness   harness.Name
	ExtraEnv  []string  // KEY=VALUE pairs for ~/.env.
	Mounts    []Mount   // Host directories bind-mounted into the fork.
	MaxCPUs   int       // Max CPU cores; 0 means use the default.
	LogWriter io.Writer // Provisioning log output.
}

// System provides all runtime capabilities used by the application.
type System interface {
	Name() Name
	Lifecycle
	Repository
	Monitor
	Inventory
	PrivilegeInfo
}

// Lifecycle manages runtime instance lifecycle operations.
type Lifecycle interface {
	// Launch starts the runtime instance and writes connection config. It does
	// not wait for transport readiness. Repos must have branches set.
	Launch(ctx context.Context, repos []Repo, opts *StartOptions) (ID, error)
	// Connect waits for transport readiness and completes provisioning for the
	// runtime instance identified by id. It returns optional connection details.
	Connect(ctx context.Context, id ID, opts *StartOptions) (ConnectionInfo, error)
	// Stop gracefully stops the runtime instance without removing it. The
	// instance can be restarted later with Revive.
	Stop(ctx context.Context, id ID) error
	// Purge stops and removes the runtime instance identified by id.
	Purge(ctx context.Context, id ID) error
	// Revive restarts a stopped runtime instance and waits for connectivity.
	// The instance's filesystem is preserved.
	Revive(ctx context.Context, id ID) error
	// Fork snapshots a running or stopped instance and creates a new one where
	// each mapped repo is checked out on a new branch. opts.Repos names the full
	// repo set and each repo's destination primary branch.
	Fork(ctx context.Context, id ID, opts *ForkOptions) (ID, ConnectionInfo, error)
	// VNCPort returns the host port mapped to the runtime instance's VNC port.
	// Returns 0 when the instance has no display.
	VNCPort(ctx context.Context, id ID) int
	// Processes returns the list of running processes inside the runtime instance.
	Processes(ctx context.Context, id ID) ([]ProcessInfo, error)
	// Signal sends a signal to a process inside the runtime instance.
	Signal(ctx context.Context, id ID, pid int, sig string) error
}

// Repository provides repository operations inside runtime instances.
type Repository interface {
	Diff(ctx context.Context, id ID, repoIdx int, args ...string) (string, error)
	Fetch(ctx context.Context, id ID) error
	RepositoryStatus(ctx context.Context, id ID, repoIdx int) (RepositoryStatus, error)
}

// Monitor reads resource usage and lifecycle events.
type Monitor interface {
	WatchStats(ctx context.Context, ids []ID) (iter.Seq2[StatsSample, error], error)
	WatchEvents(ctx context.Context, filter EventFilter) (<-chan Event, error)
}

// Inventory lists runtime instances and their observed metadata.
type Inventory interface {
	List(ctx context.Context) ([]Instance, error)
	Metadata(ctx context.Context, id ID, key MetadataKey) (string, error)
	Inspect(ctx context.Context, id ID) (*InstanceInspect, error)
}

// PrivilegeInfo reads privileged runtime instance credentials.
type PrivilegeInfo interface {
	SudoPassword(ctx context.Context, id ID) (string, error)
}
