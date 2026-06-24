// Tests for the record-trace command helpers.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/harness"
)

func TestBuildPodmanRunArgs(t *testing.T) {
	t.Run("valid_without_api_key_or_login", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		workDir := filepath.Join(t.TempDir(), "work")

		args, err := buildPodmanRunArgs(workDir, harness.Claude, "ANTHROPIC_API_KEY")
		if err != nil {
			t.Fatalf("buildPodmanRunArgs: %v", err)
		}
		if slices.Contains(args, "-e") {
			t.Fatalf("args contain API-key env injection: %v", args)
		}
		if slices.Contains(args, "--mount") {
			t.Fatalf("args contain credential mount for missing login: %v", args)
		}
	})

	t.Run("valid_mounts_logged_in_account", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		claudeDir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(claudeDir, 0o700); err != nil {
			t.Fatalf("mkdir .claude: %v", err)
		}
		workDir := filepath.Join(t.TempDir(), "work")

		args, err := buildPodmanRunArgs(workDir, harness.Claude, "")
		if err != nil {
			t.Fatalf("buildPodmanRunArgs: %v", err)
		}
		want := fmt.Sprintf("type=bind,source=%s,target=/home/user/.claude", claudeDir)
		if !slices.Contains(args, "--mount") || !slices.Contains(args, want) {
			t.Fatalf("args = %v, want credential mount %q", args, want)
		}
	})

	t.Run("valid_mounts_opencode_login_paths", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		paths := []string{".opencode", ".local/share/opencode", ".local/state/opencode"}
		for _, path := range paths {
			if err := os.MkdirAll(filepath.Join(home, path), 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", path, err)
			}
		}
		workDir := filepath.Join(t.TempDir(), "work")

		args, err := buildPodmanRunArgs(workDir, harness.OpenCode, "")
		if err != nil {
			t.Fatalf("buildPodmanRunArgs: %v", err)
		}
		wants := []string{
			fmt.Sprintf("type=bind,source=%s,target=/home/user/.opencode", filepath.Join(home, ".opencode")),
			fmt.Sprintf("type=bind,source=%s,target=/home/user/.local/share/opencode", filepath.Join(home, ".local", "share", "opencode")),
			fmt.Sprintf("type=bind,source=%s,target=/home/user/.local/state/opencode", filepath.Join(home, ".local", "state", "opencode")),
		}
		for _, want := range wants {
			if !slices.Contains(args, want) {
				t.Fatalf("args = %v, want credential mount %q", args, want)
			}
		}
	})

	t.Run("valid_api_key_env_is_optional_but_supported", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("OPENAI_API_KEY", "test-key")
		workDir := filepath.Join(t.TempDir(), "work")

		args, err := buildPodmanRunArgs(workDir, harness.Codex, "OPENAI_API_KEY")
		if err != nil {
			t.Fatalf("buildPodmanRunArgs: %v", err)
		}
		if !slices.Contains(args, "-e") || !slices.Contains(args, "OPENAI_API_KEY=test-key") {
			t.Fatalf("args = %v, want API-key env injection", args)
		}
	})
}

func TestSetupCodexAuth(t *testing.T) {
	t.Parallel()

	t.Run("valid_skips_when_api_key_env_is_unset", func(t *testing.T) {
		t.Parallel()
		if err := setupCodexAuth(t.Context(), "unused", ""); err != nil {
			t.Fatalf("setupCodexAuth: %v", err)
		}
	})
}
