// Package runtime defines caic-owned task execution runtime interfaces and types.
package runtime

import (
	"context"
	"io"
	"time"

	"github.com/caic-xyz/caic/backend/internal/harness"
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
)

// Metadata stores runtime-neutral metadata for an instance.
type Metadata map[MetadataKey]string

// EventFilter selects runtime lifecycle events.
type EventFilter struct {
	MetadataKey MetadataKey
}

// InstanceID identifies a task runtime instance.
type InstanceID string

// ConnectionTarget describes how agent relay operations reach a runtime.
type ConnectionTarget struct {
	SSHHost string
}

// Repo describes a git repository available to a runtime instance.
type Repo struct {
	HostPath   string
	MountPath  string
	Branch     string
	BaseBranch string
	Remote     string
}

// CacheMount describes a host cache directory made available to a runtime.
type CacheMount struct {
	Name        string
	Description string
	HostPath    string
	MountPath   string
	ReadOnly    bool
	Shallow     bool
}

// Mount describes a host directory bind-mounted into a runtime.
type Mount struct {
	HostPath  string
	MountPath string
	ReadOnly  bool
}

// ConnectionInfo describes connection details returned by a runtime instance.
type ConnectionInfo struct {
	AgentTarget      ConnectionTarget
	TailscaleFQDN    string
	TailscaleAuthURL string
}

// Instance describes a known runtime instance.
type Instance struct {
	ID            InstanceID
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
	Runtime  string
	ID       InstanceID
	State    string
	ImageRef string
	ImageID  string
	Platform string
	CPULimit int
	Mounts   []Mount
	Caches   []CacheMount
}

// ProcessInfo describes a single process running inside a runtime instance.
type ProcessInfo struct {
	PID     int
	PPID    int
	User    string
	State   string
	CPU     float64
	Mem     float64
	Time    string
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

// Event describes a runtime lifecycle event.
type Event struct {
	InstanceID InstanceID
}

// StartOptions holds optional flags for runtime instance startup.
type StartOptions struct {
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

// ForkOptions holds parameters for forking a runtime instance.
type ForkOptions struct {
	Metadata   Metadata
	ExtraRepos []Repo // Additional repos to map into the fork beyond the source's repos.
	Display    bool   // Inherit or enable X11/VNC.
	Tailscale  bool   // Inherit or enable Tailscale.
	USB        bool   // Inherit or enable USB.
	Sudo       bool   // Inherit or enable root access (password-based sudo).
	Harness    harness.Name
	ExtraEnv   []string  // KEY=VALUE pairs for ~/.env.
	Mounts     []Mount   // Host directories bind-mounted into the fork.
	MaxCPUs    int       // Max CPU cores; 0 means use the default.
	LogWriter  io.Writer // Provisioning log output.
}

// Backend manages runtime instance lifecycle operations.
type Backend interface {
	// Launch starts the runtime instance and writes connection config. It does
	// not wait for transport readiness. Repos must have branches set.
	Launch(ctx context.Context, repos []Repo, opts *StartOptions) (InstanceID, error)
	// Connect waits for transport readiness and completes provisioning for the
	// runtime instance identified by id. It returns optional connection details.
	Connect(ctx context.Context, id InstanceID, opts *StartOptions) (ConnectionInfo, error)
	Diff(ctx context.Context, id InstanceID, repoIdx int, args ...string) (string, error)
	Fetch(ctx context.Context, id InstanceID) error
	// Stop gracefully stops the runtime instance without removing it. The
	// instance can be restarted later with Revive.
	Stop(ctx context.Context, id InstanceID) error
	// Purge stops and removes the runtime instance identified by id.
	Purge(ctx context.Context, id InstanceID) error
	// Revive restarts a stopped runtime instance and waits for connectivity.
	// The instance's filesystem is preserved.
	Revive(ctx context.Context, id InstanceID) error
	// Fork snapshots a running instance and creates a new one where each mapped
	// repo is checked out on a new branch derived from the current state.
	Fork(ctx context.Context, id InstanceID, repos []Repo, opts *ForkOptions) (InstanceID, ConnectionInfo, []Repo, error)
	// VNCPort returns the host port mapped to the runtime instance's VNC port.
	// Returns 0 when the instance has no display.
	VNCPort(ctx context.Context, id InstanceID) int
	// Processes returns the list of running processes inside the runtime instance.
	Processes(ctx context.Context, id InstanceID) ([]ProcessInfo, error)
	// Signal sends a signal to a process inside the runtime instance.
	Signal(ctx context.Context, id InstanceID, pid int, sig string) error
}

// Monitor reads resource usage and lifecycle events.
type Monitor interface {
	StatsAll(ctx context.Context, ids []InstanceID) (map[InstanceID]*Stats, error)
	WatchEvents(ctx context.Context, filter EventFilter) (<-chan Event, error)
}

// Inventory lists runtime instances and their observed metadata.
type Inventory interface {
	List(ctx context.Context) ([]Instance, error)
	Metadata(ctx context.Context, id InstanceID, key MetadataKey) (string, error)
	Inspect(ctx context.Context, id InstanceID) (*InstanceInspect, error)
}

// PrivilegeInfo reads privileged runtime instance credentials.
type PrivilegeInfo interface {
	SudoPassword(ctx context.Context, id InstanceID) (string, error)
}
