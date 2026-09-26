// Benchmarks folding usage rows into a day aggregate, the ingest hot path.

package usagedb

import (
	"testing"

	"github.com/maruel/ksid"
)

// BenchmarkFoldUsageRow measures foldUsageRow over a fresh day aggregate per
// iteration: tokens, tool calls and timings, skill reads, repos, and model
// plus harness attribution for a batch of distinct tasks.
func BenchmarkFoldUsageRow(b *testing.B) {
	rows := make([]*UsageRow, 0, 8)
	for i := range 8 {
		rows = append(rows, &UsageRow{
			Kind:    rowKindUsage,
			Day:     "2026-02-05",
			Ts:      NewTime(atUTC(5, 10, 0, i)),
			TaskID:  ksid.NewID().String(),
			Harness: "claude",
			Repos:   []string{"github/caic", "github/sdk"},
			Model:   "claude-opus",
			Input:   100, CacheRead: 200, Output: 50, Reasoning: 10,
			Turns:       1,
			APIMs:       1500,
			WallMs:      3000,
			SkillReads:  map[string]int{"code-review": 1},
			ToolCalls:   map[string]int{"Edit": 2, "Read": 3},
			ToolTimings: map[string]ToolTiming{"Edit": {Count: 2, DurationMs: 250}},
			Spawns:      1,
			CostUSD:     0.05,
		})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		day := newDayAggregate()
		for _, row := range rows {
			foldUsageRow(day, row)
		}
	}
}
