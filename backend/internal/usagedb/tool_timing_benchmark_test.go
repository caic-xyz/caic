// Benchmarks matching a tool result to its start during usage ingestion.

package usagedb

import (
	"testing"
	"time"
)

func BenchmarkToolTimingTracker(b *testing.B) {
	var tracker ToolTimingTracker
	at := time.Date(2026, time.February, 5, 10, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	for range b.N {
		tracker.Start("tool-1", "Read", at)
		tracker.Finish("tool-1", at.Add(time.Second), 0)
	}
}
