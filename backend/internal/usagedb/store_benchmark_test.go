// Benchmarks event ingestion through a durable usage-row flush.

package usagedb

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
)

// BenchmarkObserveTurnFlush measures four live events per completed turn,
// including pending-fold accounting, row encoding, appending to an open day
// file, and aggregate/watermark updates. Eight retained tasks keep aggregate
// cardinality bounded. Initial file creation and final validation are untimed.
func BenchmarkObserveTurnFlush(b *testing.B) {
	const day = "2030-02-05" // Future day prevents maintenance from sealing the active file.
	at := time.Date(2030, time.February, 5, 10, 0, 0, 0, time.UTC)
	dir := b.TempDir()
	s, err := New(b.Context(), Config{Log: testLogger(), Dir: dir})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	const tasks = 8
	var metas [tasks]TaskMeta
	var costs [tasks]float64
	for i := range metas {
		metas[i] = testMeta(ksid.ID(i + 1))
		metas[i].Repos = []string{"github/caic", "github/sdk"}
	}
	e := Event{Model: "claude-opus", Delta: data.Delta{
		TokenBuckets: data.TokenBuckets{Input: 100, CacheRead: 200, Output: 50, Reasoning: 10},
		APIMs:        1500, WallMs: 3000, ContextWindow: 200000,
		SkillReads:  map[string]int{"review": 1},
		ToolCalls:   map[string]int{"Read": 2},
		ToolTimings: map[string]data.ToolTiming{"Read": {Count: 2, DurationMs: 250}},
	}}
	turn := 0
	observeTurn := func() {
		i := turn % tasks
		for stage := range 4 {
			e.At = at.Add(time.Duration(turn*4+stage+1) * time.Millisecond)
			costs[i] += .01
			e.CostUSD = costs[i]
			e.TurnBoundary = stage == 3
			e.Delta.Turns = 0
			if e.TurnBoundary {
				e.Delta.Turns = 1
			}
			s.Observe(metas[i], &e)
		}
		turn++
	}
	for range tasks {
		observeTurn()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		observeTurn()
	}
	b.StopTimer()
	if err := s.Close(); err != nil {
		b.Fatal(err)
	}
	// Verify the measured path reached durable storage rather than silently
	// dropping observations; decode incrementally to avoid whole-log buffers.
	f, err := os.Open(filepath.Join(dir, day+".jsonl")) //nolint:gosec // Benchmark-owned temporary directory.
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = f.Close() })
	dec := json.NewDecoder(f)
	rows := 0
	for {
		var row data.UsageRow
		if err := dec.Decode(&row); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			b.Fatal(err)
		}
		if row.Kind != "usage" || row.Turns != 1 || row.Output != 200 || row.ToolTimings["Read"].DurationMs != 1000 {
			b.Fatalf("invalid flushed row: %+v", row)
		}
		rows++
	}
	if rows != b.N+tasks {
		b.Fatalf("persisted rows = %d, want %d", rows, b.N+tasks)
	}
}
