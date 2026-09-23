// Tests missing-cost backfill, durable estimates, and live cost resume.

package usagedb

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maruel/ksid"
)

func TestBackfillMissingCosts(t *testing.T) {
	t.Parallel()
	t.Run("reconciles estimated and reported costs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s := newTestStore(t, dir)
		missing := testMeta(ksid.NewID())
		reported := testMeta(ksid.NewID())
		s.Observe(missing, &Event{At: atUTC(5, 10, 0, 0), Model: "missing", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}}})
		s.Observe(reported, &Event{At: atUTC(5, 10, 0, 0), Model: "reported", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 5}}})
		s.Observe(reported, &Event{At: atUTC(5, 10, 0, 1), Model: "reported", CostUSD: 0.4, TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 20}}})
		s.ObserveQuota(&QuotaChange{At: atUTC(5, 10, 0, 2), Provider: "codex", Window: "5h", Status: "available"})
		day := "2026-02-05"
		path := filepath.Join(dir, day+".jsonl")
		before, err := os.ReadFile(path) //nolint:gosec // test fixture path built from t.TempDir().
		if err != nil {
			t.Fatal(err)
		}
		estimate := func(row *UsageRow) (float64, bool) {
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
		if quotas := readQuotaRows(t, dir, day); len(quotas) != 1 {
			t.Errorf("quota rows after cost backfill = %+v", quotas)
		}
		if got := s.Days()[0]; got.CostUSD != 0.65 || got.Models["missing"].CostUSD != 0.25 || got.Harnesses["claude"].CostUSD != 0.65 {
			t.Errorf("day totals after cost backfill = %+v", got)
		}
		if err := s.BackfillMissingCosts(t.Context(), func(*UsageRow) (float64, bool) { return 1, true }); err != nil {
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
		reopened.Observe(missing, &Event{At: atUTC(5, 10, 0, 3), Model: "missing", CostUSD: 0.35, TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 2}}})
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
		if err := s.BackfillMissingCosts(t.Context(), func(*UsageRow) (float64, bool) { return 0.1, true }); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path) //nolint:gosec // test fixture path built from t.TempDir().
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"future_field":{"nested":true}`) || !strings.HasSuffix(string(data), `{"kind":"usage","day":"2026-02-05","task_id":"bad","cost_usd":`) {
			t.Errorf("cost backfill lost unrelated fields or truncated tail: %s", data)
		}
	})

	t.Run("skips task with failed cost append", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t, t.TempDir())
		meta := testMeta(ksid.NewID())
		s.Observe(meta, &Event{At: atUTC(5, 10, 0, 0), Model: "priced", TurnBoundary: true, Delta: Delta{TokenBuckets: TokenBuckets{Output: 10}}})
		s.Observe(meta, &Event{At: atUTC(5, 10, 0, 1), Model: "priced", CostUSD: 0.2, Delta: Delta{TokenBuckets: TokenBuckets{Output: 5}}})
		s.mu.Lock()
		s.pending[meta.TaskID.String()].costInFlight = 0.2 // Simulate cost assigned to a bucket whose append failed.
		s.mu.Unlock()
		if err := s.BackfillMissingCosts(t.Context(), func(*UsageRow) (float64, bool) { return 0.1, true }); err != nil {
			t.Fatal(err)
		}
		rows := readRows(t, s.dir, "2026-02-05")
		if len(rows) != 1 || rows[0].CostUSD != 0 || rows[0].CostEstimated {
			t.Errorf("row with cost in flight was estimated: %+v", rows)
		}
	})
}
