// Package runtime defines caic-owned task execution runtime types.
package runtime

import "time"

// InstanceID identifies a task runtime instance.
type InstanceID string

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

// ConnectionInfo describes connection details returned by a runtime instance.
type ConnectionInfo struct {
	TailscaleFQDN    string
	TailscaleAuthURL string
}

// Instance describes a known runtime instance.
type Instance struct {
	ID            InstanceID
	State         string
	Repos         []Repo
	Tailscale     bool
	TailscaleFQDN string
	USB           bool
	Display       bool
	Sudo          bool
	VNCPort       int
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
