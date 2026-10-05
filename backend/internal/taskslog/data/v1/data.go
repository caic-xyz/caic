// Package v1 defines version 1 of caic-owned physical task-log records.
package v1

import (
	"encoding/json"
	"time"
)

// DiffFileStat is the persisted DiffFileStat representation.
type DiffFileStat struct {
	Path         string `json:"path"`
	LinesAdded   int    `json:"added"`
	LinesDeleted int    `json:"deleted"`
	Binary       bool   `json:"binary,omitempty"`
	OldSize      int64  `json:"oldSize,omitempty"` // Byte size of the binary pre-image; zero for added paths.
	NewSize      int64  `json:"newSize,omitempty"` // Byte size of the binary post-image; zero for deleted paths.
}

// RepoState is the persisted RepoState representation.
type RepoState struct {
	Stale            bool   `json:"stale,omitempty"` // A failed probe retained the last known data.
	RepoIndex        int    `json:"repo_index"`
	Branch           string `json:"branch"`
	Operation        string `json:"operation,omitempty"` // runtime.RepositoryOperation while a merge/rebase is in progress.
	Ahead            int    `json:"ahead"`
	Behind           int    `json:"behind"`
	ChangedFiles     int    `json:"changed_files"`
	LinesAdded       int    `json:"added"`
	LinesDeleted     int    `json:"deleted"`
	UncommittedFiles int    `json:"uncommitted"`
	Conflicts        int    `json:"conflicts"`
}

// MetaRepo is the persisted MetaRepo representation.
type MetaRepo struct {
	Name          string `json:"name"`
	BaseBranch    string `json:"base_branch,omitempty"`
	Branch        string `json:"branch"`
	ContainerPath string `json:"containerPath,omitempty"`
}

// MetaCacheMount is the persisted MetaCacheMount representation.
type MetaCacheMount struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	HostPath    string `json:"hostPath,omitempty"`
	// ContainerPath is the resolved target path in the runtime container.
	ContainerPath string `json:"containerPath,omitempty"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
	Shallow       bool   `json:"shallow,omitempty"`
}

// MetaMount is the persisted MetaMount representation.
type MetaMount struct {
	HostPath string `json:"hostPath,omitempty"`
	// ContainerPath is the resolved target path in the runtime container.
	ContainerPath string `json:"containerPath,omitempty"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
}

// StartupFailure is the persisted StartupFailure representation.
type StartupFailure struct {
	Harness string `json:"harness"`
	Phase   string `json:"phase"`
	Cause   string `json:"cause"`
}

// RepositoryCommit is the persisted RepositoryCommit representation.
type RepositoryCommit struct {
	// RepositoryPath is the repository's absolute path inside the task runtime.
	RepositoryPath string `json:"repository_path"`
	// BranchName is the short local branch name, such as "main" or "caic-1".
	BranchName string `json:"branch_name"`
	// CommitHash is the full Git object ID of the branch tip.
	CommitHash string `json:"commit_hash"`
}

// ChangeStat is the persisted ChangeStat representation.
type ChangeStat struct {
	Files        int `json:"files"`
	LinesAdded   int `json:"added"`
	LinesDeleted int `json:"deleted"`
	BinaryFiles  int `json:"binary_files"`
}

// AskOption is the persisted AskOption representation.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskQuestion is the persisted AskQuestion representation.
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	Options     []AskOption `json:"options"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
}

// PendingAskAction is the persisted PendingAskAction representation.
type PendingAskAction struct {
	Questions []AskQuestion `json:"questions,omitzero"`
}

// PendingUserAction is the persisted PendingUserAction representation.
type PendingUserAction struct {
	Kind PendingUserActionKind `json:"kind"`

	// RequestID is the backend request ID needed to complete the action.
	RequestID string `json:"request_id,omitempty"`

	// ToolUseID is the user-visible tool call that created the action.
	ToolUseID string `json:"tool_use_id,omitempty"`

	Ask PendingAskAction `json:"ask,omitzero"`
}

// ImageData is the persisted ImageData representation.
type ImageData struct {
	MediaType string `json:"media_type"` // e.g. "image/png", "image/jpeg"
	Data      string `json:"data"`       // base64-encoded
}

// TextMessage is the persisted TextMessage representation.
type TextMessage struct {
	Text  string `json:"text"`
	Phase string `json:"phase,omitempty"` // Codex only: "commentary" | "final_answer" | "".
}

// UserInputMessage is the persisted UserInputMessage representation.
type UserInputMessage struct {
	Text   string      `json:"text,omitempty"`
	Images []ImageData `json:"images,omitempty"`
}

// SystemMessage is the persisted SystemMessage representation.
type SystemMessage struct {
	MessageType   string `json:"type"`
	Subtype       string `json:"subtype"`
	SessionID     string `json:"session_id"`
	UUID          string `json:"uuid"`
	Detail        string `json:"detail,omitempty"` // Optional human-readable detail (e.g. model names for SystemSubtypeModelRerouted).
	ReportedModel string `json:"model,omitempty"`  // Active model after SystemSubtypeModelRerouted; used to update task.reportedModel.
	// ContextTokensBefore is the harness-reported context size before a
	// SystemSubtypeCompactBoundary. Zero means the harness did not report it.
	ContextTokensBefore int64 `json:"context_tokens_before,omitempty"`
	// ContextTokensAfter is the harness's context size after a
	// SystemSubtypeCompactBoundary, measured or estimated by the harness. Zero
	// means the harness did not report it.
	ContextTokensAfter int64 `json:"context_tokens_after,omitempty"`
}

// LogMessage is the persisted LogMessage representation.
type LogMessage struct {
	MessageType string `json:"type"`
	Line        string `json:"line"`
}

// StrippedEnvMessage is the persisted StrippedEnvMessage representation.
type StrippedEnvMessage struct {
	MessageType string            `json:"type"`
	Variables   map[string]string `json:"variables"`
}

// DiffStatMessage is the persisted DiffStatMessage representation.
type DiffStatMessage struct {
	MessageType string   `json:"type"`
	DiffStat    DiffStat `json:"diff_stat"`
	// Repos carries one compact git state per task repository, filled by the
	// backend's post-tool probe. The relay watcher omits it.
	Repos []RepoState `json:"repos,omitempty"`
	Ts    float64     `json:"ts,omitempty"` // Unix epoch seconds (ms precision) when the relay emitted this record.
}

// ExitMessage is the persisted ExitMessage representation.
type ExitMessage struct {
	MessageType     string   `json:"type"`
	ExitCode        int      `json:"exit_code"`
	Command         []string `json:"cmd,omitempty"`
	Signal          int      `json:"signal,omitempty"`
	Error           string   `json:"error,omitempty"`
	StderrTruncated bool     `json:"stderr_truncated,omitempty"`
	Ts              float64  `json:"ts,omitempty"`
}

// MetaMessage is the persisted MetaMessage representation.
type MetaMessage struct {
	MessageType       string           `json:"type"`
	Version           int              `json:"version"`
	Prompt            string           `json:"prompt"`
	Title             string           `json:"title,omitempty"`
	Repos             []MetaRepo       `json:"repos"`
	Harness           string           `json:"harness"`
	RequestedModel    string           `json:"model,omitempty"`
	RequestedEffort   string           `json:"effort,omitempty"`
	StartedAt         time.Time        `json:"started_at"`
	ForgeIssue        int              `json:"forge_issue,omitempty"` // Originating issue/PR number for bot comment callbacks.
	OwnerID           string           `json:"owner_id,omitempty"`    // Human authorization principal; distinct from task lineage.
	ForkedFromTaskID  string           `json:"forked_from_task_id,omitempty"`
	ParentTaskID      string           `json:"parent_task_id,omitempty"` // Delegating task identity; empty for roots and ordinary forks.
	CaicMCP           bool             `json:"caic_mcp,omitempty"`       // Enables task-scoped CAIC MCP delegation.
	Tailscale         bool             `json:"tailscale,omitempty"`
	USB               bool             `json:"usb,omitempty"`
	Display           bool             `json:"display,omitempty"`
	Sudo              bool             `json:"sudo,omitempty"`
	GitHubToken       bool             `json:"gitHubToken,omitempty"`
	RuntimeName       string           `json:"runtimeName,omitempty"`
	BaseImage         string           `json:"baseImage,omitempty"`
	ContainerPlatform string           `json:"containerPlatform,omitempty"`
	MaxCPUs           int              `json:"maxCPUs,omitempty"`
	CacheMounts       []MetaCacheMount `json:"cacheMounts,omitempty"`
	Mounts            []MetaMount      `json:"mounts,omitempty"`
}

// MetaSessionMessage is the persisted MetaSessionMessage representation.
type MetaSessionMessage struct {
	MessageType    string `json:"type"`
	SessionID      string `json:"session_id"`
	ReportedModel  string `json:"model,omitempty"`
	ReportedEffort string `json:"reported_effort,omitempty"`
	AgentVersion   string `json:"agent_version,omitempty"`
}

// RelayGenerationMessage is the persisted RelayGenerationMessage representation.
type RelayGenerationMessage struct {
	MessageType string `json:"type"`
	Generation  string `json:"generation"`
}

// ModelInfoMessage is the persisted ModelInfoMessage representation.
type ModelInfoMessage struct {
	MessageType   string `json:"type"`
	ContextWindow int64  `json:"context_window"`
}

// MetaResultMessage is the persisted MetaResultMessage representation.
type MetaResultMessage struct {
	MessageType              string   `json:"type"`
	State                    string   `json:"state"`
	Title                    string   `json:"title,omitempty"`
	CostUSD                  float64  `json:"cost_usd,omitempty"`
	Duration                 float64  `json:"duration,omitempty"` // Seconds.
	NumTurns                 int      `json:"num_turns,omitempty"`
	InputTokens              int      `json:"input_tokens,omitempty"`
	OutputTokens             int      `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int      `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int      `json:"cache_read_input_tokens,omitempty"`
	ReasoningOutputTokens    int      `json:"reasoning_output_tokens,omitempty"`
	DiffStat                 DiffStat `json:"diff_stat,omitzero"`
	// TODO(2026-10-01): Make DiskUsedBytes an int64 value using -1 for
	// unavailable measurements after legacy result records have aged out.
	DiskUsedBytes  *int64          `json:"disk_used_bytes,omitempty"`
	Error          string          `json:"error,omitempty"`
	AgentResult    string          `json:"agent_result,omitempty"`
	StartupFailure *StartupFailure `json:"startup_failure,omitempty"`
}

// MetaPRMessage is the persisted MetaPRMessage representation.
type MetaPRMessage struct {
	MessageType string `json:"type"`
	ForgeOwner  string `json:"forge_owner"`
	ForgeRepo   string `json:"forge_repo"`
	ForgePR     int    `json:"forge_pr"`
}

// TurnCommitSnapshotMessage is the persisted TurnCommitSnapshotMessage representation.
type TurnCommitSnapshotMessage struct {
	MessageType string `json:"type"`
	// Baseline marks the snapshot taken before a newly started agent session.
	Baseline bool `json:"baseline,omitempty"`
	// RepositoryCommits contains the immutable Git branch tips fetched from
	// every repository in the task runtime at this boundary.
	RepositoryCommits []RepositoryCommit `json:"repository_commits"`
	// ChangeStat is the completed turn's net committed change since the prior
	// snapshot. It is nil when no complete comparison was available.
	ChangeStat *ChangeStat `json:"change_stat,omitempty"`
}

// PendingUserActionMessage is the persisted PendingUserActionMessage representation.
type PendingUserActionMessage struct {
	MessageType string            `json:"type"`
	Action      PendingUserAction `json:"action"`
}

// MCPRequestMessage is the persisted MCPRequestMessage representation.
type MCPRequestMessage struct {
	ID        string          `json:"id"`
	Method    string          `json:"method"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// LegacyInit is the persisted LegacyInit representation.
type LegacyInit struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Version   string `json:"version"`
}

// DiffStat summarizes the persisted per-file changes.
type DiffStat []DiffFileStat

// PendingUserActionKind identifies the serialized action discriminator.
type PendingUserActionKind string
