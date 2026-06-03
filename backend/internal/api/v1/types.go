// Exported request and response types for the caic API.

package v1

import (
	"encoding/json"
	"time"

	"github.com/maruel/ksid"

	api "github.com/caic-xyz/caic/backend/internal/api"
)

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
	HarnessPi       Harness = "pi"
)

// HarnessInfo is the JSON representation of an available harness.
type HarnessInfo struct {
	Name            string   `json:"name"`
	Models          []string `json:"models"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsCompact bool     `json:"supportsCompact"`
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
	Version              string               `json:"version,omitempty"`
	DisplayName          string               `json:"displayName"`
	TailscaleAvailable   bool                 `json:"tailscaleAvailable"`
	USBAvailable         bool                 `json:"usbAvailable"`
	DisplayAvailable     bool                 `json:"displayAvailable"`
	SudoAvailable        bool                 `json:"sudoAvailable"`
	GitHubTokenAvailable bool                 `json:"gitHubTokenAvailable"`
	VoiceGateway         VoiceGatewayMetadata `json:"voiceGateway"`
	GitHubAppEnabled     bool                 `json:"gitHubAppEnabled,omitempty"`
	AuthProviders        []string             `json:"authProviders,omitempty"` // e.g. ["github","gitlab"]
}

// VoiceGatewayMode is the advertised service-side voice gateway mode.
type VoiceGatewayMode string

// Voice gateway modes.
const (
	VoiceGatewayModeDisabled VoiceGatewayMode = "disabled"
	VoiceGatewayModeEmbedded VoiceGatewayMode = "embedded"
	VoiceGatewayModeExternal VoiceGatewayMode = "external"
)

// VoiceGatewayMetadata reports structured voice gateway support.
type VoiceGatewayMetadata struct {
	Mode               VoiceGatewayMode `json:"mode"`
	URL                string           `json:"url,omitempty"`
	MinGatewayProtocol int              `json:"minGatewayProtocol,omitempty"`
	AuthRequired       bool             `json:"authRequired,omitempty"`
	TokenEndpoint      string           `json:"tokenEndpoint,omitempty"`
	TokenAudience      string           `json:"tokenAudience,omitempty"`
	Capabilities       []string         `json:"capabilities,omitempty"`
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

// TaskState is the lifecycle state of a task.
type TaskState string

// Task lifecycle states.
const (
	TaskStatePending      TaskState = "pending"
	TaskStateBranching    TaskState = "branching"
	TaskStateProvisioning TaskState = "provisioning"
	TaskStateStarting     TaskState = "starting"
	TaskStateRunning      TaskState = "running"
	TaskStateWaiting      TaskState = "waiting"
	TaskStateAsking       TaskState = "asking"
	TaskStateHasPlan      TaskState = "has_plan"
	TaskStatePulling      TaskState = "pulling"
	TaskStatePushing      TaskState = "pushing"
	TaskStateStopping     TaskState = "stopping"
	TaskStateStopped      TaskState = "stopped"
	TaskStatePurging      TaskState = "purging"
	TaskStateFailed       TaskState = "failed"
	TaskStatePurged       TaskState = "purged"
)

// ForgePRState is the state of a pull/merge request.
type ForgePRState string

// Forge PR state values.
const (
	ForgePRStateOpen   ForgePRState = "open"
	ForgePRStateClosed ForgePRState = "closed"
	ForgePRStateMerged ForgePRState = "merged"
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
	Path       string       `json:"path"`
	Branch     string       `json:"branch"`
	BaseBranch BranchInfo   `json:"baseBranch"`
	RemoteURL  string       `json:"remoteURL,omitempty"`
	Forge      Forge        `json:"forge,omitempty"` // "github", "gitlab", or empty if unknown.
	CI         CIStatus     `json:"ci,omitempty"`
	CIChecks   []ForgeCheck `json:"ciChecks,omitempty"`
	ChecksDate time.Time    `json:"checksDate,omitzero"`
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

// RuntimeInstance holds per-task runtime metadata.
type RuntimeInstance struct {
	ID        string `json:"id"`                  // Runtime instance ID.
	Tailscale string `json:"tailscale,omitempty"` // Tailscale URL (https://fqdn) or "true" if enabled but FQDN unknown.
	USB       bool   `json:"usb,omitempty"`
	Display   bool   `json:"display,omitempty"`
	Sudo      bool   `json:"sudo,omitempty"`
	// SudoPassword is the random sudo password, only populated when Sudo is true.
	SudoPassword string `json:"sudoPassword,omitempty"`
	VNCPort      int    `json:"vncPort,omitempty"`
}

// Task is the JSON representation sent to the frontend.
type Task struct {
	ID                                 ksid.ID      `json:"id"`
	InitialPrompt                      string       `json:"initialPrompt"`
	Title                              string       `json:"title"`
	Repos                              []TaskRepo   `json:"repos,omitempty"`
	State                              TaskState    `json:"state"`
	StateUpdatedAt                     time.Time    `json:"stateUpdatedAt"` // When the task state last changed.
	DiffStat                           DiffStat     `json:"diffStat,omitzero"`
	CostUSD                            float64      `json:"costUSD"`
	Duration                           float64      `json:"duration"` // Seconds.
	NumTurns                           int          `json:"numTurns"`
	CumulativeInputTokens              int          `json:"cumulativeInputTokens"`
	CumulativeOutputTokens             int          `json:"cumulativeOutputTokens"`
	CumulativeCacheCreationInputTokens int          `json:"cumulativeCacheCreationInputTokens"`
	CumulativeCacheReadInputTokens     int          `json:"cumulativeCacheReadInputTokens"`
	ActiveInputTokens                  int          `json:"activeInputTokens"`         // Last turn's non-cached input tokens (including cache creation).
	ActiveCacheReadTokens              int          `json:"activeCacheReadTokens"`     // Last turn's cache-read input tokens.
	CacheTTLSeconds                    int          `json:"cacheTTLSeconds,omitempty"` // Effective cache TTL from last API call (seconds); 0 = unknown.
	CacheExpiresAt                     time.Time    `json:"cacheExpiresAt,omitzero"`   // When the prompt cache expires.
	ContextWindowLimit                 int          `json:"contextWindowLimit"`        // Model context window limit (tokens).
	Error                              string       `json:"error,omitempty"`
	Result                             string       `json:"result,omitempty"`
	ForgeOwner                         string       `json:"forgeOwner,omitempty"`
	ForgeRepo                          string       `json:"forgeRepo,omitempty"`
	ForgePR                            int          `json:"forgePR,omitempty"`
	ForgePRState                       ForgePRState `json:"forgePRState,omitempty"`
	ForgeIssue                         int          `json:"forgeIssue,omitempty"`
	CIStatus                           CIStatus     `json:"ciStatus,omitempty"`
	CIChecks                           []ForgeCheck `json:"ciChecks,omitempty"`
	Owner                              string       `json:"owner,omitempty"` // username of creator; omitted in no-auth mode
	// Per-task harness/agent metadata.
	Harness       Harness         `json:"harness"`
	Model         string          `json:"model,omitempty"`
	Effort        string          `json:"effort,omitempty"` // Thinking effort (e.g. "low", "medium", "high", "max"). Empty = default.
	AgentVersion  string          `json:"agentVersion,omitempty"`
	SessionID     string          `json:"sessionID,omitempty"`
	StartedAt     time.Time       `json:"startedAt,omitzero"`     // When the task was created.
	TurnStartedAt time.Time       `json:"turnStartedAt,omitzero"` // When the current turn started; zero when not running.
	InPlanMode    bool            `json:"inPlanMode,omitempty"`
	PlanContent   string          `json:"planContent,omitempty"`
	Runtime       RuntimeInstance `json:"runtime"`
	// Per-task feature flags.
	GitHubToken bool `json:"gitHubToken,omitempty"`
}

// TaskListEvent is a discriminated-union event for the task list SSE stream.
// kind=="snapshot": Tasks holds the full list on initial connect.
// kind=="upsert":   Task holds a newly created task.
// kind=="patch":    Patch holds only the changed fields (always includes "id") for an existing task.
// kind=="delete":   ID holds the string ID of the removed task.
// kind=="repos":    Repos holds the updated repo list (emitted when default-branch CI status changes).
// kind=="warning":  Warning holds a transient server warning message for the user.
type TaskListEvent struct {
	Kind     string                     `json:"kind"`
	Snapshot []Task                     `json:"snapshot,omitzero"`
	Upsert   *Task                      `json:"upsert,omitempty"`
	Patch    map[string]json.RawMessage `json:"patch,omitempty"`
	Delete   string                     `json:"delete,omitempty"`
	Repos    []Repo                     `json:"repos,omitzero"`
	Warning  string                     `json:"warning,omitempty"`
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
	Effort        string     `json:"effort,omitempty"` // Thinking effort (e.g. "low", "medium", "high", "max"). Empty = default.
	Harness       Harness    `json:"harness"`
	Tailscale     bool       `json:"tailscale,omitempty"`
	USB           bool       `json:"usb,omitempty"`
	Display       bool       `json:"display,omitempty"`
	Sudo          bool       `json:"sudo,omitempty"`
	GitHubToken   bool       `json:"gitHubToken,omitempty"`
}

// ForkTaskReq is the request body for POST /api/v1/tasks/{id}/fork.
type ForkTaskReq struct {
	Prompt      Prompt     `json:"prompt"`                // Initial prompt for the forked task.
	Harness     Harness    `json:"harness,omitempty"`     // Override harness; empty means inherit from source.
	Model       string     `json:"model,omitempty"`       // Override model; empty means inherit from source.
	Effort      string     `json:"effort,omitempty"`      // Override thinking effort; empty means inherit from source.
	ExtraRepos  []RepoSpec `json:"extraRepos,omitempty"`  // Additional repos to map into the fork.
	Tailscale   *bool      `json:"tailscale,omitempty"`   // Override Tailscale; nil means inherit from source.
	USB         *bool      `json:"usb,omitempty"`         // Override USB; nil means inherit from source.
	Display     *bool      `json:"display,omitempty"`     // Override virtual display; nil means inherit from source.
	Sudo        *bool      `json:"sudo,omitempty"`        // Override sudo; nil means inherit from source.
	GitHubToken *bool      `json:"gitHubToken,omitempty"` // Override gitHubToken; nil means inherit from source.
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

// CompactReq is the request body for POST /api/v1/tasks/{id}/compact.
type CompactReq struct {
	Instructions string `json:"instructions,omitempty"`
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

// QuotaRateLimit is a single rate-limit window snapshot from any provider.
type QuotaRateLimit struct {
	Window   string    `json:"window"`            // "5h", "7d", "primary", "secondary", "rpm", "tpd", …
	UsedPct  float64   `json:"usedPct"`           // 0–100
	ResetsAt time.Time `json:"resetsAt,omitzero"` // zero when unknown
}

// QuotaBalance is a balance/credit snapshot from any provider.
type QuotaBalance struct {
	Currency string  `json:"currency"`           // "USD", "CNY", "credits", …
	Total    float64 `json:"total"`              // total available balance
	Granted  float64 `json:"granted,omitempty"`  // unexpired promotional/grant balance
	ToppedUp float64 `json:"toppedUp,omitempty"` // self-funded recharge balance
}

// QuotaExtraUsage is pay-as-you-go usage info (Anthropic-style extra credits).
type QuotaExtraUsage struct {
	Currency     string  `json:"currency"` // "USD", "CNY", …
	IsEnabled    bool    `json:"isEnabled"`
	UsedCredits  float64 `json:"usedCredits"`
	MonthlyLimit float64 `json:"monthlyLimit"`
	UsedPct      float64 `json:"usedPct"`
}

// ProviderQuota is the quota data for one provider.
type ProviderQuota struct {
	Provider string `json:"provider"` // "anthropic", "deepseek", "gemini", "openai", "codex", "openrouter", …
	Label    string `json:"label"`    // human-readable: "Anthropic", "DeepSeek", …
	LogoURL  string `json:"logoUrl"`  // absolute URL path to provider SVG, e.g. "/logos/anthropic.svg"
	AuthKind string `json:"authKind"` // "oauth" or "apikey"
	UsageURL string `json:"usageUrl"` // link to provider's usage/billing page

	RateLimits []QuotaRateLimit `json:"rateLimits,omitzero"`
	Balance    QuotaBalance     `json:"balance,omitzero"`
	ExtraUsage QuotaExtraUsage  `json:"extraUsage,omitzero"`
}

// LocalWindow is the aggregated local cost for a rolling time window.
type LocalWindow struct {
	Duration     string  `json:"duration"` // "1h", "6h", "24h"
	CostUSD      float64 `json:"costUSD"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
}

// LocalUsage is the aggregated cost across all tasks within recent time windows.
type LocalUsage struct {
	Windows []LocalWindow `json:"windows"`
}

// UsageResp is the response for GET /api/v1/usage.
type UsageResp struct {
	Providers []ProviderQuota `json:"providers,omitempty"`
	Local     LocalUsage      `json:"local"`
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

// ProcessInfo describes a single process running inside a task runtime instance.
type ProcessInfo struct {
	PID     int     `json:"pid"`
	PPID    int     `json:"ppid"`
	User    string  `json:"user"`
	State   string  `json:"state"` // Single-character state: R, S, D, Z, T, etc.
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	Time    string  `json:"time"`    // Cumulative CPU time.
	Command string  `json:"command"` // Full command line.
}

// ProcessListResp is the response for GET /api/v1/tasks/{id}/processes.
type ProcessListResp struct {
	Processes []ProcessInfo `json:"processes"`
}

// SignalProcessReq is the request body for POST /api/v1/tasks/{id}/processes/{pid}/signal.
type SignalProcessReq struct {
	Signal string `json:"signal"` // "SIGTERM" or "SIGKILL"
	PID    int    `json:"-"      path:"pid"`
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

// MountMappingResp represents a general host-to-runtime directory mount.
type MountMappingResp struct {
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
	// BaseImage overrides the default runtime base image. Empty means use
	// the default.
	BaseImage string `json:"baseImage,omitempty"`
	// MaxCPUs limits the number of CPU cores the runtime instance may use.
	// Zero means use the system default (max(2, NumCPU-2)).
	MaxCPUs int `json:"maxCPUs,omitempty"`
	// UseDefaultCaches controls whether default harness caches are mounted.
	// When false, only custom cache mappings and custom mounts are used.
	UseDefaultCaches bool `json:"useDefaultCaches"`
	// WellKnownCaches maps cache name to enabled state. nil means use default
	// (all true), true means explicitly enabled, false means explicitly disabled.
	WellKnownCaches map[string]bool `json:"wellKnownCaches,omitempty"`
	// CacheMappings are custom host-to-runtime directory mappings.
	CacheMappings []CacheMappingResp `json:"cacheMappings,omitempty"`
	// CustomMounts are custom non-cache host-to-runtime directory mappings.
	CustomMounts []MountMappingResp `json:"customMounts,omitempty"`
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

	settingsSet bool
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

// BranchInfo describes a single branch with its origin.
type BranchInfo struct {
	Name   string `json:"name"`
	Remote string `json:"remote,omitempty"`
}

// RepoBranchesResp is the response for GET /api/v1/server/repos/branches.
type RepoBranchesResp struct {
	Branches []BranchInfo `json:"branches"`
}

// WellKnownCache describes a single well-known cache.
type WellKnownCache struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Mounts      []string `json:"mounts"` // List of runtime mount paths
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

// VersionResp is the response for GET /api/v1/server/version.
type VersionResp struct {
	Current      string `json:"current"`
	Latest       string `json:"latest,omitempty"`     // empty when check failed
	UpdateAvail  bool   `json:"updateAvailable"`      // true when Latest > Current
	CheckError   string `json:"checkError,omitempty"` // non-empty when the GitHub check failed
	AutoUpdateOn bool   `json:"autoUpdateEnabled"`    // true when autoupdate schedule is configured
}

// UpdateResp is the response for POST /api/v1/server/update.
type UpdateResp struct {
	Status string `json:"status"` // "started" or "already_up_to_date"
}

// EmptyReq is used for endpoints that take no request body.
type EmptyReq = api.EmptyReq

// ErrorResponse is the JSON envelope for error responses.
type ErrorResponse = api.ErrorResponse
