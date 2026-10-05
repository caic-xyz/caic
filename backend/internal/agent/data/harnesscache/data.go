// Package harnesscache defines the on-disk per-harness model inventory cache.
package harnesscache

import "time"

// Entry contains the inventory and freshness metadata for one harness.
type Entry struct {
	Inventory ModelInventory `json:"inventory"`
	Updated   time.Time      `json:"updated"`
	EnvHash   string         `json:"env_hash,omitempty"` // SHA-256 of *_API_KEY env vars from config.toml
}

// Model describes persisted model configuration choices.
type Model struct {
	ID            string   `json:"id"`
	EffortOptions []string `json:"effortOptions"`
	// ContextWindow is the model's context window size in tokens. It is 0 when
	// the harness does not publish one; the runtime usage the agent reports then
	// becomes the only source of the limit.
	ContextWindow int `json:"contextWindow,omitempty"`
}

// ModelInventory contains the persisted models for a harness.
type ModelInventory struct {
	Models []Model `json:"models"`
}
