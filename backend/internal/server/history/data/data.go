// Package data defines the private, rebuildable history pricing journal schema.
package data

import (
	"time"
)

// Kind identifies the pricing observation retained from an agent message.
type Kind string

// Kind values retained by the pricing journal.
const (
	KindExit   Kind = "exit"
	KindInit   Kind = "init"
	KindResult Kind = "result"
	KindSystem Kind = "system"
	KindUsage  Kind = "usage"
)

// Source preserves canonical ownership without importing another schema or runtime.
type Source struct {
	AgentID             string `json:"agentID,omitempty"`
	DelegationToolUseID string `json:"delegationToolUseID,omitempty"`
	ThreadID            string `json:"threadID,omitempty"`
	TurnID              string `json:"turnID,omitempty"`
	ItemID              string `json:"itemID,omitempty"`
	RootThreadID        string `json:"rootThreadID,omitempty"`
}

// CacheWrite preserves a disjoint cache-write bucket and its applied lifetime.
// TTL is encoded in nanoseconds; zero retains an unknown duration.
type CacheWrite struct {
	TTL    time.Duration `json:"ttl,omitempty"`
	Tokens int64         `json:"tokens"`
}

// Usage preserves request counters and duration buckets without agent runtime types.
type Usage struct {
	InputTokens              int          `json:"inputTokens,omitempty"`
	OutputTokens             int          `json:"outputTokens,omitempty"`
	CacheCreationInputTokens int          `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens     int          `json:"cacheReadInputTokens,omitempty"`
	ReasoningOutputTokens    int          `json:"reasoningOutputTokens,omitempty"`
	CacheTTLSeconds          int          `json:"cacheTTLSeconds,omitempty"`
	CacheWrites              []CacheWrite `json:"cacheWrites,omitempty"`
}

// Fact contains only inputs consumed by the authoritative pricing fold. Index
// identifies the normalized output when Kind is Result; controls need no output.
type Fact struct {
	RecordID          uint64    `json:"recordID"`
	Index             uint32    `json:"index,omitempty"`
	At                time.Time `json:"at"`
	Source            Source    `json:"source,omitzero"`
	Kind              Kind      `json:"kind"`
	Model             string    `json:"model,omitempty"`
	Subtype           string    `json:"subtype,omitempty"`
	Usage             Usage     `json:"usage,omitzero"`
	PerCall           bool      `json:"perCall,omitempty"`
	ModelDerived      bool      `json:"modelDerived,omitempty"`
	CumulativeCostUSD *float64  `json:"cumulativeCostUSD,omitempty"`
	ReportedCostUSD   *float64  `json:"reportedCostUSD,omitempty"`
	TotalCostUSD      float64   `json:"totalCostUSD,omitempty"`
}
