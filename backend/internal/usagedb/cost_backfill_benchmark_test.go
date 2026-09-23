// Benchmarks one-pass missing-cost repair of an existing usage day file.

package usagedb

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func BenchmarkBackfillMissingCosts(b *testing.B) {
	const day = "2026-02-05"
	var fixture bytes.Buffer
	for i := range 1000 {
		row := UsageRow{Kind: rowKindUsage, Day: day, TaskID: strconv.Itoa(i), Model: "priced", Output: 1000}
		if err := json.NewEncoder(&fixture).Encode(row); err != nil {
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
		s, err := New(Config{Log: testLogger(), Dir: dir})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := s.BackfillMissingCosts(b.Context(), func(*UsageRow) (float64, bool) { return 0.01, true }); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
