// Task-side usage rollup contract: the RollupSink interface, DiscardRollup, and the agent-to-event translation.
//
// The sink owns accumulation, day bucketing, and flush timing; the task
// only translates timeline messages into rollup events and forwards them.

package task

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

// RollupSink consumes task messages for the cross-task usage rollup. Tasks
// always carry a non-nil sink: NewTask defaults to DiscardRollup and the task
// manager replaces it with the wired implementation.
type RollupSink interface {
	// Observe records one translated rollup event. Called with the task
	// mutex held, so implementations must not call back into the task and
	// should keep per-call work bounded.
	Observe(meta usagedb.TaskMeta, e *usagedb.Event)
	// ObserveQuota records one provider quota-window status change.
	ObserveQuota(c *usagedb.QuotaChange)
	// Close flushes all pending deltas and releases resources.
	Close() error
}

// DiscardRollup ignores every rollup record. NewTask defaults to it so tasks
// are always safe to fold without a wired implementation; the task manager
// replaces it with the real sink at construction.
type DiscardRollup struct{}

// Observe implements RollupSink by discarding the event.
func (DiscardRollup) Observe(usagedb.TaskMeta, *usagedb.Event) {}

// ObserveQuota implements RollupSink by discarding the change.
func (DiscardRollup) ObserveQuota(*usagedb.QuotaChange) {}

// Close implements RollupSink with no resources to release.
func (DiscardRollup) Close() error { return nil }

// rollupEvent translates one agent message into a rollup event. The bool is
// false for messages the rollup does not count.
//
// Token sources are disjoint per harness so no call is counted twice:
// Claude, Codex, and OpenCode put the authoritative turn total on the result
// record (Claude's per-call assistant records arrive duplicated, and Codex's
// result copies its accumulated per-call usage), while Pi's turn total
// arrives as the turn-end UsageMessage and its result carries only the last
// call's usage. Non-token fields (turns, durations, context window) are
// counted from every record that reports them.
func rollupEvent(m agent.Message, at time.Time, model string, costUSD float64, h harness.Name) (usagedb.Event, bool) {
	e := usagedb.Event{At: at, Model: model, CostUSD: costUSD}
	switch m := m.(type) {
	case *agent.UsageMessage:
		if h == harness.Pi {
			e.Delta.TokenBuckets = rollupTokens(m.Usage)
		}
		e.Delta.ContextWindow = m.ContextWindow
	case *agent.ResultMessage:
		if h != harness.Pi {
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
// buckets. Cache writes land in the one-hour bucket when the harness
// reported a 3600s effective TTL, else in the five-minute bucket (unknown
// TTLs included).
func rollupTokens(u agent.Usage) usagedb.TokenBuckets {
	b := usagedb.TokenBuckets{
		Input:     int64(u.InputTokens),
		Output:    int64(u.OutputTokens),
		CacheRead: int64(u.CacheReadInputTokens),
		Reasoning: int64(u.ReasoningOutputTokens),
	}
	if u.CacheTTLSeconds >= 3600 {
		b.CacheWrite1h = int64(u.CacheCreationInputTokens)
	} else {
		b.CacheWrite5m = int64(u.CacheCreationInputTokens)
	}
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
