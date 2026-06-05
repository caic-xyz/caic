// Tests for the standalone voice gateway command handlers.

package main

import (
	"path/filepath"
	"testing"
)

func TestMainImpl(t *testing.T) {
	t.Parallel()
	t.Run("rejects disabled webrtc port", func(t *testing.T) {
		t.Parallel()
		err := mainImpl([]string{
			"-config", filepath.Join(t.TempDir(), "missing.toml"),
			"-udp-port", "-1",
		})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
