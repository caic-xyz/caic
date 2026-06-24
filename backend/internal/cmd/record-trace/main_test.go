// Tests for the record-trace command helpers.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/harness"
	claudedto "github.com/maruel/genai/providers/claudecode"
)

func TestBuildPodmanRunArgs(t *testing.T) {
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

func TestAnswerClaudeControlRequest(t *testing.T) {
	t.Parallel()
	t.Run("AskUserQuestion", func(t *testing.T) {
		t.Parallel()
		line := []byte(`{"type":"control_request","request_id":"req-1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which greeting should main.go print?","header":"Greeting","options":[{"label":"Hello"},{"label":"Hi"}],"multiSelect":false}]},"tool_use_id":"toolu-1"}}`)
		var out bytes.Buffer
		if err := answerClaudeControlRequest(&out, line, "Hi"); err != nil {
			t.Fatal(err)
		}

		var got claudedto.InputControlResponseMsg
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Type != claudedto.InputControlResponse {
			t.Fatalf("Type = %q, want %q", got.Type, claudedto.InputControlResponse)
		}
		if got.Response.RequestID != "req-1" {
			t.Fatalf("RequestID = %q, want req-1", got.Response.RequestID)
		}
		if got.Response.Response.Behavior != claudedto.ControlCanUseToolBehaviorAllow {
			t.Fatalf("Behavior = %q, want %q", got.Response.Response.Behavior, claudedto.ControlCanUseToolBehaviorAllow)
		}

		var updated claudedto.AskUserQuestionUpdatedInput
		if err := json.Unmarshal(got.Response.Response.UpdatedInput, &updated); err != nil {
			t.Fatal(err)
		}
		if len(updated.Questions) != 1 {
			t.Fatalf("Questions len = %d, want 1", len(updated.Questions))
		}
		const question = "Which greeting should main.go print?"
		if updated.Answers[question] != "Hi" {
			t.Fatalf("answer = %q, want Hi", updated.Answers[question])
		}
	})
	t.Run("OtherTool", func(t *testing.T) {
		t.Parallel()
		line := []byte(`{"type":"control_request","request_id":"req-2","request":{"subtype":"can_use_tool","tool_name":"Read","input":{"file_path":"main.go"},"tool_use_id":"toolu-2"}}`)
		var out bytes.Buffer
		if err := answerClaudeControlRequest(&out, line, ""); err != nil {
			t.Fatal(err)
		}

		var got claudedto.InputControlResponseMsg
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Response.RequestID != "req-2" {
			t.Fatalf("RequestID = %q, want req-2", got.Response.RequestID)
		}
		if len(got.Response.Response.UpdatedInput) != 0 {
			t.Fatalf("UpdatedInput = %s, want empty", got.Response.Response.UpdatedInput)
		}
	})
}
