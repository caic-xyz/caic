// Benchmarks sealing a past usage day with zstd compression.

package usagedb

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func BenchmarkCompressOldDays(b *testing.B) {
	const day = "2026-02-05"
	now := time.Date(2026, time.February, 10, 12, 0, 0, 0, time.UTC)
	var fixture bytes.Buffer
	for i := range 1000 {
		row := UsageRow{
			Kind: rowKindUsage, Day: day, TaskID: strconv.Itoa(i),
			Model: "model-" + strconv.Itoa(i%17),
			Ts:    Time(1_770_000_000_000 + int64(i)*1_000),
			Input: int64(i * 3), CacheRead: int64(i * 7), Output: int64(i + 1),
		}
		if err := json.NewEncoder(&fixture).Encode(row); err != nil {
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
		s, err := New(Config{Log: testLogger(), Dir: dir})
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
