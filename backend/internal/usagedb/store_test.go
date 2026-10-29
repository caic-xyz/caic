// Store tests: event round-trips, day rollover, quota dedupe, aggregates, and truncated-tail recovery.

package usagedb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
	"github.com/maruel/ksid"
)

func newTestStore(t *testing.T, dir string) *Store {
	s, err := New(t.Context(), Config{Log: testLogger(), Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func testMeta(id ksid.ID) TaskMeta {
	return TaskMeta{
		TaskID:         id,
		Harness:        "claude",
		Repos:          []string{"github/caic"},
		RequestedModel: "fallback-model",
	}
}

func atUTC(day, h, mi, sec int) time.Time {
	return time.Date(2026, time.February, day, h, mi, sec, 0, time.UTC)
}

func testEvent(day, h, mi, sec int, model string, d *data.Delta) *Event {
	return &Event{At: atUTC(day, h, mi, sec), Model: model, Delta: *d}
}

// readRows parses every usage row in the day file for day.
func readRows(t *testing.T, dir, day string) []data.UsageRow {
	raw, err := os.ReadFile(filepath.Join(dir, day+".jsonl")) //nolint:gosec // test fixture path built from t.TempDir().
	if err != nil {
		t.Fatalf("read day file %s: %v", day, err)
	}
	var rows []data.UsageRow
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row data.UsageRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse row: %v", err)
		}
		if row.Kind == rowKindUsage {
			rows = append(rows, row)
		}
	}
	return rows
}

// readQuotaRows parses the quota rows in the February 5 fixture day.
func readQuotaRows(t *testing.T, dir string) []data.QuotaRow {
	raw, err := os.ReadFile(filepath.Join(dir, "2026-02-05.jsonl")) //nolint:gosec // test fixture path built from t.TempDir().
	if err != nil {
		t.Fatalf("read quota day file: %v", err)
	}
	var rows []data.QuotaRow
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row data.QuotaRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse row: %v", err)
		}
		if row.Kind == rowKindQuota {
			rows = append(rows, row)
		}
	}
	return rows
}

func TestStore(t *testing.T) {
	t.Parallel()

	t.Run("Observe", func(t *testing.T) {
		t.Parallel()

		t.Run("turn flush", func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, t.TempDir())
			id := ksid.NewID()
			meta := testMeta(id)
			at := atUTC(5, 10, 0, 0)

			s.Observe(meta, &Event{At: at, Model: "claude-opus", CostUSD: 0.01, Delta: data.Delta{
				TokenBuckets:  data.TokenBuckets{Input: 10, CacheWrite1h: 400, CacheRead: 900, Output: 50},
				ContextWindow: 200000,
				ToolCalls:     map[string]int{"Edit": 1},
				SkillReads:    map[string]int{"code-review": 1},
				Spawns:        1, SpawnsBackground: 1,
			}})
			s.Observe(meta, &Event{
				At: at.Add(5 * time.Second), Model: "claude-opus", CostUSD: 0.02, TurnBoundary: true,
				Delta: data.Delta{
					TokenBuckets:  data.TokenBuckets{Input: 5, Output: 20},
					Turns:         1,
					APIMs:         1500,
					WallMs:        4000,
					ContextWindow: 210000,
				},
			})

			rows := readRows(t, s.dir, "2026-02-05")
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1 (turn-boundary flush)", len(rows))
			}
			row := rows[0]
			if row.TaskID != id.String() || row.Model != "claude-opus" || row.Harness != "claude" {
				t.Errorf("row identity = %s/%s/%s", row.TaskID, row.Model, row.Harness)
			}
			if len(row.Repos) != 1 || row.Repos[0] != "github/caic" {
				t.Errorf("repos = %v", row.Repos)
			}
			// Both events accumulate into one row; the 1h cache write lands in
			// the 1h bucket.
			if row.Input != 15 || row.CacheWriteCount(time.Hour) != 400 || row.CacheRead != 900 {
				t.Errorf("input buckets = %+v", row)
			}
			if row.Output != 70 || row.Turns != 1 || row.APIMs != 1500 || row.WallMs != 4000 {
				t.Errorf("turn counters = %+v", row)
			}
			if row.ToolCalls["Edit"] != 1 || row.SkillReads["code-review"] != 1 {
				t.Errorf("maps = %v / %v", row.ToolCalls, row.SkillReads)
			}
			if row.Spawns != 1 || row.SpawnsBackground != 1 {
				t.Errorf("spawns = %d/%d", row.Spawns, row.SpawnsBackground)
			}
			if row.ContextWindow != 210000 {
				t.Errorf("contextWindow = %d, want max 210000", row.ContextWindow)
			}
			if row.CostUSD != 0.02 {
				t.Errorf("costUSD = %v, want 0.02 (movement to the newest snapshot)", row.CostUSD)
			}
		})

		t.Run("cost movement across snapshots", func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, t.TempDir())
			meta := testMeta(ksid.NewID())
			at := atUTC(5, 10, 0, 0)
			// The store derives cost movement from consecutive snapshots; a
			// regression in the snapshot must not inflate the movement.
			s.Observe(meta, &Event{At: at, Model: "m", CostUSD: 0.10, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 1}}})
			s.Observe(meta, &Event{At: at.Add(time.Second), Model: "m", CostUSD: 0.10, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 1}}})
			s.flushTaskLocked(meta.TaskID.String())
			rows := readRows(t, s.dir, "2026-02-05")
			if len(rows) != 1 || rows[0].CostUSD != 0.10 {
				t.Fatalf("rows = %+v, want one row with 0.10 movement", rows)
			}
		})

		t.Run("day rollover", func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, t.TempDir())
			meta := testMeta(ksid.NewID())
			s.Observe(meta, testEvent(5, 23, 59, 59, "m", &data.Delta{Output: 10}))
			s.Observe(meta, &Event{At: atUTC(6, 0, 0, 1), Model: "m", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 5}, Turns: 1}})

			if rows := readRows(t, s.dir, "2026-02-05"); len(rows) != 1 || rows[0].Output != 10 {
				t.Errorf("day1 rows = %+v", rows)
			}
			rows := readRows(t, s.dir, "2026-02-06")
			if len(rows) != 1 || rows[0].Output != 5 || rows[0].Turns != 1 {
				t.Errorf("day2 rows = %+v", rows)
			}
			days := s.Days()
			if len(days) != 2 || days[0].Day != "2026-02-05" || days[1].Day != "2026-02-06" {
				t.Errorf("days = %+v", days)
			}
		})

		t.Run("live producer time regression is retained", func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, t.TempDir())
			meta := testMeta(ksid.NewID())
			current := atUTC(6, 0, 0, 1)
			s.Observe(meta, &Event{At: current, Model: "m", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}, Turns: 1}})
			s.Observe(meta, &Event{At: atUTC(5, 23, 59, 59), Model: "m", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 5}, Turns: 1}})

			if rows := readRows(t, s.dir, "2026-02-05"); len(rows) != 1 || rows[0].Output != 5 {
				t.Errorf("regressed-day rows = %+v, want one retained row", rows)
			}
			if got := s.watermarks[meta.TaskID.String()]; !got.Equal(current) {
				t.Errorf("watermark = %v, want %v", got, current)
			}

			// A historical replay before the watermark remains a duplicate and is
			// skipped, preserving restart-resume behavior.
			s.Observe(meta, &Event{At: atUTC(5, 23, 59, 59), Replayed: true, Model: "m", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 5}, Turns: 1}})
			if rows := readRows(t, s.dir, "2026-02-05"); len(rows) != 1 {
				t.Errorf("rows after replay = %+v, want no duplicate", rows)
			}
		})

		t.Run("discard removes task bookkeeping", func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, t.TempDir())
			meta := testMeta(ksid.NewID())
			s.Observe(meta, &Event{At: atUTC(5, 10, 0, 0), Model: "m", CostUSD: 0.10, TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}, Turns: 1}})
			s.Observe(meta, &Event{At: atUTC(5, 10, 0, 1), Model: "m", Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 5}}})
			id := meta.TaskID.String()
			if s.pending[id] == nil || s.watermarks[id].IsZero() {
				t.Fatalf("task bookkeeping missing before discard")
			}

			s.Discard(meta)
			if s.pending[id] != nil || !s.watermarks[id].IsZero() || s.flushedCost[id] != 0 {
				t.Errorf("task bookkeeping survived discard: pending=%v watermark=%v flushedCost=%v", s.pending[id], s.watermarks[id], s.flushedCost[id])
			}
		})

		t.Run("cost movement retried once after failed append", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// Make the first append attempt fail: the day file path is a
			// directory, so opening it errors and the delta stays pending.
			dayDir := filepath.Join(dir, "2026-02-05.jsonl")
			if err := os.Mkdir(dayDir, 0o700); err != nil {
				t.Fatal(err)
			}
			s := newTestStore(t, dir)
			meta := testMeta(ksid.NewID())
			s.Observe(meta, &Event{At: atUTC(5, 10, 0, 0), Model: "m", CostUSD: 0.50, TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}, Turns: 1}})
			if len(s.pending[meta.TaskID.String()].buckets) != 1 {
				t.Fatalf("failed delta must stay pending")
			}

			// Repair the write path and observe the same cost snapshot again:
			// the retry must write the row with the movement exactly once.
			if err := os.Remove(dayDir); err != nil {
				t.Fatal(err)
			}
			s.Observe(meta, &Event{At: atUTC(5, 10, 0, 1), Model: "m", CostUSD: 0.50, TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}, Turns: 1}})
			rows := readRows(t, s.dir, "2026-02-05")
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			if rows[0].CostUSD != 0.50 {
				t.Errorf("costUSD = %v, want 0.50 written exactly once", rows[0].CostUSD)
			}
			if days := s.Days(); days[0].CostUSD != 0.50 {
				t.Errorf("aggregate costUSD = %v, want 0.50", days[0].CostUSD)
			}
		})

		t.Run("append failure keeps aggregate consistent", func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, t.TempDir())
			meta := testMeta(ksid.NewID())
			at := atUTC(5, 10, 0, 0)
			s.Observe(meta, &Event{At: at, Model: "m", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}, Turns: 1}})
			if days := s.Days(); days[0].Tokens.Output != 10 {
				t.Fatalf("days = %+v, want the flushed row", days)
			}

			// Simulate a broken write path: the aggregate must not advance past
			// disk, and the delta must stay pending for the next flush.
			day := "2026-02-05"
			_ = s.files[day].Close()
			s.Observe(meta, &Event{At: at.Add(time.Second), Model: "m", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 5}, Turns: 1}})
			if days := s.Days(); days[0].Tokens.Output != 10 {
				t.Errorf("days after failed append = %+v, want unchanged (aggregate matches disk)", days)
			}
			if len(s.pending[meta.TaskID.String()].buckets) != 1 {
				t.Errorf("failed delta must stay pending for retry")
			}
		})

		t.Run("first live-row write failure leaves day absent for backfill retry", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := newTestStore(t, dir)
			meta := testMeta(ksid.NewID())
			at := atUTC(5, 10, 0, 0)
			s.Observe(meta, &Event{At: at, Model: "m", Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}}})
			//nolint:gosec // t.TempDir needs execute permission for this controlled directory test.
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				//nolint:gosec // t.TempDir needs execute permission for cleanup.
				_ = os.Chmod(dir, 0o700)
			})
			s.mu.Lock()
			s.flushTaskLocked(meta.TaskID.String())
			s.mu.Unlock()
			//nolint:gosec // t.TempDir needs execute permission for this controlled directory test.
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			day := "2026-02-05"
			if _, err := os.Stat(filepath.Join(dir, day+".jsonl")); !os.IsNotExist(err) {
				t.Fatalf("failed first append left day file: %v", err)
			}
			if len(s.pending[meta.TaskID.String()].buckets) != 1 {
				t.Fatal("failed first append did not retain the pending bucket")
			}
			row := data.UsageRow{Kind: rowKindUsage, Day: day, Ts: data.NewTime(at), TaskID: meta.TaskID.String(), Model: "m", Output: 10}
			if err := s.Backfill(t.Context(), func(yield func(data.UsageRow, error) bool) { yield(row, nil) }); err != nil {
				t.Fatal(err)
			}
			rows := readRows(t, dir, day)
			if len(rows) != 1 || rows[0].Output != 10 {
				t.Errorf("rows after retry = %+v, want one live row", rows)
			}
			if _, err := os.Stat(filepath.Join(dir, backfillSentinel)); err != nil {
				t.Errorf("backfill sentinel after retry: %v", err)
			}
		})

		t.Run("quota dedupe survives restart", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s1 := newTestStore(t, dir)
			change := QuotaChange{At: atUTC(5, 10, 0, 0), Provider: "anthropic", Window: "five_hour", Status: "allowed", Utilization: 0.10, ResetsAt: atUTC(5, 11, 0, 0)}
			s1.ObserveQuota(&change)
			if err := s1.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// The recovered dedupe state must suppress an unchanged status.
			s2, err := New(t.Context(), Config{Log: testLogger(), Dir: dir})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s2.Close() })
			s2.ObserveQuota(&change)
			if quotas := readQuotaRows(t, dir); len(quotas) != 1 {
				t.Errorf("quota rows after restart = %d, want 1 (unchanged status not rewritten)", len(quotas))
			}
		})

		t.Run("quota dedupe", func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, t.TempDir())
			at := atUTC(5, 10, 0, 0)
			change := func(status string, util float64) *QuotaChange {
				return &QuotaChange{At: at, Provider: "anthropic", Window: "five_hour", Status: status, Utilization: util, ResetsAt: at.Add(time.Hour)}
			}
			s.ObserveQuota(change("allowed", 0.10))
			s.ObserveQuota(change("allowed", 0.10))  // identical: skipped
			s.ObserveQuota(change("allowed", 0.104)) // rounding: skipped
			s.ObserveQuota(change("allowed_warning", 0.50))

			quotas := readQuotaRows(t, s.dir)
			if len(quotas) != 2 {
				t.Fatalf("quota rows = %d, want 2 (first + status change)", len(quotas))
			}
			if quotas[0].Status != "allowed" || quotas[1].Status != "allowed_warning" {
				t.Errorf("quota statuses = %s / %s", quotas[0].Status, quotas[1].Status)
			}
			if quotas[0].Window != "five_hour" || quotas[0].Provider != "anthropic" {
				t.Errorf("quota identity = %s/%s", quotas[0].Provider, quotas[0].Window)
			}
		})

		t.Run("tool timing correlation", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := newTestStore(t, dir)
			meta := testMeta(ksid.NewID())
			at := atUTC(5, 10, 0, 0)
			s.Observe(meta, &Event{At: at, ToolProducerTime: at, ToolStartID: "a", ToolName: "Bash", Delta: data.Delta{ToolCalls: map[string]int{"Bash": 1}}})
			s.Observe(meta, &Event{At: at.Add(2 * time.Second), ToolProducerTime: at.Add(2 * time.Second), ToolResultID: "a"})
			s.Observe(meta, &Event{At: at.Add(3 * time.Second), ToolProducerTime: at.Add(3 * time.Second), ToolStartID: "b", ToolName: "Bash", Delta: data.Delta{ToolCalls: map[string]int{"Bash": 1}}})
			s.Observe(meta, &Event{At: at.Add(4 * time.Second), TurnBoundary: true, Delta: data.Delta{Turns: 1}})
			day := s.Days()[0]
			if day.Tools["Bash"] != 2 || day.ToolTimings["Bash"] != (data.ToolTiming{Count: 1, DurationMs: 2000}) {
				t.Errorf("live tool totals = %v, %v", day.Tools, day.ToolTimings)
			}
			if err := s.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			reopened := newTestStore(t, dir)
			day = reopened.Days()[0]
			if day.Tools["Bash"] != 2 || day.ToolTimings["Bash"] != (data.ToolTiming{Count: 1, DurationMs: 2000}) {
				t.Errorf("recovered tool totals = %v, %v", day.Tools, day.ToolTimings)
			}
			// Adoption replays the start at the durable watermark before seeing its
			// unflushed result. The start restores correlation without recounting a call.
			reopened.Observe(meta, &Event{At: at.Add(3 * time.Second), ToolProducerTime: at.Add(3 * time.Second), Replayed: true, ToolStartID: "b", ToolName: "Bash", Delta: data.Delta{ToolCalls: map[string]int{"Bash": 1}}})
			reopened.Observe(meta, &Event{At: at.Add(5 * time.Second), ToolProducerTime: at.Add(5 * time.Second), Replayed: true, ToolResultID: "b"})
			reopened.Observe(meta, &Event{At: at.Add(6 * time.Second), Replayed: true, TurnBoundary: true, Delta: data.Delta{Turns: 1}})
			day = reopened.Days()[0]
			if day.Tools["Bash"] != 2 || day.ToolTimings["Bash"] != (data.ToolTiming{Count: 2, DurationMs: 4000}) {
				t.Errorf("resumed tool totals = %v, %v", day.Tools, day.ToolTimings)
			}
			reopened.Observe(meta, &Event{At: at.Add(7 * time.Second), ToolStartID: "c", ToolName: "Bash", Delta: data.Delta{ToolCalls: map[string]int{"Bash": 1}}})
			reopened.Observe(meta, &Event{At: at.Add(8 * time.Second), ToolResultID: "c"})
			reopened.Observe(meta, &Event{At: at.Add(9 * time.Second), TurnBoundary: true, Delta: data.Delta{Turns: 1}})
			day = reopened.Days()[0]
			if day.Tools["Bash"] != 3 || day.ToolTimings["Bash"] != (data.ToolTiming{Count: 2, DurationMs: 4000}) {
				t.Errorf("timestamp-less tool totals = %v, %v", day.Tools, day.ToolTimings)
			}
		})
	})

	t.Run("Days", func(t *testing.T) {
		t.Parallel()

		t.Run("recovered aggregates", func(t *testing.T) {
			t.Parallel()
			// Aggregates must rebuild from persisted rows alone, including the
			// leaderboard maps and the per-repo distinct task counts.
			dir := t.TempDir()
			s1 := newTestStore(t, dir)
			for range 2 {
				meta := testMeta(ksid.NewID())
				at := atUTC(5, 10, 0, 0)
				s1.Observe(meta, &Event{At: at, Model: "m1", CostUSD: 0.10, Delta: data.Delta{
					TokenBuckets: data.TokenBuckets{Output: 10},
					ToolCalls:    map[string]int{"Edit": 1},
					SkillReads:   map[string]int{"code-review": 1},
				}})
				s1.flushTaskLocked(meta.TaskID.String())
			}
			if err := s1.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			s2, err := New(t.Context(), Config{Log: testLogger(), Dir: dir})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s2.Close() })
			days := s2.Days()
			if len(days) != 1 || days[0].Day != "2026-02-05" {
				t.Fatalf("days = %+v", days)
			}
			day := days[0]
			if day.Tokens.Output != 20 || day.CostUSD != 0.20 {
				t.Errorf("day totals = %+v", day)
			}
			if m := day.Models["m1"]; m.Tokens.Output != 20 || m.CostUSD != 0.20 {
				t.Errorf("model rollup = %+v", m)
			} else {
				if m.ToolCalls["Edit"] != 2 || m.ToolTimings != nil {
					t.Errorf("model tools = %v / %v", m.ToolCalls, m.ToolTimings)
				}
				if m.Skills["code-review"] != 2 || m.Repos["github/caic"] != 2 {
					t.Errorf("model drill-down task counts = %v / %v", m.Skills, m.Repos)
				}
				if m.ErroredTurns != 0 || m.APIMs != 0 || m.WallMs != 0 || m.Compactions != 0 || m.SubagentSpawns != 0 {
					t.Errorf("model counters = %+v", m)
				}
			}
			if h := day.Harnesses["claude"]; h.Tokens.Output != 20 {
				t.Errorf("harness rollup = %+v", h)
			} else if h.ToolCalls["Edit"] != 2 || h.Skills["code-review"] != 2 {
				t.Errorf("harness drill-down = %v / %v", h.ToolCalls, h.Skills)
			} else if m := h.Models["m1"]; m.Tokens.Output != 20 || m.ToolCalls["Edit"] != 2 || m.Skills["code-review"] != 2 {
				t.Errorf("harness-by-model cross = %+v", m)
			}
			if day.Repos["github/caic"] != 2 {
				t.Errorf("repos = %v, want 2 distinct tasks", day.Repos)
			}
			if day.Tools["Edit"] != 2 || day.Skills["code-review"] != 2 {
				t.Errorf("leaderboards = %v / %v", day.Tools, day.Skills)
			}
		})

		t.Run("skill leaderboard counts tasks, not reads", func(t *testing.T) {
			t.Parallel()
			// Codex re-reads a skill every turn where Claude Code loads it once,
			// and each turn boundary appends its own row.
			s := newTestStore(t, t.TempDir())
			meta := testMeta(ksid.NewID())
			for range 5 {
				s.Observe(meta, &Event{At: atUTC(5, 10, 0, 0), Model: "m1", Delta: data.Delta{
					SkillReads: map[string]int{"code-quality": 3},
				}})
				s.flushTaskLocked(meta.TaskID.String())
			}
			other := testMeta(ksid.NewID())
			s.Observe(other, &Event{At: atUTC(5, 10, 0, 0), Model: "m1", Delta: data.Delta{
				SkillReads: map[string]int{"code-quality": 1, "review": 1},
			}})
			s.flushTaskLocked(other.TaskID.String())

			days := s.Days()
			if len(days) != 1 {
				t.Fatalf("days = %+v", days)
			}
			if got := days[0].Skills["code-quality"]; got != 2 {
				t.Errorf("Skills[code-quality] = %d, want 2 distinct tasks", got)
			}
			if got := days[0].Skills["review"]; got != 1 {
				t.Errorf("Skills[review] = %d, want 1", got)
			}
			// Drill-down rollups follow the same task-day rule per model.
			m := days[0].Models["m1"]
			if got := m.Skills["code-quality"]; got != 2 {
				t.Errorf("model Skills[code-quality] = %d, want 2 distinct tasks", got)
			}
			if got := m.Skills["review"]; got != 1 {
				t.Errorf("model Skills[review] = %d, want 1", got)
			}
		})

		t.Run("truncated tail", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			day := "2026-02-05"
			row := data.UsageRow{Kind: rowKindUsage, Day: day, Ts: 1000, TaskID: "t1", Output: 5}
			encoded, err := json.Marshal(&row)
			if err != nil {
				t.Fatal(err)
			}
			// A complete row followed by a half-written one (simulated crash).
			content := string(encoded) + "\n" + `{"kind":"usage","task_id":"t2","out`
			if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			s := newTestStore(t, dir)
			days := s.Days()
			if len(days) != 1 || days[0].Tokens.Output != 5 {
				t.Errorf("days = %+v, want only the complete row", days)
			}
		})
	})

	t.Run("BackfillMissingCosts", func(t *testing.T) {
		t.Parallel()

		t.Run("prices compressed day without unpacking it", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := newTestStore(t, dir)
			at := time.Date(2026, time.February, 5, 10, 0, 0, 0, time.UTC)
			s.Observe(testMeta(ksid.NewID()), &Event{At: at, Model: "priced", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}}})
			compressOldDaysForTest(t, s, at.AddDate(0, 0, 5))
			if err := s.BackfillMissingCosts(t.Context(), func(*data.UsageRow) (float64, bool) { return 0.25, true }); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "2026-02-05.jsonl")); !os.IsNotExist(err) {
				t.Fatalf("compressed day became plain: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := newTestStore(t, dir)
			if got := reopened.Days()[0].CostUSD; got != 0.25 {
				t.Errorf("recovered estimated cost = %v, want 0.25", got)
			}
		})
		t.Run("late restore replaces compressed candidate", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := newTestStore(t, dir)
			at := time.Date(2026, time.February, 5, 10, 0, 0, 0, time.UTC)
			meta := testMeta(ksid.NewID())
			s.Observe(meta, &Event{At: at, Model: "priced", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}}})
			compressOldDaysForTest(t, s, at.AddDate(0, 0, 5))
			s.Observe(meta, &Event{At: at.Add(time.Hour), Model: "priced", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 5}}})
			if err := backfillDayCosts(t.Context(), s, "2026-02-05.jsonl.zstd", func(*data.UsageRow) (float64, bool) { return 0.25, true }); err != nil {
				t.Fatalf("vanished compressed candidate: %v", err)
			}
			if err := s.BackfillMissingCosts(t.Context(), func(*data.UsageRow) (float64, bool) { return 0.25, true }); err != nil {
				t.Fatal(err)
			}
			if got := s.Days()[0].CostUSD; got != 0.5 {
				t.Errorf("plain replacement estimated cost = %v, want 0.5", got)
			}
		})
		t.Run("reconciles estimated and reported costs", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s := newTestStore(t, dir)
			missing := testMeta(ksid.NewID())
			reported := testMeta(ksid.NewID())
			s.Observe(missing, &Event{At: atUTC(5, 10, 0, 0), Model: "missing", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}}})
			s.Observe(reported, &Event{At: atUTC(5, 10, 0, 0), Model: "reported", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 5}}})
			s.Observe(reported, &Event{At: atUTC(5, 10, 0, 1), Model: "reported", CostUSD: 0.4, TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 20}}})
			s.ObserveQuota(&QuotaChange{At: atUTC(5, 10, 0, 2), Provider: "codex", Window: "5h", Status: "available"})
			day := "2026-02-05"
			path := filepath.Join(dir, day+".jsonl")
			before, err := os.ReadFile(path) //nolint:gosec // test fixture path built from t.TempDir().
			if err != nil {
				t.Fatal(err)
			}
			estimate := func(row *data.UsageRow) (float64, bool) {
				if row.Model == "missing" {
					return 0.25, true
				}
				return 0.5, true
			}
			if err := s.BackfillMissingCosts(t.Context(), estimate); err != nil {
				t.Fatal(err)
			}
			rows := readRows(t, dir, day)
			if len(rows) != 3 || rows[0].CostUSD != 0.25 || !rows[0].CostEstimated || rows[1].CostUSD != 0 || rows[1].CostEstimated || rows[2].CostUSD != 0.4 || rows[2].CostEstimated {
				t.Fatalf("rows after cost backfill = %+v", rows)
			}
			if quotas := readQuotaRows(t, dir); len(quotas) != 1 {
				t.Errorf("quota rows after cost backfill = %+v", quotas)
			}
			if got := s.Days()[0]; got.CostUSD != 0.65 || got.Models["missing"].CostUSD != 0.25 || got.Harnesses["claude"].CostUSD != 0.65 {
				t.Errorf("day totals after cost backfill = %+v", got)
			}
			if err := s.BackfillMissingCosts(t.Context(), func(*data.UsageRow) (float64, bool) { return 1, true }); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path) //nolint:gosec // test fixture path built from t.TempDir().
			if err != nil {
				t.Fatal(err)
			}
			if len(after) <= len(before) || s.Days()[0].CostUSD != 0.65 {
				t.Errorf("cost backfill was not idempotent: day = %+v", s.Days()[0])
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := newTestStore(t, dir)
			if reopened.flushedCost[missing.TaskID.String()] != 0.25 || reopened.flushedCost[reported.TaskID.String()] != 0.4 {
				t.Errorf("recovered live cost snapshots = %+v", reopened.flushedCost)
			}
			if got := reopened.Days()[0].CostUSD; got != 0.65 {
				t.Errorf("recovered day cost = %v, want 0.65", got)
			}
			reopened.Observe(missing, &Event{At: atUTC(5, 10, 0, 3), Model: "missing", CostUSD: 0.35, TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 2}}})
			rows = readRows(t, dir, day)
			if len(rows) != 4 || math.Abs(rows[3].CostUSD-0.1) > 1e-9 || rows[3].CostEstimated {
				t.Errorf("next live cost movement = %+v", rows)
			}
			if got := reopened.Days()[0].CostUSD; got != 0.75 {
				t.Errorf("day cost after live update = %v, want 0.75", got)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := reopened.BackfillMissingCosts(ctx, estimate); !errors.Is(err, context.Canceled) {
				t.Errorf("canceled backfill = %v, want context.Canceled", err)
			}
			if err := reopened.BackfillMissingCosts(t.Context(), nil); err == nil {
				t.Error("nil estimator unexpectedly accepted")
			}
		})

		t.Run("preserves other fields and truncated tail", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			day := "2026-02-05"
			path := filepath.Join(dir, day+".jsonl")
			original := []byte(`{"kind":"usage","day":"2026-02-05","task_id":"old","model":"priced","output_tokens":100,"future_field":{"nested":true}}` + "\n" + `{"kind":"usage","day":"2026-02-05","task_id":"bad","cost_usd":`)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			s := newTestStore(t, dir)
			if err := s.BackfillMissingCosts(t.Context(), func(*data.UsageRow) (float64, bool) { return 0.1, true }); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path) //nolint:gosec // test fixture path built from t.TempDir().
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"future_field":{"nested":true}`) || !strings.HasSuffix(string(raw), `{"kind":"usage","day":"2026-02-05","task_id":"bad","cost_usd":`) {
				t.Errorf("cost backfill lost unrelated fields or truncated tail: %s", raw)
			}
		})

		t.Run("skips task with failed cost append", func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, t.TempDir())
			meta := testMeta(ksid.NewID())
			s.Observe(meta, &Event{At: atUTC(5, 10, 0, 0), Model: "priced", TurnBoundary: true, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 10}}})
			s.Observe(meta, &Event{At: atUTC(5, 10, 0, 1), Model: "priced", CostUSD: 0.2, Delta: data.Delta{TokenBuckets: data.TokenBuckets{Output: 5}}})
			s.mu.Lock()
			s.pending[meta.TaskID.String()].costInFlight = 0.2 // Simulate cost assigned to a bucket whose append failed.
			s.mu.Unlock()
			if err := s.BackfillMissingCosts(t.Context(), func(*data.UsageRow) (float64, bool) { return 0.1, true }); err != nil {
				t.Fatal(err)
			}
			rows := readRows(t, s.dir, "2026-02-05")
			if len(rows) != 1 || rows[0].CostUSD != 0 || rows[0].CostEstimated {
				t.Errorf("row with cost in flight was estimated: %+v", rows)
			}
		})
		t.Run("cancellation discards staged costs and bytes", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "2026-02-05.jsonl")
			original := []byte(strings.Repeat(`{"kind":"usage","day":"2026-02-05","task_id":"same","model":"priced"}`+"\n", 10))
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			s := newTestStore(t, dir)
			ctx, cancel := context.WithCancel(t.Context())
			err := s.BackfillMissingCosts(ctx, func(*data.UsageRow) (float64, bool) { cancel(); return .1, true })
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("repair error=%v", err)
			}
			got, err := os.ReadFile(path) //nolint:gosec // test fixture path built from t.TempDir().
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) || s.Days()[0].CostUSD != 0 || s.flushedCost["same"] != 0 {
				t.Fatal("canceled repair published changes")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("staging files remain: %v", entries)
			}
		})
		t.Run("long unknown fields and unchanged records survive", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "2026-02-05.jsonl")
			padding := strings.Repeat("x", 150000)
			prefix := "  malformed \r\n\n"
			original := []byte(prefix + `{"kind":"usage","day":"2026-02-05","task_id":"same","model":"priced","harness":"claude","future":"` + padding + `"}`)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			s := newTestStore(t, dir)
			if err := s.BackfillMissingCosts(t.Context(), func(*data.UsageRow) (float64, bool) { return .25, true }); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path) //nolint:gosec // test fixture path built from t.TempDir().
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(got, []byte(prefix)) || !bytes.Contains(got, []byte(`"future":"`+padding+`"`)) || bytes.HasSuffix(got, []byte{'\n'}) {
				t.Fatal("records or terminal newline changed")
			}
			snapshot := s.Days()[0]
			if snapshot.Harnesses["claude"].Models["priced"].CostUSD != .25 {
				t.Fatalf("cross bucket cost=%v", snapshot.Harnesses["claude"].Models["priced"].CostUSD)
			}
		})
	})
}

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("canceled recovery returns no store", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "2026-02-05.jsonl"), []byte(`{"kind":"usage","day":"2026-02-05","output_tokens":7}`), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		s, err := New(ctx, Config{Log: testLogger(), Dir: dir})
		if s != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("New canceled = %v, %v", s, err)
		}
	})
}
