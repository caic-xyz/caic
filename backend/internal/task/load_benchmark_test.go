// Benchmarks realistic warm and cold task-log adoption scans.
//go:build adoption_benchmark

package task

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

const (
	adoptionBenchmarkBytesEnv = "CAIC_ADOPTION_BENCH_BYTES"
	defaultAdoptionBenchBytes = 128 << 20
	maxAdoptionBenchBytes     = 4 << 30
	minAdoptionBenchBytes     = 1 << 20
)

var errAdoptionColdUnsupported = errors.New("cold task-adoption benchmark unsupported")

type adoptionBenchmarkFixture struct {
	dir  string
	path string
	size int64
	id   ksid.ID
}

func (f *adoptionBenchmarkFixture) loadedTask() *LoadedTask {
	lt := &LoadedTask{
		TaskID:     f.id.String(),
		LogVersion: agent.LogVersionV1,
		Harness:    harness.Claude,
		LogSize:    f.size,
		path:       f.path,
	}
	lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
		return claudecode.New().NewWire().ParseMessage, nil
	})
	return lt
}

func (f *adoptionBenchmarkFixture) task() *Task {
	return &Task{
		ID:            f.id,
		InitialPrompt: agent.Prompt{Text: "benchmark adoption"},
		Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
		Harness:       harness.Claude,
	}
}

type adoptionBenchmarkOperation struct {
	name    string
	prepare func() func() error
}

type adoptionProcessIO struct {
	rchar     uint64
	readBytes uint64
}

func BenchmarkTaskAdoptionPrimitives(b *testing.B) {
	b.StopTimer()
	fixture := newAdoptionBenchmarkFixture(b)
	store := &LogStore{LogDir: fixture.dir}
	operations := []adoptionBenchmarkOperation{
		{
			name: "LoadLogs",
			prepare: func() func() error {
				return func() error {
					logs, err := LoadLogs(fixture.dir)
					if err != nil {
						return err
					}
					if len(logs) != 1 {
						return fmt.Errorf("LoadLogs returned %d tasks, want 1", len(logs))
					}
					return nil
				}
			},
		},
		{
			name: "LoadSessionMetadata",
			prepare: func() func() error {
				lt := fixture.loadedTask()
				return lt.LoadSessionMetadata
			},
		},
		{
			name: "LoadMessages",
			prepare: func() func() error {
				lt := fixture.loadedTask()
				return lt.LoadMessages
			},
		},
		{
			name: "LoadMessagesTail",
			prepare: func() func() error {
				lt := fixture.loadedTask()
				return lt.LoadMessagesTail
			},
		},
		{
			name: "LogStoreReopen",
			prepare: func() func() error {
				task := fixture.task()
				return func() error {
					w, err := store.Reopen(task)
					if err != nil {
						return err
					}
					return w.Close()
				}
			},
		},
		{
			name: "CombinedLiveAdoption",
			prepare: func() func() error {
				return func() error {
					logs, err := LoadLogsForTaskIDs(fixture.dir, []string{fixture.id.String()})
					if err != nil {
						return err
					}
					if len(logs) != 1 {
						return fmt.Errorf("LoadLogsForTaskIDs returned %d tasks, want 1", len(logs))
					}
					lt := logs[0]
					lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return claudecode.New().NewWire().ParseMessage, nil
					})
					if lt.SessionID == "" || lt.AgentVersion == "" {
						if err := lt.LoadSessionMetadata(); err != nil {
							return err
						}
					}
					if err := lt.LoadMessages(); err != nil {
						return err
					}
					task := fixture.task()
					task.SetLogPath(lt.LogPath())
					task.SetLogValidationSnapshot(lt.ValidatedSnapshot())
					w, err := store.Reopen(task)
					if err != nil {
						return err
					}
					return w.Close()
				}
			},
		},
	}

	b.ReportMetric(float64(fixture.size), "fixture-bytes")
	for _, op := range operations {
		op := op
		b.Run(op.name, func(b *testing.B) {
			b.Run("warm", func(b *testing.B) {
				runAdoptionSubbenchmark(b, fixture, op, false)
			})
			b.Run("cold", func(b *testing.B) {
				runAdoptionSubbenchmark(b, fixture, op, true)
			})
		})
	}
}

func runAdoptionSubbenchmark(b *testing.B, fixture *adoptionBenchmarkFixture, op adoptionBenchmarkOperation, cold bool) {
	b.StopTimer()
	b.ReportAllocs()
	b.ReportMetric(float64(fixture.size), "fixture-bytes")
	if cold {
		if err := prepareColdAdoptionFixture(fixture.path); err != nil {
			if errors.Is(err, errAdoptionColdUnsupported) {
				b.Skipf("cold cache unsupported: %v", err)
			}
			b.Fatal(err)
		}
	} else {
		if err := op.prepare()(); err != nil {
			b.Fatalf("prime %s: %v", op.name, err)
		}
	}
	goruntime.GC()
	before, haveProcessIO := readAdoptionProcessIO()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		goruntime.GC()
		if cold {
			if err := prepareColdAdoptionFixture(fixture.path); err != nil {
				if errors.Is(err, errAdoptionColdUnsupported) {
					b.Skipf("cold cache unsupported: %v", err)
				}
				b.Fatal(err)
			}
		}
		run := op.prepare()
		b.StartTimer()
		if err := run(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if after, ok := readAdoptionProcessIO(); haveProcessIO && ok {
		n := max(1, b.N)
		rchar := after.rchar - before.rchar
		readBytes := after.readBytes - before.readBytes
		b.ReportMetric(float64(rchar)/float64(n), "rchar/op")
		b.ReportMetric(float64(readBytes)/float64(n), "read_bytes/op")
		b.ReportMetric(float64(rchar)/float64(n)/float64(fixture.size), "rchar/fixture")
	}
}

func newAdoptionBenchmarkFixture(b *testing.B) *adoptionBenchmarkFixture {
	target := adoptionBenchmarkBytes(b)
	dir := b.TempDir()
	id := ksid.NewID()
	path := filepath.Join(dir, id.String()+"-org-repo-caic-0.jsonl")
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	header := benchmarkJSONLine(b, agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Prompt:      "benchmark adoption",
		Repos:       []agent.MetaRepo{{Name: "org/repo", Branch: "caic-0"}},
		Harness:     harness.Claude,
		StartedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	})
	session := benchmarkJSONLine(b, agent.MetaSessionMessage{
		MessageType:  "caic_session",
		SessionID:    "benchmark-session",
		Model:        "claude-sonnet-4-6",
		AgentVersion: "2.1.0",
	})
	records := benchmarkAdoptionRecords(header)
	written, err := writeBenchmarkRecord(bw, header)
	if err == nil {
		var n int64
		n, err = writeBenchmarkRecord(bw, session)
		written += n
	}
	const paddingReserve = int64(1 << 20)
	for i := 0; err == nil; i++ {
		record := records[i%len(records)]
		if written+int64(len(record)+1) > target-paddingReserve {
			break
		}
		var n int64
		n, err = writeBenchmarkRecord(bw, record)
		written += n
	}
	if err == nil {
		remaining := target - written
		const prefix = `{"type":"assistant","message":{"content":[{"type":"text","text":"`
		const suffix = `"}]}}`
		payloadBytes := remaining - int64(len(prefix)+len(suffix)+1)
		if payloadBytes < 0 {
			err = fmt.Errorf("fixture remainder %d cannot hold padding record", remaining)
		} else {
			if _, err = bw.WriteString(prefix); err == nil {
				_, err = bw.Write(bytes.Repeat([]byte{'p'}, int(payloadBytes)))
			}
			if err == nil {
				_, err = bw.WriteString(suffix + "\n")
			}
		}
	}
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	if info.Size() != target {
		b.Fatalf("fixture size = %d, want %d", info.Size(), target)
	}
	b.Logf("fixture=%s bytes=%d mix=small-deltas,normal-events,large-tool-output,controls,repeated-segment-headers", path, target)
	return &adoptionBenchmarkFixture{dir: dir, path: path, size: target, id: id}
}

func benchmarkAdoptionRecords(header []byte) [][]byte {
	largeOutput := strings.Repeat("tool output line\\n", 16<<10)
	return [][]byte{
		[]byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"working"}}}`),
		[]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Completed one adoption step."}]}}`),
		[]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"/workspace/repo/main.go"}}]}}`),
		[]byte(`{"type":"user","parent_tool_use_id":"tool-1","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"` + largeOutput + `"}]}}`),
		[]byte(`{"type":"caic_diff_stat","diff_stat":[{"path":"main.go","added":4,"deleted":1}],"ts":1767225600.5}`),
		[]byte(`{"type":"caic_log","line":"restoring task workspace"}`),
		header,
	}
}

func benchmarkJSONLine(b *testing.B, v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		b.Fatal(err)
	}
	return data
}

func writeBenchmarkRecord(w *bufio.Writer, record []byte) (int64, error) {
	n, err := w.Write(record)
	if err != nil {
		return int64(n), err
	}
	m, err := w.WriteString("\n")
	return int64(n + m), err
}

func adoptionBenchmarkBytes(b *testing.B) int64 {
	value := os.Getenv(adoptionBenchmarkBytesEnv)
	if value == "" {
		return defaultAdoptionBenchBytes
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		b.Fatalf("%s=%q: %v", adoptionBenchmarkBytesEnv, value, err)
	}
	if n < minAdoptionBenchBytes || n > maxAdoptionBenchBytes {
		b.Fatalf("%s=%d outside [%d,%d]", adoptionBenchmarkBytesEnv, n, minAdoptionBenchBytes, maxAdoptionBenchBytes)
	}
	return n
}

func readAdoptionProcessIO() (adoptionProcessIO, bool) {
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return adoptionProcessIO{}, false
	}
	var stats adoptionProcessIO
	for line := range strings.SplitSeq(string(data), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return adoptionProcessIO{}, false
		}
		switch name {
		case "rchar":
			stats.rchar = n
		case "read_bytes":
			stats.readBytes = n
		}
	}
	return stats, stats.rchar > 0
}
