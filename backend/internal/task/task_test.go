package task

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
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
				tk.addMessage(&agent.SystemMessage{MessageType: "system", Subtype: "status"})
			}
			// This send should trigger the slow-subscriber drop+close.
			tk.addMessage(&agent.SystemMessage{MessageType: "system", Subtype: "status"})

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
			msg2 := &agent.AssistantMessage{MessageType: "assistant"}
			tk.addMessage(msg1)
			tk.addMessage(msg2)

			history, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()
			_ = ch

			if len(history) != 2 {
				t.Fatalf("history len = %d, want 2", len(history))
			}
			if history[0].Type() != "system" {
				t.Errorf("history[0].Type() = %q, want %q", history[0].Type(), "system")
			}
			if history[1].Type() != "assistant" {
				t.Errorf("history[1].Type() = %q, want %q", history[1].Type(), "assistant")
			}
		})
		t.Run("ReplayLargeHistory", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			// Add more messages than any reasonable channel buffer to verify no deadlock.
			const n = 1000
			for range n {
				tk.addMessage(&agent.AssistantMessage{MessageType: "assistant"})
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
			tk.addMessage(&agent.SystemMessage{MessageType: "system", Subtype: "init"})

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
			tk.addMessage(&agent.AssistantMessage{MessageType: "assistant"})

			timeout := time.After(time.Second)
			for i, ch := range []<-chan agent.Message{ch1, ch2} {
				select {
				case msg := <-ch:
					if msg.Type() != "assistant" {
						t.Errorf("subscriber %d: type = %q, want %q", i, msg.Type(), "assistant")
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
			msg := &agent.AssistantMessage{MessageType: "assistant"}
			tk.addMessage(msg)

			timeout := time.After(time.Second)
			select {
			case got := <-ch:
				if got.Type() != "assistant" {
					t.Errorf("type = %q, want %q", got.Type(), "assistant")
				}
			case <-timeout:
				t.Fatal("timed out waiting for live message")
			}
		})
	})

	t.Run("SendInput", func(t *testing.T) {
		t.Run("NoSession", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateWaiting}
			err := tk.SendInput(agent.Prompt{Text: "hello"})
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
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateWaiting}
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
			err = tk.SendInput(agent.Prompt{Text: "hello"})
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
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			result := &agent.ResultMessage{MessageType: "result"}
			tk.addMessage(result)
			if tk.GetState() != StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), StateWaiting)
			}
		})
		t.Run("TransitionsToAsking", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			// Add an assistant message with an AskUserQuestion tool_use block.
			tk.addMessage(&agent.AssistantMessage{
				MessageType: "assistant",
				Message: agent.APIMessage{
					Content: []agent.ContentBlock{
						{Type: "tool_use", Name: "AskUserQuestion"},
					},
				},
			})
			// Now add a result message — should transition to StateAsking.
			tk.addMessage(&agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), StateAsking)
			}
		})
		t.Run("TransitionsToAskingWithPartialMessages", func(t *testing.T) {
			// With --include-partial-messages, Claude Code emits multiple
			// assistant snapshots per turn. AskUserQuestion appears in an
			// earlier snapshot while the final one is text-only. The state
			// machine must scan all assistant messages in the turn.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			tk.addMessage(&agent.AssistantMessage{
				MessageType: "assistant",
				Message: agent.APIMessage{
					Content: []agent.ContentBlock{
						{Type: "text", Text: "I need to ask you something."},
						{Type: "tool_use", Name: "AskUserQuestion"},
					},
				},
			})
			// Final partial snapshot: text-only, no tool_use.
			tk.addMessage(&agent.AssistantMessage{
				MessageType: "assistant",
				Message: agent.APIMessage{
					Content: []agent.ContentBlock{
						{Type: "text", Text: "I need to ask you something."},
					},
				},
			})
			tk.addMessage(&agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), StateAsking)
			}
		})
		t.Run("AssistantMessageTransitionsWaitingToRunning", func(t *testing.T) {
			// When the agent starts producing output while the task is
			// waiting (e.g. relay reconnect after server restart), the
			// state should transition back to running.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateWaiting}
			tk.addMessage(&agent.AssistantMessage{MessageType: "assistant"})
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
			}
		})
		t.Run("AssistantMessageTransitionsAskingToRunning", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateAsking}
			tk.addMessage(&agent.AssistantMessage{MessageType: "assistant"})
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
			}
		})
		t.Run("ResultTransitionsWaitingToAsking", func(t *testing.T) {
			// When watchSession sets Waiting before the ResultMessage is
			// processed, the ResultMessage should still detect
			// AskUserQuestion and correct the state to Asking.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			tk.addMessage(&agent.AssistantMessage{
				MessageType: "assistant",
				Message: agent.APIMessage{
					Content: []agent.ContentBlock{
						{Type: "tool_use", Name: "AskUserQuestion"},
					},
				},
			})
			// Simulate watchSession setting Waiting before ResultMessage
			// is processed by the dispatch goroutine.
			tk.SetState(StateWaiting)
			tk.addMessage(&agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), StateAsking)
			}
		})
		t.Run("NoTransitionForNonActiveStates", func(t *testing.T) {
			// AssistantMessages should NOT transition terminal or
			// setup states.
			for _, state := range []State{StatePending, StateBranching, StateProvisioning, StateStarting, StateTerminating, StateFailed, StateTerminated} {
				tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
				tk.SetState(state)
				tk.addMessage(&agent.AssistantMessage{MessageType: "assistant"})
				if tk.GetState() != state {
					t.Errorf("state %v changed to %v; want unchanged", state, tk.GetState())
				}
			}
		})
	})

	t.Run("addMessageDiffStat", func(t *testing.T) {
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
		ds := agent.DiffStat{
			{Path: "main.go", Added: 10, Deleted: 3},
			{Path: "img.png", Binary: true},
		}
		tk.addMessage(&agent.DiffStatMessage{
			MessageType: "caic_diff_stat",
			DiffStat:    ds,
		})
		got := tk.LiveDiffStat()
		if len(got) != 2 {
			t.Fatalf("LiveDiffStat len = %d, want 2", len(got))
		}
		if got[0].Path != "main.go" || got[0].Added != 10 {
			t.Errorf("LiveDiffStat[0] = %+v", got[0])
		}
		// Update with new diff stat.
		tk.addMessage(&agent.DiffStatMessage{
			MessageType: "caic_diff_stat",
			DiffStat:    agent.DiffStat{{Path: "new.go", Added: 1, Deleted: 0}},
		})
		got = tk.LiveDiffStat()
		if len(got) != 1 || got[0].Path != "new.go" {
			t.Errorf("LiveDiffStat after update = %+v", got)
		}
	})

	t.Run("RestoreMessagesDiffStat", func(t *testing.T) {
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateTerminated}
		tk.RestoreMessages([]agent.Message{
			&agent.DiffStatMessage{
				MessageType: "caic_diff_stat",
				DiffStat:    agent.DiffStat{{Path: "old.go", Added: 1}},
			},
			&agent.AssistantMessage{MessageType: "assistant"},
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

	t.Run("LiveUsageCumulative", func(t *testing.T) {
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
		tk.addMessage(&agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10},
		})
		tk.addMessage(&agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 200, OutputTokens: 80, CacheCreationInputTokens: 30},
		})
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
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateTerminated}
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{
				MessageType: "result",
				Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50},
			},
			&agent.AssistantMessage{MessageType: "assistant"},
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

	t.Run("RestoreMessages", func(t *testing.T) {
		t.Run("Basic", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			msgs := []agent.Message{
				&agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "sess-123"},
				&agent.AssistantMessage{MessageType: "assistant"},
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
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			msgs := []agent.Message{
				&agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "s1"},
				&agent.AssistantMessage{
					MessageType: "assistant",
					Message: agent.APIMessage{
						Content: []agent.ContentBlock{
							{Type: "tool_use", Name: "AskUserQuestion"},
						},
					},
				},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v (should infer asking from AskUserQuestion + ResultMessage)", tk.GetState(), StateAsking)
			}
		})
		t.Run("SkipsTrailingDiffStat", func(t *testing.T) {
			// The relay emits DiffStatMessage after the ResultMessage.
			// RestoreMessages should skip it and still infer Waiting.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			msgs := []agent.Message{
				&agent.AssistantMessage{MessageType: "assistant"},
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
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			msgs := []agent.Message{
				&agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "s1"},
				&agent.AssistantMessage{MessageType: "assistant"},
			}
			tk.RestoreMessages(msgs)
			// No trailing ResultMessage → agent was still producing output.
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v (no ResultMessage → still running)", tk.GetState(), StateRunning)
			}
		})
		t.Run("TerminalStatePreserved", func(t *testing.T) {
			for _, state := range []State{StateTerminated, StateFailed, StateTerminating} {
				tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
				tk.SetState(state)
				msgs := []agent.Message{
					&agent.AssistantMessage{MessageType: "assistant"},
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
				&agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "old"},
				&agent.AssistantMessage{MessageType: "assistant"},
				&agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "new"},
			}
			tk.RestoreMessages(msgs)

			if tk.GetSessionID() != "new" {
				t.Errorf("SessionID = %q, want %q", tk.GetSessionID(), "new")
			}
		})
		t.Run("RestoresPlanFile", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			msgs := []agent.Message{
				&agent.AssistantMessage{
					MessageType: "assistant",
					Message: agent.APIMessage{
						Content: []agent.ContentBlock{
							{Type: "tool_use", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/my-plan.md","content":"plan"}`)},
						},
					},
				},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)
			if tk.GetPlanFile() != "/home/user/.claude/plans/my-plan.md" {
				t.Errorf("PlanFile = %q, want %q", tk.GetPlanFile(), "/home/user/.claude/plans/my-plan.md")
			}
		})
		t.Run("RestoresInPlanMode", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			msgs := []agent.Message{
				&agent.AssistantMessage{
					MessageType: "assistant",
					Message: agent.APIMessage{
						Content: []agent.ContentBlock{
							{Type: "tool_use", Name: "EnterPlanMode"},
						},
					},
				},
				&agent.AssistantMessage{
					MessageType: "assistant",
					Message: agent.APIMessage{
						Content: []agent.ContentBlock{
							{Type: "tool_use", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/foo.md","content":"x"}`)},
							{Type: "tool_use", Name: "ExitPlanMode"},
						},
					},
				},
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
			tk2 := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning}
			tk2.RestoreMessages(msgs[:1])
			if !tk2.Snapshot().InPlanMode {
				t.Error("InPlanMode = false, want true (only EnterPlanMode seen)")
			}
		})
		t.Run("Subscribe", func(t *testing.T) {
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			msgs := []agent.Message{
				&agent.AssistantMessage{MessageType: "assistant"},
				&agent.AssistantMessage{MessageType: "assistant"},
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
			{StatePulling, "pulling"},
			{StatePushing, "pushing"},
			{StateTerminating, "terminating"},
			{StateFailed, "failed"},
			{StateTerminated, "terminated"},
		} {
			if got := tt.state.String(); got != tt.want {
				t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
			}
		}
	})
	t.Run("SetStateIf", func(t *testing.T) {
		t.Run("Match", func(t *testing.T) {
			tk := &Task{state: StateRunning}
			if !tk.SetStateIf(StateRunning, StateWaiting) {
				t.Fatal("SetStateIf returned false when state matched")
			}
			if tk.GetState() != StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), StateWaiting)
			}
		})
		t.Run("Mismatch", func(t *testing.T) {
			tk := &Task{state: StateAsking}
			if tk.SetStateIf(StateRunning, StateWaiting) {
				t.Fatal("SetStateIf returned true when state did not match")
			}
			if tk.GetState() != StateAsking {
				t.Errorf("state = %v, want %v (should be unchanged)", tk.GetState(), StateAsking)
			}
		})
	})
}
