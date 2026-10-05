// Tests historical usage and quota storage shapes, backfill, and restart recovery.

package usagedb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
)

func TestHistoricalRows(t *testing.T) {
	t.Parallel()
	t.Run("historical complete row survives backfill and restart", func(t *testing.T) {
		t.Parallel()
		const historical = `{"input_tokens":10,"cache_write_5m":20,"cache_write_1h":30,"cache_read":40,"output_tokens":50,"reasoning_tokens":60,"turns":2,"errored_turns":1,"api_ms":1500,"wall_ms":4000,"compactions":1,"skill_reads":{"review":2},"tool_calls":{"Read":3},"tool_timings":{"Read":{"count":2,"duration_ms":250}},"subagent_spawns":3,"subagent_spawns_background":1,"context_window":200000,"cost_usd":0.25,"kind":"usage","day":"2026-02-05","ts":1770285600123,"task_id":"historical","harness":"claude","repos":["github/caic"],"model":"claude-opus","cost_estimated":true}`
		var row data.UsageRow
		if err := json.Unmarshal([]byte(historical), &row); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		s := newTestStore(t, dir)
		if err := s.Backfill(t.Context(), func(yield func(data.UsageRow, error) bool) { yield(row, nil) }); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "2026-02-05.jsonl")) //nolint:gosec // Temporary fixture directory.
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != historical+"\n" {
			t.Fatalf("persisted row = %s, want historical shape", raw)
		}
		before := s.Days()
		if len(before) != 1 || before[0].Tokens.TotalInput() != 100 || before[0].ToolTimings["Read"].DurationMs != 250 || before[0].CostUSD != .25 {
			t.Fatalf("aggregate = %+v", before)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := newTestStore(t, dir)
		if got := reopened.Days(); !reflect.DeepEqual(got, before) {
			t.Fatalf("recovered = %+v, want %+v", got, before)
		}
		if got := reopened.watermarks["historical"]; !got.Equal(time.UnixMilli(1770285600123)) {
			t.Fatalf("watermark = %v", got)
		}
		if reopened.flushedCost["historical"] != .25 {
			t.Fatalf("cost baseline = %v", reopened.flushedCost)
		}
	})
	t.Run("historical quota restart deduplicates", func(t *testing.T) {
		t.Parallel()
		const historical = `{"kind":"quota","day":"2026-02-05","ts":1770285600123,"provider":"claude","window":"five_hour","status":"blocked","utilization":-1,"resets_at":1770303600123}`
		dir := t.TempDir()
		path := filepath.Join(dir, "2026-02-05.jsonl")
		if err := os.WriteFile(path, []byte(historical+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := newTestStore(t, dir)
		s.ObserveQuota(&QuotaChange{At: time.UnixMilli(1770285601123), Provider: "claude", Window: "five_hour", Status: "blocked", Utilization: -1, ResetsAt: time.UnixMilli(1770303600123)})
		if got := readQuotaRows(t, dir); len(got) != 1 {
			t.Fatalf("quota rows = %+v", got)
		}
		s.ObserveQuota(&QuotaChange{At: time.UnixMilli(1770285602123), Provider: "claude", Window: "five_hour", Status: "available", Utilization: -1, ResetsAt: time.UnixMilli(1770303600123)})
		raw, err := os.ReadFile(path) //nolint:gosec // Temporary fixture directory.
		if err != nil {
			t.Fatal(err)
		}
		const added = `{"kind":"quota","day":"2026-02-05","ts":1770285602123,"provider":"claude","window":"five_hour","status":"available","utilization":-1,"resets_at":1770303600123}`
		if string(raw) != historical+"\n"+added+"\n" {
			t.Fatalf("quota JSON = %s", raw)
		}
	})
}
