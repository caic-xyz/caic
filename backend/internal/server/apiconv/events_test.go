// Tests for agent event to API event conversion.

package apiconv

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	logv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

type conversionPricer struct{}

func (conversionPricer) ModelPrice(_ agent.QuotaProvider, model string, _ time.Time) (usage.ModelPrice, bool) {
	if model != "gpt-6-sol" {
		return usage.ModelPrice{}, false
	}
	return usage.ModelPrice{InputPerMTok: 2}, true
}

func TestToolTimingTrackerConvertMessage(t *testing.T) {
	t.Run("tool timing separates owners and late correlation", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
		root := agent.MessageSource{ThreadID: "root", TurnID: "turn"}
		child := agent.MessageSource{ThreadID: "session", DelegationToolUseID: "delegate"}
		t0 := time.Unix(10, 0)
		tracker.ConvertMessage(&agent.ToolUseMessage{Source: root, ToolUseID: "same", Name: "Bash"}, t0)
		tracker.ConvertMessage(&agent.ToolUseMessage{Source: child, ToolUseID: "same", Name: "Read"}, t0.Add(time.Second))
		child.AgentID = "resolved-child"
		childEvents := tracker.ConvertMessage(&agent.ToolResultMessage{Source: child, ToolUseID: "same"}, t0.Add(3*time.Second))
		rootEvents := tracker.ConvertMessage(&agent.ToolResultMessage{Source: root, ToolUseID: "same"}, t0.Add(5*time.Second))
		if childEvents[0].ToolResult.Duration != 2 || rootEvents[0].ToolResult.Duration != 5 {
			t.Fatalf("child/root durations = %+v / %+v", childEvents[0].ToolResult, rootEvents[0].ToolResult)
		}
	})
	t.Parallel()

	t.Run("user input image carries its content hash", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
		payload := []byte("prompt image bytes")
		events := tracker.ConvertMessage(&agent.UserInputMessage{
			Text:   "look",
			Images: []logv3.ImageData{{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(payload)}},
		}, time.Unix(1, 0))
		if len(events) != 1 || events[0].UserInput == nil || len(events[0].UserInput.Images) != 1 {
			t.Fatalf("events = %+v, want one user input image", events)
		}
		sum := sha256.Sum256(payload)
		if got, want := events[0].UserInput.Images[0].Hash, hex.EncodeToString(sum[:]); got != want {
			t.Errorf("Hash = %q, want %q", got, want)
		}
		if got := events[0].UserInput.Images[0].Data; got != base64.StdEncoding.EncodeToString(payload) {
			t.Errorf("Data = %q, want the inline payload", got)
		}
	})

	t.Run("undecodable user input image has no hash", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
		events := tracker.ConvertMessage(&agent.UserInputMessage{
			Images: []logv3.ImageData{{MediaType: "image/png", Data: "not base64"}},
		}, time.Unix(1, 0))
		if len(events) != 1 || events[0].UserInput == nil || len(events[0].UserInput.Images) != 1 {
			t.Fatalf("events = %+v, want one user input image", events)
		}
		if got := events[0].UserInput.Images[0].Hash; got != "" {
			t.Errorf("Hash = %q, want empty", got)
		}
	})

	t.Run("common provenance", func(t *testing.T) {
		t.Parallel()
		source := agent.MessageSource{AgentID: "child", DelegationToolUseID: "delegation", ThreadID: "thread", TurnID: "turn", ItemID: "item"}
		for _, message := range []agent.Message{
			&agent.TextMessage{Text: "visible"}, &agent.TextDeltaMessage{Text: "fragment"},
			&agent.ThinkingMessage{Text: "reason"}, &agent.ThinkingDeltaMessage{Text: "reason fragment"},
			&agent.ToolUseMessage{ToolUseID: "tool", Name: "Bash"},
			&agent.ToolResultMessage{ToolUseID: "tool"},
			&agent.ToolOutputDeltaMessage{ToolUseID: "tool", Delta: "output"},
			&agent.ResultMessage{}, &agent.UsageMessage{}, &agent.SystemMessage{},
			&agent.UserInputMessage{Text: "input"}, &agent.LogMessage{Line: "log"},
			&agent.DiffStatMessage{}, &agent.TurnCommitSnapshotMessage{},
		} {
			*message.Provenance() = source
			tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
			events := tracker.ConvertMessage(message, time.Unix(1, 0))
			if len(events) == 0 {
				t.Fatalf("%T produced no event", message)
			}
			for _, event := range events {
				encoded, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Source agent.MessageSource `json:"source"`
				}
				if err := json.Unmarshal(encoded, &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Source != source {
					t.Errorf("%T common provenance = %s", message, encoded)
				}
			}
		}
	})

	t.Run("result fallback excludes child output", func(t *testing.T) {
		t.Parallel()
		parent := agent.MessageSource{ThreadID: "parent", RootThreadID: "parent", TurnID: "turn", ItemID: "answer"}
		child := agent.MessageSource{ThreadID: "child", RootThreadID: "parent", TurnID: "turn", ItemID: "answer"}
		for _, messages := range [][]agent.Message{
			{&agent.TextMessage{Text: "Parent answer", Source: parent}, &agent.ToolUseMessage{Source: child, ToolUseID: "child-tool"}, &agent.ToolResultMessage{Source: child, ToolUseID: "child-tool"}, &agent.ThinkingDeltaMessage{Text: "child reasoning", Source: child}, &agent.TextMessage{Text: "Child answer", Source: child}},
			{&agent.TextDeltaMessage{Text: "Parent ", Source: parent}, &agent.TextDeltaMessage{Text: "Child answer", Source: child}, &agent.TextDeltaMessage{Text: "answer", Source: parent}},
		} {
			tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
			for _, msg := range messages {
				tracker.ConvertMessage(msg, time.Time{})
			}
			result := tracker.ConvertMessage(&agent.ResultMessage{}, time.Time{})
			if len(result) != 1 || result[0].Result == nil || result[0].Result.Result != "Parent answer" {
				t.Errorf("parent result fallback = %+v", result)
			}
		}
	})

	t.Run("empty scoped completions remain observable", func(t *testing.T) {
		t.Parallel()
		source := agent.MessageSource{ThreadID: "parent", TurnID: "turn", ItemID: "item"}
		for _, msg := range []agent.Message{
			&agent.TextMessage{Source: source},
			&agent.ThinkingMessage{Source: source},
		} {
			tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
			if events := tracker.ConvertMessage(msg, time.Unix(1, 0)); len(events) != 1 {
				t.Errorf("%T completion = %+v", msg, events)
			}
		}
		for _, partial := range []agent.MessageSource{
			{},
			{RootThreadID: "root"},
			{ThreadID: "parent"},
			{TurnID: "turn"},
			{ItemID: "item"},
			{ThreadID: "parent", TurnID: "turn"},
			{ThreadID: "parent", ItemID: "item"},
			{TurnID: "turn", ItemID: "item"},
		} {
			for _, msg := range []agent.Message{&agent.TextMessage{Source: partial}, &agent.ThinkingMessage{Source: partial}} {
				tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
				if events := tracker.ConvertMessage(msg, time.Unix(1, 0)); len(events) != 0 {
					t.Errorf("unscoped %T completion = %+v", msg, events)
				}
			}
			for _, msg := range []agent.Message{&agent.TextMessage{Text: "Visible", Source: partial}, &agent.ThinkingMessage{Text: "Visible", Source: partial}} {
				tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
				if events := tracker.ConvertMessage(msg, time.Unix(1, 0)); len(events) != 1 {
					t.Errorf("nonempty %T with partial source = %+v", msg, events)
				}
			}
		}
	})

	t.Run("cost projection preserves raw reports", func(t *testing.T) {
		t.Parallel()
		at := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
		codex := NewToolTimingTracker(harness.Codex, "gpt-6-sol", conversionPricer{}, nil)
		raw := &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 1_000_000}}
		first := codex.ConvertMessage(raw, at)[0].Result
		if first == nil || first.Cost.USD != 2 || first.Cost.Source != "estimated" || raw.TotalCostUSD != 0 {
			t.Fatalf("Codex result = %+v; raw cost = %v", first, raw.TotalCostUSD)
		}
		codex.ConvertMessage(&agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, at)
		second := codex.ConvertMessage(&agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 1_000_000}}, at)[0].Result
		if second == nil || second.Cost.USD != 2 {
			t.Fatalf("Codex result after compaction = %+v", second)
		}

		claude := NewToolTimingTracker(harness.Claude, "", nil, nil)
		claude.ConvertMessage(&agent.ResultMessage{MessageType: "result", TotalCostUSD: 2}, at)
		cumulative := claude.ConvertMessage(&agent.ResultMessage{MessageType: "result", TotalCostUSD: 3}, at)[0].Result
		if cumulative == nil || cumulative.Cost.USD != 1 || cumulative.Cost.Source != "reported" || cumulative.TotalCostUSD != 3 {
			t.Fatalf("Claude cumulative result = %+v", cumulative)
		}
	})

	t.Run("tool use includes replacement file changes display", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Pi, "", nil, nil)
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

	t.Run("native subagent preserves only harness-reported lifecycle facts", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.OpenCode, "", nil, nil)
		events := tracker.ConvertMessage(&agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{
			ID:      "child-1",
			GroupID: "spawn-1",
			Label:   "reviewer",
			Prompt:  "Review the change",
			Status:  agent.NativeSubagentStatusCompleted,
			Result:  "No findings.",
		}}, time.Unix(1, 0))
		if len(events) != 1 || events[0].Kind != v1.EventKindNativeSubagent || events[0].NativeSubagent == nil {
			t.Fatalf("events = %#v", events)
		}
		got := events[0].NativeSubagent
		if got.ID != "child-1" || got.GroupID != "spawn-1" || got.Label != "reviewer" || got.Prompt != "Review the change" || got.Status != "completed" || got.Result != "No findings." {
			t.Fatalf("native subagent = %#v", got)
		}
	})

	t.Run("tool use includes harness normalized subagent display", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Pi, "", nil, nil)
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
		tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
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
		tracker := NewToolTimingTracker(harness.Pi, "", nil, nil)
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
		tracker := NewToolTimingTracker(harness.Pi, "", nil, nil)
		events := tracker.ConvertMessage(&agent.ExitMessage{ExitCode: 0}, time.Unix(1, 0))
		if len(events) != 0 {
			t.Fatalf("got %d events, want 0", len(events))
		}
	})

	t.Run("tool result prefers harness duration", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
		start := time.Unix(1, 0)
		tracker.ConvertMessage(&agent.ToolUseMessage{ToolUseID: "tool", Name: "Bash"}, start)
		events := tracker.ConvertMessage(&agent.ToolResultMessage{ToolUseID: "tool", DurationMs: 125}, start.Add(time.Second))
		if len(events) != 1 || events[0].ToolResult == nil || events[0].ToolResult.Duration != 0.125 {
			t.Fatalf("tool result = %#v, want 125ms duration", events)
		}
	})

	t.Run("missing producer time leaves tool duration unknown", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Claude, "", nil, nil)
		tracker.ConvertMessage(&agent.ToolUseMessage{ToolUseID: "tool", Name: "Read"}, time.Time{})
		events := tracker.ConvertMessage(&agent.ToolResultMessage{ToolUseID: "tool"}, time.Time{})
		if len(events) != 1 || events[0].Ts != 0 || events[0].ToolResult == nil || events[0].ToolResult.Duration != 0 {
			t.Fatalf("tool result = %#v, want unknown timestamps and duration", events)
		}
	})

	t.Run("pending tool timings are bounded", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Claude, "", nil, nil)
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
		tracker := NewToolTimingTracker(harness.Claude, "", nil, nil)
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

	t.Run("commit snapshot is a standalone event", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Claude, "", nil, nil)
		events := tracker.ConvertMessage(agent.NewTurnCommitSnapshotMessage([]logv3.RepositoryCommit{{
			RepositoryPath: "/home/user/src/repo",
			BranchName:     "caic-1",
			CommitHash:     "1111111111111111111111111111111111111111",
		}}, true, &logv3.ChangeStat{Files: 2, LinesAdded: 6, LinesDeleted: 1, BinaryFiles: 1}), time.Unix(1, 0))
		want := &v1.EventCommitSnapshot{
			Baseline:   true,
			ChangeStat: &v1.EventChangeStat{Files: 2, LinesAdded: 6, LinesDeleted: 1, BinaryFiles: 1},
			RepositoryCommits: []v1.EventRepositoryCommit{{
				RepositoryPath: "/home/user/src/repo",
				BranchName:     "caic-1",
				CommitHash:     "1111111111111111111111111111111111111111",
			}},
		}
		if len(events) != 1 || !reflect.DeepEqual(events[0].CommitSnapshot, want) {
			t.Fatalf("commit snapshot = %+v, want %+v", events, want)
		}
	})

	t.Run("reported skill load is an event and a read is not", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Claude, "", nil, nil)
		if events := tracker.ConvertMessage(&agent.SkillReadMessage{ToolUseID: "sk", Skill: "review"}, time.Unix(1, 0)); len(events) != 0 {
			t.Fatalf("read events = %+v, want none", events)
		}
		events := tracker.ConvertMessage(&agent.SkillLoadMessage{Skill: "review", Path: "/s/review/SKILL.md"}, time.Unix(1, 0))
		want := &v1.EventSkillLoad{Skill: "review", Path: "/s/review/SKILL.md"}
		if len(events) != 1 || events[0].Kind != v1.EventKindSkillLoad || !reflect.DeepEqual(events[0].SkillLoad, want) {
			t.Fatalf("events = %+v, want %+v", events, want)
		}
	})

	t.Run("inferred skill load follows a successful tool result", func(t *testing.T) {
		t.Parallel()
		tracker := NewToolTimingTracker(harness.Codex, "", nil, nil)
		at := time.Unix(1, 0)
		for _, id := range []string{"ok", "failed"} {
			for _, m := range agent.InferredSkillReadFromPath("/home/user/.agents/skills/"+id+"/SKILL.md", id) {
				if events := tracker.ConvertMessage(m, at); len(events) != 0 {
					t.Fatalf("unconfirmed %T events = %+v, want none", m, events)
				}
			}
		}
		events := tracker.ConvertMessage(&agent.ToolResultMessage{ToolUseID: "failed", Error: "exit 1"}, at)
		if len(events) != 1 || events[0].Kind != v1.EventKindToolResult {
			t.Fatalf("failed result events = %+v, want only the tool result", events)
		}
		events = tracker.ConvertMessage(&agent.ToolResultMessage{ToolUseID: "ok"}, at)
		want := &v1.EventSkillLoad{Skill: "ok", Path: "/home/user/.agents/skills/ok/SKILL.md"}
		if len(events) != 2 || events[1].Kind != v1.EventKindSkillLoad || !reflect.DeepEqual(events[1].SkillLoad, want) {
			t.Fatalf("successful result events = %+v, want tool result then %+v", events, want)
		}
	})
}

func TestConvertMessagePlanContentProjection(t *testing.T) {
	t.Parallel()

	t.Run("plan content follows latest ExitPlanMode", func(t *testing.T) {
		t.Parallel()
		tk, err := task.NewTask(ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		planWrite := &agent.ToolUseMessage{
			ToolUseID: "tu-1", Name: "Write",
			Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan A"}`),
		}
		exit := &agent.ToolUseMessage{ToolUseID: "tu-2", Name: "ExitPlanMode"}
		tk.SeedTimeline([]agent.Message{planWrite, exit})

		tt := NewToolTimingTracker(harness.Claude, "", nil, tk.PlanContentFor)
		if events := tt.ConvertMessage(planWrite, time.Unix(1, 0)); len(events) != 1 || events[0].ToolUse == nil {
			t.Fatalf("write events = %#v, want 1 tool_use event", events)
		} else if events[0].ToolUse.PlanContent != "" {
			t.Errorf("write PlanContent = %q, want empty (superseded)", events[0].ToolUse.PlanContent)
		}
		events := tt.ConvertMessage(exit, time.Unix(2, 0))
		if len(events) != 1 || events[0].ToolUse == nil {
			t.Fatalf("exit events = %#v, want 1 tool_use event", events)
		}
		if events[0].ToolUse.PlanContent != "plan A" {
			t.Errorf("exit PlanContent = %q, want %q", events[0].ToolUse.PlanContent, "plan A")
		}
	})

	t.Run("nil resolver yields empty plan content", func(t *testing.T) {
		t.Parallel()
		tt := NewToolTimingTracker(harness.Claude, "", nil, nil)
		events := tt.ConvertMessage(&agent.ToolUseMessage{ToolUseID: "tu-1", Name: "ExitPlanMode"}, time.Unix(1, 0))
		if len(events) != 1 || events[0].ToolUse == nil {
			t.Fatalf("events = %#v, want 1 tool_use event", events)
		}
		if events[0].ToolUse.PlanContent != "" {
			t.Errorf("PlanContent = %q, want empty", events[0].ToolUse.PlanContent)
		}
	})
}
