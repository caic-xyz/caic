// Tests for agent event to API event conversion.

package v1conv

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestToolTimingTrackerConvertMessage(t *testing.T) {
	t.Parallel()

	t.Run("tool use includes normalized edit display", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Pi, nil)
		events := tracker.ConvertMessage(&agent.ToolUseMessage{
			ToolUseID: "edit-1",
			Name:      "Edit",
			Detail:    "SERVER_LIBRARY.md",
			Input:     []byte(`{"path":"gomode/docs/SERVER_LIBRARY.md","edits":[{"oldText":"before","newText":"after"}]}`),
			InputView: agent.ToolInputView{
				Kind: agent.ToolInputEdit,
				Edit: agent.EditToolInput{
					Path: "gomode/docs/SERVER_LIBRARY.md",
					Edits: []agent.EditReplacement{
						{OldText: "before", NewText: "after"},
					},
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
		if use.Detail != "SERVER_LIBRARY.md" {
			t.Fatalf("detail = %q, want file basename", use.Detail)
		}
		if use.InputView.Kind != v1.EventToolInputEdit {
			t.Fatalf("input view = %#v, want edit view", use.InputView)
		}
		if use.InputView.Edit.Path != "gomode/docs/SERVER_LIBRARY.md" || len(use.InputView.Edit.Edits) != 1 {
			t.Fatalf("edit view = %#v", use.InputView.Edit)
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
}
