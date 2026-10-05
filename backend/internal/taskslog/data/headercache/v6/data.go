// Package v6 defines the independent version 6 task-header cache schema.
package v6

import "time"

// Entry stores one task snapshot and the log identity used for invalidation.
type Entry struct {
	Version      int   `json:"version"`
	LogSize      int64 `json:"log_size"`
	LogMTimeNano int64 `json:"log_mtime_unix_nano"`
	Task         *Task `json:"task"`
}

// Task is the bounded snapshot reconstructed from a task log.
type Task struct {
	TaskID            string       `json:"task_id"`
	Prompt            string       `json:"prompt"`
	Title             string       `json:"title"`
	Repos             []Repo       `json:"repos"`
	LogVersion        int          `json:"log_version"`
	Harness           string       `json:"harness"`
	StartedAt         time.Time    `json:"started_at"`
	LastStateUpdateAt time.Time    `json:"last_state_update_at"`
	State             string       `json:"state"`
	ForgeIssue        int          `json:"forge_issue"`
	OwnerID           string       `json:"owner_id"`
	ForkedFromTaskID  string       `json:"forked_from_task_id"`
	ParentTaskID      string       `json:"parent_task_id"`
	CaicMCP           bool         `json:"caic_mcp"`
	ForgeOwner        string       `json:"forge_owner"`
	ForgeRepo         string       `json:"forge_repo"`
	ForgePR           int          `json:"forge_pr"`
	Tailscale         bool         `json:"tailscale"`
	USB               bool         `json:"usb"`
	Display           bool         `json:"display"`
	Sudo              bool         `json:"sudo"`
	GitHubToken       bool         `json:"github_token"`
	RuntimeName       string       `json:"runtime_name"`
	BaseImage         string       `json:"base_image"`
	ContainerPlatform string       `json:"container_platform"`
	MaxCPUs           int          `json:"max_cpus"`
	CacheMounts       []CacheMount `json:"cache_mounts"`
	Mounts            []Mount      `json:"mounts"`
	RequestedModel    string       `json:"model"`
	RequestedEffort   string       `json:"effort"`
	ReportedModel     string       `json:"reported_model"`
	ReportedEffort    string       `json:"reported_effort"`
	SessionID         string       `json:"session_id"`
	AgentVersion      string       `json:"agent_version"`
	LogSize           int64        `json:"log_size"`
	DiffCreated       bool         `json:"diff_created"`
	LastTrailer       *Result      `json:"result"`
}

// Repo preserves the cached RepoMount fields.
type Repo struct {
	Name          string `json:"name"`           // relative path, e.g. "github/caic"
	BaseBranch    string `json:"base_branch"`    // branch to fork from; empty = checkout default
	Branch        string `json:"branch"`         // allocated branch, e.g. "caic-0"
	GitRoot       string `json:"git_root"`       // absolute host path; empty in purged-task entries
	ContainerPath string `json:"container_path"` // path inside the runtime instance
}

// CacheMount preserves the cached CacheMount fields.
type CacheMount struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"` // Resolved target path in the runtime container.
	ReadOnly      bool   `json:"read_only"`
	Shallow       bool   `json:"shallow"`
}

// Mount preserves the cached Mount fields.
type Mount struct {
	HostPath      string `json:"host_path"`
	ContainerPath string `json:"container_path"` // Resolved target path in the runtime container.
	ReadOnly      bool   `json:"read_only"`
}

// DiffFileStat preserves the cached DiffFileStat fields.
type DiffFileStat struct {
	Path         string `json:"path"`
	LinesAdded   int    `json:"added"`
	LinesDeleted int    `json:"deleted"`
	Binary       bool   `json:"binary,omitempty"`
	OldSize      int64  `json:"oldSize,omitempty"` // Byte size of the binary pre-image; zero for added paths.
	NewSize      int64  `json:"newSize,omitempty"` // Byte size of the binary post-image; zero for deleted paths.
}

// Usage preserves the cached Usage fields.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	ReasoningOutputTokens    int `json:"reasoning_output_tokens,omitempty"`
	CacheTTLSeconds          int `json:"cache_ttl_seconds,omitempty"` // Effective cache TTL from last API call; 0 = unknown.
}

// StartupFailure preserves the cached StartupFailure fields.
type StartupFailure struct {
	Harness string `json:"harness"`
	Phase   string `json:"phase"`
	Cause   string `json:"cause"`
}

// Result stores the last recorded completion outcome.
type Result struct {
	State          string          `json:"state"`
	DiffStat       []DiffFileStat  `json:"diff_stat"`
	DiskUsedBytes  *int64          `json:"disk_used_bytes,omitempty"`
	CostUSD        float64         `json:"cost_usd"`
	Duration       time.Duration   `json:"duration"`
	NumTurns       int             `json:"num_turns"`
	Usage          Usage           `json:"usage"`
	AgentResult    string          `json:"agent_result"`
	StartupFailure *StartupFailure `json:"startup_failure,omitempty"`
	Error          string          `json:"error,omitempty"`
}
