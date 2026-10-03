// Tests and benchmarks the local caic task-log corpus without modifying its source.
//go:build real_task_logs

package taskslog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"
)

const realTaskLogDirEnv = "CAIC_REAL_TASK_LOG_DIR"

func TestCopyRealTaskLogCorpusLinksSameFilesystemLogs(t *testing.T) {
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
// -tags=real_task_logs ./backend/internal/taskslog -run '^TestRealTaskLogCorpus$'
// -timeout=10m.
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
	tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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
		if err := loaded.LogVersion.Validate(); err != nil {
			t.Fatalf("loaded task %s has invalid log version: %v", loaded.TaskID, err)
		}
	}
	t.Logf("loaded logs=%d candidates=%d physical-bytes=%d source=%s", len(tasks), len(paths), bytes, source)
}

// BenchmarkRealTaskLogCorpus measures inventory loading of a copied production
// task-log corpus. It is disabled by default; run it with: go test
// -tags=real_task_logs ./backend/internal/taskslog -run '^$' -bench
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
		if _, err := NewStore(testLogger(), dir).LoadUnsettled(); err != nil {
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
		if entry.IsDir() || !IsLogName(entry.Name()) {
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
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
// go test -tags=real_task_logs ./backend/internal/taskslog -run '^$'
// -bench '^BenchmarkRealTaskLogReplay$' -benchtime=1x -benchmem -timeout=10m.
// Large logs can require several GiB of memory; this is outside make benchmark.
func BenchmarkRealTaskLogReplay(b *testing.B) {
	path := os.Getenv("CAIC_REAL_TASK_LOG_FILE")
	if path == "" {
		b.Skip("set CAIC_REAL_TASK_LOG_FILE to a frozen production log")
	}
	info, err := os.Stat(path)
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
		b.ReportMetric(float64(len(loaded.RelayRecords)), "records/op")
	}
}
