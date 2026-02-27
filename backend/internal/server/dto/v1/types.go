// Exported request and response types for the caic API.
package v1

import (
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	"github.com/maruel/ksid"
)

//go:generate go tool tygo generate --config ../../../../../backend/tygo.yaml
//go:generate go run github.com/caic-xyz/caic/backend/internal/cmd/gen-api-sdk

// Harness identifies the coding agent harness.
// Values must match agent.Harness constants.
type Harness string

// Supported agent harnesses.
const (
	HarnessClaude Harness = "claude"
	HarnessCodex  Harness = "codex"
	HarnessGemini Harness = "gemini"
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
	TailscaleAvailable bool `json:"tailscaleAvailable"`
	USBAvailable       bool `json:"usbAvailable"`
	DisplayAvailable   bool `json:"displayAvailable"`
}

// Repo is the JSON representation of a discovered repo.
type Repo struct {
	Path       string `json:"path"`
	BaseBranch string `json:"baseBranch"`
	RepoURL    string `json:"repoURL,omitempty"`
}

// Task is the JSON representation sent to the frontend.
type Task struct {
	ID                                 ksid.ID  `json:"id"`
	InitialPrompt                      string   `json:"initialPrompt"`
	Title                              string   `json:"title"`
	Repo                               string   `json:"repo"`
	RepoURL                            string   `json:"repoURL,omitempty"`
	BaseBranch                         string   `json:"baseBranch,omitempty"` // branch the task was forked from
	Branch                             string   `json:"branch"`
	Container                          string   `json:"container"`
	State                              string   `json:"state"`
	StateUpdatedAt                     float64  `json:"stateUpdatedAt"` // Unix epoch seconds (ms precision) of last state change.
	DiffStat                           DiffStat `json:"diffStat,omitzero"`
	CostUSD                            float64  `json:"costUSD"`
	Duration                           float64  `json:"duration"` // Seconds.
	NumTurns                           int      `json:"numTurns"`
	CumulativeInputTokens              int      `json:"cumulativeInputTokens"`
	CumulativeOutputTokens             int      `json:"cumulativeOutputTokens"`
	CumulativeCacheCreationInputTokens int      `json:"cumulativeCacheCreationInputTokens"`
	CumulativeCacheReadInputTokens     int      `json:"cumulativeCacheReadInputTokens"`
	ActiveInputTokens                  int      `json:"activeInputTokens"`     // Last turn's non-cached input tokens (including cache creation).
	ActiveCacheReadTokens              int      `json:"activeCacheReadTokens"` // Last turn's cache-read input tokens.
	ContextWindowLimit                 int      `json:"contextWindowLimit"`    // Model context window limit (tokens).
	Error                              string   `json:"error,omitempty"`
	Result                             string   `json:"result,omitempty"`
	// Per-task harness/container metadata.
	Harness           Harness `json:"harness"`
	Model             string  `json:"model,omitempty"`
	AgentVersion      string  `json:"agentVersion,omitempty"`
	SessionID         string  `json:"sessionID,omitempty"`
	ContainerUptimeMs int64   `json:"containerUptimeMs,omitempty"`
	InPlanMode        bool    `json:"inPlanMode,omitempty"`
	PlanContent       string  `json:"planContent,omitempty"`
	Tailscale         string  `json:"tailscale,omitempty"` // Tailscale URL (https://fqdn) or "true" if enabled but FQDN unknown.
	USB               bool    `json:"usb,omitempty"`
	Display           bool    `json:"display,omitempty"`
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

// CreateTaskReq is the request body for POST /api/v1/tasks.
type CreateTaskReq struct {
	InitialPrompt Prompt  `json:"initialPrompt"`
	Repo          string  `json:"repo"`
	BaseBranch    string  `json:"baseBranch,omitempty"` // branch to fork from; defaults to repo's default branch
	Model         string  `json:"model,omitempty"`
	Harness       Harness `json:"harness"`
	Image         string  `json:"image,omitempty"`
	Tailscale     bool    `json:"tailscale,omitempty"`
	USB           bool    `json:"usb,omitempty"`
	Display       bool    `json:"display,omitempty"`
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
}

// UsageWindow represents a single quota window (5-hour or 7-day).
type UsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resetsAt"`
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
	BaseImage  string `json:"baseImage,omitempty"`
}

// PreferencesResp is the response for GET /api/v1/server/preferences.
type PreferencesResp struct {
	Repositories []RepoPrefsResp   `json:"repositories"`
	Harness      string            `json:"harness,omitempty"`
	Models       map[string]string `json:"models,omitempty"`
	BaseImage    string            `json:"baseImage,omitempty"`
}

// CloneRepoReq is the request body for POST /api/v1/server/repos.
type CloneRepoReq struct {
	URL   string `json:"url"`            // Git clone URL (HTTPS or SSH).
	Path  string `json:"path,omitempty"` // Target subdirectory under rootDir; defaults to repo basename.
	Depth int    `json:"depth,omitempty"`
}

// EmptyReq is used for endpoints that take no request body.
type EmptyReq = dto.EmptyReq
