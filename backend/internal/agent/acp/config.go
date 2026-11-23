// ACP protocol configuration selects harness-specific model routes, permissions, and tool events.

package acp

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/acp"
)

// Config selects protocol behavior for one harness.
//
// Immutable.
type Config struct {
	// ResumeMethod selects the native method for reopening persisted sessions.
	ResumeMethod acp.Method
	// AuthMethod is the advertised authMethods ID sent in an authenticate
	// request before session creation; empty sends none.
	AuthMethod string
	// EffortID is the native reasoning-effort configuration option ID.
	EffortID acp.ConfigOptionID
	// Images permits image encoding when ACP also advertises image support.
	Images bool
	// ImagesByPath sends images that are not encoded natively as resource_link
	// blocks to their agent.AttachmentPath, which the session writes first.
	ImagesByPath bool
	// CompactPrompt is the verified native compaction command; empty disables it.
	// Custom compaction instructions are not supported. A successful prompt of
	// exactly this text ends with a compaction boundary.
	CompactPrompt string
	// Models translates native routes; nil leaves model IDs unchanged.
	Models ModelCodec
	// NewTools creates fresh per-wire native evidence state; nil adds no evidence.
	NewTools func() ToolParser
	// Permissions answers native permission requests; nil leaves requests raw.
	Permissions agent.MessageInterceptor
	// VersionExecutable names the CLI whose --version Start records, for an
	// agent whose initialize agentInfo.version is not its release; that value
	// is then ignored. Empty trusts agentInfo.version.
	VersionExecutable string
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
	// Parse receives the session/update notification's sessionId, whether a
	// session/prompt awaits its response, and the tool update; tool is nil for
	// other records.
	Parse(sessionID string, prompted bool, tool *acp.ToolCallUpdateUpdate, messages []agent.Message) ([]agent.Message, error)
	// SettlePrompt returns the evidence a session/prompt response settles; it
	// precedes the turn result. stop is empty for a JSON-RPC error response.
	SettlePrompt(stop acp.StopReason) []agent.Message
}
