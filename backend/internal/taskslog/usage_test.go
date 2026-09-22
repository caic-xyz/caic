// Usage rollup projection tests reconstruct retained logs and publish their rows.

package taskslog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

func TestStoreUsageRows(t *testing.T) {
	t.Parallel()

	t.Run("reconstructs v2 and v3 by producer day and skips v1", func(t *testing.T) {
		t.Parallel()
		usageDir, logStore, rollup := newUsageStores(t)
		writeUsageLog(t, logStore.LogDir, ksid.NewID().String(), agent.LogVersionV1, usageAt(4), usageAt(4))
		writeUsageLog(t, logStore.LogDir, ksid.NewID().String(), agent.LogVersionV2, usageAt(5), usageAt(5))
		writeUsageLog(t, logStore.LogDir, ksid.NewID().String(), agent.LogVersionV3, usageAt(6), usageAt(6))
		if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context(), usageResolver())); err != nil {
			t.Fatal(err)
		}
		days := rollup.Days()
		if len(days) != 2 || days[0].Day != "2026-02-05" || days[1].Day != "2026-02-06" {
			t.Fatalf("days = %+v", days)
		}
		for _, day := range days {
			if day.Tokens.Output != 7 || day.Turns != 1 || day.CostUSD != 0 || len(day.Skills) != 0 {
				t.Errorf("day %s = %+v", day.Day, day)
			}
		}
		if _, err := os.Stat(filepath.Join(usageDir, ".backfill.done")); err != nil {
			t.Fatalf("backfill sentinel: %v", err)
		}
	})

	t.Run("skips unreadable logs and backfills valid history", func(t *testing.T) {
		t.Parallel()
		usageDir, logStore, rollup := newUsageStores(t)
		if err := os.MkdirAll(logStore.LogDir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeUsageLog(t, logStore.LogDir, "valid", agent.LogVersionV2, usageAt(5), usageAt(5))
		if err := os.WriteFile(filepath.Join(logStore.LogDir, "broken.jsonl"), []byte("not json\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context(), usageResolver())); err != nil {
			t.Fatal(err)
		}
		days := rollup.Days()
		if len(days) != 1 || days[0].Day != "2026-02-05" || days[0].Tokens.Output != 7 {
			t.Errorf("days = %+v", days)
		}
		if _, err := os.Stat(filepath.Join(usageDir, ".backfill.done")); err != nil {
			t.Fatalf("backfill sentinel: %v", err)
		}
	})

	t.Run("reports cancellation before an empty scan", func(t *testing.T) {
		t.Parallel()
		_, logStore, _ := newUsageStores(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var got error
		for _, err := range logStore.UsageRows(ctx, usageResolver()) {
			got = err
		}
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("UsageRows error = %v, want context.Canceled", got)
		}
	})

	t.Run("orders tasks by start time across header batches", func(t *testing.T) {
		t.Parallel()
		_, logStore, _ := newUsageStores(t)
		const tasks = 9
		for i := range tasks {
			id := fmt.Sprintf("task%02d", i)
			writeUsageLog(t, logStore.LogDir, id, agent.LogVersionV2, usageAt(5), usageAt(tasks-i))
		}
		var got []string
		for row, err := range logStore.UsageRows(t.Context(), usageResolver()) {
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, row.TaskID)
		}
		want := []string{"task08", "task07", "task06", "task05", "task04", "task03", "task02", "task01", "task00"}
		if !slices.Equal(got, want) {
			t.Errorf("TaskIDs = %v, want %v", got, want)
		}
	})
}

func newUsageStores(t *testing.T) (usageDir string, logStore *taskslog.Store, rollup *usagedb.Store) {
	dir := t.TempDir()
	usageDir = filepath.Join(dir, "usagedb")
	var err error
	rollup, err = usagedb.New(usagedb.Config{Log: slog.New(slog.DiscardHandler), Dir: usageDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rollup.Close() })
	return usageDir, taskslog.NewStore(slog.New(slog.DiscardHandler), filepath.Join(dir, "tasks")), rollup
}

func usageResolver() agent.Backends {
	return agent.Backends{harness.Claude: claudecode.New()}
}

func usageAt(day int) time.Time {
	return time.Date(2026, time.February, day, 10, 0, 0, 0, time.UTC)
}

func writeUsageLog(t *testing.T, dir, id string, version agent.LogVersion, at, startedAt time.Time) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := agent.MetaMessage{
		MessageType:    "caic_meta",
		Version:        int(version),
		Prompt:         "backfill",
		Repos:          []agent.MetaRepo{{Name: "org/repo"}},
		Harness:        harness.Claude,
		RequestedModel: "claude-test",
		StartedAt:      startedAt,
	}
	header, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if version != agent.LogVersionV1 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(header, &obj); err != nil {
			t.Fatal(err)
		}
		delete(obj, "type")
		obj["t"] = json.RawMessage(`"caic_meta"`)
		header, err = json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
	}
	data := make([]byte, 0, len(header)+128)
	data = append(data, header...)
	data = append(data, '\n')
	native := `{"type":"result","duration_api_ms":3,"duration_ms":7,"num_turns":1,"usage":{"output_tokens":7}}`
	if version == agent.LogVersionV1 {
		data = append(data, native...)
	} else {
		data = append(data, fmt.Sprintf(`{"t":"agent","ts":%d.%03d,"msg":%s}`, at.Unix(), at.Nanosecond()/int(time.Millisecond), native)...)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
