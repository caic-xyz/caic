// Tests for compact EventMessage replay sidecars.

package eventreplay

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestCacheWriter(t *testing.T) {
	t.Parallel()

	t.Run("live_append_without_seed_does_not_commit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("existing raw log\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		w, err := NewMessageWriter(logPath, harness.Claude)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteMessage(&agent.TextMessage{Text: "partial append"})
		if err := w.Commit(logPath); err == nil {
			t.Fatal("Commit succeeded for partial live append cache without seed")
		}

		if _, ok := OpenReplay(logPath); ok {
			t.Fatal("partial live append cache was committed for existing raw log without seed")
		}
	})

	t.Run("full_rebuild_can_commit_existing_log", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("existing raw log\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		w, err := NewCacheWriter(logPath)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"rebuilt"}}`))
		if err := w.Commit(logPath); err != nil {
			t.Fatal(err)
		}

		if replay, ok := OpenReplay(logPath); !ok {
			t.Fatal("rebuilt cache was not committed")
		} else {
			replay.Close()
		}
	})
}

func TestPruneStaleCaches(t *testing.T) {
	t.Parallel()

	t.Run("valid_kept", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("raw\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := NewCacheWriter(logPath)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"ok"}}`))
		if err := w.Commit(logPath); err != nil {
			t.Fatal(err)
		}

		removed, err := PruneStaleCaches(dir)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 0 {
			t.Fatalf("removed = %d, want 0", removed)
		}
		if _, err := os.Stat(CachePath(logPath)); err != nil {
			t.Fatalf("valid cache removed: %v", err)
		}
	})

	t.Run("orphan_removed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cachePath := filepath.Join(dir, "orphan.events.zst")
		if err := os.WriteFile(cachePath, []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}

		removed, err := PruneStaleCaches(dir)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 1 {
			t.Fatalf("removed = %d, want 1", removed)
		}
		if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
			t.Fatalf("cache still exists: %v", err)
		}
	})

	t.Run("stale_removed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := NewCacheWriter(logPath)
		if err != nil {
			t.Fatal(err)
		}
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"old"}}`))
		if err := w.Commit(logPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte("new content\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(logPath, future, future); err != nil {
			t.Fatal(err)
		}

		removed, err := PruneStaleCaches(dir)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 1 {
			t.Fatalf("removed = %d, want 1", removed)
		}
		if _, err := os.Stat(CachePath(logPath)); !os.IsNotExist(err) {
			t.Fatalf("stale cache still exists: %v", err)
		}
	})

	t.Run("temp_files_removed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		paths := []string{
			filepath.Join(dir, "task.events.zst.123.body"),
			filepath.Join(dir, "task.events.zst.456.tmp"),
			filepath.Join(dir, "unrelated.tmp"),
		}
		for _, path := range paths {
			if err := os.WriteFile(path, []byte("temp"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		removed, err := PruneStaleCaches(dir)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 2 {
			t.Fatalf("removed = %d, want 2", removed)
		}
		for _, path := range paths[:2] {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("temp file still exists: %s: %v", path, err)
			}
		}
		if _, err := os.Stat(paths[2]); err != nil {
			t.Fatalf("unrelated temp file removed: %v", err)
		}
	})
}
