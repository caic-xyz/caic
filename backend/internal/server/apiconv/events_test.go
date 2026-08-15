// Tests for agent event to API event conversion.

package apiconv

import (
	"fmt"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestToolTimingTrackerConvertMessage(t *testing.T) {
	t.Parallel()

	t.Run("tool use includes replacement file changes display", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Pi, nil)
		events := tracker.ConvertMessage(&agent.ToolUseMessage{
			ToolUseID: "edit-1",
			Name:      "Edit",
			Detail:    "SERVER_LIBRARY.md",
			Input:     []byte(`{"path":"gomode/docs/SERVER_LIBRARY.md","edits":[{"oldText":"before","newText":"after"}]}`),
			InputView: agent.FileChangesInputViewFromReplacements(
				"gomode/docs/SERVER_LIBRARY.md",
				[]agent.TextReplacement{{OldText: "before", NewText: "after"}},
			),
		}, time.Unix(1, 0))
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		use := events[0].ToolUse
		if use == nil {
			t.Fatalf("tool use is nil")
		}
		if use.Detail != "SERVER_LIBRARY.md" {
			t.Fatalf("detail = %q, want file basename", use.Detail)
		}
		if use.InputView.Kind != v1.EventToolInputFileChanges {
			t.Fatalf("input view = %#v, want file changes view", use.InputView)
		}
		if len(use.InputView.Files) != 1 {
			t.Fatalf("files = %#v, want one file", use.InputView.Files)
		}
		file := use.InputView.Files[0]
		if file.Path != "gomode/docs/SERVER_LIBRARY.md" || file.Patch == "" {
			t.Fatalf("file change = %#v", file)
		}
	})

	t.Run("tool use includes harness normalized subagent display", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Pi, nil)
		events := tracker.ConvertMessage(&agent.ToolUseMessage{
			ToolUseID: "agent-1",
			Name:      "Agent",
			Detail:    "chain · reviewer ×2, worker",
			InputView: agent.ToolInputView{
				Kind: agent.ToolInputSubagents,
				Subagents: []agent.SubagentSpawn{
					{Agent: "reviewer", Task: "a"},
					{Agent: "reviewer", Task: "b"},
					{Agent: "worker", Task: "c"},
				},
			},
		}, time.Unix(1, 0))
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		use := events[0].ToolUse
		if use == nil {
			t.Fatalf("tool use is nil")
		}
		if use.Detail != "chain · reviewer ×2, worker" {
			t.Fatalf("detail = %q", use.Detail)
		}
		if use.InputView.Kind != v1.EventToolInputSubagents || len(use.InputView.Subagents) != 3 {
			t.Fatalf("input view = %#v, want subagent view", use.InputView)
		}
	})

	t.Run("tool use includes patch file changes display", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Codex, nil)
		events := tracker.ConvertMessage(&agent.ToolUseMessage{
			ToolUseID: "edit-1",
			Name:      "Edit",
			Detail:    "main.go",
			InputView: agent.FileChangesInputView([]agent.FileChange{{
				Path:  "/workspace/main.go",
				Patch: "@@ -1 +1 @@\n-old\n+new\n",
			}}),
		}, time.Unix(1, 0))
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		use := events[0].ToolUse
		if use == nil {
			t.Fatalf("tool use is nil")
		}
		if use.InputView.Kind != v1.EventToolInputFileChanges {
			t.Fatalf("input view = %#v, want file changes view", use.InputView)
		}
		if len(use.InputView.Files) != 1 {
			t.Fatalf("files = %#v, want one file", use.InputView.Files)
		}
		if use.InputView.Files[0].Path != "/workspace/main.go" || use.InputView.Files[0].Patch == "" {
			t.Fatalf("file change = %#v", use.InputView.Files[0])
		}
	})

	t.Run("nonzero exit becomes error event", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Pi, nil)
		events := tracker.ConvertMessage(&agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}, time.Unix(1, 0))
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if events[0].Kind != v1.EventKindError {
			t.Fatalf("kind = %q, want %q", events[0].Kind, v1.EventKindError)
		}
		if events[0].Error == nil || events[0].Error.Err != "Unknown option: --approve" {
			t.Fatalf("error event = %+v, want relay stderr", events[0].Error)
		}
	})

	t.Run("zero exit is hidden", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Pi, nil)
		events := tracker.ConvertMessage(&agent.ExitMessage{ExitCode: 0}, time.Unix(1, 0))
		if len(events) != 0 {
			t.Fatalf("got %d events, want 0", len(events))
		}
	})

	t.Run("pending tool timings are bounded", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Claude, nil)
		start := time.Unix(1, 0)
		for i := range maxPendingToolTimings {
			tracker.ConvertMessage(&agent.ToolUseMessage{ToolUseID: fmt.Sprintf("tool-%d", i), Name: "Tool"}, start)
		}
		if len(tracker.pending) != maxPendingToolTimings {
			t.Fatalf("pending len = %d, want %d", len(tracker.pending), maxPendingToolTimings)
		}
		tracker.ConvertMessage(&agent.ToolUseMessage{ToolUseID: "overflow", Name: "Tool"}, start)
		if len(tracker.pending) != 1 {
			t.Fatalf("pending len after reset = %d, want 1", len(tracker.pending))
		}
		events := tracker.ConvertMessage(&agent.ToolResultMessage{ToolUseID: "tool-0"}, start.Add(time.Second))
		if len(events) != 1 || events[0].ToolResult == nil || events[0].ToolResult.Duration != 0 {
			t.Fatalf("evicted tool result = %#v, want zero duration", events)
		}
	})

	t.Run("rate limit uses API status", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Claude, nil)
		events := tracker.ConvertMessage(&agent.RateLimitMessage{
			Status: agent.RateLimitStatusAllowedWarning,
		}, time.Unix(1, 0))
		if len(events) != 1 {
			t.Fatalf("got %d events, want 1", len(events))
		}
		if events[0].RateLimit == nil || events[0].RateLimit.Status != v1.EventRateLimitStatusAllowedWarning {
			t.Fatalf("rate limit status = %#v, want %q", events[0].RateLimit, v1.EventRateLimitStatusAllowedWarning)
		}
	})
}
