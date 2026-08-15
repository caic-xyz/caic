// Tests LogStore task-log compression and terminal-log selection.

package task

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestLogStoreCompressPath(t *testing.T) {
	t.Parallel()
	t.Run("Success", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		const contents = "first record\nsecond record\n"
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}

		compressed, err := (&LogStore{}).compressPath(path)
		if err != nil {
			t.Fatal(err)
		}
		if compressed != compressedLogPath(path) {
			t.Fatalf("compressed path = %q, want %q", compressed, compressedLogPath(path))
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("plain log stat = %v, want os.ErrNotExist", err)
		}
		r, err := openLogReader(compressed)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(r)
		closeErr := r.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if string(data) != contents {
			t.Fatalf("compressed contents = %q, want %q", data, contents)
		}
	})

	t.Run("FailurePreservesPlainSource", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		const contents = "task log\n"
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(compressedLogPath(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if _, err := (&LogStore{}).compressPath(path); err == nil {
			t.Fatal("compressPath returned nil error")
		}
		data, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != contents {
			t.Fatalf("plain contents = %q, want %q", data, contents)
		}
		if _, err := os.Stat(compressedLogTempPath(path)); !os.IsNotExist(err) {
			t.Fatalf("temporary compressed log stat = %v, want os.ErrNotExist", err)
		}
	})
}

func TestLogStoreCompressTerminalLogs(t *testing.T) {
	t.Parallel()
	t.Run("CompressesPurged", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "purged.jsonl")
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: "task"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, filepath.Base(path), meta, trailer)
		updated := time.Date(2024, time.June, 1, 2, 3, 4, 0, time.UTC)
		if err := os.Chtimes(path, updated, updated); err != nil {
			t.Fatal(err)
		}
		logs := []*LoadedTask{{path: path, State: StatePurged}}
		if err := (&LogStore{LogDir: dir}).CompressTerminalLogs(logs); err != nil {
			t.Fatal(err)
		}
		if logs[0].LogPath() != compressedLogPath(path) {
			t.Fatalf("purged log path = %q, want %q", logs[0].LogPath(), compressedLogPath(path))
		}
		info, err := os.Stat(logs[0].LogPath())
		if err != nil {
			t.Fatal(err)
		}
		if logs[0].LogSize != info.Size() {
			t.Fatalf("compressed log size = %d, want %d", logs[0].LogSize, info.Size())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("plain log stat = %v, want os.ErrNotExist", err)
		}
		reloaded, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(reloaded) != 1 {
			t.Fatalf("reloaded logs = %d, want 1", len(reloaded))
		}
		if !reloaded[0].LastStateUpdateAt.Equal(updated) {
			t.Fatalf("reloaded update time = %s, want %s", reloaded[0].LastStateUpdateAt, updated)
		}
	})

	t.Run("RetriesPlainSourceAfterInterruptedReplacement", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "purged.jsonl")
		if err := os.WriteFile(path, []byte("stale source\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (&LogStore{}).compressPath(path); err != nil {
			t.Fatal(err)
		}
		const contents = "authoritative source\n"
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		paths, err := logPaths(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 || paths[0] != path {
			t.Fatalf("log paths = %q, want plain source %q", paths, path)
		}
		logs := []*LoadedTask{{path: path, State: StatePurged}}
		if err := (&LogStore{LogDir: dir}).CompressTerminalLogs(logs); err != nil {
			t.Fatal(err)
		}
		r, err := openLogReader(logs[0].LogPath())
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(r)
		closeErr := r.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if string(data) != contents {
			t.Fatalf("retried compressed contents = %q, want %q", data, contents)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("plain source stat = %v, want os.ErrNotExist", err)
		}
	})

	t.Run("SkipsStopped", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "stopped.jsonl")
		if err := os.WriteFile(path, []byte("task log\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		logs := []*LoadedTask{{path: path, State: StateStopped}}
		if err := (&LogStore{LogDir: filepath.Dir(path)}).CompressTerminalLogs(logs); err != nil {
			t.Fatal(err)
		}
		if logs[0].LogPath() != path {
			t.Fatalf("stopped log path = %q, want %q", logs[0].LogPath(), path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stopped plain log stat = %v", err)
		}
	})
}
