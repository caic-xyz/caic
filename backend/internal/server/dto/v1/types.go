// Exported request and response types for the caic API.
package v1

import (
	"encoding/json"
	"time"

	"github.com/caic-xyz/caic/backend/internal/server/dto"
	"github.com/maruel/ksid"
)

//go:generate go tool tygo generate --config ../../../../../backend/tygo.yaml
//go:generate go run github.com/caic-xyz/caic/backend/internal/cmd/gen-api-sdk

// Forge identifies the code hosting forge.
// Values must match forge.Kind constants.
type Forge string

// Supported forges.
const (
	ForgeGitHub Forge = "github"
	ForgeGitLab Forge = "gitlab"
)

// Harness identifies the coding agent harness.
// Values must match agent.Harness constants.
type Harness string

// Supported agent harnesses.
const (
	HarnessClaude   Harness = "claude"
	HarnessCodex    Harness = "codex"
	HarnessGemini   Harness = "gemini"
	HarnessKilo     Harness = "kilo"
	HarnessOpenCode Harness = "opencode"
)

// HarnessInfo is the JSON representation of an available harness.
type HarnessInfo struct {
	Name           string   `json:"name"`
	Models         []string `json:"models"`
	SupportsImages bool     `json:"supportsImages"`
}

// ImageData carries a single base64-encoded image.
type ImageData struct {
	MediaType string `json:"mediaType"` // e.g. "image/png", "image/jpeg"
	Data      string `json:"data"`      // base64-encoded
}

// Prompt bundles user text with optional images.
type Prompt struct {
	Text   string      `json:"text"`
	Images []ImageData `json:"images,omitempty"`
}

// Config reports server capabilities to the frontend.
type Config struct {
	Version            string   `json:"version,omitempty"`
	TailscaleAvailable bool     `json:"tailscaleAvailable"`
	USBAvailable       bool     `json:"usbAvailable"`
	DisplayAvailable   bool     `json:"displayAvailable"`
	GitHubAppEnabled   bool     `json:"gitHubAppEnabled,omitempty"`
	AuthProviders      []string `json:"authProviders,omitempty"` // e.g. ["github","gitlab"]
}

// UserResp is returned by GET /api/v1/auth/me.
type UserResp struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatarURL,omitempty"`
}

// CIStatus is the CI check state for a task or repo default branch.
type CIStatus string

// CI status values.
const (
	CIStatusPending CIStatus = "pending"
	CIStatusSuccess CIStatus = "success"
	CIStatusFailure CIStatus = "failure"
)

// CheckConclusion is the conclusion of a completed CI check run.
type CheckConclusion string

// CI check-run conclusion values.
const (
	CheckConclusionSuccess        CheckConclusion = "success"
	CheckConclusionFailure        CheckConclusion = "failure"
	CheckConclusionNeutral        CheckConclusion = "neutral"
	CheckConclusionSkipped        CheckConclusion = "skipped"
	CheckConclusionCancelled      CheckConclusion = "cancelled"
	CheckConclusionTimedOut       CheckConclusion = "timed_out"
	CheckConclusionActionRequired CheckConclusion = "action_required"
	CheckConclusionStale          CheckConclusion = "stale"
)

// CheckStatus is the status of a CI check run.
type CheckStatus string

// CI check-run status values.
const (
	CheckStatusQueued     CheckStatus = "queued"
	CheckStatusInProgress CheckStatus = "in_progress"
	CheckStatusCompleted  CheckStatus = "completed"
)

// ForgeCheck describes a CI check run with its status, conclusion, and timing.
type ForgeCheck struct {
	Name        string          `json:"name"`
	Owner       string          `json:"owner"`
	Repo        string          `json:"repo"`
	RunID       int64           `json:"runID"`                // Pipeline/workflow run ID.
	JobID       int64           `json:"jobID"`                // Check run / job ID.
	Status      CheckStatus     `json:"status"`               // queued, in_progress, completed.
	Conclusion  CheckConclusion `json:"conclusion"`           // Empty when not completed.
	QueuedAt    time.Time       `json:"queuedAt,omitzero"`    // When the check was created/queued.
	StartedAt   time.Time       `json:"startedAt,omitzero"`   // When execution began.
	CompletedAt time.Time       `json:"completedAt,omitzero"` // When execution finished.
}

// Repo is the JSON representation of a discovered repo.
type Repo struct {
	Path                  string       `json:"path"`
	BaseBranch            string       `json:"baseBranch"`
	RemoteURL             string       `json:"remoteURL,omitempty"`
	Forge                 Forge        `json:"forge,omitempty"` // "github", "gitlab", or empty if unknown.
	DefaultBranchCIStatus CIStatus     `json:"defaultBranchCIStatus,omitempty"`
	DefaultBranchChecks   []ForgeCheck `json:"defaultBranchChecks,omitempty"`
}

// RepoSpec describes a repository to associate with a task at creation time.
type RepoSpec struct {
	Name       string `json:"name"`
	BaseBranch string `json:"baseBranch,omitempty"`
}

// TaskRepo describes a repository associated with a task in the API response.
type TaskRepo struct {
	Name       string `json:"name"`
	BaseBranch string `json:"baseBranch,omitempty"`
	Branch     string `json:"branch"`
	RemoteURL  string `json:"remoteURL,omitempty"`
	Forge      Forge  `json:"forge,omitempty"` // "github", "gitlab", or empty if unknown.
}

// Task is the JSON representation sent to the frontend.
type Task struct {
	ID                                 ksid.ID      `json:"id"`
	InitialPrompt                      string       `json:"initialPrompt"`
	Title                              string       `json:"title"`
	Repos                              []TaskRepo   `json:"repos,omitempty"`
	Container                          string       `json:"container"`
	State                              string       `json:"state"`
	StateUpdatedAt                     float64      `json:"stateUpdatedAt"` // Unix epoch seconds (ms precision) of last state change.
	DiffStat                           DiffStat     `json:"diffStat,omitzero"`
	CostUSD                            float64      `json:"costUSD"`
	Duration                           float64      `json:"duration"` // Seconds.
	NumTurns                           int          `json:"numTurns"`
	CumulativeInputTokens              int          `json:"cumulativeInputTokens"`
	CumulativeOutputTokens             int          `json:"cumulativeOutputTokens"`
	CumulativeCacheCreationInputTokens int          `json:"cumulativeCacheCreationInputTokens"`
	CumulativeCacheReadInputTokens     int          `json:"cumulativeCacheReadInputTokens"`
	ActiveInputTokens                  int          `json:"activeInputTokens"`     // Last turn's non-cached input tokens (including cache creation).
	ActiveCacheReadTokens              int          `json:"activeCacheReadTokens"` // Last turn's cache-read input tokens.
	ContextWindowLimit                 int          `json:"contextWindowLimit"`    // Model context window limit (tokens).
	Error                              string       `json:"error,omitempty"`
	Result                             string       `json:"result,omitempty"`
	ForgeOwner                         string       `json:"forgeOwner,omitempty"`
	ForgeRepo                          string       `json:"forgeRepo,omitempty"`
	ForgePR                            int          `json:"forgePR,omitempty"`
	ForgeIssue                         int          `json:"forgeIssue,omitempty"`
	CIStatus                           CIStatus     `json:"ciStatus,omitempty"`
	CIChecks                           []ForgeCheck `json:"ciChecks,omitempty"`
	Owner                              string       `json:"owner,omitempty"` // username of creator; omitted in no-auth mode
	// Per-task harness/container metadata.
	Harness       Harness `json:"harness"`
	Model         string  `json:"model,omitempty"`
	AgentVersion  string  `json:"agentVersion,omitempty"`
	SessionID     string  `json:"sessionID,omitempty"`
	StartedAt     float64 `json:"startedAt,omitempty"`     // Unix epoch seconds (ms precision) when the container started.
	TurnStartedAt float64 `json:"turnStartedAt,omitempty"` // Unix epoch seconds; non-zero only while state is "running".
	InPlanMode    bool    `json:"inPlanMode,omitempty"`
	PlanContent   string  `json:"planContent,omitempty"`
	Tailscale     string  `json:"tailscale,omitempty"` // Tailscale URL (https://fqdn) or "true" if enabled but FQDN unknown.
	USB           bool    `json:"usb,omitempty"`
	Display       bool    `json:"display,omitempty"`
}

// TaskListEvent is a discriminated-union event for the task list SSE stream.
// kind=="snapshot": Tasks holds the full list on initial connect.
// kind=="upsert":   Task holds a newly created task.
// kind=="patch":    Patch holds only the changed fields (always includes "id") for an existing task.
// kind=="delete":   ID holds the string ID of the removed task.
// kind=="repos":    Repos holds the updated repo list (emitted when default-branch CI status changes).
type TaskListEvent struct {
	Kind  string                     `json:"kind"`
	Tasks []Task                     `json:"tasks,omitempty"`
	Task  *Task                      `json:"task,omitempty"`
	Patch map[string]json.RawMessage `json:"patch,omitempty"`
	ID    string                     `json:"id,omitempty"`
	Repos []Repo                     `json:"repos,omitempty"`
}

// TaskToolInputResp is the response for GET /api/v1/tasks/{id}/tool/{toolUseID}.
// It returns the full (untruncated) input for a tool call.
type TaskToolInputResp struct {
	ToolUseID string          `json:"toolUseID"`
	Input     json.RawMessage `json:"input"`
}

// StatusResp is a common response for mutation endpoints.
type StatusResp struct {
	Status string `json:"status"`
}

// CreateTaskResp is the response for POST /api/v1/tasks.
type CreateTaskResp struct {
	Status string  `json:"status"`
	ID     ksid.ID `json:"id"`
}

// CILogResp is the response for GET /api/v1/tasks/{id}/ci-log.
// It contains the name of the first failed CI step and its log tail.
type CILogResp struct {
	StepName string `json:"stepName"`
	Log      string `json:"log"`
}

// CreateTaskReq is the request body for POST /api/v1/tasks.
type CreateTaskReq struct {
	InitialPrompt Prompt     `json:"initialPrompt"`
	Repos         []RepoSpec `json:"repos,omitempty"`
	Model         string     `json:"model,omitempty"`
	Harness       Harness    `json:"harness"`
	Tailscale     bool       `json:"tailscale,omitempty"`
	USB           bool       `json:"usb,omitempty"`
	Display       bool       `json:"display,omitempty"`
}

// BotFixCIReq is the request body for POST /api/v1/bot/fix-ci.
// The server fetches CI logs, builds a prompt, and creates a fix task.
type BotFixCIReq struct {
	Repo string `json:"repo"`
}

// BotFixPRReq is the request body for POST /api/v1/bot/fix-pr.
// The server fetches CI logs for the task's PR and injects a fix command.
type BotFixPRReq struct {
	TaskID string `json:"taskId"`
}

// InputReq is the request body for POST /api/v1/tasks/{id}/input.
type InputReq struct {
	Prompt Prompt `json:"prompt"`
}

// RestartReq is the request body for POST /api/v1/tasks/{id}/restart.
type RestartReq struct {
	Prompt Prompt `json:"prompt"`
}

// DiffFileStat describes changes to a single file.
type DiffFileStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// DiffStat summarises the changes in a branch relative to its base.
type DiffStat []DiffFileStat

// SafetyIssue describes a potential problem detected before pushing to origin.
type SafetyIssue struct {
	File   string `json:"file"`
	Kind   string `json:"kind"`   // "large_binary" or "secret"
	Detail string `json:"detail"` // Human-readable description.
}

// SyncTarget selects where to push changes.
type SyncTarget string

// Supported sync targets.
const (
	SyncTargetBranch  SyncTarget = "branch"  // Push to the task's own branch (default).
	SyncTargetDefault SyncTarget = "default" // Squash-push to the repo's default branch.
)

// SyncReq is the request body for POST /api/v1/tasks/{id}/sync.
type SyncReq struct {
	Force  bool       `json:"force,omitempty"`
	Target SyncTarget `json:"target,omitempty"`
}

// SyncResp is the response for POST /api/v1/tasks/{id}/sync.
type SyncResp struct {
	Status       string        `json:"status"` // "synced", "blocked", or "empty"
	Branch       string        `json:"branch,omitempty"`
	DiffStat     DiffStat      `json:"diffStat,omitzero"`
	SafetyIssues []SafetyIssue `json:"safetyIssues,omitempty"`
	PRNumber     int           `json:"prNumber,omitempty"` // non-zero if a PR/MR was created
}

// UsageWindow represents a single quota window (5-hour or 7-day).
type UsageWindow struct {
	// From Claude OAuth API (rate-limit quota); zero when OAuth unavailable.
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resetsAt"`
	// From local task streaming data (always populated).
	CostUSD      float64 `json:"costUSD"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
}

// ExtraUsage represents the extra (pay-as-you-go) usage state.
type ExtraUsage struct {
	IsEnabled    bool    `json:"isEnabled"`
	MonthlyLimit float64 `json:"monthlyLimit"`
	UsedCredits  float64 `json:"usedCredits"`
	Utilization  float64 `json:"utilization"`
}

// UsageResp is the response for GET /api/v1/usage.
type UsageResp struct {
	FiveHour   UsageWindow `json:"fiveHour"`
	SevenDay   UsageWindow `json:"sevenDay"`
	ExtraUsage ExtraUsage  `json:"extraUsage"`
}

// VoiceTokenResp is the response for GET /api/v1/voice/token.
type VoiceTokenResp struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
	Ephemeral bool   `json:"ephemeral"`
}

// DiffResp is the response for GET /api/v1/tasks/{id}/diff.
type DiffResp struct {
	Diff string `json:"diff"`
}

// RepoPrefsResp holds per-repository preferences.
type RepoPrefsResp struct {
	Path       string `json:"path"`
	BaseBranch string `json:"baseBranch,omitempty"`
	Harness    string `json:"harness,omitempty"`
	Model      string `json:"model,omitempty"`
}

// CacheMappingResp represents a directory mapping for cache/state sharing.
type CacheMappingResp struct {
	HostPath      string `json:"hostPath"`
	ContainerPath string `json:"containerPath"`
}

// UserSettings holds user-configurable behavioral settings.
type UserSettings struct {
	// AutoFixOnCIFailure automatically starts a new task to fix CI when a
	// task's PR CI fails and the original task can no longer receive input.
	// Only effective when the GitHub App is configured.
	AutoFixOnCIFailure bool `json:"autoFixOnCIFailure"`
	// AutoFixOnPROpen automatically creates a task to review and fix a pull
	// request when it is opened or reopened via a forge webhook.
	AutoFixOnPROpen bool `json:"autoFixOnPROpen"`
	// BaseImage overrides the default container base image. Empty means use
	// the default.
	BaseImage string `json:"baseImage,omitempty"`
	// UseDefaultCaches controls whether default harness caches are mounted.
	// When false, only custom CacheMappings are used.
	UseDefaultCaches bool `json:"useDefaultCaches,omitempty"`
	// WellKnownCaches maps cache name to enabled state. nil means use default
	// (all true), true means explicitly enabled, false means explicitly disabled.
	WellKnownCaches map[string]bool `json:"wellKnownCaches,omitempty"`
	// CacheMappings are custom host-to-container directory mappings.
	CacheMappings []CacheMappingResp `json:"cacheMappings,omitempty"`
}

// PreferencesResp is the response for GET /api/v1/server/preferences.
type PreferencesResp struct {
	Repositories []RepoPrefsResp   `json:"repositories"`
	Harness      string            `json:"harness,omitempty"`
	Models       map[string]string `json:"models,omitempty"`
	Settings     UserSettings      `json:"settings"`
}

// UpdatePreferencesReq is the request body for POST /api/v1/server/preferences.
type UpdatePreferencesReq struct {
	Settings UserSettings `json:"settings"`
}

// CloneRepoReq is the request body for POST /api/v1/server/repos.
type CloneRepoReq struct {
	URL   string `json:"url"`            // Git clone URL (HTTPS or SSH).
	Path  string `json:"path,omitempty"` // Target subdirectory under rootDir; defaults to repo basename.
	Depth int    `json:"depth,omitempty"`
}

// WebFetchReq is the request body for POST /api/v1/web/fetch.
type WebFetchReq struct {
	URL string `json:"url"`
}

// WebFetchResp is the response for POST /api/v1/web/fetch.
type WebFetchResp struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

// RepoBranchesResp is the response for GET /api/v1/server/repos/branches.
type RepoBranchesResp struct {
	Branches []string `json:"branches"`
}

// WellKnownCache describes a single well-known cache.
type WellKnownCache struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Mounts      []string `json:"mounts"` // List of container paths
}

// WellKnownCachesResp is the response for GET /api/v1/server/caches.
type WellKnownCachesResp struct {
	HarnessMounts []string         `json:"harnessMounts"` // e.g. "~/.claude", "~/.codex"
	WellKnown     []WellKnownCache `json:"wellKnown"`
}

// VoiceRTCOfferReq is the request body for POST /api/v1/voice/rtc/offer.
type VoiceRTCOfferReq struct {
	SDP string `json:"sdp"`
}

// VoiceRTCAnswerResp is the response for POST /api/v1/voice/rtc/offer.
type VoiceRTCAnswerResp struct {
	SDP       string `json:"sdp"`
	SessionID string `json:"sessionID"`
}

// EmptyReq is used for endpoints that take no request body.
type EmptyReq = dto.EmptyReq
