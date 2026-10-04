// Tests Codex model-discovery subprocess failures.

package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

func TestBackend(t *testing.T) {
	t.Run("FetchModelInventory", func(t *testing.T) {
		t.Run("stderr_failure", func(t *testing.T) {
			dir := t.TempDir()
			// A failed SSH discovery must report an error even when stderr arrives.
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf 'discovery failed\\n' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // Fake SSH executable in a private test directory.
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			backend := New("", nil)
			if _, err := backend.FetchModelInventory(t.Context(), runtime.ConnectionTarget{SSHHost: "fake-container"}, nil); err == nil {
				t.Fatal("failed discovery returned no error")
			}
		})
	})
}
