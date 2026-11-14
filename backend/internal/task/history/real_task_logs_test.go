// Tests and benchmarks the local caic task-log corpus without modifying its source.
//go:build real_task_logs

package history

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/taskslog"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"
)

const realTaskLogDirEnv = "CAIC_REAL_TASK_LOG_DIR"

func TestCopyRealTaskLogCorpusLinksSameFilesystemLogs(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "tasks")
	sourcePath := filepath.Join(source, "0123456789AB-test-main.jsonl")
	if err := os.WriteFile(sourcePath, []byte("task log\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bytes, err := copyRealTaskLogCorpus(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if bytes != int64(len("task log\n")) {
		t.Errorf("bytes = %d, want %d", bytes, len("task log\n"))
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	destinationInfo, err := os.Stat(filepath.Join(destination, filepath.Base(sourcePath)))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceInfo, destinationInfo) {
		t.Error("staged task log is not linked to its source")
	}
}

// TestRealTaskLogCorpus verifies that the production loader accepts every
// copied log it recognizes and skips corrupt historical files. It is disabled
// by default because the corpus is large. Run it with: go test
// -tags=real_task_logs ./backend/internal/task/history -run '^TestRealTaskLogCorpus$'
// -timeout=10m.
//
//nolint:paralleltest // Replaying the corpus can need several GiB of memory.
func TestRealTaskLogCorpus(t *testing.T) {
	source, err := realTaskLogSource()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "tasks")
	bytes, err := copyRealTaskLogCorpus(source, dir)
	if err != nil {
		t.Fatal(err)
	}

	paths, err := logPaths(testLogger(), dir, nil, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := NewStore(testLogger(), taskslog.NewStore(dir), backends.Default("", nil)).LoadUnsettled()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Fatal("Store.Load returned no task logs")
	}
	for _, loaded := range tasks {
		if loaded.LogPath() == "" {
			t.Fatal("loaded task has no log path")
		}
	}
	t.Logf("loaded logs=%d candidates=%d physical-bytes=%d source=%s", len(tasks), len(paths), bytes, source)
}

// BenchmarkRealTaskLogCorpus measures inventory loading of a copied production
// task-log corpus. It is disabled by default; run it with: go test
// -tags=real_task_logs ./backend/internal/task/history -run '^$' -bench
// '^BenchmarkRealTaskLogCorpus$' -benchtime=1x -timeout=10m.
func BenchmarkRealTaskLogCorpus(b *testing.B) {
	source, err := realTaskLogSource()
	if err != nil {
		b.Fatal(err)
	}
	dir := filepath.Join(b.TempDir(), "tasks")
	bytes, err := copyRealTaskLogCorpus(source, dir)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(bytes)
	b.ResetTimer()
	for range b.N {
		// Clear the header cache each iteration to measure the cold scan, the
		// same way the synthetic settled benchmarks do.
		clearHeaderCaches(b, dir)
		if _, err := NewStore(testLogger(), taskslog.NewStore(dir), backends.Default("", nil)).LoadUnsettled(); err != nil {
			b.Fatal(err)
		}
	}
}

func realTaskLogSource() (string, error) {
	if source := os.Getenv(realTaskLogDirEnv); source != "" {
		return source, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".cache", "caic", "tasks"), nil
}

func copyRealTaskLogCorpus(source, destination string) (int64, error) {
	if _, err := os.Stat(source); err != nil {
		return 0, fmt.Errorf("stat task-log corpus %s: %w", source, err)
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return 0, fmt.Errorf("create copied task-log corpus: %w", err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return 0, fmt.Errorf("read task-log corpus: %w", err)
	}
	var bytes int64
	for _, entry := range entries {
		if entry.IsDir() || !taskslog.IsLogName(entry.Name()) {
			continue
		}
		sourcePath := filepath.Join(source, entry.Name())
		destinationPath := filepath.Join(destination, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return 0, fmt.Errorf("stat task log %s: %w", sourcePath, err)
		}
		if err := linkOrCopyRealTaskLog(sourcePath, destinationPath); err != nil {
			return 0, fmt.Errorf("copy task log %s: %w", sourcePath, err)
		}
		bytes += info.Size()
	}
	return bytes, nil
}

func linkOrCopyRealTaskLog(source, destination string) error {
	if err := os.Link(source, destination); err == nil {
		return nil
	}
	in, err := os.Open(source) //nolint:gosec // Paths come from the operator's corpus and a test-owned directory.
	if err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // Paths come from the operator's corpus and a test-owned directory.
	if err != nil {
		return errors.Join(err, in.Close())
	}
	_, copyErr := io.Copy(out, in)
	if err := errors.Join(copyErr, out.Close(), in.Close()); err != nil {
		removeErr := os.Remove(destination)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return errors.Join(err, removeErr)
	}
	return nil
}

// BenchmarkRealTaskLogReplay measures complete semantic replay of one frozen
// production log. Set CAIC_REAL_TASK_LOG_FILE to a snapshot, then run:
// go test -tags=real_task_logs ./backend/internal/task/history -run '^$'
// -bench '^BenchmarkRealTaskLogReplay$' -benchtime=1x -benchmem -timeout=10m.
// Large logs can require several GiB of memory; this is outside make benchmark.
func BenchmarkRealTaskLogReplay(b *testing.B) {
	path := os.Getenv("CAIC_REAL_TASK_LOG_FILE")
	if path == "" {
		b.Skip("set CAIC_REAL_TASK_LOG_FILE to a frozen production log")
	}
	info, err := os.Stat(path) //nolint:gosec // The operator names the frozen log.
	if err != nil {
		b.Fatal(err)
	}
	resolver := backends.Default(b.TempDir(), nil)
	b.SetBytes(info.Size())
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		loaded, err := loadSemanticTask(path, resolver)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(loaded.Timeline)), "messages/op")
	}
}

// TestRealTaskLogRecordedResultParity verifies the premise of answering a
// completed task's latest message from its recorded result: for every staged
// terminal log whose newest root message is a text or result, the result trailer
// holds the same text. A task stopped while it waits for an answer legitimately
// ends on a question and is skipped. It is disabled by default; run it with: go
// test -tags=real_task_logs ./backend/internal/task/history -run
// '^TestRealTaskLogRecordedResultParity$' -timeout=30m.
//
//nolint:paralleltest // Replaying the corpus can need several GiB of memory.
func TestRealTaskLogRecordedResultParity(t *testing.T) {
	checked := 0
	for _, c := range realTaskLogColdQueryFixture(t, realTaskLogColdQueryLogs*4) {
		lt, err := loadLogHeader(testLogger(), c.path, false, resolverForBenchmark())
		if err != nil {
			continue
		}
		last := lastAgentMessageOf(t, lt)
		if last == nil {
			continue
		}
		if _, ok := last.(*agent.AskMessage); ok {
			continue
		}
		checked++
		if got, want := agentMessageText(last), lt.LastTrailer.AgentResult; got != want {
			t.Errorf("%s: ordered scan found %q, recorded result is %q", filepath.Base(c.path), clip(got), clip(want))
		}
	}
	if checked == 0 {
		t.Fatal("no terminal log carried a comparable recorded result")
	}
	t.Logf("recorded result matched the ordered scan for %d logs", checked)
}

// BenchmarkRealTaskLogColdQuery compares the two ways to answer "what did this
// task say last" for the largest terminal logs in the corpus: reading the
// completion result already recorded in the log, or replaying the whole log in
// recorded order. It stages links in a temporary directory so the header cache
// it writes never lands beside the corpus. Run it with: go test
// -tags=real_task_logs ./backend/internal/task/history -run '^$'
// -bench '^BenchmarkRealTaskLogColdQuery$' -benchtime=1x -timeout=30m.
func BenchmarkRealTaskLogColdQuery(b *testing.B) {
	for _, c := range realTaskLogColdQueryFixture(b, realTaskLogColdQueryLogs) {
		lt, err := loadLogHeader(testLogger(), c.path, true, resolverForBenchmark())
		if err != nil {
			b.Fatal(err)
		}
		b.Run(filepath.Base(c.path), func(b *testing.B) {
			b.ReportMetric(float64(c.size), "log-bytes")
			b.Run("header-read", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if _, err := loadLogHeader(testLogger(), c.path, true, resolverForBenchmark()); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("ordered-scan", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if last := lastAgentMessageOf(b, lt); last == nil {
						b.Fatal("history holds no candidate message")
					}
				}
			})
		})
	}
}

// realTaskLogColdQueryLogs bounds how many staged corpus logs the cold-query
// benchmark and parity test keep, largest first.
const realTaskLogColdQueryLogs = 6

// realTaskLogColdQueryFixture stages the largest terminal corpus logs that carry
// a recorded result, so the header cache these consumers write never lands beside
// the corpus.
func realTaskLogColdQueryFixture(t testing.TB, limit int) []realTaskLogCandidate {
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	var staged []realTaskLogCandidate
	for _, c := range largestRealTaskLogs(t, limit*4) {
		lt, err := loadLogHeader(testLogger(), c.path, false, resolverForBenchmark())
		if err != nil || !lt.State.IsTerminal() || lt.LastTrailer == nil || lt.LastTrailer.AgentResult == "" {
			// The corpus keeps retired log formats the loader rejects.
			continue
		}
		if len(staged) == limit {
			break
		}
		stagedPath := filepath.Join(dir, filepath.Base(c.path))
		if err := linkOrCopyRealTaskLog(c.path, stagedPath); err != nil {
			t.Fatal(err)
		}
		staged = append(staged, realTaskLogCandidate{stagedPath, c.size})
	}
	if len(staged) == 0 {
		t.Fatal("task-log corpus holds no loadable terminal log with a recorded result")
	}
	return staged
}

// lastAgentMessageOf returns the newest root text, question, or result message
// carrying a payload in recorded order.
func lastAgentMessageOf(t testing.TB, lt *LoadedTask) agent.Message {
	var last agent.Message
	for parsed, err := range lt.StreamMessages(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		candidate := parsed.Message
		if candidate.Provenance().IsChild() {
			continue
		}
		switch m := candidate.(type) {
		case *agent.ResultMessage:
			if m.Result != "" {
				last = m
			}
		case *agent.AskMessage:
			if len(m.Questions) > 0 {
				last = m
			}
		case *agent.TextMessage:
			if m.Text != "" {
				last = m
			}
		}
	}
	return last
}

// agentMessageText returns the payload of a compared last-message candidate.
func agentMessageText(m agent.Message) string {
	switch m := m.(type) {
	case *agent.ResultMessage:
		return m.Result
	case *agent.TextMessage:
		return m.Text
	default:
		return ""
	}
}

func clip(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

type realTaskLogCandidate struct {
	path string
	size int64
}

// largestRealTaskLogs returns the biggest plain or compressed corpus logs.
func largestRealTaskLogs(t testing.TB, limit int) []realTaskLogCandidate {
	dir, err := realTaskLogSource()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var cands []realTaskLogCandidate
	for _, entry := range entries {
		if entry.IsDir() || !taskslog.IsLogName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		cands = append(cands, realTaskLogCandidate{filepath.Join(dir, entry.Name()), info.Size()})
	}
	slices.SortFunc(cands, func(a, b realTaskLogCandidate) int { return int(b.size - a.size) })
	return cands[:min(len(cands), limit)]
}

func resolverForBenchmark() ParserResolver { return backends.Default("", nil) }
