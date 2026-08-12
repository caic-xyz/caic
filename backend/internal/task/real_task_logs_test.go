// Tests and benchmarks the local caic task-log corpus without modifying its source.
//go:build real_task_logs

package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const realTaskLogDirEnv = "CAIC_REAL_TASK_LOG_DIR"

// TestRealTaskLogCorpus verifies that the production loader accepts every
// copied log it recognizes and skips corrupt historical files. It is disabled
// by default because the corpus is large. Run it with: go test
// -tags=real_task_logs ./backend/internal/task -run '^TestRealTaskLogCorpus$'
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

	paths, err := logPaths(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := LoadLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Fatal("LoadLogs returned no task logs")
	}
	for _, task := range tasks {
		snapshot := task.ValidatedSnapshot()
		if snapshot == nil {
			t.Errorf("load %s: no validated snapshot", filepath.Base(task.LogPath()))
			continue
		}
		if snapshot.Authority.Version != task.LogVersion {
			t.Errorf("load %s: snapshot version %d, want %d", filepath.Base(task.LogPath()), snapshot.Authority.Version, task.LogVersion)
		}
	}
	t.Logf("loaded logs=%d candidates=%d physical-bytes=%d source=%s", len(tasks), len(paths), bytes, source)
}

// BenchmarkRealTaskLogCorpus measures inventory loading of a copied production
// task-log corpus. It is disabled by default; run it with: go test
// -tags=real_task_logs ./backend/internal/task -run '^$' -bench
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
	b.Run("LoadLogsWithoutSummaries", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(bytes)
		b.ResetTimer()
		for range b.N {
			if err := removeRealTaskLogSummaries(dir); err != nil {
				b.Fatal(err)
			}
			if _, err := LoadLogs(dir); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("LoadLogsWithSummaries", func(b *testing.B) {
		if _, err := LoadLogs(dir); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.SetBytes(bytes)
		b.ResetTimer()
		for range b.N {
			if _, err := LoadLogs(dir); err != nil {
				b.Fatal(err)
			}
		}
	})
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
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			return 0, fmt.Errorf("read task log %s: %w", sourcePath, err)
		}
		if err := os.WriteFile(destinationPath, data, 0o600); err != nil {
			return 0, fmt.Errorf("copy task log %s: %w", sourcePath, err)
		}
		bytes += int64(len(data))
	}
	return bytes, nil
}

func removeRealTaskLogSummaries(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".taskmeta.json") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
