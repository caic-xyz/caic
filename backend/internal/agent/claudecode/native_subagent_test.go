// Tests native Claude lifecycle correlation, detached shell folding, queued notification turns, and snapshot retirement from recorded evidence.

package claudecode

import (
	"slices"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

func TestNativeSubagentResumedRun(t *testing.T) {
	t.Parallel()
	// Claude 2.1.319 task 4V6FPSTTU000 completed a child while its shell
	// checks ran, then woke the same child with a newer run_id.
	observations := wireSubagents(t,
		`{"type":"system","subtype":"task_started","task_type":"local_agent","task_id":"child","run_id":"001","is_backgrounded":true}`,
		`{"type":"system","subtype":"task_notification","task_id":"child","run_id":"001","status":"completed","summary":"Waiting for checks"}`,
		`{"type":"system","subtype":"task_started","task_type":"local_agent","task_id":"child","run_id":"002","is_backgrounded":true}`,
		`{"type":"system","subtype":"task_notification","task_id":"child","run_id":"001","status":"completed","summary":"Stale completion"}`,
	)
	cards, active := agenttest.FoldNativeSubagents(observations)
	if len(cards) != 1 || cards[0].Status != agent.NativeSubagentStatusRunning || cards[0].Result != "" || active != 1 {
		t.Fatalf("cards = %#v, active = %d, want one reopened child without its previous result", cards, active)
	}
}

func TestNativeSubagentSendMessageResume(t *testing.T) {
	t.Parallel()
	// Claude task 4VN78Q1IU000 resumed a completed background agent with
	// SendMessage. The new run's task_started names the SendMessage tool use, and
	// its tool result only acknowledges the resumption while the run continues.
	got := wireSubagents(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"spawn","name":"Agent","input":{"description":"Implement","prompt":"Implement","subagent_type":"general-purpose","run_in_background":true}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"child","run_id":"001","tool_use_id":"spawn","is_backgrounded":true,"task_type":"local_agent"}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"spawn","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"tool_use_result":{"status":"async_launched"}}`,
		`{"type":"system","subtype":"task_notification","task_id":"child","run_id":"001","tool_use_id":"spawn","status":"completed","summary":"Done"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"resume","name":"SendMessage","input":{"to":"child","message":"Redesign"}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"child","run_id":"002","tool_use_id":"resume","is_backgrounded":true,"task_type":"local_agent","prompt":"Redesign"}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"resume","type":"tool_result","content":[{"type":"text","text":"{\"success\":true,\"message\":\"Resuming agent child\"}"}]}]},"tool_use_result":{"success":true,"resumedAgentId":"child"}}`,
	)
	cards, active := agenttest.FoldNativeSubagents(got)
	if len(cards) != 1 || cards[0].Status != agent.NativeSubagentStatusRunning || !cards[0].Background || active != 1 {
		t.Fatalf("cards = %#v, active = %d, want one running background child", cards, active)
	}
	if cards[0].Prompt != "Redesign" || cards[0].Label != "Implement" {
		t.Fatalf("card = %#v, want the resumed run's prompt under the original label", cards[0])
	}
}

// TestNativeSubagentJokeEvidence replays the minimized Claude 2.1.270
// recording of the standardized delegation request ("Use a native subagent to
// tell a joke about README.md...") through the live wire parser and both task
// log versions. The recording carries the Agent tool use, task_started,
// task_updated, task_notification, and the parent tool result.
func TestNativeSubagentJokeEvidence(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", func() agent.WireFormat {
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
		{"v3 replay", evidence.Replay},
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
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", func() agent.WireFormat {
		return New().NewWire()
	})
	// History through task_started; the tail continues with the child stream
	// message, task_updated, task_notification, and the parent tool result.
	history, tail := evidence.Payloads[:2], evidence.Payloads[2:]
	restored, restoredActive := agenttest.FoldNativeSubagents(wireSubagents(t, history...))
	if len(restored) != 1 || restored[0].Status != agent.NativeSubagentStatusRunning || restoredActive != 1 {
		t.Fatalf("restored = %#v active = %d, want one running card", restored, restoredActive)
	}
	live := agenttest.RestartNativeSubagents(t, history, tail, New().NewWire())

	observations := append(wireSubagents(t, history...), live...)
	cards, active := agenttest.FoldNativeSubagents(observations)
	if len(cards) != 1 || cardStatus(cards) != agent.NativeSubagentStatusCompleted || active != 0 {
		t.Fatalf("cards = %#v active = %d, want one completed card after restart", cards, active)
	}
	if cards[0].ID != restored[0].ID {
		t.Fatalf("card ID = %q, want the restored identity %q", cards[0].ID, restored[0].ID)
	}
}

// historyObservations returns the canonical observations of a record prefix.
// wireSubagents parses records through one fresh live wire and returns every
// native-subagent observation it emits, in order.
func wireSubagents[T string | []byte](t *testing.T, records ...T) []agent.NativeSubagent {
	wire := New().NewWire()
	var out []agent.NativeSubagent
	for index, record := range records {
		messages, err := wire.ParseOutput([]byte(record))
		if err != nil {
			t.Fatalf("record %d: %v", index, err)
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
	toolUse := func(id, background string) []string {
		return []string{
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"spawn","name":"Agent","input":{"description":"Joke","prompt":"Tell a joke","subagent_type":"general-purpose","run_in_background":` + background + `}}]}}`,
			`{"type":"system","subtype":"task_started","task_id":"` + id + `","tool_use_id":"spawn","subagent_type":"general-purpose","task_type":"local_agent"}`,
		}
	}
	if got := wireSubagents(t, toolUse("bg-input", "true")...); len(got) != 1 || !got[0].Background {
		t.Fatalf("tool input = %#v, want one detached card", got)
	}
	if got := wireSubagents(t, toolUse("fg", "false")...); len(got) != 1 || got[0].Background {
		t.Fatalf("foreground = %#v, want one attached card", got)
	}
	taskRecord := `{"type":"system","subtype":"task_started","task_id":"bg-record","tool_use_id":"spawn","subagent_type":"general-purpose","is_backgrounded":true,"task_type":"local_agent"}`
	if got := wireSubagents(t,
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
	got := wireSubagents(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"test-spawn","name":"Agent","input":{"description":"Joke","prompt":"Tell a joke","subagent_type":"general-purpose"}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"test-agent","tool_use_id":"test-spawn","task_type":"local_agent"}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"test-spawn","type":"tool_result","content":[{"type":"text","text":"child noise"}]}]},"parent_tool_use_id":"test-spawn"}`,
	)
	if len(got) != 1 || got[0].Status != agent.NativeSubagentStatusRunning {
		t.Fatalf("observations = %#v, want only the running task_started card", got)
	}
}

func TestNativeSubagentStringContentUserMessage(t *testing.T) {
	t.Parallel()
	// Claude task 4VN78Q1IU000 resumed after compaction with a user record whose
	// content is a string, not a block list.
	got := wireSubagents(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"spawn","name":"Agent","input":{"description":"Joke","prompt":"Tell a joke","subagent_type":"general-purpose"}}]}}`,
		`{"type":"system","subtype":"task_started","task_id":"child","tool_use_id":"spawn","task_type":"local_agent"}`,
		`{"type":"user","message":{"role":"user","content":"This session is being continued from a previous conversation."}}`,
	)
	if len(got) != 1 || got[0].Status != agent.NativeSubagentStatusRunning {
		t.Fatalf("observations = %#v, want only the running task_started card", got)
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

	// Minimized from task 50R144VRE000: a Monitor watch that expired after its
	// 30-minute timeout. The task records match a backgrounded Bash command.
	monitorToolUse  = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01Tfpv2sbytgxvgA5J7UKtJW","name":"Monitor","input":{"command":"tail -f out | grep --line-buffered exit=","description":"make fix/verify/test results","timeout_ms":1800000}}]},"session_id":"f48cd6f1-296a-4c99-a453-a18a42c76e99"}`
	monitorStarted  = `{"type":"system","subtype":"task_started","task_id":"bizvx8b6o","run_id":"0mwjohbq7-83f2cd99","tool_use_id":"toolu_01Tfpv2sbytgxvgA5J7UKtJW","description":"make fix/verify/test results","is_backgrounded":true,"task_type":"local_bash","session_id":"f48cd6f1-296a-4c99-a453-a18a42c76e99"}`
	monitorKilled   = `{"type":"system","subtype":"task_updated","task_id":"bizvx8b6o","run_id":"0mwjohbq7-83f2cd99","patch":{"status":"killed","end_time":1794860421350},"session_id":"f48cd6f1-296a-4c99-a453-a18a42c76e99"}`
	monitorNotified = `{"type":"system","subtype":"task_notification","task_id":"bizvx8b6o","run_id":"0mwjohbq7-83f2cd99","tool_use_id":"toolu_01Tfpv2sbytgxvgA5J7UKtJW","status":"stopped","output_file":"/tmp/claude-1000/tasks/bizvx8b6o.output","summary":"make fix/verify/test results","session_id":"f48cd6f1-296a-4c99-a453-a18a42c76e99"}`

	backgroundNotifiedNoExit = `{"type":"system","subtype":"task_notification","task_id":"b3xwrrd6m","tool_use_id":"toolu_019JYuc8XsZmLd6qJNCkXMB6","status":"completed","output_file":"/tmp/claude-1000/tasks/b3xwrrd6m.output","summary":"Background command \"Install and run new lint in pitchpal\" completed","uuid":"73e900a7-7010-4c59-af96-1bf090ea9370","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
)

// feedBackgroundLines runs lines through one live wire and returns every
// background-command message it emitted.
func feedBackgroundLines(t *testing.T, lines ...string) []agent.BackgroundCommand {
	wire := newWireFormat()
	var cards []agent.BackgroundCommand
	for _, line := range lines {
		msgs, err := wire.ParseOutput([]byte(line))
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

	t.Run("SnapshotCreatesNoCard", func(t *testing.T) {
		t.Parallel()
		for _, line := range []string{backgroundSnapshotOne, backgroundSnapshotTwo} {
			wire := newWireFormat()
			msgs, err := wire.ParseOutput([]byte(line))
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

	t.Run("MonitorBecomesWatch", func(t *testing.T) {
		t.Parallel()
		cards := feedBackgroundLines(t, monitorToolUse, monitorStarted, monitorKilled, monitorNotified)
		if len(cards) != 3 {
			t.Fatalf("emitted cards = %d, want 3 (start, update, notification)", len(cards))
		}
		if !cards[0].Watch || cards[0].Status != agent.BackgroundCommandStatusRunning {
			t.Fatalf("started card = %#v, want a running watch", cards[0])
		}
		if final := cards[2]; !final.Watch || final.Status != agent.BackgroundCommandStatusInterrupted {
			t.Fatalf("final card = %#v, want an interrupted watch", final)
		}
		if cards := feedBackgroundLines(t, backgroundStartedLint); len(cards) != 1 || cards[0].Watch {
			t.Fatalf("Bash command cards = %#v, want one non-watch card", cards)
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

func TestTurnQueue(t *testing.T) {
	t.Parallel()
	// queued returns, per line, the card kinds in emission order, with "turn"
	// for a TurnQueuedMessage.
	queued := func(t *testing.T, lines ...string) [][]string {
		wire := newWireFormat()
		out := make([][]string, len(lines))
		for i, line := range lines {
			msgs, err := wire.ParseOutput([]byte(line))
			if err != nil {
				t.Fatalf("line %d: %v", i, err)
			}
			for _, msg := range msgs {
				switch m := msg.(type) {
				case *agent.TurnQueuedMessage:
					if m.Source.IsChild() {
						t.Fatalf("line %d: queued turn has child provenance %#v", i, m.Source)
					}
					out[i] = append(out[i], "turn")
				case *agent.NativeSubagentMessage:
					out[i] = append(out[i], "agent:"+string(m.Subagent.Status))
				case *agent.BackgroundCommandMessage:
					out[i] = append(out[i], "command:"+string(m.Command.Status))
				}
			}
		}
		return out
	}
	want := func(t *testing.T, got [][]string, line int, kinds ...string) {
		for i, g := range got {
			if i == line {
				if !slices.Equal(g, kinds) {
					t.Errorf("line %d = %q, want %q", i, g, kinds)
				}
			} else if slices.Contains(g, "turn") {
				t.Errorf("line %d = %q, want no queued turn", i, g)
			}
		}
	}
	// Minimized from task 4VN78Q1IU000: a background agent ends after the
	// parent result; Claude Code then starts a turn to deliver the notification.
	const (
		agentUse      = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_014N3CgJTcnLGnbSbium2crh","name":"Agent","input":{"description":"Commit fixture fix","prompt":"Fix","subagent_type":"general-purpose","run_in_background":true}}]},"session_id":"fe242f82"}`
		agentStarted  = `{"type":"system","subtype":"task_started","task_id":"a3635a9373e98f76a","run_id":"0mwjtvyk3-60e1451f","tool_use_id":"toolu_014N3CgJTcnLGnbSbium2crh","description":"Commit fixture fix","is_backgrounded":true,"task_type":"local_agent","session_id":"fe242f82"}`
		agentUpdated  = `{"type":"system","subtype":"task_updated","task_id":"a3635a9373e98f76a","run_id":"0mwjtvyk3-60e1451f","patch":{"status":"completed","end_time":1794867732246},"session_id":"fe242f82"}`
		agentNotified = `{"type":"system","subtype":"task_notification","task_id":"a3635a9373e98f76a","run_id":"0mwjtvyk3-60e1451f","tool_use_id":"toolu_014N3CgJTcnLGnbSbium2crh","status":"completed","summary":"Fixed","session_id":"fe242f82"}`
	)
	t.Run("BackgroundAgent", func(t *testing.T) {
		t.Parallel()
		got := queued(t, agentUse, agentStarted, agentUpdated, agentNotified)
		want(t, got, 2, "turn", "agent:completed")
	})
	t.Run("ResumedAgentQueuesAgain", func(t *testing.T) {
		t.Parallel()
		resumed := strings.ReplaceAll(agentStarted, "0mwjtvyk3-60e1451f", "0mwjtvyk4-00000000")
		got := queued(t, agentUse, agentStarted, agentNotified, resumed, strings.ReplaceAll(agentUpdated, "0mwjtvyk3-60e1451f", "0mwjtvyk4-00000000"))
		if !slices.Equal(got[2], []string{"turn", "agent:completed"}) || !slices.Equal(got[4], []string{"turn", "agent:completed"}) {
			t.Fatalf("got %q, want a queued turn at the end of each run", got)
		}
	})
	t.Run("BackgroundCommandNotificationOnly", func(t *testing.T) {
		t.Parallel()
		got := queued(t, backgroundStartedRerun, backgroundNotifiedRerun)
		want(t, got, 1, "turn", "command:completed")
	})
	t.Run("BackgroundCommandUpdateThenNotification", func(t *testing.T) {
		t.Parallel()
		got := queued(t, backgroundStartedLint, backgroundUpdatedLint, backgroundNotifiedLint)
		want(t, got, 1, "turn", "command:completed")
	})
	t.Run("WatchExpiry", func(t *testing.T) {
		t.Parallel()
		got := queued(t, monitorToolUse, monitorStarted, monitorKilled, monitorNotified)
		want(t, got, 2, "turn", "command:interrupted")
	})
	t.Run("ForegroundTasks", func(t *testing.T) {
		t.Parallel()
		got := queued(t,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"spawn","name":"Agent","input":{"description":"Joke","prompt":"Tell a joke","subagent_type":"general-purpose"}}]}}`,
			`{"type":"system","subtype":"task_started","task_id":"fg","tool_use_id":"spawn","task_type":"local_agent"}`,
			`{"type":"system","subtype":"task_notification","task_id":"fg","tool_use_id":"spawn","status":"completed","summary":"Joke"}`,
			foregroundStartedCommit,
			foregroundNotifiedCommit,
		)
		want(t, got, -1)
	})
	t.Run("SubagentCommand", func(t *testing.T) {
		t.Parallel()
		// Minimized from task 4VN78Q1IU000: the watch a subagent armed expired
		// and resumed that subagent; the parent started no turn.
		childUse := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01BZYJG5Hou8GZuKXutqD1Jf","name":"Monitor","input":{"command":"tail -f log","description":"NeMo smoke results"}}]},"parent_tool_use_id":"toolu_011Y7rdVxNjW1QcFj63n31FG","session_id":"fe242f82"}`
		started := `{"type":"system","subtype":"task_started","task_id":"bw2u31182","run_id":"0mwif2bex-f5f2cde1","tool_use_id":"toolu_01BZYJG5Hou8GZuKXutqD1Jf","description":"NeMo smoke results","is_backgrounded":true,"task_type":"local_bash","session_id":"fe242f82"}`
		killed := `{"type":"system","subtype":"task_updated","task_id":"bw2u31182","run_id":"0mwif2bex-f5f2cde1","patch":{"status":"killed","end_time":1794783538399},"session_id":"fe242f82"}`
		if got := queued(t, childUse, started, killed); slices.ContainsFunc(got, func(g []string) bool { return slices.Contains(g, "turn") }) {
			t.Errorf("subagent tool use: got %q, want no queued turn", got)
		}
		owned := strings.Replace(started, `"is_backgrounded":true`, `"owned_by_subagent":true,"is_backgrounded":true,"parent_task_id":"a5ae28ce78d5b5f5e"`, 1)
		if got := queued(t, owned, killed); slices.ContainsFunc(got, func(g []string) bool { return slices.Contains(g, "turn") }) {
			t.Errorf("owned_by_subagent: got %q, want no queued turn", got)
		}
	})
}

// Records from task 50R144VRE000 (Claude Code 2.1.325). A snapshot names a
// task before its task_started; the snapshot after the terminal records drops
// it. Monitor records above come from the same session.
const (
	liveSession          = `"session_id":"f48cd6f1-296a-4c99-a453-a18a42c76e99"`
	liveSnapshotCommand  = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bhvck4yky","run_id":"0mwjoh868-a2e497da","task_type":"local_bash","description":"Run fix, verify, test in caic and mddb"}],` + liveSession + `}`
	liveStartedCommand   = `{"type":"system","subtype":"task_started","task_id":"bhvck4yky","run_id":"0mwjoh868-a2e497da","tool_use_id":"toolu_01TA2sDMpDmEFCS1oXRsK8tT","description":"Run fix, verify, test in caic and mddb","is_backgrounded":true,"task_type":"local_bash",` + liveSession + `}`
	liveUpdatedCommand   = `{"type":"system","subtype":"task_updated","task_id":"bhvck4yky","run_id":"0mwjoh868-a2e497da","patch":{"status":"completed","end_time":1794858889153},` + liveSession + `}`
	liveNotifiedCommand  = `{"type":"system","subtype":"task_notification","task_id":"bhvck4yky","run_id":"0mwjoh868-a2e497da","tool_use_id":"toolu_01TA2sDMpDmEFCS1oXRsK8tT","status":"completed","output_file":"/tmp/claude-1000/tasks/bhvck4yky.output","summary":"Background command \"Run fix, verify, test in caic and mddb\" completed (exit code 0)",` + liveSession + `}`
	liveSnapshotDropped  = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bizvx8b6o","run_id":"0mwjohbq7-83f2cd99","task_type":"local_bash","description":"caic/mddb make fix/verify/test results"},{"task_id":"bivzo0lc8","run_id":"0mwjohf5a-17c20a43","task_type":"local_bash","description":"wait for all six make runs"}],` + liveSession + `}`
	liveSnapshotWatch    = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bizvx8b6o","run_id":"0mwjohbq7-83f2cd99","task_type":"local_bash","description":"caic/mddb make fix/verify/test results"}],` + liveSession + `}`
	liveSnapshotEmpty    = `{"type":"system","subtype":"background_tasks_changed","tasks":[],` + liveSession + `}`
	liveInit             = `{"type":"system","subtype":"init","cwd":"/home/user/src/gomode",` + liveSession + `}`
	liveStatusRequesting = `{"type":"system","subtype":"status","status":"requesting",` + liveSession + `}`

	// Task 4VN78Q1IU000 (Claude Code 2.1.325): a background agent run.
	agentSession       = `"session_id":"fe242f82-bdf6-4830-aaa7-3ed146718f34"`
	agentSnapshotLive  = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a3635a9373e98f76a","run_id":"0mwjtvyk3-60e1451f","task_type":"local_agent","subagent_type":"general-purpose","description":"Phase go-test-helper-check"}],` + agentSession + `}`
	agentStartedLive   = `{"type":"system","subtype":"task_started","task_id":"a3635a9373e98f76a","run_id":"0mwjtvyk3-60e1451f","tool_use_id":"toolu_014N3CgJTcnLGnbSbium2crh","description":"Phase go-test-helper-check","subagent_type":"general-purpose","is_backgrounded":true,"spawn_depth":1,"task_type":"local_agent","prompt":"Review BLOCKED one item.",` + agentSession + `}`
	agentSnapshotEmpty = `{"type":"system","subtype":"background_tasks_changed","tasks":[],` + agentSession + `}`
	agentInit          = `{"type":"system","subtype":"init","cwd":"/home/user/src/genai",` + agentSession + `}`

	// Task 44HVBS5RG000 (Claude Code 2.1.283) emits the dropping snapshot
	// before the terminal task_updated.
	backgroundSnapshotEmpty = `{"type":"system","subtype":"background_tasks_changed","tasks":[],"uuid":"d657ecde-e616-4889-8b13-c1580134f270","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
	backgroundStatus        = `{"type":"system","subtype":"status","status":"requesting","session_id":"6fe924d6-10b5-4c51-8d98-e50e07518c37"}`
)

func TestLiveTasks(t *testing.T) {
	t.Parallel()
	// events returns, per line, what the wire emitted for the cards: "turn",
	// "retired:<id>", "agent:<status>", or "command:<status>".
	events := func(t *testing.T, lines ...string) [][]string {
		wire := newWireFormat()
		out := make([][]string, len(lines))
		for i, line := range lines {
			msgs, err := wire.ParseOutput([]byte(line))
			if err != nil {
				t.Fatalf("line %d: %v", i, err)
			}
			for _, msg := range msgs {
				switch m := msg.(type) {
				case *agent.TurnQueuedMessage:
					out[i] = append(out[i], "turn")
				case *agent.BackgroundRetiredMessage:
					if m.Source.IsChild() {
						t.Fatalf("line %d: retirement has child provenance %#v", i, m.Source)
					}
					out[i] = append(out[i], "retired:"+m.ID)
				case *agent.NativeSubagentMessage:
					out[i] = append(out[i], "agent:"+string(m.Subagent.Status))
				case *agent.BackgroundCommandMessage:
					out[i] = append(out[i], "command:"+string(m.Command.Status))
				}
			}
		}
		return out
	}
	check := func(t *testing.T, got, want [][]string) {
		if !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("got  %q\nwant %q", got, want)
		}
	}
	t.Run("LostTerminalRetired", func(t *testing.T) {
		t.Parallel()
		t.Run("Command", func(t *testing.T) {
			t.Parallel()
			got := events(t, liveSnapshotCommand, liveStartedCommand, liveSnapshotDropped, liveInit, liveStatusRequesting)
			check(t, got, [][]string{nil, {"command:running"}, nil, {"retired:claude:shell:bhvck4yky"}, nil})
		})
		t.Run("Watch", func(t *testing.T) {
			t.Parallel()
			got := events(t, monitorToolUse, liveSnapshotWatch, monitorStarted, liveSnapshotEmpty, liveInit)
			check(t, got, [][]string{nil, nil, {"command:running"}, nil, {"retired:claude:shell:bizvx8b6o"}})
		})
		t.Run("Agent", func(t *testing.T) {
			t.Parallel()
			got := events(t, agentSnapshotLive, agentStartedLive, agentSnapshotEmpty, agentInit)
			check(t, got, [][]string{nil, {"agent:running"}, nil, {"retired:claude:task:a3635a9373e98f76a"}})
		})
		t.Run("NextSnapshot", func(t *testing.T) {
			t.Parallel()
			// A snapshot that follows the dropping snapshot ends the drop too.
			got := events(t, liveSnapshotCommand, liveStartedCommand, liveSnapshotDropped, liveSnapshotEmpty)
			check(t, got, [][]string{nil, {"command:running"}, nil, {"retired:claude:shell:bhvck4yky"}})
		})
	})
	t.Run("SnapshotBeforeStartKeepsCard", func(t *testing.T) {
		t.Parallel()
		got := events(t, liveSnapshotCommand, liveStartedCommand, liveInit, liveSnapshotCommand, liveStatusRequesting)
		check(t, got, [][]string{nil, {"command:running"}, nil, nil, nil})
	})
	t.Run("StartAfterDropKeepsCard", func(t *testing.T) {
		t.Parallel()
		// Only a task the previous snapshot listed can be dropped, and its
		// task_started cancels the drop.
		got := events(t, liveSnapshotDropped, liveStartedCommand, liveSnapshotEmpty, liveInit)
		check(t, got, [][]string{nil, {"command:running"}, nil, nil})
		got = events(t, liveSnapshotCommand, liveSnapshotEmpty, liveStartedCommand, liveInit)
		check(t, got, [][]string{nil, nil, {"command:running"}, nil})
	})
	t.Run("TerminalNotReopened", func(t *testing.T) {
		t.Parallel()
		got := events(t, liveSnapshotCommand, liveStartedCommand, liveUpdatedCommand, liveNotifiedCommand, liveSnapshotDropped, liveInit, liveSnapshotCommand, liveStatusRequesting)
		check(t, got, [][]string{nil, {"command:running"}, {"turn", "command:completed"}, {"command:completed"}, nil, nil, nil, nil})
	})
	t.Run("DropBeforeTerminal", func(t *testing.T) {
		t.Parallel()
		// Claude Code 2.1.283: the terminal records follow the dropping snapshot
		// and settle the card before the drop takes effect.
		got := events(t, backgroundSnapshotOne, backgroundStartedLint, backgroundSnapshotEmpty, backgroundUpdatedLint, backgroundNotifiedLint, backgroundStatus)
		check(t, got, [][]string{nil, {"command:running"}, nil, {"turn", "command:completed"}, {"command:completed"}, nil})
	})
	t.Run("ForegroundAgentKept", func(t *testing.T) {
		t.Parallel()
		foreground := strings.Replace(agentStartedLive, `"is_backgrounded":true`, `"is_backgrounded":false`, 1)
		got := events(t, agentSnapshotLive, foreground, agentSnapshotEmpty, agentInit)
		check(t, got, [][]string{nil, {"agent:running"}, nil, nil})
	})
	t.Run("InterleavedTasks", func(t *testing.T) {
		t.Parallel()
		// The watch's task_started does not cancel the command's drop, and each
		// drop retires only its own card.
		got := events(t, monitorToolUse, liveSnapshotCommand, liveStartedCommand, liveSnapshotWatch, monitorStarted, liveSnapshotDropped, liveInit, liveSnapshotEmpty, liveStatusRequesting)
		check(t, got, [][]string{nil, nil, {"command:running"}, nil, {"command:running"}, {"retired:claude:shell:bhvck4yky"}, nil, nil, {"retired:claude:shell:bizvx8b6o"}})
	})
}
