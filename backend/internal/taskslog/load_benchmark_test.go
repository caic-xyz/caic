// Benchmarks realistic warm and cold task-log adoption scans.

package taskslog

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

	"github.com/klauspost/compress/zstd"
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
	name string
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

func (f *adoptionBenchmarkFixture) header() *agent.MetaMessage {
	return &agent.MetaMessage{Prompt: "benchmark adoption", Repos: []agent.MetaRepo{{Name: "org/repo", Branch: "caic-0"}}, Harness: harness.Claude}
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
	store := NewStore(testLogger(), fixture.dir)
	bl := testLogger()
	operations := []adoptionBenchmarkOperation{
		{
			name: "LoadLogHeader",
			prepare: func() func() error {
				return func() error {
					if _, err := loadLogHeader(bl, fixture.path, true); err != nil {
						return err
					}
					return nil
				}
			},
		},
		{
			// LoadLogHeaderScan measures the full-decode scan path by dropping
			// the header cache before every iteration, the first-run and
			// log-changed case.
			name: "LoadLogHeaderScan",
			prepare: func() func() error {
				return func() error {
					if err := os.Remove(logHeaderCachePath(fixture.path)); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
					if _, err := loadLogHeader(bl, fixture.path, true); err != nil {
						return err
					}
					return nil
				}
			},
		},
		{
			name: "StoreLoad",
			prepare: func() func() error {
				return func() error {
					logs, err := NewStore(bl, fixture.dir).LoadUnsettled()
					if err != nil {
						return err
					}
					if len(logs) != 1 {
						return fmt.Errorf("Store.Load returned %d tasks, want 1", len(logs))
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
			name: "StreamMessagesLatestMatch",
			prepare: func() func() error {
				lt := fixture.loadedTask()
				return func() error {
					var latest *agent.TextMessage
					for parsed, err := range lt.StreamMessages(b.Context()) {
						if err != nil {
							return err
						}
						if text, ok := parsed.Message.(*agent.TextMessage); ok {
							latest = text
						}
					}
					if latest == nil {
						return errors.New("stream contained no text message")
					}
					return nil
				}
			},
		},
		{
			name: "BackwardMessagesLatestMatch",
			prepare: func() func() error {
				lt := fixture.loadedTask()
				return func() error {
					for parsed, err := range lt.BackwardMessages(b.Context()) {
						if err != nil {
							return err
						}
						if _, ok := parsed.Message.(*agent.TextMessage); ok {
							return nil
						}
					}
					return errors.New("stream contained no text message")
				}
			},
		},
		{
			name: "StoreReopen",
			prepare: func() func() error {
				return func() error {
					w, _, err := store.Reopen(fixture.name, fixture.header())
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
					logs, err := NewStore(bl, fixture.dir).LoadForTaskIDs([]string{fixture.id.String()})
					if err != nil {
						return err
					}
					if len(logs) != 1 {
						return fmt.Errorf("Store.LoadForTaskIDs returned %d tasks, want 1", len(logs))
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
					w, _, err := store.Reopen(fixture.name, fixture.header())
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
	name := id.String() + "-org-repo-caic-0.jsonl"
	path := filepath.Join(dir, name)
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
		// End the log with a terminal result trailer so the fixture models a
		// compressed, terminal task log — the set the header cache targets.
		// Its size is reserved from the padding so the fixture stays exactly
		// target bytes.
		result := benchmarkJSONLine(b, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		resultBytes := int64(len(result) + 1)
		remaining := target - written
		const prefix = `{"type":"assistant","message":{"content":[{"type":"text","text":"`
		const suffix = `"}]}}`
		payloadBytes := remaining - int64(len(prefix)+len(suffix)+1) - resultBytes
		if payloadBytes < 0 {
			err = fmt.Errorf("fixture remainder %d cannot hold padding and result", remaining)
		} else {
			if _, err = bw.WriteString(prefix); err == nil {
				_, err = bw.Write(bytes.Repeat([]byte{'p'}, int(payloadBytes)))
			}
			if err == nil {
				_, err = bw.WriteString(suffix + "\n")
			}
			if err == nil {
				_, err = writeBenchmarkRecord(bw, result)
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
	return &adoptionBenchmarkFixture{dir: dir, name: name, path: path, size: target, id: id}
}

// BenchmarkDecodeDiscriminatorProbe measures per-line discriminator cost over
// realistic record shapes, dominated by long non-meta records that carry large
// tool outputs. b/report bytes/sec tracks the line size so shape changes are
// comparable.
func BenchmarkDecodeDiscriminatorProbe(b *testing.B) {
	largeOutput := strings.Repeat("tool output line\\n", 16<<10)
	cases := []struct {
		name    string
		version agent.LogVersion
		line    string
	}{
		{
			name:    "V1LargeToolResult",
			version: agent.LogVersionV1,
			line:    `{"type":"user","parent_tool_use_id":"tool-1","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"` + largeOutput + `"}]}}`,
		},
		{
			name:    "V1Assistant",
			version: agent.LogVersionV1,
			line:    `{"type":"assistant","message":{"content":[{"type":"text","text":"Completed one adoption step."}]}}`,
		},
		{
			name:    "V1Meta",
			version: agent.LogVersionV1,
			line:    `{"type":"caic_meta","version":1,"harness":"claude","prompt":"benchmark","repos":[{"name":"org/repo","branch":"caic-0"}]}`,
		},
		{
			name:    "V1MarkerLater",
			version: agent.LogVersionV1,
			line:    `{"type":"system","subtype":"init","model":"claude","padding":"` + strings.Repeat("x", 1<<16) + `","t":"caic_meta"}`,
		},
		{
			name:    "V2Agent",
			version: agent.LogVersionV2,
			line:    `{"t":"agent","ts":1767225600.5,"message":{"content":[{"type":"text","text":"working"}]}}`,
		},
		{
			name:    "V2LargeToolResult",
			version: agent.LogVersionV2,
			line:    `{"t":"user","ts":1767225600.5,"message":{"content":[{"t":"tool_result","tool_use_id":"tool-1","content":"` + largeOutput + `"}]}}`,
		},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			line := []byte(tc.line)
			b.SetBytes(int64(len(line)))
			b.ResetTimer()
			for range b.N {
				_, _, _ = decodeDiscriminatorProbe(line, tc.version)
			}
		})
	}
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

// BenchmarkSettledScanAgeFilter measures the cold-start scan of the settled
// (compressed) log set with and without the mtime retention cutoff. The cutoff
// is applied before decompression, so old logs are never decoded. The fixture
// is half recent and half old .zst logs; both variants clear the header cache
// each iteration to model a cold start, so the timing is dominated by zstd
// decompression and the ratio shows the cutoff's win.
func BenchmarkSettledScanAgeFilter(b *testing.B) {
	b.StopTimer()
	const nLogs = 16
	const perLog = 384 << 10 // 384 KiB each -> 6 MiB total
	dir := b.TempDir()
	oldTime := time.Now().UTC().Add(-30 * 24 * time.Hour)
	cutoff := time.Now().UTC().Add(-14 * 24 * time.Hour)
	for i := range nLogs {
		name := fmt.Sprintf("%02d.jsonl.zst", i)
		writeSettledBenchLog(b, dir, name, "org/repo", perLog)
		if i%2 == 0 {
			path := filepath.Join(dir, name)
			if err := os.Chtimes(path, oldTime, oldTime); err != nil {
				b.Fatal(err)
			}
		}
	}
	clearHeaderCaches(b, dir)
	goruntime.GC()

	for _, tc := range []struct {
		name   string
		cutoff time.Time
	}{
		{name: "no_age_filter", cutoff: time.Time{}},
		{name: "with_age_filter", cutoff: cutoff},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.StopTimer()
			clearHeaderCaches(b, dir)
			b.StartTimer()
			for range b.N {
				b.StopTimer()
				clearHeaderCaches(b, dir)
				b.StartTimer()
				st := NewStore(testLogger(), dir)
				st.cutoff, st.maxSettledPerRepo = tc.cutoff, 0
				if _, err := st.LoadSettled(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// writeSettledBenchLog writes a zstd-compressed terminal task log for repo of
// roughly size uncompressed bytes (multi-block) so decompressing has a cost.
func writeSettledBenchLog(b *testing.B, dir, name, repo string, size int) {
	meta := benchmarkJSONLine(b, agent.MetaMessage{
		MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "settled bench",
		Repos: []agent.MetaRepo{{Name: repo, Branch: "caic-0"}}, Harness: harness.Claude,
		StartedAt: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	})
	trailer := benchmarkJSONLine(b, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
	const prefix = `{"type":"assistant","message":{"content":[{"type":"text","text":"`
	const suffix = `"}]}}`
	textLen := max(size-len(meta)-len(trailer)-len(prefix)-len(suffix)-4, 0)
	lines := [][]byte{meta, []byte(prefix + strings.Repeat("p", textLen) + suffix), trailer}

	path := filepath.Clean(filepath.Join(dir, name))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	for _, line := range lines {
		if _, err := enc.Write(line); err != nil {
			_ = errors.Join(enc.Close(), f.Close())
			b.Fatal(err)
		}
		if _, err := enc.Write([]byte("\n")); err != nil {
			_ = errors.Join(enc.Close(), f.Close())
			b.Fatal(err)
		}
	}
	if err := errors.Join(enc.Close(), f.Close()); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkSettledLoadCap compares fully decoding every settled log against the
// header-only per-repo pre-cap that decodes only the kept survivors. Both runs
// cap reads at 8 workers (as the real load does). The header cache is cleared
// each iteration to model a cold start. Files are multi-block so the header
// read (first block) is meaningfully cheaper than the full decode.
func BenchmarkSettledLoadCap(b *testing.B) {
	b.StopTimer()
	const nRepos = 5
	const perRepo = 16
	const perLog = 1 << 20 // 1 MiB each -> 80 MiB total, multi-block
	dir := b.TempDir()
	cutoff := time.Now().UTC().Add(-14 * 24 * time.Hour)
	for r := range nRepos {
		for i := range perRepo {
			name := fmt.Sprintf("b%03d-org-repo%02d-caic-0.jsonl.zst", r*perRepo+i, r)
			writeSettledBenchLog(b, dir, name, fmt.Sprintf("org/repo%02d", r), perLog)
		}
	}
	goruntime.GC()

	for _, tc := range []struct {
		name         string
		limitPerRepo int
	}{
		{name: "full_decode", limitPerRepo: 0}, // decode every settled log
		{name: "precap", limitPerRepo: 5},      // decode only the 5/repo survivors
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				b.StopTimer()
				clearHeaderCaches(b, dir)
				b.StartTimer()
				st := NewStore(testLogger(), dir)
				st.cutoff, st.maxSettledPerRepo = cutoff, tc.limitPerRepo
				if _, err := st.LoadSettled(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// clearHeaderCaches removes the per-log header cache sidecars so the next scan
// decodes from the .zst files, modeling a cold start.
func clearHeaderCaches(b testing.TB, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), headerCacheExt) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.Fatal(err)
		}
	}
}
