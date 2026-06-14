// Tests for compact EventMessage replay sidecars.

package eventreplay

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneStaleCaches(t *testing.T) {
	t.Parallel()

	t.Run("valid_kept", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("raw\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		w := NewCacheWriter(logPath)
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"ok"}}`))
		w.Commit(logPath)

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
		w := NewCacheWriter(logPath)
		w.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"old"}}`))
		w.Commit(logPath)
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
}
