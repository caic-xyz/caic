// Local task cost aggregation for usage reporting.
package server

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

// computeClaudeUsage aggregates task cost and token usage within rolling
// 5-hour and 7-day windows. Tasks are attributed to the window that contains
// their StartedAt time. For running tasks without a final result, the current
// live stats are used.
func computeClaudeUsage(tasks map[string]*taskEntry, now time.Time) v1.ClaudeUsage {
	cutoff5h := now.Add(-5 * time.Hour)
	cutoff7d := now.Add(-7 * 24 * time.Hour)

	var out v1.ClaudeUsage
	for _, e := range tasks {
		if e.task.StartedAt.IsZero() || !e.task.StartedAt.After(cutoff7d) {
			continue
		}
		var costUSD float64
		var usage agent.Usage
		if e.result != nil {
			costUSD = e.result.CostUSD
			usage = e.result.Usage
		} else {
			costUSD, _, _, usage, _ = e.task.LiveStats()
		}
		total := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
		out.SevenDay.CostUSD += costUSD
		out.SevenDay.InputTokens += total
		out.SevenDay.OutputTokens += usage.OutputTokens
		if e.task.StartedAt.After(cutoff5h) {
			out.FiveHour.CostUSD += costUSD
			out.FiveHour.InputTokens += total
			out.FiveHour.OutputTokens += usage.OutputTokens
		}
	}
	return out
}
