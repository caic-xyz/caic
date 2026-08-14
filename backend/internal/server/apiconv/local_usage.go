// Local task cost aggregation for usage reporting.

package apiconv

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
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

// LocalUsageInput is a task's resolved local-usage contribution.
type LocalUsageInput struct {
	StartedAt time.Time
	CostUSD   float64
	Usage     agent.Usage
}

// LocalUsage aggregates resolved task cost and token usage within rolling time
// windows.
func LocalUsage(inputs []LocalUsageInput, now time.Time) v1.LocalUsage {
	out := v1.LocalUsage{
		Windows: make([]v1.LocalWindow, len(localUsageWindows)),
	}
	for i, w := range localUsageWindows {
		out.Windows[i] = v1.LocalWindow{Duration: w.label}
	}
	for _, input := range inputs {
		if input.StartedAt.IsZero() {
			continue
		}
		usage := input.Usage
		totalInput := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
		for i, w := range localUsageWindows {
			if input.StartedAt.After(now.Add(-w.duration)) {
				out.Windows[i].CostUSD += input.CostUSD
				out.Windows[i].InputTokens += totalInput
				out.Windows[i].OutputTokens += usage.OutputTokens
			}
		}
	}
	return out
}
