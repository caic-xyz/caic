// Local task cost aggregation for usage reporting.

package v1conv

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// localUsageWindows defines the rolling time windows for local cost aggregation.
var localUsageWindows = []struct {
	duration time.Duration
	label    string
}{
	{1 * time.Hour, "1h"},
	{6 * time.Hour, "6h"},
	{24 * time.Hour, "24h"},
}

// LocalUsage aggregates task cost and token usage within rolling time windows
// across all tasks.
//
// For running tasks without a final result, the current live stats are used.
func LocalUsage(mgr *taskmgr.Manager, now time.Time) v1.LocalUsage {
	out := v1.LocalUsage{
		Windows: make([]v1.LocalWindow, len(localUsageWindows)),
	}
	for i, w := range localUsageWindows {
		out.Windows[i] = v1.LocalWindow{Duration: w.label}
	}
	mgr.Range(func(_ string, e *taskmgr.Entry) bool {
		t := e.Task()
		if t.StartedAt.IsZero() {
			return true
		}
		var costUSD float64
		var usage agent.Usage
		if r := e.Result(); r != nil {
			costUSD = r.CostUSD
			usage = r.Usage
		} else {
			costUSD, _, _, usage, _ = t.LiveStats()
		}
		totalInput := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
		for i, w := range localUsageWindows {
			if t.StartedAt.After(now.Add(-w.duration)) {
				out.Windows[i].CostUSD += costUSD
				out.Windows[i].InputTokens += totalInput
				out.Windows[i].OutputTokens += usage.OutputTokens
			}
		}
		return true
	})
	return out
}
