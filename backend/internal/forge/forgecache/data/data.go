// Package data defines the on-disk CI results and notification cache.
package data

import "time"

// CheckRunStatus is the persisted CI discriminator.
type CheckRunStatus string

// Persisted CheckRunStatus values.
const (
	CheckRunStatusCompleted  CheckRunStatus = "completed"
	CheckRunStatusInProgress CheckRunStatus = "in_progress"
	CheckRunStatusQueued     CheckRunStatus = "queued"
)

// CheckRunConclusion is the persisted CI discriminator.
type CheckRunConclusion string

// Persisted CheckRunConclusion values.
const (
	CheckRunConclusionActionRequired CheckRunConclusion = "action_required"
	CheckRunConclusionCancelled      CheckRunConclusion = "cancelled"
	CheckRunConclusionFailure        CheckRunConclusion = "failure"
	CheckRunConclusionNeutral        CheckRunConclusion = "neutral"
	CheckRunConclusionSkipped        CheckRunConclusion = "skipped"
	CheckRunConclusionStale          CheckRunConclusion = "stale"
	CheckRunConclusionSuccess        CheckRunConclusion = "success"
	CheckRunConclusionTimedOut       CheckRunConclusion = "timed_out"
)

// CIStatus is the persisted CI discriminator.
type CIStatus string

// Persisted CIStatus values.
const (
	CIStatusFailure CIStatus = "failure"
	CIStatusNone    CIStatus = ""
	CIStatusPending CIStatus = "pending"
	CIStatusSuccess CIStatus = "success"
)

// Check contains a persisted fully-qualified CI check run.
type Check struct {
	Name        string             `json:"name"`
	Owner       string             `json:"owner"`
	Repo        string             `json:"repo"`
	RunID       int64              `json:"runID"`
	JobID       int64              `json:"jobID"`
	Status      CheckRunStatus     `json:"status"`
	Conclusion  CheckRunConclusion `json:"conclusion"`
	Labels      []string           `json:"labels,omitempty"`
	QueuedAt    time.Time          `json:"queuedAt,omitzero"`
	StartedAt   time.Time          `json:"startedAt,omitzero"`
	CompletedAt time.Time          `json:"completedAt,omitzero"`
}

// Result contains the persisted outcome for a commit.
type Result struct {
	Status   CIStatus  `json:"status"`
	Checks   []Check   `json:"checks,omitempty"`
	CachedAt time.Time `json:"cachedAt"`
}

// File contains results and notification deduplication timestamps.
type File struct {
	Results  map[string]Result    `json:"results"`
	Notified map[string]time.Time `json:"notified,omitempty"`
}
