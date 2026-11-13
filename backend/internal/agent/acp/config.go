// ACP configuration separates protocol mechanics from harness-specific model routes, permissions, and tool events.

package acp

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/acp"
)

// Config selects native launch and protocol behavior for one harness.
//
// Immutable.
type Config struct {
	// Args is the native executable and argument vector passed to the relay.
	Args []string
	// Env holds KEY=VALUE pairs the relay sets in the native process
	// environment, replacing inherited values.
	Env []string
	// ResumeMethod selects the native method for reopening persisted sessions.
	ResumeMethod acp.Method
	// EffortID is the native reasoning-effort configuration option ID.
	EffortID acp.ConfigOptionID
	// Images permits image encoding when ACP also advertises image support.
	Images bool
	// CompactPrompt is the verified native compaction command; empty disables it.
	// Custom compaction instructions are not supported.
	CompactPrompt string
	// Models translates native routes; nil leaves model IDs unchanged.
	Models ModelCodec
	// NewTools creates fresh per-wire native evidence state; nil adds no evidence.
	NewTools func() ToolParser
	// Permissions answers native permission requests; nil leaves requests raw.
	Permissions agent.MessageInterceptor
}

// ModelCodec preserves harness-native model routes at the protocol boundary.
type ModelCodec interface {
	Select(requested, current string, options []acp.ConfigOptionValue) (string, error)
	Decode(route string) (string, error)
}

// ToolParser adds harness-owned evidence to normalized ACP tool events.
// A fresh parser belongs to each wire. Shared ACP deduplicates inferred skill
// reads by active tool-call ID and settles them at turn completion.
type ToolParser interface {
	Parse(tool *acp.ToolCallUpdateUpdate, messages []agent.Message) ([]agent.Message, error)
}
