// Translates ordered task messages into usage rollup events.

package taskrun

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
)

// rollupEvent translates one agent message into a rollup event. The bool is
// false for messages the rollup does not count.
//
// Adapters identify additive usage and context-only result snapshots so live
// and restored rollups count the same token contributions for every harness.
func rollupEvent(m agent.Message, at, producerAt time.Time, replayed bool, model string, costUSD float64) (usagedb.Event, bool) {
	e := usagedb.Event{At: at, Replayed: replayed, Model: model, CostUSD: costUSD}
	switch m := m.(type) {
	case *agent.UsageMessage:
		if m.Additive {
			e.Delta.TokenBuckets = rollupTokens(m.Usage)
		}
		if !m.Source.IsChild() {
			e.Delta.ContextWindow = m.ContextWindow
		}
	case *agent.ResultMessage:
		if m.Source.IsChild() {
			return e, false
		}
		if !m.UsageIsSnapshot {
			e.Delta.TokenBuckets = rollupTokens(m.Usage)
		}
		e.Delta.Turns++
		if m.IsError {
			e.Delta.ErroredTurns++
		}
		e.Delta.APIMs = m.DurationAPIMs
		e.Delta.WallMs = m.DurationMs
		e.Delta.ContextWindow = m.ContextWindow
		// Turn boundary: the sink flushes so a crash never loses
		// completed-turn usage.
		e.TurnBoundary = true
	case *agent.SystemMessage:
		if m.Subtype != "compact_boundary" {
			return e, false
		}
		e.Delta.Compactions++
	case *agent.SkillReadMessage:
		if m.Skill == "" {
			return e, false
		}
		e.Delta.SkillReads = map[string]int{m.Skill: 1}
	case *agent.ToolUseMessage:
		e.Delta.ToolCalls = map[string]int{m.Name: 1}
		key := agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)
		e.ToolStartID = key.String()
		e.ToolName = m.Name
		e.ToolProducerTime = producerAt
	case *agent.ToolResultMessage:
		if m.ToolUseID == "" {
			return e, false
		}
		key := agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)
		e.ToolResultID = key.String()
		e.ToolNativeDurationMs = m.DurationMs
		e.ToolProducerTime = producerAt
	case *agent.NativeSubagentMessage:
		e.Delta.Spawns++
		if m.Subagent.Background {
			e.Delta.SpawnsBackground++
		}
	default:
		return e, false
	}
	return e, true
}

// rollupTokens splits one harness usage report into the disjoint token
// buckets, retaining mixed and unknown cache lifetimes.
func rollupTokens(u agent.Usage) data.TokenBuckets {
	b := data.TokenBuckets{
		Input:     int64(u.InputTokens),
		Output:    int64(u.OutputTokens),
		CacheRead: int64(u.CacheReadInputTokens),
		Reasoning: int64(u.ReasoningOutputTokens),
	}
	b.CacheWrites = usagedb.CacheWritesToData(u.CacheWriteBuckets())
	return b
}

// quotaChange translates one rate-limit message into a quota change, falling
// back to the harness-native window id when the canonical window is unknown.
func quotaChange(m *agent.RateLimitMessage, at time.Time) usagedb.QuotaChange {
	window := m.QuotaWindow
	if window == "" {
		window = m.RateLimitType
	}
	return usagedb.QuotaChange{
		At:          at,
		Provider:    string(m.QuotaProvider),
		Window:      window,
		Status:      string(m.Status),
		Utilization: m.Utilization,
		ResetsAt:    m.ResetsAt,
	}
}
