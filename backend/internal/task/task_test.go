package task

import (
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
)

func TestTask(t *testing.T) {
	t.Run("Subscribe", func(t *testing.T) {
		t.Run("Replay", func(t *testing.T) {
			tk := &Task{Prompt: "test"}
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
			tk := &Task{Prompt: "test"}
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
			tk := &Task{Prompt: "test"}
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
			tk := &Task{Prompt: "test"}

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
			tk := &Task{Prompt: "test"}
			err := tk.SendInput("hello")
			if err == nil {
				t.Error("expected error when no session is active")
			}
		})
		t.Run("DeadSessionCleared", func(t *testing.T) {
			// Simulate a session that has already finished (e.g. relay
			// subprocess exited). SendInput should detect it and return
			// "no active session" without changing state.
			tk := &Task{Prompt: "test", State: StateWaiting}
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
			s := agent.NewSession(cmd, stdin, stdout, nil, nil, &testWire{})
			<-s.Done()
			tk.session = s
			err = tk.SendInput("hello")
			if err == nil {
				t.Error("expected error for dead session")
			}
			if tk.State != StateWaiting {
				t.Errorf("state = %v, want %v", tk.State, StateWaiting)
			}
			tk.mu.Lock()
			cleared := tk.session == nil
			tk.mu.Unlock()
			if !cleared {
				t.Error("dead session was not cleared")
			}
		})
	})

	t.Run("Terminate", func(t *testing.T) {
		t.Run("ClosesAndIdempotent", func(t *testing.T) {
			tk := &Task{Prompt: "test"}
			tk.InitDoneCh()

			// Done should not be closed yet.
			select {
			case <-tk.Done():
				t.Fatal("doneCh closed prematurely")
			default:
			}

			tk.Terminate()

			// Done should be closed now.
			select {
			case <-tk.Done():
			default:
				t.Fatal("doneCh not closed after Terminate")
			}

			// Idempotent.
			tk.Terminate()
		})
	})

	t.Run("addMessage", func(t *testing.T) {
		t.Run("TransitionsToWaiting", func(t *testing.T) {
			tk := &Task{Prompt: "test", State: StateRunning}
			result := &agent.ResultMessage{MessageType: "result"}
			tk.addMessage(result)
			if tk.State != StateWaiting {
				t.Errorf("state = %v, want %v", tk.State, StateWaiting)
			}
		})
		t.Run("TransitionsToAsking", func(t *testing.T) {
			tk := &Task{Prompt: "test", State: StateRunning}
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
			if tk.State != StateAsking {
				t.Errorf("state = %v, want %v", tk.State, StateAsking)
			}
		})
	})

	t.Run("LiveUsageCumulative", func(t *testing.T) {
		tk := &Task{Prompt: "test", State: StateRunning}
		tk.addMessage(&agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10},
		})
		tk.addMessage(&agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 200, OutputTokens: 80, CacheCreationInputTokens: 30},
		})
		_, _, _, usage := tk.LiveStats()
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
	})

	t.Run("RestoreMessagesUsageCumulative", func(t *testing.T) {
		tk := &Task{Prompt: "test", State: StateTerminated}
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
		_, _, _, usage := tk.LiveStats()
		if usage.InputTokens != 300 {
			t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
		}
		if usage.OutputTokens != 130 {
			t.Errorf("OutputTokens = %d, want 130", usage.OutputTokens)
		}
	})

	t.Run("RestoreMessages", func(t *testing.T) {
		t.Run("Basic", func(t *testing.T) {
			tk := &Task{Prompt: "test", State: StateRunning}
			msgs := []agent.Message{
				&agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "sess-123"},
				&agent.AssistantMessage{MessageType: "assistant"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)

			if len(tk.Messages()) != 3 {
				t.Fatalf("Messages() len = %d, want 3", len(tk.Messages()))
			}
			if tk.SessionID != "sess-123" {
				t.Errorf("SessionID = %q, want %q", tk.SessionID, "sess-123")
			}
			if tk.State != StateWaiting {
				t.Errorf("state = %v, want %v (should infer waiting from trailing ResultMessage)", tk.State, StateWaiting)
			}
		})
		t.Run("InfersAsking", func(t *testing.T) {
			tk := &Task{Prompt: "test", State: StateRunning}
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
			if tk.State != StateAsking {
				t.Errorf("state = %v, want %v (should infer asking from AskUserQuestion + ResultMessage)", tk.State, StateAsking)
			}
		})
		t.Run("NoResultKeepsState", func(t *testing.T) {
			tk := &Task{Prompt: "test", State: StateRunning}
			msgs := []agent.Message{
				&agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "s1"},
				&agent.AssistantMessage{MessageType: "assistant"},
			}
			tk.RestoreMessages(msgs)
			// No trailing ResultMessage → agent was still producing output.
			if tk.State != StateRunning {
				t.Errorf("state = %v, want %v (no ResultMessage → still running)", tk.State, StateRunning)
			}
		})
		t.Run("TerminalStatePreserved", func(t *testing.T) {
			for _, state := range []State{StateTerminated, StateFailed, StateTerminating} {
				tk := &Task{Prompt: "test", State: state}
				msgs := []agent.Message{
					&agent.AssistantMessage{MessageType: "assistant"},
					&agent.ResultMessage{MessageType: "result"},
				}
				tk.RestoreMessages(msgs)
				if tk.State != state {
					t.Errorf("state = %v, want %v (terminal state must not be overridden)", tk.State, state)
				}
			}
		})
		t.Run("UsesLastSessionID", func(t *testing.T) {
			tk := &Task{Prompt: "test"}
			msgs := []agent.Message{
				&agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "old"},
				&agent.AssistantMessage{MessageType: "assistant"},
				&agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "new"},
			}
			tk.RestoreMessages(msgs)

			if tk.SessionID != "new" {
				t.Errorf("SessionID = %q, want %q", tk.SessionID, "new")
			}
		})
		t.Run("RestoresPlanFile", func(t *testing.T) {
			tk := &Task{Prompt: "test", State: StateRunning}
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
			if tk.PlanFile != "/home/user/.claude/plans/my-plan.md" {
				t.Errorf("PlanFile = %q, want %q", tk.PlanFile, "/home/user/.claude/plans/my-plan.md")
			}
		})
		t.Run("RestoresInPlanMode", func(t *testing.T) {
			tk := &Task{Prompt: "test", State: StateRunning}
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
			if tk.InPlanMode {
				t.Error("InPlanMode = true, want false (ExitPlanMode should clear it)")
			}
			if tk.PlanFile != "/home/user/.claude/plans/foo.md" {
				t.Errorf("PlanFile = %q, want %q", tk.PlanFile, "/home/user/.claude/plans/foo.md")
			}

			// Without ExitPlanMode, should stay in plan mode.
			tk2 := &Task{Prompt: "test", State: StateRunning}
			tk2.RestoreMessages(msgs[:1])
			if !tk2.InPlanMode {
				t.Error("InPlanMode = false, want true (only EnterPlanMode seen)")
			}
		})
		t.Run("Subscribe", func(t *testing.T) {
			tk := &Task{Prompt: "test"}
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
}
