// Shared test helpers and fixtures for the task package.

package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

type failingConn struct {
	err error
}

func (c *failingConn) SendPrompt(agent.Prompt) error { return c.err }

func (*failingConn) SendRaw([]byte) error { return nil }

func (*failingConn) SendCompact(string) error { return nil }

func (*failingConn) ReadMessages(io.Reader, chan<- agent.Message) error { return nil }

func (*failingConn) SendStop(context.Context) {}

func (*failingConn) Close() error { return nil }

func TestTask(t *testing.T) {
	t.Parallel()
	t.Run("ConcurrentMetadataSnapshots", func(t *testing.T) {
		t.Parallel()
		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
			Repos: []RepoMount{
				{Name: "org/repo", Branch: "main"},
				{Name: "org/extra", Branch: "main"},
			},
			Tailscale:   true,
			Sudo:        true,
			GitHubToken: true,
		}

		const iterations = 1000
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, write := range []func(int){
			func(i int) { tk.SetRepoBranch(0, "task-"+strconv.Itoa(i)) },
			func(i int) { tk.SetRepoBranch(1, "extra-"+strconv.Itoa(i)) },
			func(i int) {
				id := runtime.InstanceID("ctr-" + strconv.Itoa(i))
				tk.SetRuntimeConnectionInfo(id, runtime.ConnectionTarget{SSHHost: string(id)}, "host.example", "https://auth.example", 5900+i)
			},
			func(i int) { tk.SetVNCPort(6000 + i) },
			func(i int) { tk.SetRelayOffset(int64(i)) },
			func(i int) { tk.SetSudoPassword("pw-" + strconv.Itoa(i)) },
		} {
			wg.Go(func() {
				<-start
				for i := range iterations {
					write(i)
				}
			})
		}
		for range 4 {
			wg.Go(func() {
				<-start
				for range iterations {
					snap := tk.Snapshot()
					if len(snap.Repos) > 0 {
						snap.Repos[0].Branch = "mutated"
					}
					_ = tk.Primary()
					_ = tk.RuntimeRepos()
					_ = tk.ExtraRuntimeRepos()
					_ = tk.RuntimeInstanceID()
					_ = tk.RelayOffsetValue()
					_, _, _ = tk.SudoLookupState()
					_ = tk.GitHubTokenEnabled()
				}
			})
		}

		close(start)
		wg.Wait()

		repos := tk.ReposSnapshot()
		if len(repos) == 0 {
			t.Fatal("ReposSnapshot returned no repos")
		}
		if repos[0].Branch == "mutated" {
			t.Fatal("Snapshot exposed mutable repo storage")
		}
	})
	t.Run("RestoreMessagesCapturesExitError", func(t *testing.T) {
		t.Parallel()
		tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
		tk.RestoreMessages([]agent.Message{&agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}})
		if got := tk.LastExitError(); got != "Unknown option: --approve" {
			t.Errorf("LastExitError = %q, want relay stderr", got)
		}
	})
	t.Run("RecordSessionFailure", func(t *testing.T) {
		t.Parallel()
		tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StateStarting)
		tk.addMessage(t.Context(), &agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}, true)

		if !tk.RecordSessionFailure(t.Context(), errors.New("agent exited: exit status 2")) {
			t.Fatal("RecordSessionFailure returned false")
		}
		if got := tk.GetState(); got != StateFailed {
			t.Errorf("state = %v, want %v", got, StateFailed)
		}
		if got := tk.LastAgentResult(); !strings.Contains(got, "Unknown option: --approve") {
			t.Errorf("LastAgentResult = %q, want relay stderr", got)
		}
	})
	t.Run("Subscribe", func(t *testing.T) {
		t.Parallel()
		t.Run("SlowSubscriberThenCancel", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
		t.Parallel()
		t.Run("error_delivery_failure_preserves_waiting_state", func(t *testing.T) {
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateWaiting)

			cmdCtx, cmdCancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(cmdCtx, "sleep", "60")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			sendErr := errors.New("delivery failed")
			s := agent.NewSession(cmd, &failingConn{err: sendErr}, stdout, make(chan agent.Message, 256), nil)
			t.Cleanup(func() {
				cmdCancel()
				_ = s.Wait()
			})
			tk.AttachSession(&SessionHandle{Session: s})

			err = tk.SendInput(t.Context(), agent.Prompt{Text: "follow up"})
			if !errors.Is(err, sendErr) {
				t.Fatalf("SendInput err = %v, want %v", err, sendErr)
			}
			if got := tk.GetState(); got != StateWaiting {
				t.Errorf("state = %s, want %s", got, StateWaiting)
			}
			if msgs := tk.Messages(); len(msgs) != 0 {
				t.Fatalf("messages = %d, want 0", len(msgs))
			}
		})
		t.Run("PreservesPlanContent", func(t *testing.T) {
			t.Parallel()
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
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cmdCancel()
			cmd := exec.CommandContext(cmdCtx, "cat")
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
			s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.Message, 256), nil)
			tk.AttachSession(&SessionHandle{Session: s})
			defer func() { _ = stdin.Close(); _ = s.Wait() }()

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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cmdCancel()
			cmd := exec.CommandContext(cmdCtx, "cat")
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
			s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.Message, 256), nil)
			tk.AttachSession(&SessionHandle{Session: s})
			defer func() { _ = stdin.Close(); _ = s.Wait() }()

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
			t.Parallel()
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
			t.Parallel()
			// Simulate a session that has already finished (e.g. relay
			// subprocess exited). SendInput should detect it and return
			// "no active session" without changing state.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateWaiting)
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cmdCancel()
			cmd := exec.CommandContext(cmdCtx, "true")
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
			s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.Message, 256), nil)
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
		t.Parallel()
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		if tk.SessionDone() != nil {
			t.Error("SessionDone() should be nil when no session attached")
		}
		if tk.DetachSession() != nil {
			t.Error("DetachSession() should return nil when no session attached")
		}

		cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cmdCancel()
		cmd := exec.CommandContext(cmdCtx, "cat")
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.Message, 256), nil)
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

		// Cleanup: close stdin so the process exits, then wait via Session
		// (which owns cmd.Wait) to avoid a double-Wait race.
		_ = stdin.Close()
		_ = s.Wait()
	})

	t.Run("addMessage", func(t *testing.T) {
		t.Parallel()
		t.Run("TransitionsToWaiting", func(t *testing.T) {
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			result := &agent.ResultMessage{MessageType: "result"}
			tk.addMessage(t.Context(), result, false)
			if tk.GetState() != StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), StateWaiting)
			}
		})
		t.Run("TransitionsToAsking", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateAsking)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{ToolUseID: "tu1", Name: "Read"}, false)
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
			}
		})
		t.Run("ResultTransitionsWaitingToAsking", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
			// TextMessage while HasPlan → Running.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateHasPlan)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "output"}, false)
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
			}
		})
		t.Run("TextMessageTransitionsStartingToRunning", func(t *testing.T) {
			t.Parallel()
			// When the agent subprocess produces output before
			// Runner.Start calls SetState(Running), StateStarting
			// must transition to Running so the subsequent
			// ResultMessage can transition further.
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateStarting)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "output"}, false)
			if tk.GetState() != StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
			}
		})
		t.Run("EmptyResultUsesTurnText", func(t *testing.T) {
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "I checked the files."}, false)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{ToolUseID: "tu1", Name: "Read"}, false)
			tk.addMessage(t.Context(), &agent.ToolResultMessage{ToolUseID: "tu1"}, false)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "There is nothing to change."}, false)

			result := &agent.ResultMessage{MessageType: "result"}
			tk.addMessage(t.Context(), result, false)
			if result.Result != "There is nothing to change." {
				t.Errorf("Result = %q", result.Result)
			}
			if tk.LastAgentResult() != result.Result {
				t.Errorf("LastAgentResult = %q, want %q", tk.LastAgentResult(), result.Result)
			}
		})
		t.Run("EmptyResultStopsAtThinking", func(t *testing.T) {
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "before thinking"}, false)
			tk.addMessage(t.Context(), &agent.ThinkingMessage{Text: "private chain"}, false)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "after thinking"}, false)

			result := &agent.ResultMessage{MessageType: "result"}
			tk.addMessage(t.Context(), result, false)
			if result.Result != "after thinking" {
				t.Errorf("Result = %q", result.Result)
			}
		})
		t.Run("NonEmptyResultIsPreserved", func(t *testing.T) {
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "intermediate"}, false)

			result := &agent.ResultMessage{MessageType: "result", Result: "final"}
			tk.addMessage(t.Context(), result, false)
			if result.Result != "final" {
				t.Errorf("Result = %q, want final", result.Result)
			}
		})
		t.Run("NoTransitionForNonActiveStates", func(t *testing.T) {
			t.Parallel()
			// TextMessages should NOT transition terminal or
			// setup states (except StateStarting, which is
			// tested separately above).
			for _, state := range []State{StatePending, StateBranching, StateProvisioning, StatePurging, StateFailed, StatePurged} {
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
		t.Parallel()
		t.Run("DiffStatMessage", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
		t.Parallel()
		t.Run("DiffStatMessage", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
		t.Parallel()
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StateRunning)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10, ReasoningOutputTokens: 7},
		}, false)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 200, OutputTokens: 80, CacheCreationInputTokens: 30, ReasoningOutputTokens: 11},
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
		if usage.ReasoningOutputTokens != 18 {
			t.Errorf("ReasoningOutputTokens = %d, want 18", usage.ReasoningOutputTokens)
		}
		// lastUsage should reflect only the most recent ResultMessage.
		if lastUsage.InputTokens != 200 {
			t.Errorf("lastUsage.InputTokens = %d, want 200", lastUsage.InputTokens)
		}
		if lastUsage.CacheCreationInputTokens != 30 {
			t.Errorf("lastUsage.CacheCreationInputTokens = %d, want 30", lastUsage.CacheCreationInputTokens)
		}
		if lastUsage.ReasoningOutputTokens != 11 {
			t.Errorf("lastUsage.ReasoningOutputTokens = %d, want 11", lastUsage.ReasoningOutputTokens)
		}
	})

	t.Run("RestoreMessagesUsageCumulative", func(t *testing.T) {
		t.Parallel()
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(StatePurged)
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{
				MessageType: "result",
				Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50, ReasoningOutputTokens: 7},
			},
			&agent.TextMessage{Text: "hello"},
			&agent.ResultMessage{
				MessageType: "result",
				Usage:       agent.Usage{InputTokens: 200, OutputTokens: 80, ReasoningOutputTokens: 11},
			},
		})
		_, _, _, usage, lastUsage := tk.LiveStats()
		if usage.InputTokens != 300 {
			t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
		}
		if usage.OutputTokens != 130 {
			t.Errorf("OutputTokens = %d, want 130", usage.OutputTokens)
		}
		if usage.ReasoningOutputTokens != 18 {
			t.Errorf("ReasoningOutputTokens = %d, want 18", usage.ReasoningOutputTokens)
		}
		// lastUsage should reflect only the last ResultMessage.
		if lastUsage.InputTokens != 200 {
			t.Errorf("lastUsage.InputTokens = %d, want 200", lastUsage.InputTokens)
		}
		if lastUsage.OutputTokens != 80 {
			t.Errorf("lastUsage.OutputTokens = %d, want 80", lastUsage.OutputTokens)
		}
		if lastUsage.ReasoningOutputTokens != 11 {
			t.Errorf("lastUsage.ReasoningOutputTokens = %d, want 11", lastUsage.ReasoningOutputTokens)
		}
	})

	t.Run("LiveCostCumulativeAcrossSessions", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
		t.Parallel()
		t.Run("ResetsPlanState", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
		t.Parallel()
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
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
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			msgs := []agent.Message{
				&agent.TextMessage{Text: "msg1"},
				&agent.TextMessage{Text: "msg2"},
			}
			tk.RestoreMessages(msgs)

			// A subscriber should see restored messages in the history snapshot.
			history, _, unsub := tk.Subscribe(t.Context())
			t.Cleanup(unsub)

			if len(history) != 2 {
				t.Fatalf("history len = %d, want 2", len(history))
			}
		})
		t.Run("EmptyResultUsesRestoredTurnText", func(t *testing.T) {
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			msgs := []agent.Message{
				&agent.UserInputMessage{Text: "first"},
				&agent.TextMessage{Text: "first result"},
				&agent.ResultMessage{MessageType: "result", Result: "first result"},
				&agent.UserInputMessage{Text: "second"},
				&agent.TextMessage{Text: "status update"},
				&agent.ToolUseMessage{ToolUseID: "tu1", Name: "Read"},
				&agent.ToolResultMessage{ToolUseID: "tu1"},
				&agent.TextMessage{Text: "done"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)

			rm, ok := msgs[len(msgs)-1].(*agent.ResultMessage)
			if !ok {
				t.Fatalf("last message type = %T, want *agent.ResultMessage", msgs[len(msgs)-1])
			}
			if rm.Result != "done" {
				t.Errorf("Result = %q", rm.Result)
			}
		})
		t.Run("EmptyResultStopsAtRestoredThinking", func(t *testing.T) {
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			msgs := []agent.Message{
				&agent.UserInputMessage{Text: "test"},
				&agent.TextMessage{Text: "before thinking"},
				&agent.ThinkingDeltaMessage{Text: "reasoning"},
				&agent.TextDeltaMessage{Text: "after "},
				&agent.TextDeltaMessage{Text: "thinking"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.RestoreMessages(msgs)

			rm, ok := msgs[len(msgs)-1].(*agent.ResultMessage)
			if !ok {
				t.Fatalf("last message type = %T, want *agent.ResultMessage", msgs[len(msgs)-1])
			}
			if rm.Result != "after thinking" {
				t.Errorf("Result = %q", rm.Result)
			}
		})
	})

	t.Run("ExtraRuntimeRepos", func(t *testing.T) {
		t.Parallel()
		t.Run("NoRepos", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			if extra := tk.ExtraRuntimeRepos(); extra != nil {
				t.Fatalf("ExtraRuntimeRepos with no repos = %+v, want nil", extra)
			}
		})
		t.Run("OneRepo", func(t *testing.T) {
			t.Parallel()
			tk := &Task{Repos: []RepoMount{{Name: "a/b", Branch: "caic-0", GitRoot: "/foo"}}}
			if extra := tk.ExtraRuntimeRepos(); extra != nil {
				t.Fatalf("ExtraRuntimeRepos with one repo = %+v, want nil", extra)
			}
		})
		t.Run("MultipleRepos", func(t *testing.T) {
			t.Parallel()
			tk := &Task{Repos: []RepoMount{
				{Name: "a/b", Branch: "caic-0", GitRoot: "/foo"},
				{Name: "c/d", Branch: "caic-1", GitRoot: "/bar"},
			}}
			extra := tk.ExtraRuntimeRepos()
			if len(extra) != 1 {
				t.Fatalf("ExtraRuntimeRepos len = %d, want 1", len(extra))
			}
			if extra[0].Branch != "caic-1" {
				t.Errorf("extra[0].Branch = %q, want %q", extra[0].Branch, "caic-1")
			}
		})
	})

	t.Run("SetStateAt", func(t *testing.T) {
		t.Parallel()
		tk := &Task{}
		now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		tk.SetStateAt(StateRunning, now)
		if tk.GetState() != StateRunning {
			t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
		}
		snap := tk.Snapshot()
		if !snap.StateUpdatedAt.Equal(now) {
			t.Errorf("StateUpdatedAt = %v, want %v", snap.StateUpdatedAt, now)
		}
		if !snap.TurnStartedAt.IsZero() {
			t.Error("TurnStartedAt should be zero when SetStateAt is used")
		}
	})

	t.Run("SetTurnStartedAt", func(t *testing.T) {
		t.Parallel()
		t.Run("Running", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			tk.SetState(StateRunning)
			now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
			tk.SetTurnStartedAt(now)
			if snap := tk.Snapshot(); !snap.TurnStartedAt.Equal(now) {
				t.Errorf("TurnStartedAt = %v, want %v", snap.TurnStartedAt, now)
			}
		})
		t.Run("NonRunning", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			tk.SetState(StateWaiting)
			now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
			tk.SetTurnStartedAt(now)
			if snap := tk.Snapshot(); !snap.TurnStartedAt.IsZero() {
				t.Errorf("TurnStartedAt = %v, want zero when non-running", snap.TurnStartedAt)
			}
		})
	})

	t.Run("GetModel", func(t *testing.T) {
		t.Parallel()
		t.Run("FallbackToModel", func(t *testing.T) {
			t.Parallel()
			tk := &Task{Model: "gpt-4"}
			if got := tk.GetModel(); got != "gpt-4" {
				t.Errorf("GetModel = %q, want %q", got, "gpt-4")
			}
		})
		t.Run("UsesReportedModel", func(t *testing.T) {
			t.Parallel()
			tk := &Task{Model: "gpt-4"}
			tk.addMessage(t.Context(), &agent.InitMessage{SessionID: "s1", Model: "claude-3-opus"}, false)
			if got := tk.GetModel(); got != "claude-3-opus" {
				t.Errorf("GetModel = %q, want %q", got, "claude-3-opus")
			}
		})
	})

	t.Run("SetPR", func(t *testing.T) {
		t.Parallel()
		tk := &Task{}
		tk.SetPR("octocat", "hello-world", 42)
		if got := tk.GetPR(); got != 42 {
			t.Errorf("GetPR = %d, want 42", got)
		}
		snap := tk.Snapshot()
		if snap.ForgeOwner != "octocat" {
			t.Errorf("ForgeOwner = %q, want %q", snap.ForgeOwner, "octocat")
		}
		if snap.ForgeRepo != "hello-world" {
			t.Errorf("ForgeRepo = %q, want %q", snap.ForgeRepo, "hello-world")
		}
		if snap.ForgePR != 42 {
			t.Errorf("ForgePR = %d, want 42", snap.ForgePR)
		}
		if snap.ForgePRState != forge.PRStateOpen {
			t.Errorf("ForgePRState = %q, want %q", snap.ForgePRState, forge.PRStateOpen)
		}
	})

	t.Run("SetPRState", func(t *testing.T) {
		t.Parallel()
		tk := &Task{}
		tk.SetPR("octocat", "hello-world", 42)
		tk.SetPRState(forge.PRStateClosed)
		snap := tk.Snapshot()
		if snap.ForgePRState != forge.PRStateClosed {
			t.Errorf("ForgePRState = %q, want %q", snap.ForgePRState, forge.PRStateClosed)
		}
	})

	t.Run("SetCIStatus", func(t *testing.T) {
		t.Parallel()
		tk := &Task{}
		checks := []forge.Check{{Name: "lint", Status: "success"}}
		tk.SetCIStatus(forge.CIStatusPending, checks)
		snap := tk.Snapshot()
		if snap.CIStatus != forge.CIStatusPending {
			t.Errorf("CIStatus = %v, want %v", snap.CIStatus, forge.CIStatusPending)
		}
		if len(snap.CIChecks) != 1 || snap.CIChecks[0].Name != "lint" {
			t.Errorf("CIChecks = %+v", snap.CIChecks)
		}
	})

	t.Run("SetTitle", func(t *testing.T) {
		t.Parallel()
		t.Run("SetsTitle", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			tk.SetTitle("hello")
			if tk.Title() != "hello" {
				t.Errorf("Title = %q, want %q", tk.Title(), "hello")
			}
		})
		t.Run("EmptyIgnored", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			tk.SetTitle("first")
			tk.SetTitle("")
			if tk.Title() != "first" {
				t.Errorf("Title = %q, want %q (empty string should be ignored)", tk.Title(), "first")
			}
		})
	})

	t.Run("SetAgentVersion", func(t *testing.T) {
		t.Parallel()
		tk := &Task{}
		tk.SetAgentVersion("2.0.0")
		snap := tk.Snapshot()
		if snap.AgentVersion != "2.0.0" {
			t.Errorf("AgentVersion = %q, want %q", snap.AgentVersion, "2.0.0")
		}
	})
	t.Run("SetSessionMetadata", func(t *testing.T) {
		t.Parallel()
		tk := &Task{Model: "requested"}
		tk.SetSessionMetadata("session-1", "reported", "2.0.0")
		if got := tk.GetSessionID(); got != "session-1" {
			t.Errorf("SessionID = %q, want session-1", got)
		}
		snap := tk.Snapshot()
		if snap.Model != "reported" {
			t.Errorf("Model = %q, want reported", snap.Model)
		}
		if snap.AgentVersion != "2.0.0" {
			t.Errorf("AgentVersion = %q, want 2.0.0", snap.AgentVersion)
		}
	})

	t.Run("WriteToLog", func(t *testing.T) {
		t.Parallel()
		t.Run("NoSession", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			err := tk.WriteToLog(&agent.TextMessage{Text: "hello"})
			if !errors.Is(err, ErrNoLog) {
				t.Fatalf("WriteToLog err = %v, want ErrNoLog", err)
			}
		})
		t.Run("WithSession", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			var buf bytes.Buffer
			h := &SessionHandle{LogW: nopCloser{&buf}}
			tk.AttachSession(h)
			if err := tk.WriteToLog(&agent.TextMessage{Text: "hello"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), `"text"`) {
				t.Errorf("log buffer = %q, want text message", buf.String())
			}
		})
		t.Run("ReopensPersistedLogWithoutSession", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tk := &Task{ID: ksid.NewID()}
			path := filepath.Join(dir, "task.jsonl")
			if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
				t.Fatal(err)
			}
			tk.SetLogPath(path)

			if err := tk.WriteToLog(&agent.MetaPRMessage{
				MessageType: "caic_pr",
				ForgeOwner:  "acme",
				ForgeRepo:   "widget",
				ForgePR:     42,
			}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"type":"caic_pr"`) {
				t.Fatalf("log = %q, want caic_pr", string(data))
			}
		})
		t.Run("ReturnsAppendError", func(t *testing.T) {
			t.Parallel()
			tk := &Task{ID: ksid.NewID()}
			tk.SetLogPath(filepath.Join(t.TempDir(), "missing", "task.jsonl"))
			if err := tk.WriteToLog(&agent.TextMessage{Text: "hello"}); err == nil {
				t.Fatal("WriteToLog err = nil, want append error")
			}
		})
	})

	t.Run("PushStats", func(t *testing.T) {
		t.Parallel()
		t.Run("SubscribeStats", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			tk.PushStats(&runtime.Stats{CPUPerc: 50.0, MemUsed: 1024})
			tk.PushStats(&runtime.Stats{CPUPerc: 75.0, MemUsed: 2048})

			ctx := t.Context()
			history, live, unsub := tk.SubscribeStats(ctx)
			t.Cleanup(unsub)

			if len(history) != 2 {
				t.Fatalf("history len = %d, want 2", len(history))
			}
			if history[0].CPUPerc != 50.0 {
				t.Errorf("history[0].CPUPerc = %v, want 50.0", history[0].CPUPerc)
			}
			if history[1].CPUPerc != 75.0 {
				t.Errorf("history[1].CPUPerc = %v, want 75.0", history[1].CPUPerc)
			}

			tk.PushStats(&runtime.Stats{CPUPerc: 100.0, MemUsed: 4096})
			timeout := time.After(time.Second)
			select {
			case s := <-live:
				if s.CPUPerc != 100.0 {
					t.Errorf("live.CPUPerc = %v, want 100.0", s.CPUPerc)
				}
			case <-timeout:
				t.Fatal("timed out waiting for live stat")
			}
		})

		t.Run("RingOverflow", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			for i := range 65 {
				tk.PushStats(&runtime.Stats{CPUPerc: float64(i)})
			}
			ctx := t.Context()
			history, _, unsub := tk.SubscribeStats(ctx)
			t.Cleanup(unsub)

			if len(history) != statsRingSize {
				t.Errorf("history len = %d, want %d (ring capped at size)", len(history), statsRingSize)
			}
			if history[0].CPUPerc != 5.0 {
				t.Errorf("history[0].CPUPerc = %v, want 5.0 (oldest should be off by delta)", history[0].CPUPerc)
			}
			if history[len(history)-1].CPUPerc != 64.0 {
				t.Errorf("history[last].CPUPerc = %v, want 64.0", history[len(history)-1].CPUPerc)
			}
		})
	})

	t.Run("SendCompact", func(t *testing.T) {
		t.Parallel()
		t.Run("NoSession", func(t *testing.T) {
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateWaiting)
			err := tk.SendCompact(t.Context(), "compact now")
			if err == nil {
				t.Fatal("expected error when no session is active")
			}
			if !strings.Contains(err.Error(), "session="+string(SessionNone)) {
				t.Errorf("error = %q, want session=%s", err.Error(), SessionNone)
			}
		})
		t.Run("DeadSession", func(t *testing.T) {
			t.Parallel()
			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateWaiting)
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			t.Cleanup(cmdCancel)
			cmd := exec.CommandContext(cmdCtx, "true")
			stdin, _ := cmd.StdinPipe()
			stdout, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.Message, 256), nil)
			<-s.Done()
			tk.AttachSession(&SessionHandle{Session: s})
			err := tk.SendCompact(t.Context(), "compact now")
			if err == nil {
				t.Fatal("expected error for dead session")
			}
			if !strings.Contains(err.Error(), "session="+string(SessionExited)) {
				t.Errorf("error = %q, want session=%s", err.Error(), SessionExited)
			}
		})
	})

	t.Run("SyntheticUserInput", func(t *testing.T) {
		t.Parallel()
		img := agent.ImageData{Data: "base64data", MediaType: "image/png"}
		prompt := agent.Prompt{Text: "look at this", Images: []agent.ImageData{img}}
		msg := syntheticUserInput(prompt)
		if msg.Text != "look at this" {
			t.Errorf("Text = %q, want %q", msg.Text, "look at this")
		}
		if len(msg.Images) != 1 {
			t.Fatalf("Images len = %d, want 1", len(msg.Images))
		}
		if msg.Images[0].Data != "base64data" {
			t.Errorf("Images[0].Data = %q, want %q", msg.Images[0].Data, "base64data")
		}
		prompt.Images[0].Data = "modified"
		if msg.Images[0].Data != "base64data" {
			t.Error("message images not independent copy")
		}
	})

	t.Run("LastAgentMessage", func(t *testing.T) {
		t.Parallel()
		t.Run("Empty", func(t *testing.T) {
			t.Parallel()
			if lastAgentMessage(nil) != nil {
				t.Error("lastAgentMessage(nil) should be nil")
			}
		})
		t.Run("SkipsNonSemantic", func(t *testing.T) {
			t.Parallel()
			msgs := []agent.Message{
				&agent.ResultMessage{MessageType: "result", Result: "done"},
				&agent.DiffStatMessage{MessageType: "caic_diff_stat"},
				&agent.TextDeltaMessage{Text: "partial"},
				&agent.RawMessage{},
				&agent.UsageMessage{},
			}
			got := lastAgentMessage(msgs)
			if got == nil || got.Result != "done" {
				t.Errorf("lastAgentMessage should find ResultMessage skipping non-semantic, got %+v", got)
			}
		})
		t.Run("NonResultLast", func(t *testing.T) {
			t.Parallel()
			msgs := []agent.Message{
				&agent.ResultMessage{MessageType: "result"},
				&agent.TextMessage{Text: "mid output"},
			}
			if lastAgentMessage(msgs) != nil {
				t.Error("lastAgentMessage should be nil when last semantic is not result")
			}
		})
	})
}

// nopCloser wraps an io.Writer to implement io.WriteCloser with a no-op Close.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func TestSessionHandle(t *testing.T) {
	t.Parallel()
	t.Run("Drain", func(t *testing.T) {
		t.Parallel()
		cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cmdCancel)
		cmd := exec.CommandContext(cmdCtx, "cat")
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.Message, 256), nil)
		ch := make(chan agent.Message)
		done := make(chan struct{})
		go func() {
			for range ch {
			}
			close(done)
		}()
		h := &SessionHandle{Session: s, MsgCh: ch, DispatchDone: done}
		_ = stdin.Close()
		h.Drain()
		select {
		case <-done:
		default:
			t.Error("DispatchDone should be closed after Drain")
		}
	})

	t.Run("CloseMsgCh", func(t *testing.T) {
		t.Parallel()
		for range 2 {
			h := &SessionHandle{MsgCh: make(chan agent.Message)}
			h.CloseMsgCh()
		}
	})

	t.Run("StatsSubClose", func(t *testing.T) {
		t.Parallel()
		s := &statsSub{ch: make(chan runtime.Stats)}
		s.close()
		s.close() // double close must not panic.
	})
}

func TestState(t *testing.T) {
	t.Parallel()
	t.Run("String", func(t *testing.T) {
		t.Parallel()
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
			{StateStopping, "stopping"},
			{StateStopped, "stopped"},
			{StatePurged, "purged"},
			{State(999), "unknown"},
		} {
			if got := tt.state.String(); got != tt.want {
				t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
			}
		}
	})
	t.Run("SetStateIf", func(t *testing.T) {
		t.Parallel()
		t.Run("Match", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
	t.Run("SetStateUnless", func(t *testing.T) {
		t.Parallel()
		t.Run("Transitions", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			tk.SetState(StateRunning)
			prev, changed := tk.SetStateUnless(StateStopped, StatePurged, StateStopping)
			if !changed {
				t.Fatal("changed = false, want true when state not excluded")
			}
			if prev != StateRunning {
				t.Errorf("prev = %v, want %v", prev, StateRunning)
			}
			if tk.GetState() != StateStopped {
				t.Errorf("state = %v, want %v", tk.GetState(), StateStopped)
			}
		})
		t.Run("Excluded", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			tk.SetState(StatePurging)
			prev, changed := tk.SetStateUnless(StateStopped, StatePurging, StateStopping)
			if changed {
				t.Fatal("changed = true, want false when state excluded")
			}
			if prev != StatePurging {
				t.Errorf("prev = %v, want %v", prev, StatePurging)
			}
			if tk.GetState() != StatePurging {
				t.Errorf("state = %v, want %v (should be unchanged)", tk.GetState(), StatePurging)
			}
		})
	})
	t.Run("SetStateIfAny", func(t *testing.T) {
		t.Parallel()
		t.Run("Transitions", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			tk.SetState(StateAsking)
			prev, changed := tk.SetStateIfAny(StateStarting, StateWaiting, StateAsking, StateHasPlan)
			if !changed {
				t.Fatal("changed = false, want true when state is allowed")
			}
			if prev != StateAsking {
				t.Errorf("prev = %v, want %v", prev, StateAsking)
			}
			if tk.GetState() != StateStarting {
				t.Errorf("state = %v, want %v", tk.GetState(), StateStarting)
			}
		})
		t.Run("Rejected", func(t *testing.T) {
			t.Parallel()
			tk := &Task{}
			tk.SetState(StatePurging)
			prev, changed := tk.SetStateIfAny(StateStopping, StateWaiting, StateRunning)
			if changed {
				t.Fatal("changed = true, want false when state is not allowed")
			}
			if prev != StatePurging {
				t.Errorf("prev = %v, want %v", prev, StatePurging)
			}
			if tk.GetState() != StatePurging {
				t.Errorf("state = %v, want %v (should be unchanged)", tk.GetState(), StatePurging)
			}
		})
	})
}
