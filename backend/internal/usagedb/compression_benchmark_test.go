// Benchmarks usage-day compression and streaming restart recovery.

package usagedb

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
)

func BenchmarkCompressOldDays(b *testing.B) {
	const day = "2026-02-05"
	now := time.Date(2026, time.February, 10, 12, 0, 0, 0, time.UTC)
	var fixture bytes.Buffer
	for i := range 1000 {
		row := data.UsageRow{
			Kind: rowKindUsage, Day: day, TaskID: strconv.Itoa(i),
			Model: "model-" + strconv.Itoa(i%17),
			Ts:    data.Time(1_770_000_000_000 + int64(i)*1_000),
			Input: int64(i * 3), CacheRead: int64(i * 7), Output: int64(i + 1),
		}
		if err := json.NewEncoder(&fixture).Encode(&row); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(int64(fixture.Len()))
	b.ReportAllocs()
	root := b.TempDir()
	var compressedBytes int64
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
		ran, err := s.tryCompressOldDays(now)
		if err != nil || !ran {
			b.Fatalf("compress old days = %v/%v, want completed", ran, err)
		}
		b.StopTimer()
		info, err := os.Stat(filepath.Join(dir, day+compressedDaySuffix))
		if err != nil {
			b.Fatal(err)
		}
		compressedBytes = info.Size()
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(compressedBytes)/float64(fixture.Len()), "compressed/raw")
}

func BenchmarkUsageDayRecovery(b *testing.B) {
	for _, n := range []int{1000, 100000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.StopTimer()
			const day = "2030-02-05"
			dir := b.TempDir()
			f, err := os.Create(filepath.Join(dir, day+".jsonl")) //nolint:gosec // benchmark fixture inside b.TempDir().
			if err != nil {
				b.Fatal(err)
			}
			enc := json.NewEncoder(f)
			row := data.UsageRow{Kind: rowKindUsage, Day: day, TaskID: "same", Model: "priced", Harness: "claude", Output: 1000}
			for range n {
				if err := enc.Encode(&row); err != nil {
					b.Fatal(err)
				}
			}
			info, err := f.Stat()
			if err != nil {
				b.Fatal(err)
			}
			if err := f.Close(); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(info.Size())
			b.ReportAllocs()
			b.StartTimer()
			for range b.N {
				s, err := New(b.Context(), Config{Log: testLogger(), Dir: dir})
				if err != nil {
					b.Fatal(err)
				}
				if err := s.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
