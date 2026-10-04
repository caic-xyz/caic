// Tests sealed usage days, late writes, and restart recovery across compression.

package usagedb

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/maruel/ksid"
)

func TestCompressOldDays(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.February, 10, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -4)
	recent := now.AddDate(0, 0, -1)
	oldDay := old.Format(dayFormat)
	recentDay := recent.Format(dayFormat)

	t.Run("busy backfill skips maintenance without delaying flush", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t, t.TempDir())
		s.Observe(testMeta(ksid.NewID()), &Event{At: old, Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}}})
		s.backfillMu.Lock()
		ran, err := s.tryCompressOldDays(now)
		s.backfillMu.Unlock()
		if err != nil || ran {
			t.Fatalf("maintenance while backfill owns directory = %v/%v, want skipped", ran, err)
		}
		if _, err := os.Stat(filepath.Join(s.dir, oldDay+".jsonl")); err != nil {
			t.Fatalf("skipped compression changed old day: %v", err)
		}
		ran, err = s.tryCompressOldDays(now)
		if err != nil || !ran {
			t.Fatalf("maintenance after backfill = %v/%v, want compressed", ran, err)
		}
		if _, err := os.Stat(filepath.Join(s.dir, oldDay+compressedDaySuffix)); err != nil {
			t.Fatalf("compressed old day after retry: %v", err)
		}
	})

	t.Run("grace period and late write", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := newTestStore(t, dir)
		meta := testMeta(ksid.NewID())
		s.Observe(meta, &Event{At: old, Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}}})
		s.Observe(meta, &Event{At: recent, Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 5}}})
		compressOldDaysForTest(t, s, now)
		if _, err := os.Stat(filepath.Join(dir, oldDay+".jsonl.zstd")); err != nil {
			t.Fatalf("compressed old day: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, oldDay+".jsonl")); !os.IsNotExist(err) {
			t.Fatalf("old plain day still present: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, recentDay+".jsonl")); err != nil {
			t.Fatalf("recent day should stay plain: %v", err)
		}
		s.Observe(meta, &Event{At: old.Add(time.Hour), Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 3}}})
		if _, err := os.Stat(filepath.Join(dir, oldDay+".jsonl.zstd")); !os.IsNotExist(err) {
			t.Fatalf("compressed copy still present after late write: %v", err)
		}
		if rows := readRows(t, dir, oldDay); len(rows) != 2 {
			t.Fatalf("old day rows after late write = %d, want 2", len(rows))
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := newTestStore(t, dir)
		if got := reopened.Days()[0].Tokens.Output; got != 13 {
			t.Errorf("recovered old-day output = %d, want 13", got)
		}
	})

	t.Run("historical backfill recognizes compressed day", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := newTestStore(t, dir)
		s.Observe(testMeta(ksid.NewID()), &Event{At: old, Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}}})
		compressOldDaysForTest(t, s, now)
		if err := s.Backfill(t.Context(), func(yield func(UsageRow, error) bool) {
			yield(UsageRow{Kind: rowKindUsage, Day: oldDay, TaskID: "historical", Model: "m", Output: 10}, nil)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, oldDay+".jsonl")); !os.IsNotExist(err) {
			t.Fatalf("historical backfill duplicated compressed day: %v", err)
		}
		if got := s.Days()[0].Tokens.Output; got != 10 {
			t.Errorf("usage after backfill = %d, want 10", got)
		}
	})

	t.Run("plain copy wins interrupted compression", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := newTestStore(t, dir)
		s.Observe(testMeta(ksid.NewID()), &Event{At: old, Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}}})
		plain := filepath.Join(dir, oldDay+".jsonl")
		original, err := os.ReadFile(plain) //nolint:gosec // test fixture path built from t.TempDir().
		if err != nil {
			t.Fatal(err)
		}
		compressOldDaysForTest(t, s, now)
		compressed := filepath.Join(dir, oldDay+".jsonl.zstd")
		var data []byte
		for row, err := range dayRecords(t.Context(), compressed) {
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, row...)
		}
		if !bytes.Equal(data, original) {
			t.Fatal("compressed day did not round trip byte for byte")
		}
		late, err := json.Marshal(UsageRow{Kind: rowKindUsage, Day: oldDay, TaskID: "late", Model: "m", Output: 3})
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, append(late, '\n')...)
		if err := os.WriteFile(plain, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := newTestStore(t, dir)
		if got := reopened.Days()[0].Tokens.Output; got != 13 {
			t.Errorf("recovered day = %d output tokens, want 13 from plain copy", got)
		}
	})
}

func compressOldDaysForTest(t *testing.T, s *Store, now time.Time) {
	ran, err := s.tryCompressOldDays(now)
	if err != nil || !ran {
		t.Fatalf("compress old days = %v/%v, want completed", ran, err)
	}
}

func TestDayRecords(t *testing.T) {
	t.Parallel()
	t.Run("corruption never publishes earlier records", func(t *testing.T) {
		t.Parallel()
		for _, truncated := range []bool{false, true} {
			name := "corrupt"
			if truncated {
				name = "truncated"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				path := filepath.Join(dir, "2026-02-05.jsonl.zstd")
				enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
				if err != nil {
					t.Fatal(err)
				}
				row := []byte(`{"kind":"usage","day":"2026-02-05","task_id":"bad","ts":123,"model":"priced","cost_usd":0,"output_tokens":10}` + "\n" + `{"kind":"quota","day":"2026-02-05","provider":"bad","status":"blocked"}` + "\n")
				data := enc.EncodeAll(bytes.Repeat(row, 2000), nil)
				if err := enc.Close(); err != nil {
					t.Fatal(err)
				}
				if truncated {
					data = data[:len(data)-2]
				} else {
					data = append(data, []byte("not a zstd frame")...)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				good := []byte(`{"kind":"usage","day":"2026-02-04","task_id":"good","cost_usd":2,"output_tokens":3}` + "\n")
				if err := os.WriteFile(filepath.Join(dir, "2026-02-04.jsonl"), good, 0o600); err != nil {
					t.Fatal(err)
				}
				s := newTestStore(t, dir)
				days := s.Days()
				if len(days) != 1 || days[0].CostUSD != 2 || days[0].Tokens.Output != 3 || s.watermarks["bad"] != (time.Time{}) || s.flushedCost["bad"] != 0 || len(s.lastQuota) != 0 || len(s.reportedCostTasks) != 1 {
					t.Fatalf("partial recovery published: days=%+v, watermarks=%v, costs=%v, quotas=%v", days, s.watermarks, s.flushedCost, s.lastQuota)
				}
				if err := s.BackfillMissingCosts(t.Context(), func(*UsageRow) (float64, bool) { return .25, true }); err == nil {
					t.Fatal("corrupt repair reported success")
				}
				got, err := os.ReadFile(path) //nolint:gosec // test fixture path built from t.TempDir().
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data) {
					t.Fatal("corrupt source replaced")
				}
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 2 {
					t.Fatalf("staging files remain: %v", entries)
				}
			})
		}
	})
}
