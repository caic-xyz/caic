// Usage rollup projection benchmarks measure retained-log reconstruction and publication.

package taskslog_test

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

const (
	usageBenchmarkTasks   = 8
	usageBenchmarkRecords = 64
)

// BenchmarkUsageBackfill measures the background path from a read-only scan
// of retained v3 logs through publication of their neutral daily rows.
func BenchmarkUsageBackfill(b *testing.B) {
	b.StopTimer()
	dir := b.TempDir()
	logDir := filepath.Join(dir, "tasks")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		b.Fatal(err)
	}
	times := make([]time.Time, usageBenchmarkRecords)
	for i := range times {
		times[i] = time.Date(2026, time.February, 5, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour)
	}
	var bytes int
	for range usageBenchmarkTasks {
		bytes += writeUsageBenchmarkLog(b, logDir, ksid.NewID().String(), times)
	}

	logStore := taskslog.NewStore(slog.New(slog.DiscardHandler), logDir)
	resolver := usageResolver()
	b.SetBytes(int64(bytes))
	b.ReportAllocs()
	b.StartTimer()
	for i := range b.N {
		rollup, err := usagedb.New(b.Context(), usagedb.Config{
			Log: slog.New(slog.DiscardHandler),
			Dir: filepath.Join(dir, "rollups", strconv.Itoa(i)),
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := rollup.Backfill(b.Context(), logStore.UsageRows(b.Context(), resolver)); err != nil {
			b.Fatal(err)
		}
		if err := rollup.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func writeUsageBenchmarkLog(b *testing.B, dir, id string, times []time.Time) int {
	meta := agent.MetaMessage{
		MessageType:    "caic_meta",
		Version:        3,
		Prompt:         "backfill benchmark",
		Repos:          []v3.MetaRepo{{Name: "org/repo"}},
		Harness:        string(harness.Claude),
		RequestedModel: "claude-test",
	}
	header, err := agent.MarshalLogMessage(&meta)
	if err != nil {
		b.Fatal(err)
	}

	data := make([]byte, 0, len(header)+len(times)*128)
	data = append(data, header...)
	data = append(data, '\n')
	native := `{"type":"result","duration_api_ms":3,"duration_ms":7,"num_turns":1,"usage":{"output_tokens":7}}`
	for _, at := range times {
		data = append(data, fmt.Sprintf(`{"t":"agent","ts":%d.%03d,"msg":%s}`, at.Unix(), at.Nanosecond()/int(time.Millisecond), native)...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), data, 0o600); err != nil {
		b.Fatal(err)
	}
	return len(data)
}
