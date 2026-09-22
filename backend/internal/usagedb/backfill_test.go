// Backfill tests cover neutral-row staging, retry safety, and the done sentinel.

package usagedb_test

import (
	"encoding/json"
	"errors"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

func TestStoreBackfill(t *testing.T) {
	t.Parallel()

	t.Run("sentinel suppresses rescan", func(t *testing.T) {
		t.Parallel()
		dir, store := newStore(t)
		calls := 0
		if err := store.Backfill(t.Context(), func(yield func(usagedb.UsageRow, error) bool) {
			calls++
			usageRows(testRow(5))(yield)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".backfill.done")); err != nil {
			t.Fatalf("backfill sentinel: %v", err)
		}
		if err := store.Backfill(t.Context(), func(yield func(usagedb.UsageRow, error) bool) {
			calls++
			usageRows(testRow(6))(yield)
		}); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Errorf("row-loader calls = %d, want 1", calls)
		}
		if _, err := os.Stat(filepath.Join(dir, "2026-02-06.jsonl")); !os.IsNotExist(err) {
			t.Errorf("sentinel allowed second day: %v", err)
		}
	})

	t.Run("retry after published days is duplicate free", func(t *testing.T) {
		t.Parallel()
		dir, store := newStore(t)
		rows := []usagedb.UsageRow{testRow(5), testRow(6)}
		if err := store.Backfill(t.Context(), usageRowsLoader(rows)); err != nil {
			t.Fatal(err)
		}
		before := readRows(t, dir, "2026-02-05")
		if err := os.Remove(filepath.Join(dir, ".backfill.done")); err != nil {
			t.Fatal(err)
		}
		if err := store.Backfill(t.Context(), usageRowsLoader(rows)); err != nil {
			t.Fatal(err)
		}
		if got := len(readRows(t, dir, "2026-02-05")); got != len(before) {
			t.Errorf("day rows after retry = %d, want %d", got, len(before))
		}
	})

	t.Run("requires a producer day", func(t *testing.T) {
		t.Parallel()
		_, store := newStore(t)
		if err := store.Backfill(t.Context(), usageRowsLoader([]usagedb.UsageRow{{Kind: "usage"}})); err == nil {
			t.Error("Backfill unexpectedly accepted a row without a day")
		}
	})

	t.Run("source error discards every staged day", func(t *testing.T) {
		t.Parallel()
		dir, store := newStore(t)
		source := func(yield func(usagedb.UsageRow, error) bool) {
			if !yield(testRow(5), nil) {
				return
			}
			yield(usagedb.UsageRow{}, errors.New("source interrupted"))
		}
		if err := store.Backfill(t.Context(), source); err == nil {
			t.Fatal("Backfill unexpectedly accepted a source error")
		}
		if _, err := os.Stat(filepath.Join(dir, "2026-02-05.jsonl")); !os.IsNotExist(err) {
			t.Errorf("source error published a staged day: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".backfill.done")); !os.IsNotExist(err) {
			t.Errorf("source error wrote sentinel: %v", err)
		}
	})

	t.Run("flushes pending live rows before publishing the same day", func(t *testing.T) {
		t.Parallel()
		_, store := newStore(t)
		id := ksid.NewID()
		meta := usagedb.TaskMeta{
			TaskID:  id,
			Harness: "claude",
			Repos:   []string{"github/caic"},
		}
		at := time.Date(2026, time.February, 5, 10, 0, 0, 0, time.UTC)
		store.Observe(meta, &usagedb.Event{
			At:      at,
			Model:   "claude-test",
			CostUSD: 0.50,
			Delta:   usagedb.Delta{TokenBuckets: usagedb.TokenBuckets{Output: 10}},
		})
		backfillRow := usagedb.UsageRow{
			Kind:    "usage",
			Day:     at.Format("2006-01-02"),
			Ts:      usagedb.NewTime(at),
			TaskID:  id.String(),
			Harness: "claude",
			Repos:   []string{"github/caic"},
			Model:   "claude-test",
			Output:  10,
		}
		if err := store.Backfill(t.Context(), usageRowsLoader([]usagedb.UsageRow{backfillRow})); err != nil {
			t.Fatal(err)
		}
		store.Observe(meta, &usagedb.Event{
			At:           at.Add(time.Second),
			Model:        "claude-test",
			CostUSD:      0.75,
			TurnBoundary: true,
			Delta: usagedb.Delta{
				TokenBuckets: usagedb.TokenBuckets{Output: 2},
				Turns:        1,
			},
		})
		days := store.Days()
		if len(days) != 1 || days[0].Tokens.Output != 12 || days[0].Turns != 1 || days[0].CostUSD != 0.75 {
			t.Errorf("day totals = %+v, want output=12 turns=1 cost=0.75", days)
		}
	})
}

func newStore(t *testing.T) (string, *usagedb.Store) {
	dir := t.TempDir()
	store, err := usagedb.New(usagedb.Config{Log: testLogger(), Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return dir, store
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testRow(day int) usagedb.UsageRow {
	at := time.Date(2026, time.February, day, 10, 0, 0, 0, time.UTC)
	return usagedb.UsageRow{
		Kind:   "usage",
		Day:    at.Format("2006-01-02"),
		Ts:     usagedb.NewTime(at),
		TaskID: "task",
		Turns:  1,
	}
}

func usageRowsLoader(rows []usagedb.UsageRow) iter.Seq2[usagedb.UsageRow, error] {
	return usageRows(rows...)
}

func usageRows(rows ...usagedb.UsageRow) iter.Seq2[usagedb.UsageRow, error] {
	return func(yield func(usagedb.UsageRow, error) bool) {
		for i := range rows {
			if !yield(rows[i], nil) {
				return
			}
		}
	}
}

func readRows(t *testing.T, dir, day string) []usagedb.UsageRow {
	//nolint:gosec // day is a fixed YYYY-MM-DD test fixture, not user input.
	data, err := os.ReadFile(filepath.Join(dir, day+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]usagedb.UsageRow, 0)
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row usagedb.UsageRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}
