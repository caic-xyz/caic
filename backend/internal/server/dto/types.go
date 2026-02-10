// Exported request and response types for the wmao API.
package dto

import "github.com/maruel/ksid"

//go:generate go tool tygo generate --config ../../../../backend/tygo.yaml
//go:generate go run github.com/maruel/wmao/backend/internal/cmd/gen-api-client

// RepoJSON is the JSON representation of a discovered repo.
type RepoJSON struct {
	Path       string `json:"path"`
	BaseBranch string `json:"baseBranch"`
}

// TaskJSON is the JSON representation sent to the frontend.
type TaskJSON struct {
	ID               ksid.ID `json:"id"`
	Task             string  `json:"task"`
	Repo             string  `json:"repo"`
	Branch           string  `json:"branch"`
	Container        string  `json:"container"`
	State            string  `json:"state"`
	StateUpdatedAtMs int64   `json:"stateUpdatedAtMs"` // Unix millis UTC of last state change.
	DiffStat         string  `json:"diffStat"`
	CostUSD          float64 `json:"costUSD"`
	DurationMs       int64   `json:"durationMs"`
	NumTurns         int     `json:"numTurns"`
	Error            string  `json:"error,omitempty"`
	Result           string  `json:"result,omitempty"`
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
	Prompt string `json:"prompt"`
	Repo   string `json:"repo"`
}

// InputReq is the request body for POST /api/v1/tasks/{id}/input.
type InputReq struct {
	Prompt string `json:"prompt"`
}

// PullResp is the response for POST /api/v1/tasks/{id}/pull.
type PullResp struct {
	Status   string `json:"status"`
	DiffStat string `json:"diffStat"`
}

// EmptyReq is used for endpoints that take no request body.
type EmptyReq struct{}
