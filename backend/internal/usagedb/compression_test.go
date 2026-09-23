// Tests sealed usage days, late writes, and restart recovery across compression.

package usagedb

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		data, err := readDayFile(compressed)
		if err != nil {
			t.Fatal(err)
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
