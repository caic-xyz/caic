// Tests startup removal of obsolete replay artifacts.

package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupLegacyReplayArtifacts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"task.events.zst", "task.taskmeta.json", "task.jsonl", "task.jsonl.zst", "notes.events.zst.keep"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, ".replay-tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".replay-tmp", "interrupted.tmp"), []byte("temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested.events.zst"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := cleanupLegacyReplayArtifacts(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"task.events.zst", "task.taskmeta.json", ".replay-tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("legacy artifact %q stat error = %v, want not exist", name, err)
		}
	}
	for _, name := range []string{"task.jsonl", "task.jsonl.zst", "notes.events.zst.keep", "nested.events.zst"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("non-legacy entry %q stat error = %v", name, err)
		}
	}
}
