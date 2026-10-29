// Benchmarks event ingestion through a durable usage-row flush.

package usagedb

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

func BenchmarkBackfillMissingCosts(b *testing.B) {
	const day = "2026-02-05"
	var fixture bytes.Buffer
	for i := range 1000 {
		row := data.UsageRow{Kind: rowKindUsage, Day: day, TaskID: strconv.Itoa(i), Model: "priced", Output: 1000}
		if err := json.NewEncoder(&fixture).Encode(&row); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(int64(fixture.Len()))
	b.ReportAllocs()
	root := b.TempDir()
	for i := range b.N {
		b.StopTimer()
		dir := filepath.Join(root, strconv.Itoa(i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), fixture.Bytes(), 0o600); err != nil {
			b.Fatal(err)
		}
		s, err := New(b.Context(), Config{Log: testLogger(), Dir: dir})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := s.BackfillMissingCosts(b.Context(), func(*data.UsageRow) (float64, bool) { return 0.01, true }); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBackfillMissingCostsLiveHeap samples post-GC live heap at estimator
// calls, where the source and previously changed rows remain reachable. Repeated
// task identity keeps required aggregate cardinality constant as input grows.
// GC sampling is intentionally outside the throughput benchmark above.
func BenchmarkBackfillMissingCostsLiveHeap(b *testing.B) {
	for _, n := range []int{1000, 100000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			const day = "2026-02-05"
			dir := b.TempDir()
			path := filepath.Join(dir, day+".jsonl")
			for range b.N {
				b.StopTimer()
				f, err := os.Create(path) //nolint:gosec // benchmark fixture inside b.TempDir().
				if err != nil {
					b.Fatal(err)
				}
				enc := json.NewEncoder(f)
				row := data.UsageRow{Kind: rowKindUsage, Day: day, TaskID: "same", Model: "priced", Output: 1000}
				for range n {
					if err := enc.Encode(&row); err != nil {
						b.Fatal(err)
					}
				}
				if err := f.Close(); err != nil {
					b.Fatal(err)
				}
				s, err := New(b.Context(), Config{Log: testLogger(), Dir: dir})
				if err != nil {
					b.Fatal(err)
				}
				runtime.GC()
				var before, sample runtime.MemStats
				runtime.ReadMemStats(&before)
				peak := before.HeapAlloc
				calls := 0
				b.StartTimer()
				err = s.BackfillMissingCosts(b.Context(), func(*data.UsageRow) (float64, bool) {
					calls++
					if calls == 1 || calls%1024 == 0 || calls == n {
						runtime.GC()
						runtime.ReadMemStats(&sample)
						peak = max(peak, sample.HeapAlloc)
					}
					return .01, true
				})
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(peak-before.HeapAlloc), "live-heap-B")
				if err := s.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
