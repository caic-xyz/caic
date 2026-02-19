// Exported request and response types for the caic API.
package dto

import "github.com/maruel/ksid"

//go:generate go tool tygo generate --config ../../../../backend/tygo.yaml
//go:generate go run github.com/maruel/caic/backend/internal/cmd/gen-api-sdk

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
	Task                               string   `json:"task"`
	Repo                               string   `json:"repo"`
	RepoURL                            string   `json:"repoURL,omitempty"`
	Branch                             string   `json:"branch"`
	Container                          string   `json:"container"`
	State                              string   `json:"state"`
	StateUpdatedAt                     float64  `json:"stateUpdatedAt"` // Unix epoch seconds (ms precision) of last state change.
	DiffStat                           DiffStat `json:"diffStat,omitzero"`
	CostUSD                            float64  `json:"costUSD"`
	DurationMs                         int64    `json:"durationMs"`
	NumTurns                           int      `json:"numTurns"`
	CumulativeInputTokens              int      `json:"cumulativeInputTokens"`
	CumulativeOutputTokens             int      `json:"cumulativeOutputTokens"`
	CumulativeCacheCreationInputTokens int      `json:"cumulativeCacheCreationInputTokens"`
	CumulativeCacheReadInputTokens     int      `json:"cumulativeCacheReadInputTokens"`
	ActiveInputTokens                  int      `json:"activeInputTokens"`     // Last turn's non-cached input tokens (including cache creation).
	ActiveCacheReadTokens              int      `json:"activeCacheReadTokens"` // Last turn's cache-read input tokens.
	Error                              string   `json:"error,omitempty"`
	Result                             string   `json:"result,omitempty"`
	// Per-task harness/container metadata.
	Harness           Harness `json:"harness"`
	Model             string  `json:"model,omitempty"`
	AgentVersion      string  `json:"agentVersion,omitempty"`
	SessionID         string  `json:"sessionID,omitempty"`
	ContainerUptimeMs int64   `json:"containerUptimeMs,omitempty"`
	InPlanMode        bool    `json:"inPlanMode,omitempty"`
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
	Prompt    string      `json:"prompt"`
	Repo      string      `json:"repo"`
	Model     string      `json:"model,omitempty"`
	Harness   Harness     `json:"harness"`
	Image     string      `json:"image,omitempty"`
	Images    []ImageData `json:"images,omitempty"`
	Tailscale bool        `json:"tailscale,omitempty"`
	USB       bool        `json:"usb,omitempty"`
	Display   bool        `json:"display,omitempty"`
}

// InputReq is the request body for POST /api/v1/tasks/{id}/input.
type InputReq struct {
	Prompt string      `json:"prompt"`
	Images []ImageData `json:"images,omitempty"`
}

// RestartReq is the request body for POST /api/v1/tasks/{id}/restart.
type RestartReq struct {
	Prompt string `json:"prompt"`
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

// SyncReq is the request body for POST /api/v1/tasks/{id}/sync.
type SyncReq struct {
	Force bool `json:"force,omitempty"`
}

// SyncResp is the response for POST /api/v1/tasks/{id}/sync.
type SyncResp struct {
	Status       string        `json:"status"` // "synced", "blocked", or "empty"
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

// EmptyReq is used for endpoints that take no request body.
type EmptyReq struct{}
