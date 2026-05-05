// Local task cost aggregation for usage reporting. Computes rolling-window
// cost and token sums across all tasks, regardless of harness or provider. Computes rolling-window
// cost and token sums across all tasks, regardless of harness or provider.
package server

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

// localWindows defines the rolling time windows for local cost aggregation.
var localWindows = []struct {
	duration time.Duration
	label    string
}{
	{1 * time.Hour, "1h"},
	{6 * time.Hour, "6h"},
	{24 * time.Hour, "24h"},
}

// computeLocalUsage aggregates task cost and token usage within rolling
// time windows across all tasks. For running tasks without a final result,
// the current live stats are used.
func computeLocalUsage(tasks map[string]*taskEntry, now time.Time) v1.LocalUsage {
	out := v1.LocalUsage{
		Windows: make([]v1.LocalWindow, len(localWindows)),
	}
	for i, w := range localWindows {
		out.Windows[i] = v1.LocalWindow{Duration: w.label}
	}
	for _, e := range tasks {
		if e.task.StartedAt.IsZero() {
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
		totalInput := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
		for i, w := range localWindows {
			if e.task.StartedAt.After(now.Add(-w.duration)) {
				out.Windows[i].CostUSD += costUSD
				out.Windows[i].InputTokens += totalInput
				out.Windows[i].OutputTokens += usage.OutputTokens
			}
		}
	}
	return out
}
