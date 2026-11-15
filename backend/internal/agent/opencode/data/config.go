// Package data defines native OpenCode launch configuration owned by its adapter.
package data

// Config is the per-process OpenCode configuration overlay.
type Config struct {
	Agent      AgentConfig       `json:"agent"`
	Compaction *CompactionConfig `json:"compaction,omitempty"`
}

// AgentConfig holds hidden native-agent configuration.
type AgentConfig struct {
	Title TitleConfig `json:"title"`
}

// TitleConfig disables OpenCode-generated titles because caic owns task titles.
type TitleConfig struct {
	Disable bool `json:"disable"`
}

// CompactionConfig carries only explicitly configured native budgets.
type CompactionConfig struct {
	Buffer *int        `json:"buffer,omitempty"`
	Keep   *KeepConfig `json:"keep,omitempty"`
}

// KeepConfig controls history retained alongside a summary.
type KeepConfig struct {
	Tokens int `json:"tokens"`
}
