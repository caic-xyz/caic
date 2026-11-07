// Tests task-discussion Markdown rendering against reviewed golden fixtures.

package agent

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"

	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

var updateDiscussionGoldens = flag.Bool("update-discussion-goldens", false, "update reviewed discussion Markdown fixtures")

func TestRenderDiscussion(t *testing.T) {
	t.Parallel()

	t.Run("full_conversation", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "fix the bug", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, &MetaResultMessage{State: "completed", CostUSD: 0.05, Duration: 120, NumTurns: 3}, nil, []Message{
			&UserInputMessage{Text: "fix the bug"},
			&ThinkingMessage{Text: "Let me analyze the issue carefully and find the root cause."},
			&TextMessage{Text: "I'll look at the error logs first."},
			&ToolUseMessage{ToolUseID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"cat /var/log/app.log"}`)},
			&ToolResultMessage{ToolUseID: "t1"},
			&ToolUseMessage{ToolUseID: "t2", Name: "Edit", Input: json.RawMessage(`{"path":"main.go","old_text":"buggy","new_text":"fixed"}`)},
			&ToolResultMessage{ToolUseID: "t2"},
			&TextMessage{Text: "The bug is fixed. The issue was a nil pointer dereference."},
		})

		assertDiscussionGolden(t, "full-conversation.md", md)
	})

	t.Run("metadata_only", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "hello", Harness: "claude", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, nil)
		assertContains(t, md, "**Prompt**: hello")
		assertContains(t, md, "**Harness**: claude")
		assertContains(t, md, "# Task Discussion")
		assertContains(t, md, "---")
	})

	t.Run("with_PR_info", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "add feature", Harness: "claude", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, &MetaPRMessage{ForgeOwner: "octo", ForgeRepo: "app", ForgePR: 42}, []Message{
			&TextMessage{Text: "done"},
		})
		assertContains(t, md, "**PR**: octo/app#42")
	})

	t.Run("reported_settings", func(t *testing.T) {
		t.Parallel()
		meta := &MetaMessage{
			Prompt:          "task",
			Harness:         string(harness.Codex),
			RequestedModel:  "requested-model",
			RequestedEffort: "low",
		}
		md := RenderDiscussion(meta, nil, nil, []Message{
			&InitMessage{ReportedModel: "reported-model", ReportedEffort: "high"},
			&SystemMessage{Subtype: SystemSubtypeModelRerouted, ReportedModel: "rerouted-model"},
		})
		assertContains(t, md, "**Model**: rerouted-model")
		assertContains(t, md, "**Effort**: high")
		if strings.Contains(md, "requested-model") || strings.Contains(md, "**Effort**: low") {
			t.Fatalf("exported requested settings:\n%s", md)
		}
	})

	t.Run("with_flags", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "test", Harness: "pi", Tailscale: true, Display: true, StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, nil)
		assertContains(t, md, "**Flags**: tailscale, display")
	})

	t.Run("with_repos", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "test", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC), Repos: []v3.MetaRepo{{Name: "app", BaseBranch: "main", Branch: "fix-bug"}}}, nil, nil, nil)
		assertContains(t, md, "**Repo**: app (`main..fix-bug`)")
	})

	t.Run("with_error_result", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, &MetaResultMessage{State: "failed", CostUSD: 0.01, Duration: 10, Error: "container died"}, nil, []Message{
			&TextMessage{Text: "something went wrong"},
		})
		assertContains(t, md, "**State**: failed")
		assertContains(t, md, "**Error**: container died")
	})

	t.Run("tool_error", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&ToolResultMessage{ToolUseID: "t1", Error: "command not found"},
		})
		assertContains(t, md, "⚠️ Tool error: command not found")
	})

	t.Run("native_subagent_activity", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&NativeSubagentMessage{Subagent: NativeSubagent{ID: "child-1", Label: "analyze logs", Status: NativeSubagentStatusRunning}},
			&NativeSubagentMessage{Subagent: NativeSubagent{ID: "child-1", Scope: NativeSubagentScopeBatch, Status: NativeSubagentStatusPaused, Result: "awaiting input"}},
		})
		assertContains(t, md, "### 🤖 Native agent: analyze logs — running")
		assertContains(t, md, "### 🤖 Native batch: child-1 — paused")
		assertContains(t, md, "awaiting input")
	})

	t.Run("unreported_native_result", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "delegate", Harness: "dsh", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&NativeSubagentMessage{Subagent: NativeSubagent{ID: "child-1", Label: "analyze logs", Prompt: "Find the failure", Status: NativeSubagentStatusUnknown, Background: true}},
			&NativeSubagentMessage{Subagent: NativeSubagent{ID: "child-2", Label: "partial report", Status: NativeSubagentStatusUnknown, Result: "Work is still in progress"}},
		})
		assertDiscussionGolden(t, "unreported-native-result.md", md)
	})

	t.Run("compaction_boundary", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&SystemMessage{Subtype: "compact_boundary"},
		})
		assertContains(t, md, "Context compaction boundary")
	})

	t.Run("skips_control_messages", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&DiffStatMessage{},
			&ExitMessage{ExitCode: 0},
			&ModelInfoMessage{ContextWindow: 200000},
			&TextMessage{Text: "visible"},
		})
		assertDiscussionGolden(t, "skips-control-messages.md", md)
	})

	t.Run("thinking_truncated", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("a", 1000)
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&ThinkingMessage{Text: long},
		})
		if !strings.Contains(md, strings.Repeat("a", 500)+"...") {
			t.Error("thinking text was not truncated")
		}
	})

	t.Run("duration_hours", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, &MetaResultMessage{State: "completed", Duration: 7200}, nil, nil)
		assertContains(t, md, "**Duration**: 2.0h")
	})

	t.Run("duration_seconds", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, &MetaResultMessage{State: "completed", Duration: 45}, nil, nil)
		assertContains(t, md, "**Duration**: 45s")
	})

	t.Run("skips_empty_tool_use", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&TextMessage{Text: "Before"},
			&ToolUseMessage{ToolUseID: "t1", Name: "Read", Input: json.RawMessage(`{}`)},
			&ToolUseMessage{ToolUseID: "t2", Name: "Read", Input: json.RawMessage(`{"path":"main.go"}`)},
			&TextMessage{Text: "After"},
		})
		// The empty ToolUseMessage should be skipped.
		assertContains(t, md, "**File**: `main.go`")
		// There should be exactly one Tool use line.
		if strings.Count(md, "### 🔧 Tool:") != 1 {
			t.Errorf("expected 1 tool use, got %d", strings.Count(md, "### 🔧 Tool:"))
		}
	})

	t.Run("skips_null_tool_use_input", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&ToolUseMessage{ToolUseID: "t1", Name: "Read", Input: json.RawMessage(`null`)},
			&TextMessage{Text: "done"},
		})
		if strings.Contains(md, "### 🔧 Tool:") {
			t.Errorf("empty tool input rendered: %s", md)
		}
	})

	t.Run("write_tool_preview", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&ToolUseMessage{ToolUseID: "t1", Name: "Write", Input: json.RawMessage(`{"path":"out.txt","content":"hello world"}`)},
		})
		assertContains(t, md, "**File**: `out.txt`")
		assertContains(t, md, "hello world")
	})

	t.Run("read_tool_path", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&ToolUseMessage{ToolUseID: "t1", Name: "Read", Input: json.RawMessage(`{"path":"main.go"}`)},
		})
		assertContains(t, md, "**File**: `main.go`")
	})

	t.Run("grep_tool_pattern", func(t *testing.T) {
		t.Parallel()
		md := RenderDiscussion(&MetaMessage{Prompt: "task", Harness: "pi", StartedAt: time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC)}, nil, nil, []Message{
			&ToolUseMessage{ToolUseID: "t1", Name: "Grep", Input: json.RawMessage(`{"pattern":"TODO","path":"."}`)},
		})
		assertContains(t, md, "**Pattern**: `TODO`")
	})

	t.Run("title", func(t *testing.T) {
		t.Parallel()
		meta := &MetaMessage{
			Prompt:    "do something",
			Title:     "My Task Title",
			Harness:   string(harness.Pi),
			StartedAt: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		}
		md := RenderDiscussion(meta, nil, nil, nil)
		if !strings.Contains(md, "**Title**: My Task Title") {
			t.Errorf("missing title in output:\n%s", md)
		}
	})
}

func TestIsInputEmpty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		raw   json.RawMessage
		empty bool
	}{
		{"nil", nil, true},
		{"empty_slice", json.RawMessage{}, true},
		{"empty_obj", json.RawMessage(`{}`), true},
		{"null", json.RawMessage(`null`), true},
		{"empty_str", json.RawMessage(`""`), true},
		{"real_obj", json.RawMessage(`{"path":"x"}`), false},
		{"real_str", json.RawMessage(`"hello"`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isInputEmpty(tt.raw); got != tt.empty {
				t.Errorf("isInputEmpty(%s) = %v, want %v", tt.raw, got, tt.empty)
			}
		})
	}
}

func TestTrunc(t *testing.T) {
	t.Parallel()

	t.Run("short", func(t *testing.T) {
		t.Parallel()
		if got := trunc("hello", 10); got != "hello" {
			t.Errorf("trunc(%q, 10) = %q, want %q", "hello", got, "hello")
		}
	})

	t.Run("exact", func(t *testing.T) {
		t.Parallel()
		if got := trunc("hello", 5); got != "hello" {
			t.Errorf("trunc(%q, 5) = %q, want %q", "hello", got, "hello")
		}
	})

	t.Run("truncated", func(t *testing.T) {
		t.Parallel()
		if got := trunc("hello world", 5); got != "hello..." {
			t.Errorf("trunc(%q, 5) = %q, want %q", "hello world", got, "hello...")
		}
	})

	t.Run("unicode", func(t *testing.T) {
		t.Parallel()
		if got := trunc("héllo", 3); got != "hél..." {
			t.Errorf("trunc(%q, 3) = %q, want %q", "héllo", got, "hél...")
		}
	})
}

func assertContains(t *testing.T, s, substr string) {
	if !strings.Contains(s, substr) {
		t.Errorf("expected output to contain %q, but it didn't.\nGot:\n%s", substr, s)
	}
}

// Update fixtures with:
// go test ./backend/internal/agent -run '^TestRenderDiscussion$' -args -update-discussion-goldens
// Review the resulting testdata/render-discussion diff before committing.
func assertDiscussionGolden(t *testing.T, name, got string) {
	path := filepath.Join("testdata", "render-discussion", name)
	if *updateDiscussionGoldens {
		if err := os.WriteFile(filepath.Clean(path), []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Errorf("discussion golden %s mismatch (-want +got):\n%s", path, diff)
	}
}
