package task

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestTask(t *testing.T) {
	t.Run("Subscribe", func(t *testing.T) {
		t.Run("SlowSubscriberThenCancel", func(t *testing.T) {
			// Regression test: if the fan-out drops a slow subscriber
			// (buffer full) and closes its channel, the context-done
			// goroutine must not panic on a double close.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			ctx, cancel := context.WithCancel(t.Context())
			_, ch, unsub := tk.Subscribe(ctx)
			defer unsub()

			// Fill the subscriber buffer (256) so the next send overflows.
			for range 256 {
				tk.addMessage(t.Context(), &agent.SystemMessage{MessageType: "system", Subtype: "status"}, false)
			}
			// This send should trigger the slow-subscriber drop+close.
			tk.addMessage(t.Context(), &agent.SystemMessage{MessageType: "system", Subtype: "status"}, false)

			// Drain to confirm channel was closed by fan-out.
			for range ch {
			}

			// Cancel the context. The goroutine must not panic.
			cancel()
			// Give the goroutine time to execute.
			time.Sleep(50 * time.Millisecond)
		})
		t.Run("Replay", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			// Add messages before subscribing.
			msg1 := &agent.SystemMessage{MessageType: "system", Subtype: "status"}
			msg2 := &agent.TextMessage{Text: "hello"}
			tk.addMessage(t.Context(), msg1, false)
			tk.addMessage(t.Context(), msg2, false)

			history, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()
			_ = ch

			if len(history) != 2 {
				t.Fatalf("history len = %d, want 2", len(history))
			}
			if history[0].Type() != "system" {
				t.Errorf("history[0].Type() = %q, want %q", history[0].Type(), "system")
			}
			if history[1].Type() != "text" {
				t.Errorf("history[1].Type() = %q, want %q", history[1].Type(), "text")
			}
		})
		t.Run("ReplayLargeHistory", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			// Add more messages than any reasonable channel buffer to verify no deadlock.
			const n = 1000
			for range n {
				tk.addMessage(t.Context(), &agent.TextMessage{Text: "msg"}, false)
			}

			history, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()
			_ = ch

			if len(history) != n {
				t.Fatalf("history len = %d, want %d", len(history), n)
			}
		})
		t.Run("MultipleListeners", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.addMessage(t.Context(), &agent.SystemMessage{MessageType: "system", Subtype: "init"}, false)

			// Start two subscribers.
			h1, ch1, unsub1 := tk.Subscribe(t.Context())
			defer unsub1()
			h2, ch2, unsub2 := tk.Subscribe(t.Context())
			defer unsub2()

			// Both get the same history.
			if len(h1) != 1 || len(h2) != 1 {
				t.Fatalf("history lens = %d, %d; want 1, 1", len(h1), len(h2))
			}

			// Send a live message — both channels should receive it.
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "live"}, false)

			timeout := time.After(time.Second)
			for i, ch := range []<-chan agent.Message{ch1, ch2} {
				select {
				case msg := <-ch:
					if msg.Type() != "text" {
						t.Errorf("subscriber %d: type = %q, want %q", i, msg.Type(), "text")
					}
				case <-timeout:
					t.Fatalf("subscriber %d: timed out waiting for live message", i)
				}
			}
		})
		t.Run("Live", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}

			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			// Send a live message after subscribing.
			msg := &agent.TextMessage{Text: "live"}
			tk.addMessage(t.Context(), msg, false)

			timeout := time.After(time.Second)
			select {
			case got := <-ch:
				if got.Type() != "text" {
					t.Errorf("type = %q, want %q", got.Type(), "text")
				}
			case <-timeout:
				t.Fatal("timed out waiting for live message")
			}
		})
	})

	t.Run("SendInput", func(t *testing.T) {
		t.Run("PreservesPlanContent", func(t *testing.T) {
			// When the user sends regular input (instead of clicking
			// "Clear and execute plan"), planContent must be preserved
			// so the plan UI reappears after the agent finishes. The
			// UI hides naturally while the task is Running.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			// Simulate: agent entered plan mode, wrote a plan, exited.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "EnterPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "ExitPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)

			snap := tk.Snapshot()
			if snap.PlanContent != "the plan" {
				t.Fatalf("PlanContent = %q before SendInput, want %q", snap.PlanContent, "the plan")
			}

			// Attach a live session so SendInput succeeds past the handle check.
			cmd := exec.Command("cat")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := agent.NewSession(cmd, stdin, stdout, nil, nil, &testWire{}, nil)
			tk.AttachSession(&SessionHandle{Session: s})
			defer func() { _ = stdin.Close(); _ = cmd.Wait() }()

			// User sends a regular message instead of "Clear and execute plan".
			_ = tk.SendInput(t.Context(), agent.Prompt{Text: "improve the plan"})

			snap = tk.Snapshot()
			if snap.PlanContent != "the plan" {
				t.Errorf("PlanContent = %q after SendInput, want %q", snap.PlanContent, "the plan")
			}
			if snap.PlanFile != "/home/user/.claude/plans/p.md" {
				t.Errorf("PlanFile = %q after SendInput, want %q", snap.PlanFile, "/home/user/.claude/plans/p.md")
			}
		})
		t.Run("EditUpdatesPlanContent", func(t *testing.T) {
			// When the agent uses the Edit tool on a plan file, the
			// in-memory planContent must be updated so the UI shows
			// the revised plan.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			// Agent writes the initial plan.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"step 1\nstep 2\n"}`),
			}, false)
			if snap := tk.Snapshot(); snap.PlanContent != "step 1\nstep 2\n" {
				t.Fatalf("PlanContent = %q after Write, want %q", snap.PlanContent, "step 1\nstep 2\n")
			}
			// Agent edits the plan file.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Edit",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","old_string":"step 2","new_string":"step 2 (revised)\nstep 3"}`),
			}, false)
			snap := tk.Snapshot()
			if snap.PlanContent != "step 1\nstep 2 (revised)\nstep 3\n" {
				t.Errorf("PlanContent = %q after Edit, want %q", snap.PlanContent, "step 1\nstep 2 (revised)\nstep 3\n")
			}
		})
		t.Run("EditReplaceAll", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"TODO\nTODO\n"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Edit",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","old_string":"TODO","new_string":"DONE","replace_all":true}`),
			}, false)
			snap := tk.Snapshot()
			if snap.PlanContent != "DONE\nDONE\n" {
				t.Errorf("PlanContent = %q after replace_all Edit, want %q", snap.PlanContent, "DONE\nDONE\n")
			}
		})
		t.Run("EditIgnoresNonPlanFile", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			}, false)
			// Edit a non-plan file — planContent must be unchanged.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Edit",
				Input: json.RawMessage(`{"file_path":"/home/user/src/main.go","old_string":"foo","new_string":"bar"}`),
			}, false)
			snap := tk.Snapshot()
			if snap.PlanContent != "the plan" {
				t.Errorf("PlanContent = %q after non-plan Edit, want %q", snap.PlanContent, "the plan")
			}
		})
		t.Run("EditAfterSendInputUpdatesPlan", func(t *testing.T) {
			// Core regression test: user rejects plan and asks for
			// improvement, agent edits the plan file.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"original plan"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)

			// Attach a live session.
			cmd := exec.Command("cat")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := agent.NewSession(cmd, stdin, stdout, nil, nil, &testWire{}, nil)
			tk.AttachSession(&SessionHandle{Session: s})
			defer func() { _ = stdin.Close(); _ = cmd.Wait() }()

			// User rejects plan and sends feedback.
			_ = tk.SendInput(t.Context(), agent.Prompt{Text: "add error handling"})

			// Agent edits the plan file.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "Edit",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","old_string":"original plan","new_string":"updated plan with error handling"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu4", Name: "ExitPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)

			snap := tk.Snapshot()
			if snap.PlanContent != "updated plan with error handling" {
				t.Errorf("PlanContent = %q, want %q", snap.PlanContent, "updated plan with error handling")
			}
		})
		t.Run("NoSession", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateWaiting)
			err := tk.SendInput(t.Context(), agent.Prompt{Text: "hello"})
			if err == nil {
				t.Fatal("expected error when no session is active")
			}
			msg := err.Error()
			if !strings.Contains(msg, "session="+string(SessionNone)) {
				t.Errorf("error = %q, want session=%s", msg, SessionNone)
			}
			if !strings.Contains(msg, "state=waiting") {
				t.Errorf("error = %q, want state=waiting", msg)
			}
		})
		t.Run("DeadSessionDetected", func(t *testing.T) {
			// Simulate a session that has already finished (e.g. relay
			// subprocess exited). SendInput should detect it and return
			// "no active session" without changing state.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateWaiting)
			cmd := exec.Command("true")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := agent.NewSession(cmd, stdin, stdout, nil, nil, &testWire{}, nil)
			<-s.Done()
			tk.AttachSession(&SessionHandle{Session: s})
			err = tk.SendInput(t.Context(), agent.Prompt{Text: "hello"})
			if err == nil {
				t.Fatal("expected error for dead session")
			}
			msg := err.Error()
			if !strings.Contains(msg, "session="+string(SessionExited)) {
				t.Errorf("error = %q, want session=%s", msg, SessionExited)
			}
			if !strings.Contains(msg, "state=waiting") {
				t.Errorf("error = %q, want state=waiting", msg)
			}
		})
	})

	t.Run("AttachDetachSession", func(t *testing.T) {
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		if tk.SessionDone() != nil {
			t.Error("SessionDone() should be nil when no session attached")
		}
		if tk.DetachSession() != nil {
			t.Error("DetachSession() should return nil when no session attached")
		}

		cmd := exec.Command("cat")
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		s := agent.NewSession(cmd, stdin, stdout, nil, nil, &testWire{}, nil)
		h := &SessionHandle{Session: s}
		tk.AttachSession(h)

		if tk.SessionDone() == nil {
			t.Error("SessionDone() should not be nil after AttachSession")
		}

		got := tk.DetachSession()
		if got != h {
			t.Error("DetachSession() returned wrong handle")
		}
		if tk.SessionDone() != nil {
			t.Error("SessionDone() should be nil after DetachSession")
		}

		// Cleanup.
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	t.Run("addMessage", func(t *testing.T) {
		t.Run("TransitionsToWaiting", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			result := &agent.ResultMessage{MessageType: "result"}
			tk.addMessage(t.Context(), result, false)
			if tk.GetState() != StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), StateWaiting)
			}
		})
		t.Run("TransitionsToAsking", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			// Add an AskMessage.
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []agent.AskQuestion{{Question: "which?"}},
			}, false)
			// Now add a result message — should transition to StateAsking.
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), StateAsking)
			}
		})
		t.Run("TransitionsToAskingWithPartialMessages", func(t *testing.T) {
			// With --include-partial-messages, Claude Code emits multiple
			// assistant snapshots per turn. AskUserQuestion appears in an
			// earlier snapshot while the final one is text-only. The state
			// machine must scan all messages in the turn.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "I need to ask you something."}, false)
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []agent.AskQuestion{{Question: "which?"}},
			}, false)
			// Final partial snapshot: text-only, no tool_use.
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "I need to ask you something."}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), StateAsking)
			}
		})
		t.Run("TextMessageTransitionsWaitingToRunning", func(t *testing.T) {
			// When the agent starts producing output while the task is
			// waiting (e.g. relay reconnect after server restart), the
			// state should transition back to running.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateWaiting)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "output"}, false)
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
			}
		})
		t.Run("ToolUseMessageTransitionsAskingToRunning", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateAsking)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{ToolUseID: "tu1", Name: "Read"}, false)
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
			}
		})
		t.Run("ResultTransitionsWaitingToAsking", func(t *testing.T) {
			// When watchSession sets Waiting before the ResultMessage is
			// processed, the ResultMessage should still detect
			// AskMessage and correct the state to Asking.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []agent.AskQuestion{{Question: "which?"}},
			}, false)
			// Simulate watchSession setting Waiting before ResultMessage
			// is processed by the dispatch goroutine.
			tk.SetState(StateWaiting)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), StateAsking)
			}
		})
		t.Run("TransitionsToHasPlan", func(t *testing.T) {
			// ExitPlanMode + plan content + ResultMessage → StateHasPlan.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)
			if tk.GetState() != StateHasPlan {
				t.Errorf("state = %v, want %v", tk.GetState(), StateHasPlan)
			}
		})
		t.Run("AskingTakesPriorityOverHasPlan", func(t *testing.T) {
			// Both AskMessage and ExitPlanMode in same turn → StateAsking.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []agent.AskQuestion{{Question: "which?"}},
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), StateAsking)
			}
		})
		t.Run("NoHasPlanWithoutPlanContent", func(t *testing.T) {
			// ExitPlanMode without plan content → StateWaiting.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "ExitPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)
			if tk.GetState() != StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), StateWaiting)
			}
		})
		t.Run("ExitPlanModeSnapshotsPlanContent", func(t *testing.T) {
			// trackToolUse must snapshot planContent onto the ExitPlanMode
			// ToolUseMessage so the SSE converter can include it.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan A"}`),
			}, false)
			exitMsg := &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"}
			tk.addMessage(t.Context(), exitMsg, false)
			if exitMsg.PlanContent != "plan A" {
				t.Errorf("ExitPlanMode.PlanContent = %q, want %q", exitMsg.PlanContent, "plan A")
			}
		})
		t.Run("NewExitPlanModeClearsPreviousPlanContent", func(t *testing.T) {
			// When a second ExitPlanMode arrives, the first one's PlanContent
			// must be cleared so the frontend doesn't list the stale plan.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v1"}`),
			}, false)
			exitMsg1 := &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"}
			tk.addMessage(t.Context(), exitMsg1, false)
			if exitMsg1.PlanContent != "plan v1" {
				t.Fatalf("exitMsg1.PlanContent = %q before update, want %q", exitMsg1.PlanContent, "plan v1")
			}
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)

			// Agent updates the plan.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v2"}`),
			}, false)
			exitMsg2 := &agent.ToolUseMessage{ToolUseID: "tu4", Name: "ExitPlanMode"}
			tk.addMessage(t.Context(), exitMsg2, false)
			if exitMsg1.PlanContent != "" {
				t.Errorf("exitMsg1.PlanContent = %q, want empty (superseded by plan v2)", exitMsg1.PlanContent)
			}
			if exitMsg2.PlanContent != "plan v2" {
				t.Errorf("exitMsg2.PlanContent = %q, want %q", exitMsg2.PlanContent, "plan v2")
			}
		})
		t.Run("HasPlanToRunningOnText", func(t *testing.T) {
			// TextMessage while HasPlan → Running.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateHasPlan)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "output"}, false)
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
			}
		})
		t.Run("NoTransitionForNonActiveStates", func(t *testing.T) {
			// TextMessages should NOT transition terminal or
			// setup states.
			for _, state := range []State{StatePending, StateBranching, StateProvisioning, StateStarting, StatePurging, StateFailed, StatePurged} {
				tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
				tk.SetState(state)
				tk.addMessage(t.Context(), &agent.TextMessage{Text: "output"}, false)
				if tk.GetState() != state {
					t.Errorf("state %v changed to %v; want unchanged", state, tk.GetState())
				}
			}
		})
	})

	t.Run("addMessageDiffStat", func(t *testing.T) {
		t.Run("DiffStatMessage", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			ds := agent.DiffStat{
				{Path: "main.go", Added: 10, Deleted: 3},
				{Path: "img.png", Binary: true},
			}
			tk.addMessage(t.Context(), &agent.DiffStatMessage{
				MessageType: "caic_diff_stat",
				DiffStat:    ds,
			}, false)
			got := tk.LiveDiffStat()
			if len(got) != 2 {
				t.Fatalf("LiveDiffStat len = %d, want 2", len(got))
			}
			if got[0].Path != "main.go" || got[0].Added != 10 {
				t.Errorf("LiveDiffStat[0] = %+v", got[0])
			}
			// Update with new diff stat.
			tk.addMessage(t.Context(), &agent.DiffStatMessage{
				MessageType: "caic_diff_stat",
				DiffStat:    agent.DiffStat{{Path: "new.go", Added: 1, Deleted: 0}},
			}, false)
			got = tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "new.go" {
				t.Errorf("LiveDiffStat after update = %+v", got)
			}
		})

		t.Run("ResultMessageUpdatesLiveDiffStat", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ResultMessage{
				MessageType: "result",
				DiffStat:    agent.DiffStat{{Path: "a.go", Added: 5, Deleted: 2}},
			}, false)
			got := tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "a.go" || got[0].Added != 5 {
				t.Errorf("LiveDiffStat = %+v, want [{a.go 5 2}]", got)
			}
		})
	})

	t.Run("RestoreMessagesDiffStat", func(t *testing.T) {
		t.Run("DiffStatMessage", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StatePurged)
			tk.RestoreMessages([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    agent.DiffStat{{Path: "old.go", Added: 1}},
				},
				&agent.TextMessage{Text: "hello"},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    agent.DiffStat{{Path: "latest.go", Added: 5}},
				},
			})
			got := tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "latest.go" {
				t.Errorf("LiveDiffStat = %+v, want latest.go", got)
			}
		})

		t.Run("ResultMessageAfterDiffStat", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StatePurged)
			tk.RestoreMessages([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    agent.DiffStat{{Path: "stale.go", Added: 1}},
				},
				&agent.ResultMessage{
					MessageType: "result",
					DiffStat:    agent.DiffStat{{Path: "authoritative.go", Added: 10}},
				},
			})
			got := tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "authoritative.go" {
				t.Errorf("LiveDiffStat = %+v, want authoritative.go", got)
			}
		})

		t.Run("DiffStatAfterResult", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StatePurged)
			tk.RestoreMessages([]agent.Message{
				&agent.ResultMessage{
					MessageType: "result",
					DiffStat:    agent.DiffStat{{Path: "result.go", Added: 5}},
				},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    agent.DiffStat{{Path: "relay.go", Added: 3}},
				},
			})
			got := tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "relay.go" {
				t.Errorf("LiveDiffStat = %+v, want relay.go", got)
			}
		})

		// Regression: the relay's diff_watcher computes git diff HEAD
		// (uncommitted changes). After the agent commits, this is empty.
		// The ResultMessage.DiffStat (host-side branch diff) is set
		// in-memory by startMessageDispatch and NOT persisted to the
		// relay output, so RestoreMessages only sees the empty relay
		// diff. Callers (adoptOne) must compute the host-side diff stat
		// separately after RestoreMessages.
		t.Run("EmptyRelayDiffAfterCommit", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			// Simulate relay output: ResultMessage without DiffStat
			// (host-side mutation not persisted) followed by an empty
			// DiffStatMessage (relay sees no uncommitted changes).
			tk.RestoreMessages([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    agent.DiffStat{{Path: "main.go", Added: 10, Deleted: 2}},
				},
				&agent.ResultMessage{MessageType: "result"},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    agent.DiffStat{},
				},
			})
			got := tk.LiveDiffStat()
			if len(got) != 0 {
				t.Fatalf("LiveDiffStat = %+v, want empty (relay reported no uncommitted changes)", got)
			}
			// After adoption, the caller should compute the host-side
			// diff stat and set it.
			tk.SetLiveDiffStat(agent.DiffStat{{Path: "main.go", Added: 10, Deleted: 2}})
			got = tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "main.go" {
				t.Errorf("LiveDiffStat after set = %+v, want main.go", got)
			}
		})
	})

	t.Run("LiveUsageCumulative", func(t *testing.T) {
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StateRunning)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10},
		}, false)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 200, OutputTokens: 80, CacheCreationInputTokens: 30},
		}, false)
		_, _, _, usage, lastUsage := tk.LiveStats()
		if usage.InputTokens != 300 {
			t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
		}
		if usage.OutputTokens != 130 {
			t.Errorf("OutputTokens = %d, want 130", usage.OutputTokens)
		}
		if usage.CacheReadInputTokens != 10 {
			t.Errorf("CacheReadInputTokens = %d, want 10", usage.CacheReadInputTokens)
		}
		if usage.CacheCreationInputTokens != 30 {
			t.Errorf("CacheCreationInputTokens = %d, want 30", usage.CacheCreationInputTokens)
		}
		// lastUsage should reflect only the most recent ResultMessage.
		if lastUsage.InputTokens != 200 {
			t.Errorf("lastUsage.InputTokens = %d, want 200", lastUsage.InputTokens)
		}
		if lastUsage.CacheCreationInputTokens != 30 {
			t.Errorf("lastUsage.CacheCreationInputTokens = %d, want 30", lastUsage.CacheCreationInputTokens)
		}
	})

	t.Run("RestoreMessagesUsageCumulative", func(t *testing.T) {
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StatePurged)
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{
				MessageType: "result",
				Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50},
			},
			&agent.TextMessage{Text: "hello"},
			&agent.ResultMessage{
				MessageType: "result",
				Usage:       agent.Usage{InputTokens: 200, OutputTokens: 80},
			},
		})
		_, _, _, usage, lastUsage := tk.LiveStats()
		if usage.InputTokens != 300 {
			t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
		}
		if usage.OutputTokens != 130 {
			t.Errorf("OutputTokens = %d, want 130", usage.OutputTokens)
		}
		// lastUsage should reflect only the last ResultMessage.
		if lastUsage.InputTokens != 200 {
			t.Errorf("lastUsage.InputTokens = %d, want 200", lastUsage.InputTokens)
		}
		if lastUsage.OutputTokens != 80 {
			t.Errorf("lastUsage.OutputTokens = %d, want 80", lastUsage.OutputTokens)
		}
	})

	t.Run("LiveCostCumulativeAcrossSessions", func(t *testing.T) {
		// Cost, turns, and duration must accumulate across sessions separated
		// by ClearMessages. computeCost uses TotalCostUSD as the base and adds
		// the cache-read surcharge.
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StateRunning)
		// Session 1: TotalCostUSD = $10.00.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 10.0,
			NumTurns:     3,
			DurationMs:   5000,
		}, false)
		tk.ClearMessages(t.Context())
		tk.SetState(StateRunning)
		// Session 2: TotalCostUSD = $5.00.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 5.0,
			NumTurns:     2,
			DurationMs:   3000,
		}, false)
		costUSD, numTurns, duration, _, _ := tk.LiveStats()
		if costUSD != 15.0 {
			t.Errorf("costUSD = %v, want 15.0", costUSD)
		}
		if numTurns != 5 {
			t.Errorf("numTurns = %d, want 5", numTurns)
		}
		if duration != 8*time.Second {
			t.Errorf("duration = %v, want 8s", duration)
		}
	})

	t.Run("LiveDurationAccumulatesWithinSession", func(t *testing.T) {
		// Multiple result events within a single session (no ClearMessages/compact_boundary)
		// must accumulate duration rather than overwriting with only the last invocation's value.
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StateRunning)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			NumTurns:    1,
			DurationMs:  946943,
		}, false)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			NumTurns:    1,
			DurationMs:  5278,
		}, false)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			NumTurns:    1,
			DurationMs:  214500,
		}, false)
		_, numTurns, duration, _, _ := tk.LiveStats()
		if numTurns != 3 {
			t.Errorf("numTurns = %d, want 3", numTurns)
		}
		wantDuration := time.Duration(946943+5278+214500) * time.Millisecond
		if duration != wantDuration {
			t.Errorf("duration = %v, want %v", duration, wantDuration)
		}
	})

	t.Run("LiveCostCumulativeAcrossThreeSessions", func(t *testing.T) {
		// Regression: ClearMessages used += (double-count) instead of = assignment.
		// Verify cost is correct after two ClearMessages calls.
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StateRunning)
		// Session 1: $75.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 75.0,
			NumTurns:     1,
		}, false)
		tk.ClearMessages(t.Context())
		tk.SetState(StateRunning)
		// Session 2: $75.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 75.0,
			NumTurns:     1,
		}, false)
		tk.ClearMessages(t.Context())
		tk.SetState(StateRunning)
		// Session 3: $75.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 75.0,
			NumTurns:     1,
		}, false)
		costUSD, numTurns, _, _, _ := tk.LiveStats()
		if costUSD != 225.0 {
			t.Errorf("costUSD = %v, want 225.0 (three sessions × $75)", costUSD)
		}
		if numTurns != 3 {
			t.Errorf("numTurns = %d, want 3", numTurns)
		}
	})

	t.Run("RestoreMessagesDurationAccumulatesWithinSession", func(t *testing.T) {
		// RestoreMessages (reloadFromMsgs) must accumulate DurationMs across
		// multiple result events within a single session.
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StatePurged)
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{MessageType: "result", NumTurns: 1, DurationMs: 946943},
			&agent.ResultMessage{MessageType: "result", NumTurns: 1, DurationMs: 5278},
			&agent.ResultMessage{MessageType: "result", NumTurns: 1, DurationMs: 214500},
		})
		_, numTurns, duration, _, _ := tk.LiveStats()
		if numTurns != 3 {
			t.Errorf("numTurns = %d, want 3", numTurns)
		}
		wantDuration := time.Duration(946943+5278+214500) * time.Millisecond
		if duration != wantDuration {
			t.Errorf("duration = %v, want %v", duration, wantDuration)
		}
	})

	t.Run("RestoreMessagesCostCumulativeAcrossSessions", func(t *testing.T) {
		// RestoreMessages must sum cost/turns/duration across context_cleared
		// boundaries, mirroring the live path.
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StatePurged)
		tk.RestoreMessages([]agent.Message{
			// Session 1: TotalCostUSD = $10.00.
			&agent.ResultMessage{
				MessageType:  "result",
				TotalCostUSD: 10.0,
				NumTurns:     3,
				DurationMs:   5000,
			},
			&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
			// Session 2: TotalCostUSD = $5.00.
			&agent.ResultMessage{
				MessageType:  "result",
				TotalCostUSD: 5.0,
				NumTurns:     2,
				DurationMs:   3000,
			},
		})
		costUSD, numTurns, duration, _, _ := tk.LiveStats()
		if costUSD != 15.0 {
			t.Errorf("costUSD = %v, want 15.0", costUSD)
		}
		if numTurns != 5 {
			t.Errorf("numTurns = %d, want 5", numTurns)
		}
		if duration != 8*time.Second {
			t.Errorf("duration = %v, want 8s", duration)
		}
	})

	t.Run("LiveCostIncludesCacheRead", func(t *testing.T) {
		// Regression: TotalCostUSD from Claude Code omits cache_read cost.
		// computeCost must add the surcharge on top of TotalCostUSD.
		// Setup: TotalCostUSD = $1.50 from 100K input tokens (price = $0.000015/tok).
		// Cache read surcharge = 10M × 0.10 × $0.000015 = $15.00.
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StateRunning)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 1.50,
			Usage: agent.Usage{
				InputTokens:          100_000,
				CacheReadInputTokens: 10_000_000,
			},
		}, false)
		costUSD, _, _, _, _ := tk.LiveStats()
		if costUSD != 16.50 { // $1.50 (reported) + $15.00 (cache read surcharge)
			t.Errorf("costUSD = %v, want 16.50 (cache reads must be added to TotalCostUSD)", costUSD)
		}
	})

	t.Run("CompactBoundaryAccumulatesStats", func(t *testing.T) {
		// compact_boundary resets NumTurns, DurationMs, and TotalCostUSD in
		// Claude Code's subsequent ResultMessages. Stats must be accumulated
		// across the boundary, just like context_cleared.
		newTask := func() *Task {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			return tk
		}
		result1 := &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 10.0,
			NumTurns:     3,
			DurationMs:   5000,
		}
		compact := &agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary"}
		result2 := &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 5.0,
			NumTurns:     2,
			DurationMs:   3000,
		}

		t.Run("Live", func(t *testing.T) {
			tk := newTask()
			tk.addMessage(t.Context(), result1, false)
			tk.addMessage(t.Context(), compact, false)
			tk.addMessage(t.Context(), result2, false)
			costUSD, numTurns, duration, _, _ := tk.LiveStats()
			if costUSD != 15.0 {
				t.Errorf("costUSD = %v, want 15.0", costUSD)
			}
			if numTurns != 5 {
				t.Errorf("numTurns = %d, want 5", numTurns)
			}
			if duration != 8*time.Second {
				t.Errorf("duration = %v, want 8s", duration)
			}
		})

		t.Run("Restore", func(t *testing.T) {
			tk := newTask()
			tk.SetState(StatePurged)
			tk.RestoreMessages([]agent.Message{result1, compact, result2})
			costUSD, numTurns, duration, _, _ := tk.LiveStats()
			if costUSD != 15.0 {
				t.Errorf("costUSD = %v, want 15.0", costUSD)
			}
			if numTurns != 5 {
				t.Errorf("numTurns = %d, want 5", numTurns)
			}
			if duration != 8*time.Second {
				t.Errorf("duration = %v, want 8s", duration)
			}
		})
	})

	t.Run("ClearMessages", func(t *testing.T) {
		t.Run("ResetsPlanState", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			// Simulate an agent entering plan mode and writing a plan file.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "EnterPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			}, false)
			snap := tk.Snapshot()
			if !snap.InPlanMode {
				t.Fatal("InPlanMode = false before ClearMessages, want true")
			}
			if snap.PlanContent != "the plan" {
				t.Fatalf("PlanContent = %q before ClearMessages, want %q", snap.PlanContent, "the plan")
			}

			tk.ClearMessages(t.Context())

			snap = tk.Snapshot()
			if snap.InPlanMode {
				t.Error("InPlanMode = true after ClearMessages, want false")
			}
			if snap.PlanContent != "" {
				t.Errorf("PlanContent = %q after ClearMessages, want empty", snap.PlanContent)
			}
			if tk.GetPlanFile() != "" {
				t.Errorf("PlanFile = %q after ClearMessages, want empty", tk.GetPlanFile())
			}
		})
		t.Run("SuppressesPlanRewrite", func(t *testing.T) {
			// After ClearMessages (restart), the agent may re-enter plan mode
			// and write to .claude/plans/. The plan must not resurface.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			// Original plan.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)

			// User clicks "Clear and execute plan".
			tk.ClearMessages(t.Context())
			tk.SetState(StateRunning)

			// Agent re-enters plan mode during execution.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "EnterPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu4", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"rewritten plan"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu5", Name: "ExitPlanMode",
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)

			snap := tk.Snapshot()
			if snap.PlanContent != "" {
				t.Errorf("PlanContent = %q, want empty (plan written after ClearMessages should be suppressed)", snap.PlanContent)
			}
			if tk.GetPlanFile() != "" {
				t.Errorf("PlanFile = %q, want empty", tk.GetPlanFile())
			}
		})
		t.Run("ClearsExitPlanModePlanContent", func(t *testing.T) {
			// After ClearMessages the ExitPlanMode message's PlanContent in
			// history must be erased so new subscribers don't see stale plans.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			exitMsg := &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"}
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			}, false)
			tk.addMessage(t.Context(), exitMsg, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)
			if exitMsg.PlanContent != "the plan" {
				t.Fatalf("exitMsg.PlanContent = %q before ClearMessages, want %q", exitMsg.PlanContent, "the plan")
			}

			tk.ClearMessages(t.Context())

			if exitMsg.PlanContent != "" {
				t.Errorf("exitMsg.PlanContent = %q after ClearMessages, want empty", exitMsg.PlanContent)
			}
		})
		t.Run("SuppressionLiftsAfterTurn", func(t *testing.T) {
			// After the restart turn completes, a subsequent user-initiated turn
			// must be able to produce a plan again.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)

			// Restart.
			tk.ClearMessages(t.Context())
			tk.SetState(StateRunning)
			// Turn completes without plan.
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "done"}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)

			// Suppression lifted — next turn can set plan.
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"fresh plan"}`),
			}, false)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"}, false)

			snap := tk.Snapshot()
			if snap.PlanContent != "fresh plan" {
				t.Errorf("PlanContent = %q, want %q (suppression should have lifted)", snap.PlanContent, "fresh plan")
			}
		})
	})

	t.Run("RestoreMessages", func(t *testing.T) {
		t.Run("Basic", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "sess-123"},
				&agent.TextMessage{Text: "hello"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)

			if len(tk.Messages()) != 3 {
				t.Fatalf("Messages() len = %d, want 3", len(tk.Messages()))
			}
			if tk.GetSessionID() != "sess-123" {
				t.Errorf("SessionID = %q, want %q", tk.GetSessionID(), "sess-123")
			}
			if tk.GetState() != StateWaiting {
				t.Errorf("state = %v, want %v (should infer waiting from trailing ResultMessage)", tk.GetState(), StateWaiting)
			}
		})
		t.Run("InfersAsking", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "s1"},
				&agent.AskMessage{
					ToolUseID: "ask1",
					Questions: []agent.AskQuestion{{Question: "which?"}},
				},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v (should infer asking from AskMessage + ResultMessage)", tk.GetState(), StateAsking)
			}
		})
		t.Run("InfersHasPlan", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			if tk.GetState() != StateHasPlan {
				t.Errorf("state = %v, want %v", tk.GetState(), StateHasPlan)
			}
		})
		t.Run("SkipsTrailingDiffStat", func(t *testing.T) {
			// The relay emits DiffStatMessage after the ResultMessage.
			// RestoreMessages should skip it and still infer Waiting.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			msgs := []agent.Message{
				&agent.TextMessage{Text: "hello"},
				&agent.ResultMessage{MessageType: "result"},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    agent.DiffStat{{Path: "main.go", Added: 1}},
				},
			}
			tk.RestoreMessages(msgs)
			if tk.GetState() != StateWaiting {
				t.Errorf("state = %v, want %v (trailing DiffStatMessage should be skipped)", tk.GetState(), StateWaiting)
			}
		})
		t.Run("NoResultKeepsState", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "s1"},
				&agent.TextMessage{Text: "hello"},
			}
			tk.RestoreMessages(msgs)
			// No trailing ResultMessage → agent was still producing output.
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v (no ResultMessage → still running)", tk.GetState(), StateRunning)
			}
		})
		t.Run("TerminalStatePreserved", func(t *testing.T) {
			for _, state := range []State{StatePurged, StateFailed, StatePurging} {
				tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
				tk.SetState(state)
				msgs := []agent.Message{
					&agent.TextMessage{Text: "hello"},
					&agent.ResultMessage{MessageType: "result"},
				}
				tk.RestoreMessages(msgs)
				if tk.GetState() != state {
					t.Errorf("state = %v, want %v (terminal state must not be overridden)", tk.GetState(), state)
				}
			}
		})
		t.Run("UsesLastSessionID", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "old"},
				&agent.TextMessage{Text: "hello"},
				&agent.InitMessage{SessionID: "new"},
			}
			tk.RestoreMessages(msgs)

			if tk.GetSessionID() != "new" {
				t.Errorf("SessionID = %q, want %q", tk.GetSessionID(), "new")
			}
		})
		t.Run("RestoresPlanFile", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/my-plan.md","content":"plan"}`),
				},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			if tk.GetPlanFile() != "/home/user/.claude/plans/my-plan.md" {
				t.Errorf("PlanFile = %q, want %q", tk.GetPlanFile(), "/home/user/.claude/plans/my-plan.md")
			}
		})
		t.Run("RestoresInPlanMode", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{ToolUseID: "tu1", Name: "EnterPlanMode"},
				&agent.ToolUseMessage{
					ToolUseID: "tu2", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/foo.md","content":"x"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu3", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			if tk.Snapshot().InPlanMode {
				t.Error("InPlanMode = true, want false (ExitPlanMode should clear it)")
			}
			if tk.GetPlanFile() != "/home/user/.claude/plans/foo.md" {
				t.Errorf("PlanFile = %q, want %q", tk.GetPlanFile(), "/home/user/.claude/plans/foo.md")
			}

			// Without ExitPlanMode, should stay in plan mode.
			tk2 := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk2.SetState(StateRunning)
			tk2.RestoreMessages(msgs[:1])
			if !tk2.Snapshot().InPlanMode {
				t.Error("InPlanMode = false, want true (only EnterPlanMode seen)")
			}
		})
		t.Run("ContextClearedResetsPlanState", func(t *testing.T) {
			// Simulates relay output containing a plan, then a context_cleared
			// marker (from ClearMessages on restart), then a new session without
			// a plan. RestoreMessages must not carry over the stale plan.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{ToolUseID: "tu1", Name: "EnterPlanMode"},
				&agent.ToolUseMessage{
					ToolUseID: "tu2", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"old plan"}`),
				},
				&agent.ResultMessage{MessageType: "result"},
				// context_cleared injected by ClearMessages on restart.
				&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
				// New session starts — no plan tools used.
				&agent.TextMessage{Text: "done"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			snap := tk.Snapshot()
			if snap.InPlanMode {
				t.Error("InPlanMode = true, want false (context_cleared should reset)")
			}
			if snap.PlanContent != "" {
				t.Errorf("PlanContent = %q, want empty (context_cleared should reset)", snap.PlanContent)
			}
			if tk.GetPlanFile() != "" {
				t.Errorf("PlanFile = %q, want empty (context_cleared should reset)", tk.GetPlanFile())
			}
		})
		t.Run("ContextClearedSuppressesPlanRewrite", func(t *testing.T) {
			// After "Clear and execute plan", the agent may re-enter plan mode
			// and write to .claude/plans/ during execution. The dismissed plan
			// must not resurface when the turn completes.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			msgs := []agent.Message{
				// Original plan.
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"old plan"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
				// User clicked "Clear and execute plan".
				&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
				// Agent re-enters plan mode during execution.
				&agent.ToolUseMessage{ToolUseID: "tu3", Name: "EnterPlanMode"},
				&agent.ToolUseMessage{
					ToolUseID: "tu4", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"new plan"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu5", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			snap := tk.Snapshot()
			if snap.PlanContent != "" {
				t.Errorf("PlanContent = %q, want empty (plan written after context_cleared should be suppressed)", snap.PlanContent)
			}
			if tk.GetPlanFile() != "" {
				t.Errorf("PlanFile = %q, want empty", tk.GetPlanFile())
			}
		})
		t.Run("ContextClearedClearsExitPlanModePlanContent", func(t *testing.T) {
			// context_cleared in history must zero PlanContent on preceding
			// ExitPlanMode events so new subscribers see no stale plan.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			exitMsg1 := &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"}
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"old plan"}`),
				},
				exitMsg1,
				&agent.ResultMessage{MessageType: "result"},
				&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
				&agent.TextMessage{Text: "done"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			if exitMsg1.PlanContent != "" {
				t.Errorf("exitMsg1.PlanContent = %q, want empty (context_cleared should clear it)", exitMsg1.PlanContent)
			}
		})
		t.Run("PlanUpdateClearsPreviousExitPlanModePlanContent", func(t *testing.T) {
			// When a plan is updated (two ExitPlanMode without context_cleared),
			// only the latest ExitPlanMode should retain its PlanContent.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			exitMsg1 := &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"}
			exitMsg2 := &agent.ToolUseMessage{ToolUseID: "tu5", Name: "ExitPlanMode"}
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v1"}`),
				},
				exitMsg1,
				&agent.ResultMessage{MessageType: "result"},
				&agent.ToolUseMessage{
					ToolUseID: "tu3", Name: "EnterPlanMode",
				},
				&agent.ToolUseMessage{
					ToolUseID: "tu4", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v2"}`),
				},
				exitMsg2,
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			if exitMsg1.PlanContent != "" {
				t.Errorf("exitMsg1.PlanContent = %q, want empty (superseded by plan v2)", exitMsg1.PlanContent)
			}
			if exitMsg2.PlanContent != "plan v2" {
				t.Errorf("exitMsg2.PlanContent = %q, want %q", exitMsg2.PlanContent, "plan v2")
			}
		})
		t.Run("Subscribe", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			msgs := []agent.Message{
				&agent.TextMessage{Text: "msg1"},
				&agent.TextMessage{Text: "msg2"},
			}
			tk.RestoreMessages(msgs)

			// A subscriber should see restored messages in the history snapshot.
			history, _, unsub := tk.Subscribe(t.Context())
			defer unsub()

			if len(history) != 2 {
				t.Fatalf("history len = %d, want 2", len(history))
			}
		})
	})
}

func TestState(t *testing.T) {
	t.Run("String", func(t *testing.T) {
		for _, tt := range []struct {
			state State
			want  string
		}{
			{StatePending, "pending"},
			{StateBranching, "branching"},
			{StateProvisioning, "provisioning"},
			{StateStarting, "starting"},
			{StateRunning, "running"},
			{StateWaiting, "waiting"},
			{StateAsking, "asking"},
			{StateHasPlan, "has_plan"},
			{StatePulling, "pulling"},
			{StatePushing, "pushing"},
			{StatePurging, "purging"},
			{StateFailed, "failed"},
			{StatePurged, "purged"},
		} {
			if got := tt.state.String(); got != tt.want {
				t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
			}
		}
	})
	t.Run("SetStateIf", func(t *testing.T) {
		t.Run("Match", func(t *testing.T) {
			tk := &Task{}
			tk.SetState(StateRunning)
			if !tk.SetStateIf(StateRunning, StateWaiting) {
				t.Fatal("SetStateIf returned false when state matched")
			}
			if tk.GetState() != StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), StateWaiting)
			}
		})
		t.Run("Mismatch", func(t *testing.T) {
			tk := &Task{}
			tk.SetState(StateAsking)
			if tk.SetStateIf(StateRunning, StateWaiting) {
				t.Fatal("SetStateIf returned true when state did not match")
			}
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v (should be unchanged)", tk.GetState(), StateAsking)
			}
		})
	})
}
