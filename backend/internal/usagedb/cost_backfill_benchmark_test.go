// Benchmarks one-pass missing-cost repair of an existing usage day file.

package usagedb

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
)

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
