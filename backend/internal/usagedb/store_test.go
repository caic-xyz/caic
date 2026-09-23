// Store tests: event round-trips, day rollover, quota dedupe, aggregates, and truncated-tail recovery.

package usagedb

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"
)

func newTestStore(t *testing.T, dir string) *Store {
	s, err := New(Config{Log: testLogger(), Dir: dir})
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

func testEvent(day, h, mi, sec int, model string, d *Delta) *Event {
	return &Event{At: atUTC(day, h, mi, sec), Model: model, Delta: *d}
}

// readRows parses every usage row in the day file for day.
func readRows(t *testing.T, dir, day string) []UsageRow {
	data, err := os.ReadFile(filepath.Join(dir, day+".jsonl")) //nolint:gosec // test fixture path built from t.TempDir().
	if err != nil {
		t.Fatalf("read day file %s: %v", day, err)
	}
	var rows []UsageRow
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row UsageRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse row: %v", err)
		}
		if row.Kind == rowKindUsage {
			rows = append(rows, row)
		}
	}
	return rows
}

// readQuotaRows parses every quota row in the day file for day.
func readQuotaRows(t *testing.T, dir, day string) []QuotaRow {
	data, err := os.ReadFile(filepath.Join(dir, day+".jsonl")) //nolint:gosec // test fixture path built from t.TempDir().
	if err != nil {
		t.Fatalf("read day file %s: %v", day, err)
	}
	var rows []QuotaRow
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row QuotaRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse row: %v", err)
		}
		if row.Kind == rowKindQuota {
			rows = append(rows, row)
		}
	}
	return rows
}

func TestObserve(t *testing.T) {
	t.Parallel()

	t.Run("turn flush", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t, t.TempDir())
		id := ksid.NewID()
		meta := testMeta(id)
		at := atUTC(5, 10, 0, 0)

		s.Observe(meta, &Event{At: at, Model: "claude-opus", CostUSD: 0.01, Delta: Delta{
			TokenBuckets:  TokenBuckets{Input: 10, CacheWrite1h: 400, CacheRead: 900, Output: 50},
			ContextWindow: 200000,
			ToolCalls:     map[string]int{"Edit": 1},
			SkillReads:    map[string]int{"code-review": 1},
			Spawns:        1, SpawnsBackground: 1,
		}})
		s.Observe(meta, &Event{
			At: at.Add(5 * time.Second), Model: "claude-opus", CostUSD: 0.02, TurnBoundary: true,
			Delta: Delta{
				TokenBuckets:  TokenBuckets{Input: 5, Output: 20},
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
		if row.Input != 15 || row.CacheWrite1h != 400 || row.CacheRead != 900 {
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
		s.Observe(meta, &Event{At: at, Model: "m", CostUSD: 0.10, Delta: Delta{TokenBuckets: TokenBuckets{Output: 1}}})
		s.Observe(meta, &Event{At: at.Add(time.Second), Model: "m", CostUSD: 0.10, Delta: Delta{TokenBuckets: TokenBuckets{Output: 1}}})
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
		s.Observe(meta, testEvent(5, 23, 59, 59, "m", &Delta{Output: 10}))
		s.Observe(meta, &Event{At: atUTC(6, 0, 0, 1), Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 5}, Turns: 1}})

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
		s.Observe(meta, &Event{At: current, Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}, Turns: 1}})
		s.Observe(meta, &Event{At: atUTC(5, 23, 59, 59), Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 5}, Turns: 1}})

		if rows := readRows(t, s.dir, "2026-02-05"); len(rows) != 1 || rows[0].Output != 5 {
			t.Errorf("regressed-day rows = %+v, want one retained row", rows)
		}
		if got := s.watermarks[meta.TaskID.String()]; !got.Equal(current) {
			t.Errorf("watermark = %v, want %v", got, current)
		}

		// A historical replay before the watermark remains a duplicate and is
		// skipped, preserving restart-resume behavior.
		s.Observe(meta, &Event{At: atUTC(5, 23, 59, 59), Replayed: true, Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 5}, Turns: 1}})
		if rows := readRows(t, s.dir, "2026-02-05"); len(rows) != 1 {
			t.Errorf("rows after replay = %+v, want no duplicate", rows)
		}
	})

	t.Run("discard removes task bookkeeping", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t, t.TempDir())
		meta := testMeta(ksid.NewID())
		s.Observe(meta, &Event{At: atUTC(5, 10, 0, 0), Model: "m", CostUSD: 0.10, TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}, Turns: 1}})
		s.Observe(meta, &Event{At: atUTC(5, 10, 0, 1), Model: "m", Delta: Delta{TokenBuckets: TokenBuckets{Output: 5}}})
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
		s.Observe(meta, &Event{At: atUTC(5, 10, 0, 0), Model: "m", CostUSD: 0.50, TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}, Turns: 1}})
		if len(s.pending[meta.TaskID.String()].buckets) != 1 {
			t.Fatalf("failed delta must stay pending")
		}

		// Repair the write path and observe the same cost snapshot again:
		// the retry must write the row with the movement exactly once.
		if err := os.Remove(dayDir); err != nil {
			t.Fatal(err)
		}
		s.Observe(meta, &Event{At: atUTC(5, 10, 0, 1), Model: "m", CostUSD: 0.50, TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}, Turns: 1}})
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
		s.Observe(meta, &Event{At: at, Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}, Turns: 1}})
		if days := s.Days(); days[0].Tokens.Output != 10 {
			t.Fatalf("days = %+v, want the flushed row", days)
		}

		// Simulate a broken write path: the aggregate must not advance past
		// disk, and the delta must stay pending for the next flush.
		day := "2026-02-05"
		_ = s.files[day].Close()
		s.Observe(meta, &Event{At: at.Add(time.Second), Model: "m", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 5}, Turns: 1}})
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
		s.Observe(meta, &Event{At: at, Model: "m", Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}}})
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
		row := UsageRow{Kind: rowKindUsage, Day: day, Ts: NewTime(at), TaskID: meta.TaskID.String(), Model: "m", Output: 10}
		if err := s.Backfill(t.Context(), func(yield func(UsageRow, error) bool) { yield(row, nil) }); err != nil {
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
		s2, err := New(Config{Log: testLogger(), Dir: dir})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = s2.Close() })
		s2.ObserveQuota(&change)
		if quotas := readQuotaRows(t, dir, "2026-02-05"); len(quotas) != 1 {
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

		quotas := readQuotaRows(t, s.dir, "2026-02-05")
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
}

func TestDays(t *testing.T) {
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
			s1.Observe(meta, &Event{At: at, Model: "m1", CostUSD: 0.10, Delta: Delta{
				TokenBuckets: TokenBuckets{Output: 10},
				ToolCalls:    map[string]int{"Edit": 1},
				SkillReads:   map[string]int{"code-review": 1},
			}})
			s1.flushTaskLocked(meta.TaskID.String())
		}
		if err := s1.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		s2, err := New(Config{Log: testLogger(), Dir: dir})
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
		}
		if h := day.Harnesses["claude"]; h.Tokens.Output != 20 {
			t.Errorf("harness rollup = %+v", h)
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
			s.Observe(meta, &Event{At: atUTC(5, 10, 0, 0), Model: "m1", Delta: Delta{
				SkillReads: map[string]int{"code-quality": 3},
			}})
			s.flushTaskLocked(meta.TaskID.String())
		}
		other := testMeta(ksid.NewID())
		s.Observe(other, &Event{At: atUTC(5, 10, 0, 0), Model: "m1", Delta: Delta{
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
	})

	t.Run("truncated tail", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		day := "2026-02-05"
		row := UsageRow{Kind: rowKindUsage, Day: day, Ts: 1000, TaskID: "t1", Output: 5}
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		// A complete row followed by a half-written one (simulated crash).
		content := string(data) + "\n" + `{"kind":"usage","task_id":"t2","out`
		if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		s := newTestStore(t, dir)
		days := s.Days()
		if len(days) != 1 || days[0].Tokens.Output != 5 {
			t.Errorf("days = %+v, want only the complete row", days)
		}
	})
}
