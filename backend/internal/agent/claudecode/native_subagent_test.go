// Tests native Claude lifecycle correlation and detached shell folding from recorded evidence.

package claudecode

import (
	"strings"
	"testing"

	"slices"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// TestNativeSubagentJokeEvidence replays the minimized Claude 2.1.270
// recording of the standardized delegation request ("Use a native subagent to
// tell a joke about README.md...") through the live wire parser and both task
// log versions. The recording carries the Agent tool use, task_started,
// task_updated, task_notification, and the parent tool result.
func TestNativeSubagentJokeEvidence(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New().NewWire()
	})

	// task_started proves the native agent runs, task_updated settles it, and the
	// task notification or parent tool result reports the same completed state
	// with the harness summary.
	want := []agent.NativeSubagentStatus{
		agent.NativeSubagentStatusRunning,
		agent.NativeSubagentStatusCompleted,
		agent.NativeSubagentStatusCompleted,
	}
	for _, test := range []struct {
		name         string
		observations []agent.NativeSubagent
	}{
		{"live", evidence.Live},
		{"v2 replay", evidence.V2},
		{"v3 replay", evidence.V3},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if len(test.observations) != len(want) {
				t.Fatalf("observations = %#v, want %d", test.observations, len(want))
			}
			for i, observation := range test.observations {
				if observation.Status != want[i] {
					t.Fatalf("observation %d status = %q, want %q", i, observation.Status, want[i])
				}
			}
			cards, active := agenttest.FoldNativeSubagents(test.observations)
			if len(cards) != 1 {
				t.Fatalf("cards = %#v, want one native agent", cards)
			}
			card := cards[0]
			if card.ID != "claude:task:test-agent" || card.ToolUseID != "test-spawn" {
				t.Fatalf("identity = %#v, want the native task identity with tool correlation", card)
			}
			if card.Scope != "" || card.Label != "Tell a joke about README.md" || !strings.Contains(card.Prompt, "joke about README.md") {
				t.Fatalf("metadata = %#v, want the delegation description and prompt", card)
			}
			if card.Status != agent.NativeSubagentStatusCompleted || !strings.Contains(card.Result, "documentation lead") {
				t.Fatalf("outcome = %#v, want the subagent's joke as the result", card)
			}
			if card.Background {
				t.Fatalf("card = %#v, want a foreground delegation", card)
			}
			if active != 0 {
				t.Fatalf("active = %d, want 0 after the recorded completion", active)
			}
		})
	}
}

// TestNativeSubagentRestartSettlesSameCard models a server restart or relay
// adoption between the recorded spawn and the recorded completion: the restored
// history shows the running card, and the live tail continues on a fresh wire
// warmed from that history. It must settle the same identity instead of adding a
// second card and leaving the first one running.
func TestNativeSubagentRestartSettlesSameCard(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat {
		return New().NewWire()
	})
	// History through task_started; the tail continues with the child stream
	// message, task_updated, task_notification, and the parent tool result.
	history, tail := evidence.Payloads[:2], evidence.Payloads[2:]
	restored, restoredActive := agenttest.FoldNativeSubagents(historyObservations(t, history))
	if len(restored) != 1 || restored[0].Status != agent.NativeSubagentStatusRunning || restoredActive != 1 {
		t.Fatalf("restored = %#v active = %d, want one running card", restored, restoredActive)
	}
	live := agenttest.RestartNativeSubagents(t, agent.LogVersionV3, history, tail, New().NewWire())

	observations := append(historyObservations(t, history), live...)
	cards, active := agenttest.FoldNativeSubagents(observations)
	if len(cards) != 1 || cardStatus(cards) != agent.NativeSubagentStatusCompleted || active != 0 {
		t.Fatalf("cards = %#v active = %d, want one completed card after restart", cards, active)
	}
	if cards[0].ID != restored[0].ID {
		t.Fatalf("card ID = %q, want the restored identity %q", cards[0].ID, restored[0].ID)
	}
}

// historyObservations returns the canonical observations of a record prefix.
func historyObservations(t *testing.T, payloads [][]byte) []agent.NativeSubagent {
	t.Helper()
	wire := New().NewWire()
	var out []agent.NativeSubagent
	for index, payload := range payloads {
		messages, err := wire.ParseMessage(payload)
		if err != nil {
			t.Fatalf("history record %d: %v", index, err)
		}
		for _, message := range messages {
			if native, ok := message.(*agent.NativeSubagentMessage); ok {
				out = append(out, native.Subagent)
			}
		}
	}
	return out
}

func cardStatus(cards []agent.NativeSubagent) agent.NativeSubagentStatus {
	if len(cards) == 0 {
		return ""
	}
	return cards[0].Status
}

// TestClaudeSubagentStatus pins the protocol status mapping, including the
// paused and killed states the wire reports.
func TestClaudeSubagentStatus(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		status string
		want   agent.NativeSubagentStatus
	}{
		{"running", agent.NativeSubagentStatusRunning},
		{"in_progress", agent.NativeSubagentStatusRunning},
		{"paused", agent.NativeSubagentStatusPaused},
		{"completed", agent.NativeSubagentStatusCompleted},
		{"failed", agent.NativeSubagentStatusFailed},
		{"killed", agent.NativeSubagentStatusInterrupted},
		{"stopped", agent.NativeSubagentStatusInterrupted},
		{"interrupted", agent.NativeSubagentStatusInterrupted},
		{"pending", agent.NativeSubagentStatusUnknown},
		{"", agent.NativeSubagentStatusUnknown},
		{"reticulating", agent.NativeSubagentStatusUnknown},
	} {
		if got := claudeSubagentStatus(test.status); got != test.want {
			t.Fatalf("claudeSubagentStatus(%q) = %q, want %q", test.status, got, test.want)
		}
	}
}

// TestNativeSubagentBackground pins that a run_in_background delegation is
// detached. The tool input marks it when retained, and the task record's
// is_backgrounded is authoritative when it is not.
func TestNativeSubagentBackground(t *testing.T) {
	t.Parallel()
	observations := func(t *testing.T, lines ...string) []agent.NativeSubagent {
		t.Helper()
		wire := New().NewWire()
		var out []agent.NativeSubagent
		for _, line := range lines {
			msgs, err := wire.ParseMessage([]byte(line))
			if err != nil {
				t.Fatalf("ParseMessage(%s): %v", line, err)
			}
			for _, message := range msgs {
				if native, ok := message.(*agent.NativeSubagentMessage); ok {
					out = append(out, native.Subagent)
				}
			}
		}
		return out
	}
	toolUse := func(id, background string) []string {
		return []string{
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"spawn","name":"Agent","input":{"description":"Joke","prompt":"Tell a joke","subagent_type":"general-purpose","run_in_background":` + background + `}}]}}`,
			`{"type":"system","subtype":"task_started","task_id":"` + id + `","tool_use_id":"spawn","subagent_type":"general-purpose","task_type":"local_agent"}`,
		}
	}
	if got := observations(t, toolUse("bg-input", "true")...); len(got) != 1 || !got[0].Background {
		t.Fatalf("tool input = %#v, want one detached card", got)
	}
	if got := observations(t, toolUse("fg", "false")...); len(got) != 1 || got[0].Background {
		t.Fatalf("foreground = %#v, want one attached card", got)
	}
	taskRecord := `{"type":"system","subtype":"task_started","task_id":"bg-record","tool_use_id":"spawn","subagent_type":"general-purpose","is_backgrounded":true,"task_type":"local_agent"}`
	if got := observations(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"spawn","name":"Agent","input":{"description":"Joke","prompt":"Tell a joke","subagent_type":"general-purpose"}}]}}`,
		taskRecord,
	); len(got) != 1 || !got[0].Background {
		t.Fatalf("task record = %#v, want one detached card", got)
	}
}

// TestNativeSubagentChildStreamCannotSettle guards the parent_tool_use_id rule:
// a child stream message is never the parent's Agent tool result, even when it
// carries a tool result for the same tool use ID.
func TestNativeSubagentChildStreamCannotSettle(t *testing.T) {
	t.Parallel()
	wire := New().NewWire()
	feed := func(line string) []agent.Message {
		msgs, err := wire.ParseMessage([]byte(line))
		if err != nil {
			t.Fatalf("ParseMessage(%s): %v", line, err)
		}
		return msgs
	}
	feed(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"test-spawn","name":"Agent","input":{"description":"Joke","prompt":"Tell a joke","subagent_type":"general-purpose"}}]}}`)
	feed(`{"type":"system","subtype":"task_started","task_id":"test-agent","tool_use_id":"test-spawn","task_type":"local_agent"}`)

	child := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"test-spawn","type":"tool_result","content":[{"type":"text","text":"child noise"}]}]},"parent_tool_use_id":"test-spawn"}`
	var native []*agent.NativeSubagentMessage
	for _, message := range feed(child) {
		if m, ok := message.(*agent.NativeSubagentMessage); ok {
			native = append(native, m)
		}
	}
	if len(native) != 0 {
		t.Fatalf("child stream produced %d lifecycle updates, want 0", len(native))
	}
}

// sameCard compares two folded cards by value: the exit code is a pointer in
// the canonical type, so replay equality must dereference it.
func sameCard(a, b *agent.BackgroundCommand) bool {
	if (a.ExitCode == nil) != (b.ExitCode == nil) {
		return false
	}
	if a.ExitCode != nil && *a.ExitCode != *b.ExitCode {
		return false
	}
	a.ExitCode, b.ExitCode = nil, nil
	return *a == *b
}

// Fixture lines extracted verbatim from the retained recording of task
// 44HVBS5RG000, a Claude Code session that ran three background lint commands.
const (
	backgroundSnapshotOne = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"b3xwrrd6m","task_type":"local_bash","description":"Install and run new lint in pitchpal"}],"uuid":"542c2404-17e6-4ba2-a379-c5e87ee6b83d","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
	backgroundSnapshotTwo = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bu2hdmg1p","task_type":"local_bash","description":"Re-run pitchpal lint with deps built"},{"task_id":"bhlpwfs1v","task_type":"local_bash","description":"Wait for pitchpal lint to finish"}],"uuid":"8b48f4b4-8015-4437-8289-ee717d975d1d","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`

	backgroundStartedLint  = `{"type":"system","subtype":"task_started","task_id":"b3xwrrd6m","tool_use_id":"toolu_019JYuc8XsZmLd6qJNCkXMB6","description":"Install and run new lint in pitchpal","is_backgrounded":true,"task_type":"local_bash","uuid":"1c3e373e-6fb3-4e1c-a313-074a197c748d","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
	backgroundUpdatedLint  = `{"type":"system","subtype":"task_updated","task_id":"b3xwrrd6m","patch":{"status":"completed","end_time":1789999973722},"uuid":"7c505bee-a57a-4806-ad59-f78027d0adc5","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
	backgroundNotifiedLint = `{"type":"system","subtype":"task_notification","task_id":"b3xwrrd6m","tool_use_id":"toolu_019JYuc8XsZmLd6qJNCkXMB6","status":"completed","output_file":"/tmp/claude-1000/-home-user-src-pitchpal-ca/6fe924d6-10b5-4c51-8d98-e50e07518c37/tasks/b3xwrrd6m.output","summary":"Background command \"Install and run new lint in pitchpal\" completed (exit code 0)","uuid":"73e900a7-7010-4c59-af96-1bf090ea9370","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`

	backgroundStartedRerun  = `{"type":"system","subtype":"task_started","task_id":"bu2hdmg1p","tool_use_id":"toolu_01R456k6iCx8EwpvB1eTSN6q","description":"Re-run pitchpal lint with deps built","is_backgrounded":true,"task_type":"local_bash","uuid":"66bfb079-eefb-41bf-bd3b-958cf53b921d","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
	backgroundUpdatedRerun  = `{"type":"system","subtype":"task_updated","task_id":"bu2hdmg1p","patch":{"status":"completed","end_time":1790000022891},"uuid":"b766f377-0f41-41c6-8fd4-5778132f55c5","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
	backgroundNotifiedRerun = `{"type":"system","subtype":"task_notification","task_id":"bu2hdmg1p","tool_use_id":"toolu_01R456k6iCx8EwpvB1eTSN6q","status":"completed","output_file":"/tmp/claude-1000/-home-user-src-pitchpal-ca/6fe924d6-10b5-4c51-8d98-e50e07518c37/tasks/bu2hdmg1p.output","summary":"Background command \"Re-run pitchpal lint with deps built\" completed (exit code 0)","uuid":"d1d69738-baaa-4f1e-941f-69b7ebae5db7","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`

	backgroundStartedWait  = `{"type":"system","subtype":"task_started","task_id":"bhlpwfs1v","tool_use_id":"toolu_01YSdyZHvJMqYsm8m576ethf","description":"Wait for pitchpal lint to finish","is_backgrounded":true,"task_type":"local_bash","uuid":"502c6eda-95b4-4aaa-8b28-d7281acc3d0b","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
	backgroundNotifiedWait = `{"type":"system","subtype":"task_notification","task_id":"bhlpwfs1v","tool_use_id":"toolu_01YSdyZHvJMqYsm8m576ethf","status":"completed","output_file":"/tmp/claude-1000/-home-user-src-pitchpal-ca/6fe924d6-10b5-4c51-8d98-e50e07518c37/tasks/bhlpwfs1v.output","summary":"Background command \"Wait for pitchpal lint to finish\" completed (exit code 0)","uuid":"48e26ffa-1c84-432e-b06b-fd6b08627a76","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`

	foregroundStartedCommit  = `{"type":"system","subtype":"task_started","task_id":"bkmzvu4dn","tool_use_id":"toolu_01EiiP9xQhNNYsUgFZb9gxqK","description":"Commit caic","is_backgrounded":false,"task_type":"local_bash","uuid":"fc77a164-7574-456d-9fc6-4f2b0c2c870c","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
	foregroundNotifiedCommit = `{"type":"system","subtype":"task_notification","task_id":"bkmzvu4dn","tool_use_id":"toolu_01EiiP9xQhNNYsUgFZb9gxqK","status":"completed","output_file":"","summary":"Commit caic","uuid":"2a75abe0-9b81-4f49-8be8-e886fbb21dfc","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`

	backgroundNotifiedNoExit = `{"type":"system","subtype":"task_notification","task_id":"b3xwrrd6m","tool_use_id":"toolu_019JYuc8XsZmLd6qJNCkXMB6","status":"completed","output_file":"/tmp/claude-1000/tasks/b3xwrrd6m.output","summary":"Background command \"Install and run new lint in pitchpal\" completed","uuid":"73e900a7-7010-4c59-af96-1bf090ea9370","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
)

// feedBackgroundLines runs lines through one live wire and returns every
// background-command message it emitted.
func feedBackgroundLines(t *testing.T, lines ...string) []agent.BackgroundCommand {
	t.Helper()
	wire := newWireFormat()
	var cards []agent.BackgroundCommand
	for _, line := range lines {
		msgs, err := wire.ParseMessage([]byte(line))
		if err != nil {
			t.Fatalf("line %s: %v", line, err)
		}
		for _, msg := range msgs {
			if m, ok := msg.(*agent.BackgroundCommandMessage); ok {
				cards = append(cards, m.Command)
			}
		}
	}
	return cards
}

func TestBackgroundCommandLifecycle(t *testing.T) {
	t.Parallel()
	t.Run("FullLifecycleFoldsOneCard", func(t *testing.T) {
		t.Parallel()
		cards := feedBackgroundLines(t,
			backgroundSnapshotOne, // Redundant snapshot before the start.
			backgroundStartedLint,
			backgroundUpdatedLint,
			backgroundNotifiedLint,
		)
		if len(cards) != 3 {
			t.Fatalf("emitted cards = %d, want 3 (start, update, notification)", len(cards))
		}
		final := cards[len(cards)-1]
		if final.ID != "claude:shell:b3xwrrd6m" {
			t.Errorf("ID = %q, want claude:shell:b3xwrrd6m", final.ID)
		}
		if final.Label != "Install and run new lint in pitchpal" {
			t.Errorf("Label = %q", final.Label)
		}
		if final.Status != agent.BackgroundCommandStatusCompleted {
			t.Errorf("Status = %q, want completed", final.Status)
		}
		if final.ExitCode == nil || *final.ExitCode != 0 {
			t.Errorf("ExitCode = %v, want 0", final.ExitCode)
		}
		if final.OutputRef == "" {
			t.Error("OutputRef = empty, want the harness output file")
		}
		if final.ToolUseID != "toolu_019JYuc8XsZmLd6qJNCkXMB6" {
			t.Errorf("ToolUseID = %q", final.ToolUseID)
		}
		if final.Result != `Background command "Install and run new lint in pitchpal" completed (exit code 0)` {
			t.Errorf("Result = %q", final.Result)
		}
	})

	t.Run("TwoConcurrentCommandsFoldIndependently", func(t *testing.T) {
		t.Parallel()
		cards := feedBackgroundLines(t,
			backgroundStartedRerun,
			backgroundSnapshotTwo, // Snapshot naming both commands proves nothing new.
			backgroundStartedWait,
			backgroundUpdatedRerun,
			backgroundNotifiedRerun,
		)
		if len(cards) != 4 {
			t.Fatalf("emitted cards = %d, want 4", len(cards))
		}
		// The rerun command settled; the wait command has no terminal record yet.
		ids := []string{cards[0].ID, cards[1].ID}
		if !slices.Contains(ids, "claude:shell:bu2hdmg1p") || !slices.Contains(ids, "claude:shell:bhlpwfs1v") {
			t.Fatalf("card IDs = %v, want both concurrent commands", ids)
		}
		terminal := cards[len(cards)-1]
		if terminal.ID != "claude:shell:bu2hdmg1p" {
			t.Fatalf("last card ID = %q, want the rerun command's notification", terminal.ID)
		}
		if terminal.Status != agent.BackgroundCommandStatusCompleted {
			t.Errorf("rerun Status = %q, want completed", terminal.Status)
		}
	})

	t.Run("SnapshotDropped", func(t *testing.T) {
		t.Parallel()
		for _, line := range []string{backgroundSnapshotOne, backgroundSnapshotTwo} {
			wire := newWireFormat()
			msgs, err := wire.ParseMessage([]byte(line))
			if err != nil {
				t.Fatalf("line %s: %v", line, err)
			}
			if len(msgs) != 0 {
				t.Fatalf("snapshot line emitted %d messages (%T), want none", len(msgs), msgs)
			}
		}
	})

	t.Run("UnknownTaskNotificationIgnored", func(t *testing.T) {
		t.Parallel()
		if cards := feedBackgroundLines(t, backgroundNotifiedLint); len(cards) != 0 {
			t.Fatalf("unproven task notification invented %d cards, want none", len(cards))
		}
	})

	t.Run("ForegroundTaskNeverBecomesCard", func(t *testing.T) {
		t.Parallel()
		if cards := feedBackgroundLines(t, foregroundStartedCommit, foregroundNotifiedCommit); len(cards) != 0 {
			t.Fatalf("foreground shell task produced %d cards, want none", len(cards))
		}
	})

	t.Run("NotificationWithoutExitCodeLeavesExitCodeNil", func(t *testing.T) {
		t.Parallel()
		cards := feedBackgroundLines(t, backgroundStartedLint, backgroundNotifiedNoExit)
		if len(cards) != 2 {
			t.Fatalf("emitted cards = %d, want 2", len(cards))
		}
		final := cards[len(cards)-1]
		if final.Status != agent.BackgroundCommandStatusCompleted {
			t.Errorf("Status = %q, want completed", final.Status)
		}
		if final.ExitCode != nil {
			t.Errorf("ExitCode = %v, want nil", final.ExitCode)
		}
	})

	t.Run("ReplayProducesSameCards", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			backgroundSnapshotOne,
			backgroundStartedLint,
			backgroundUpdatedLint,
			backgroundNotifiedLint,
			backgroundStartedRerun,
			backgroundUpdatedRerun,
		}
		first := feedBackgroundLines(t, lines...)
		second := feedBackgroundLines(t, lines...)
		if !slices.EqualFunc(first, second, func(a, b agent.BackgroundCommand) bool { return sameCard(&a, &b) }) {
			t.Fatalf("replay cards differ:\nfirst  = %#v\nsecond = %#v", first, second)
		}
	})
}
