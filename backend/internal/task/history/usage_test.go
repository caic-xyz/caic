// Tests and benchmarks usage rollup projections from retained logs through publication.

package history_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"strconv"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task/history"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
	"github.com/maruel/ksid"
)

func TestStore(t *testing.T) {
	t.Parallel()

	t.Run("UsageRows", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			native   string
			wantUSD  float64
			want     data.CostProvenance
			estimate bool
		}{
			{"reported", `{"type":"result","total_cost_usd":0.25,"num_turns":1,"usage":{"output_tokens":100}}`, .25, data.CostProvenance{ReportedUSD: .25, ReportedParts: 1}, false},
			{"reported zero", `{"type":"result","total_cost_usd":0,"num_turns":1,"usage":{"output_tokens":100}}`, 0, data.CostProvenance{ReportedParts: 1}, false},
			{"estimated", `{"type":"result","num_turns":1,"usage":{"output_tokens":100}}`, .5, data.CostProvenance{EstimatedUSD: .5, EstimatedParts: 1}, true},
			{"unknown", `{"type":"result","num_turns":1}`, 0, data.CostProvenance{UnknownParts: 1}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				usageDir, logStore, rollup := newUsageStores(t)
				id := ksid.NewID().String()
				writeUsageLog(t, logStore.Journal.LogDir, id, usageAt(5), usageAt(5))
				path := filepath.Join(logStore.Journal.LogDir, id+".jsonl")
				raw, err := os.ReadFile(path) //nolint:gosec // test fixture path is built from t.TempDir().
				if err != nil {
					t.Fatal(err)
				}
				// Replace the helper's native result rather than appending an extra turn.
				lines := strings.Split(string(raw), "\n")
				lines[1] = fmt.Sprintf(`{"t":"agent","ts":%d.000,"msg":%s}`, usageAt(5).Unix(), tc.native)
				original := []byte(strings.Join(lines, "\n"))
				if err := os.WriteFile(path, original, 0o600); err != nil { //nolint:gosec // test fixture path is built from t.TempDir().
					t.Fatal(err)
				}
				if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context())); err != nil {
					t.Fatal(err)
				}
				if err := rollup.BackfillMissingCosts(t.Context(), func(row *data.UsageRow) (float64, bool) {
					if tc.want.ReportedParts != 0 {
						t.Error("reported cost reached mutable estimator")
					}
					return .5, tc.estimate
				}); err != nil {
					t.Fatal(err)
				}
				if err := rollup.Close(); err != nil {
					t.Fatal(err)
				}
				restored, err := usagedb.New(t.Context(), usagedb.Config{Log: slog.New(slog.DiscardHandler), Dir: usageDir})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = restored.Close() })
				days := restored.Days()
				if len(days) != 1 || days[0].CostUSD != tc.wantUSD || days[0].Cost != tc.want {
					t.Fatalf("restored cost = %+v; want %v %+v", days, tc.wantUSD, tc.want)
				}
				after, err := os.ReadFile(path) //nolint:gosec // owned test fixture.
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(after, original) {
					t.Fatal("backfill modified immutable native log")
				}
			})
		}

		t.Run("undated controls preserve reported session totals", func(t *testing.T) {
			t.Parallel()
			usageDir, logStore, rollup := newUsageStores(t)
			id := ksid.NewID().String()
			writeUsageLog(t, logStore.Journal.LogDir, id, usageAt(5), usageAt(5))
			path := filepath.Join(logStore.Journal.LogDir, id+".jsonl")
			raw, err := os.ReadFile(path) //nolint:gosec // owned test fixture.
			if err != nil {
				t.Fatal(err)
			}
			header, _, _ := strings.Cut(string(raw), "\n")
			original := []byte(header + "\n" +
				fmt.Sprintf(`{"t":"agent","ts":%d.000,"msg":{"type":"system","subtype":"init","model":"first-session"}}`+"\n", usageAt(5).Unix()) +
				fmt.Sprintf(`{"t":"agent","ts":%d.000,"msg":{"type":"result","total_cost_usd":2,"num_turns":1}}`+"\n", usageAt(5).Unix()) +
				`{"t":"context_cleared"}` + "\n" +
				`{"t":"exit","exit_code":0}` + "\n" +
				fmt.Sprintf(`{"t":"agent","ts":%d.000,"msg":{"type":"system","subtype":"init","model":"second-session"}}`+"\n", usageAt(6).Unix()) +
				fmt.Sprintf(`{"t":"agent","ts":%d.000,"msg":{"type":"result","total_cost_usd":0.5,"num_turns":1}}`+"\n", usageAt(6).Unix()))
			if err := os.WriteFile(path, original, 0o600); err != nil { //nolint:gosec // test fixture path is built from t.TempDir().
				t.Fatal(err)
			}
			if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context())); err != nil {
				t.Fatal(err)
			}
			if err := rollup.Close(); err != nil {
				t.Fatal(err)
			}
			restored, err := usagedb.New(t.Context(), usagedb.Config{Log: slog.New(slog.DiscardHandler), Dir: usageDir})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restored.Close() })
			days := restored.Days()
			if len(days) != 2 || days[0].Day != "2026-02-05" || days[1].Day != "2026-02-06" {
				t.Fatalf("dated rows=%+v", days)
			}
			for i, want := range []struct {
				usd   float64
				model string
			}{{2, "first-session"}, {.5, "second-session"}} {
				day := days[i]
				if day.CostUSD != want.usd || day.Cost != (data.CostProvenance{ReportedUSD: want.usd, ReportedParts: 1}) || day.Turns != 1 || len(day.Models) != 1 || day.Models[want.model].CostUSD != want.usd {
					t.Fatalf("session day=%+v, want reported %v on %s", day, want.usd, want.model)
				}
			}
			if days[0].CostUSD+days[1].CostUSD != 2.5 {
				t.Fatalf("session reports were replaced: %+v", days)
			}
			after, err := os.ReadFile(path) //nolint:gosec // owned test fixture.
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, original) {
				t.Fatal("backfill modified immutable native log")
			}
		})

		t.Run("Pi contributions survive native replay", func(t *testing.T) {
			t.Parallel()
			_, logStore, rollup := newUsageStores(t)
			if err := os.MkdirAll(logStore.Journal.LogDir, 0o700); err != nil {
				t.Fatal(err)
			}
			events := []string{
				`{"type":"agent_start"}`,
				`{"type":"turn_end","message":{"role":"assistant","usage":{"input":10,"totalTokens":10}}}`,
				`{"type":"compaction_end","reason":"manual","result":{"usage":{"input":3,"totalTokens":3}}}`,
				`{"type":"turn_end","message":{"role":"assistant","usage":{"input":20,"totalTokens":20}}}`,
				`{"type":"agent_end","messages":[{"role":"assistant","usage":{"input":10}},{"role":"assistant","usage":{"input":20}}]}`,
				`{"type":"agent_start"}`,
				`{"type":"agent_end","messages":[{"role":"assistant","usage":{"input":7}}]}`,
			}
			for range 2 {
				header, err := taskslog.EncodeHeader(&v3.MetaMessage{MessageType: "caic_meta", Version: 3, Harness: string(harness.Pi), Prompt: "usage", StartedAt: usageAt(5), RequestedModel: "anthropic/claude-opus-4-6"})
				if err != nil {
					t.Fatal(err)
				}
				raw := header
				raw = append(raw, '\n')
				for _, event := range events {
					raw = append(raw, fmt.Sprintf(`{"t":"agent","ts":%d.000,"msg":%s}`+"\n", usageAt(5).Unix(), event)...)
				}
				if err := os.WriteFile(filepath.Join(logStore.Journal.LogDir, ksid.NewID().String()+".jsonl"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context())); err != nil {
				t.Fatal(err)
			}
			days := rollup.Days()
			if len(days) != 1 || days[0].Tokens.Input != 80 || days[0].Turns != 4 || days[0].Compactions != 2 {
				t.Fatalf("restored accounting = %+v", days)
			}
		})

		t.Run("reconstructs mixed cache duration counts", func(t *testing.T) {
			t.Parallel()
			_, logStore, rollup := newUsageStores(t)
			for range 2 {
				writeUsageLog(t, logStore.Journal.LogDir, ksid.NewID().String(), usageAt(5), usageAt(5),
					`{"type":"result","num_turns":1,"usage":{"cache_creation_input_tokens":110,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":100}}}`)
			}
			if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context())); err != nil {
				t.Fatal(err)
			}
			days := rollup.Days()
			if len(days) != 1 || days[0].Tokens.CacheWriteCount(5*time.Minute) != 20 || days[0].Tokens.CacheWriteCount(time.Hour) != 200 {
				t.Fatalf("mixed cache tokens = %+v", days)
			}
		})

		t.Run("reconstructs producer days", func(t *testing.T) {
			t.Parallel()
			usageDir, logStore, rollup := newUsageStores(t)
			writeUsageLog(t, logStore.Journal.LogDir, ksid.NewID().String(), usageAt(5), usageAt(5))
			writeUsageLog(t, logStore.Journal.LogDir, ksid.NewID().String(), usageAt(6), usageAt(6))
			if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context())); err != nil {
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

		t.Run("counts reconstructed skill reads by name", func(t *testing.T) {
			t.Parallel()
			_, logStore, rollup := newUsageStores(t)
			writeUsageLog(t, logStore.Journal.LogDir, ksid.NewID().String(), usageAt(5), usageAt(5),
				usageSkillUse("sk_1", `{"skill":"go-code-quality"}`),
				usageSkillUse("sk_2", `{"skill":"go-code-quality"}`),
				usageSkillUse("sk_3", `{"skill":"review"}`),
				// A Skill block naming no skill is machinery, not a read.
				usageSkillUse("sk_4", `{}`),
			)
			if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context())); err != nil {
				t.Fatal(err)
			}
			days := rollup.Days()
			if len(days) != 1 {
				t.Fatalf("days = %+v", days)
			}
			// The leaderboard counts tasks, so the repeated read folds into one.
			want := map[string]int{"go-code-quality": 1, "review": 1}
			if !maps.Equal(days[0].Skills, want) {
				t.Errorf("Skills = %v, want %v", days[0].Skills, want)
			}
			if len(days[0].Tools) != 0 {
				t.Errorf("Tools = %v, want none", days[0].Tools)
			}
		})

		t.Run("reconstructed inferred reads require successful result", func(t *testing.T) {
			t.Parallel()
			_, logStore, rollup := newUsageStores(t)
			writeUsageLog(t, logStore.Journal.LogDir, ksid.NewID().String(), usageAt(5), usageAt(5),
				`{"type":"assistant","message":{"model":"claude-test","content":[{"type":"tool_use","id":"failed","name":"Read","input":{"file_path":"/home/user/.agents/skills/review/SKILL.md"}}],"usage":{}}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"failed","is_error":true,"content":"missing file"}]}}`,
				`{"type":"assistant","message":{"model":"claude-test","content":[{"type":"tool_use","id":"ok","name":"Read","input":{"file_path":"/home/user/.agents/skills/go-code-quality/SKILL.md"}}],"usage":{}}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ok","content":"loaded"}]}}`,
			)
			if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context())); err != nil {
				t.Fatal(err)
			}
			days := rollup.Days()
			if len(days) != 1 || days[0].Skills["review"] != 0 || days[0].Skills["go-code-quality"] != 1 {
				t.Errorf("confirmed skill reads = %+v", days)
			}
		})

		t.Run("reconstructs measured tool duration from producer timestamps", func(t *testing.T) {
			t.Parallel()
			_, logStore, rollup := newUsageStores(t)
			id := ksid.NewID().String()
			at := usageAt(5)
			writeUsageLog(t, logStore.Journal.LogDir, id, at, at,
				`{"type":"assistant","message":{"model":"claude-test","content":[{"type":"tool_use","id":"read-1","name":"Read","input":{"file_path":"/tmp/example"}}],"usage":{}}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"read-1","content":"ok"}]}}`,
			)
			path := filepath.Join(logStore.Journal.LogDir, id+".jsonl")
			raw, err := os.ReadFile(path) //nolint:gosec // test fixture path is built from t.TempDir().
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(raw), "\n")
			lines[2] = strings.Replace(lines[2], fmt.Sprintf(`"ts":%d.000`, at.Unix()), fmt.Sprintf(`"ts":%d.500`, at.Unix()+2), 1)
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil { //nolint:gosec // test fixture path is built from t.TempDir().
				t.Fatal(err)
			}
			if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context())); err != nil {
				t.Fatal(err)
			}
			days := rollup.Days()
			if len(days) != 1 || days[0].Tools["Read"] != 1 || !reflect.DeepEqual(days[0].ToolTimings["Read"], data.ToolCallTiming(2500)) {
				t.Errorf("backfilled tool totals = %+v", days)
			}
		})

		t.Run("skips unreadable logs and backfills valid history", func(t *testing.T) {
			t.Parallel()
			usageDir, logStore, rollup := newUsageStores(t)
			if err := os.MkdirAll(logStore.Journal.LogDir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeUsageLog(t, logStore.Journal.LogDir, "valid", usageAt(5), usageAt(5))
			if err := os.WriteFile(filepath.Join(logStore.Journal.LogDir, "broken.jsonl"), []byte("not json\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := rollup.Backfill(t.Context(), logStore.UsageRows(t.Context())); err != nil {
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
			for _, err := range logStore.UsageRows(ctx) {
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
				writeUsageLog(t, logStore.Journal.LogDir, id, usageAt(5), usageAt(tasks-i))
			}
			var got []string
			for row, err := range logStore.UsageRows(t.Context()) {
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
	})
}

func newUsageStores(t *testing.T) (usageDir string, logStore *history.Store, rollup *usagedb.Store) {
	dir := t.TempDir()
	usageDir = filepath.Join(dir, "usagedb")
	var err error
	rollup, err = usagedb.New(t.Context(), usagedb.Config{Log: slog.New(slog.DiscardHandler), Dir: usageDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rollup.Close() })
	return usageDir, history.NewStore(slog.New(slog.DiscardHandler), taskslog.NewStore(filepath.Join(dir, "tasks")), usageResolver()), rollup
}

func usageResolver() agent.Backends {
	return backends.Default("", nil)
}

func usageAt(day int) time.Time {
	return time.Date(2026, time.February, day, 10, 0, 0, 0, time.UTC)
}

// usageSkillUse renders one native Claude assistant record whose sole content
// block invokes the Skill tool with input.
func usageSkillUse(id, input string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"model":"claude-test","content":[{"type":"tool_use","id":%q,"name":"Skill","input":%s}],"usage":{}}}`, id, input)
}

// writeUsageLog writes one retained task log. Each extra record is a native
// harness message stamped at and emitted before the trailing result.
func writeUsageLog(t *testing.T, dir, id string, at, startedAt time.Time, extra ...string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := v3.MetaMessage{
		MessageType:    "caic_meta",
		Version:        3,
		Prompt:         "backfill",
		Repos:          []v3.MetaRepo{{Name: "org/repo"}},
		Harness:        string(harness.Claude),
		RequestedModel: "claude-test",
		StartedAt:      startedAt,
	}
	header, err := taskslog.EncodeHeader(&meta)
	if err != nil {
		t.Fatal(err)
	}

	encodedLog := make([]byte, 0, len(header)+128)
	encodedLog = append(encodedLog, header...)
	encodedLog = append(encodedLog, '\n')
	records := append(slices.Clone(extra), `{"type":"result","duration_api_ms":3,"duration_ms":7,"num_turns":1,"usage":{"output_tokens":7}}`)
	for _, native := range records {
		encodedLog = append(encodedLog, fmt.Sprintf(`{"t":"agent","ts":%d.%03d,"msg":%s}`, at.Unix(), at.Nanosecond()/int(time.Millisecond), native)...)

		encodedLog = append(encodedLog, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), encodedLog, 0o600); err != nil {
		t.Fatal(err)
	}
}

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
	var logBytes int
	for range usageBenchmarkTasks {
		logBytes += writeUsageBenchmarkLog(b, logDir, ksid.NewID().String(), times)
	}

	logStore := history.NewStore(slog.New(slog.DiscardHandler), taskslog.NewStore(logDir), usageResolver())

	b.SetBytes(int64(logBytes))
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
		if err := rollup.Backfill(b.Context(), logStore.UsageRows(b.Context())); err != nil {
			b.Fatal(err)
		}
		if err := rollup.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func writeUsageBenchmarkLog(b *testing.B, dir, id string, times []time.Time) int {
	meta := v3.MetaMessage{
		MessageType:    "caic_meta",
		Version:        3,
		Prompt:         "backfill benchmark",
		Repos:          []v3.MetaRepo{{Name: "org/repo"}},
		Harness:        string(harness.Claude),
		RequestedModel: "claude-test",
	}
	header, err := taskslog.EncodeHeader(&meta)
	if err != nil {
		b.Fatal(err)
	}

	buf := make([]byte, 0, len(header)+len(times)*128)
	buf = append(buf, header...)
	buf = append(buf, '\n')
	native := `{"type":"result","duration_api_ms":3,"duration_ms":7,"num_turns":1,"usage":{"output_tokens":7}}`
	for _, at := range times {
		buf = append(buf, fmt.Sprintf(`{"t":"agent","ts":%d.%03d,"msg":%s}`, at.Unix(), at.Nanosecond()/int(time.Millisecond), native)...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), buf, 0o600); err != nil {
		b.Fatal(err)
	}
	return len(buf)
}
