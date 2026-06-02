// Tests for md runtime adapter configuration initialization and defaults.

package mdruntime

import (
	"path/filepath"
	"testing"
)

func TestNew(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpDir, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmpDir, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, ".local", "state"))
	c, err := New("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("New returned nil client")
	}
}
