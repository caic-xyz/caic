// Tests for SessionRunner: agent session reconnect, log lifecycle, and
// message dispatch.

package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

func TestSessionRunner(t *testing.T) {
	t.Parallel()
	t.Run("Reconnect", func(t *testing.T) {
		t.Parallel()
		t.Run("error_stateful_missing_session_id", func(t *testing.T) {
			t.Parallel()
			for _, harness := range []harness.Name{harness.Codex, harness.OpenCode} {
				t.Run(string(harness), func(t *testing.T) {
					t.Parallel()
					r := newTestSessionRunner(nil, "", nil)
					tk := &Task{
						ID:            ksid.NewID(),
						InitialPrompt: agent.Prompt{Text: "test"},
						Harness:       harness,
					}
					tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
					tk.SetState(StateRunning)
					_, err := r.Reconnect(t.Context(), tk, true)
					if err == nil {
						t.Fatal("Reconnect returned nil error")
					}
					if !strings.Contains(err.Error(), "session ID missing") {
						t.Fatalf("err = %v, want missing session ID", err)
					}
				})
			}
		})
		t.Run("passes_history_to_attach_backend", func(t *testing.T) {
			t.Parallel()
			backend := &attachCaptureBackend{}
			r := newTestSessionRunner(nil, filepath.Join(t.TempDir(), "logs"), map[harness.Name]agent.Backend{"test": backend})
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       "test",
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateRunning)
			tk.RestoreMessages([]agent.Message{
				&agent.AskMessage{
					ToolUseID: "toolu-1",
					Questions: []agent.AskQuestion{{Question: "Which?"}},
				},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: agent.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: agent.PendingAskAction{
							Questions: []agent.AskQuestion{{Question: "Which?"}},
						},
					},
				},
			})

			h, err := r.Reconnect(t.Context(), tk, true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = h.GracefulStop(t.Context(), time.Second)
				_ = h.Drain()
			})

			pending := backend.capturedAttachOpts.PendingUserActions
			if len(pending) != 1 {
				t.Fatalf("PendingUserActions len = %d, want 1", len(pending))
			}
			if pending[0].Kind != agent.PendingUserActionAskUserQuestion {
				t.Errorf("Kind = %q, want %q", pending[0].Kind, agent.PendingUserActionAskUserQuestion)
			}
			if pending[0].RequestID != "req-1" {
				t.Errorf("RequestID = %q, want req-1", pending[0].RequestID)
			}
			if len(pending[0].Ask.Questions) != 1 {
				t.Fatalf("Questions len = %d, want 1", len(pending[0].Ask.Questions))
			}
			if pending[0].Ask.Questions[0].Question != "Which?" {
				t.Errorf("question = %q, want Which?", pending[0].Ask.Questions[0].Question)
			}
		})
	})

	t.Run("LogStore", func(t *testing.T) {
		t.Parallel()
		t.Run("CreatesFile", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			logDir := filepath.Join(dir, "logs")
			store := &LogStore{LogDir: logDir}
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
				Model:         "model-1",
				Effort:        "high",
			}
			w, err := store.Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			// Write something and close.
			_, _ = w.Write([]byte("test\n"))
			_ = w.Close()

			entries, err := os.ReadDir(logDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 file, got %d", len(entries))
			}
			name := entries[0].Name()
			want := tk.ID.String() + "-org-repo-caic-0.jsonl"
			if name != want {
				t.Errorf("filename = %q, want %q", name, want)
			}
			data, err := os.ReadFile(filepath.Join(logDir, name)) //nolint:gosec // path is test-controlled
			if err != nil {
				t.Fatal(err)
			}
			var meta map[string]any
			if err := json.Unmarshal(bytes.SplitN(data, []byte("\n"), 2)[0], &meta); err != nil {
				t.Fatal(err)
			}
			if meta["model"] != "model-1" {
				t.Errorf("meta model = %v, want model-1", meta["model"])
			}
			if meta["effort"] != "high" {
				t.Errorf("meta effort = %v, want high", meta["effort"])
			}
		})
		t.Run("ReopenNoDuplicateHeader", func(t *testing.T) {
			t.Parallel()
			// Reconnect appends via Reopen, which must not write a second
			// caic_meta header. Otherwise every server restart that re-adopts
			// a running instance duplicates the header.
			logDir := filepath.Join(t.TempDir(), "logs")
			store := &LogStore{LogDir: logDir}
			tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Name: "org/repo", Branch: "caic-0"}}}

			// Initial Start writes the header.
			w, err := store.Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			_ = w.Close()

			// Several reconnects (simulating repeated server restarts) append
			// without writing a new header.
			for range 3 {
				w, err := store.Reopen(tk)
				if err != nil {
					t.Fatal(err)
				}
				_ = w.Close()
			}

			name := tk.ID.String() + "-org-repo-caic-0.jsonl"
			data, err := os.ReadFile(filepath.Join(logDir, name)) //nolint:gosec // path is test-controlled
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(data), `"type":"caic_meta"`); got != 1 {
				t.Errorf("caic_meta count = %d, want 1", got)
			}
		})
		t.Run("ReopenMissingFile", func(t *testing.T) {
			t.Parallel()
			// Reopen must report os.ErrNotExist when no log exists yet so
			// Reconnect can fall back to Open (which writes the header).
			store := &LogStore{LogDir: filepath.Join(t.TempDir(), "logs")}
			tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Name: "org/repo", Branch: "caic-0"}}}
			if _, err := store.Reopen(tk); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Reopen err = %v, want os.ErrNotExist", err)
			}
		})
	})

	t.Run("RuntimeDir", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			dir  string
			task *Task
			want string
		}{
			{task: &Task{Repos: []RepoMount{{MountedPath: "~/src/caic-xyz/md"}}}, want: "/home/user/src/caic-xyz/md"},
			{dir: "/home/maruel/src/caic", want: "/home/user/src/caic"},
			{dir: "/home/alice/projects/myrepo", want: "/home/user/src/myrepo"},
			{dir: "/opt/repos/foo", want: "/home/user/src/foo"},
		}
		for _, tc := range tests {
			r := newTestSessionRunner(newTestRepoWorkspace("", tc.dir, nil), "", nil)
			tk := tc.task
			if tk == nil {
				tk = &Task{}
			}
			got := r.runtimeDir(tk)
			if got != tc.want {
				t.Errorf("runtimeDir(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		}
	})

	t.Run("StartMessageDispatch", func(t *testing.T) {
		t.Parallel()
		t.Run("ResultMessage", func(t *testing.T) {
			t.Parallel()
			stub := &stubContainer{}
			r := newTestSessionRunner(newTestRepoWorkspace("", "/repo", stub), "", nil)

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh, _ := r.startMessageDispatch(t.Context(), tk, false)

			rm := &agent.ResultMessage{MessageType: "result"}
			msgCh <- rm
			close(msgCh)

			// Wait for the dispatched message.
			timeout := time.After(time.Second)
			select {
			case got := <-ch:
				rr, ok := got.(*agent.ResultMessage)
				if !ok {
					t.Fatalf("expected *agent.ResultMessage, got %T", got)
				}
				if len(rr.DiffStat) != 1 || rr.DiffStat[0].Path != "main.go" {
					t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", rr.DiffStat)
				}
			case <-timeout:
				t.Fatal("timed out waiting for message")
			}
			if !stub.fetched {
				t.Error("Fetch was not called on result message")
			}
		})

		t.Run("MutatingToolEmitsDiffStatWithoutFetch", func(t *testing.T) {
			t.Parallel()
			for _, tool := range []string{"Edit", "Bash", "Write", "NotebookEdit"} {
				t.Run(tool, func(t *testing.T) {
					t.Parallel()
					stub := &stubContainer{}
					r := newTestSessionRunner(newTestRepoWorkspace("", "/repo", stub), "", nil)

					tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
					tk.SetState(StateRunning)
					_, ch, unsub := tk.Subscribe(t.Context())
					defer unsub()

					msgCh, _ := r.startMessageDispatch(t.Context(), tk, false)

					// Send a ToolUseMessage with a mutating tool.
					toolID := "tool_edit_1"
					msgCh <- &agent.ToolUseMessage{
						ToolUseID: toolID,
						Name:      tool,
						Input:     json.RawMessage(`{}`),
					}
					// Drain the tool_use message from the subscriber.
					recvMsg(t, ch)

					// Send the tool result.
					msgCh <- &agent.ToolResultMessage{
						ToolUseID: toolID,
					}

					msg := recvMsg(t, ch)
					if _, ok := msg.(*agent.ToolResultMessage); !ok {
						t.Fatalf("expected *agent.ToolResultMessage, got %T", msg)
					}
					msg = recvMsg(t, ch)
					ds, ok := msg.(*agent.DiffStatMessage)
					if !ok {
						t.Fatalf("expected *agent.DiffStatMessage, got %T", msg)
					}
					if len(ds.DiffStat) != 1 || ds.DiffStat[0].Path != "main.go" {
						t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", ds.DiffStat)
					}
					if stub.fetched {
						t.Error("Fetch was called for mutating tool result")
					}
					close(msgCh)
				})
			}
		})

		t.Run("NonMutatingToolNoDiffStat", func(t *testing.T) {
			t.Parallel()
			stub := &stubContainer{}
			r := newTestSessionRunner(newTestRepoWorkspace("", "", stub), "", nil)

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh, _ := r.startMessageDispatch(t.Context(), tk, false)

			toolID := "tool_read_1"
			msgCh <- &agent.ToolUseMessage{
				ToolUseID: toolID,
				Name:      "Read",
				Input:     json.RawMessage(`{}`),
			}
			recvMsg(t, ch) // drain tool_use

			msgCh <- &agent.ToolResultMessage{
				ToolUseID: toolID,
			}
			// Only expect the ToolResultMessage, no DiffStatMessage.
			msg := recvMsg(t, ch)
			if _, ok := msg.(*agent.DiffStatMessage); ok {
				t.Error("unexpected DiffStatMessage emitted for non-mutating tool")
			}
			if stub.fetched {
				t.Error("Fetch was called for non-mutating tool")
			}
			close(msgCh)
		})

		t.Run("SkipSideEffects", func(t *testing.T) {
			t.Parallel()
			stub := &stubContainer{}
			r := newTestSessionRunner(newTestRepoWorkspace("", "/repo", stub), "", nil)

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh, done := r.startMessageDispatch(t.Context(), tk, true)

			// Send a mutating tool use + result and a ResultMessage.
			toolID := "tool_edit_1"
			msgCh <- &agent.ToolUseMessage{ToolUseID: toolID, Name: "Edit", Input: json.RawMessage(`{}`)}
			recvMsg(t, ch)
			msgCh <- &agent.ToolResultMessage{ToolUseID: toolID}
			recvMsg(t, ch)
			msgCh <- &agent.ResultMessage{MessageType: "result"}
			recvMsg(t, ch)
			close(msgCh)
			<-done

			if stub.fetched {
				t.Error("Fetch was called despite skipSideEffects=true")
			}
		})

		t.Run("DispatchDrainBeforeClose", func(t *testing.T) {
			t.Parallel()
			r := newTestSessionRunner(nil, "", nil)

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)

			msgCh, done := r.startMessageDispatch(t.Context(), tk, false)

			// Buffer several messages, then close without draining.
			msgs := []*agent.TextMessage{
				{Text: "first"},
				{Text: "second"},
				{Text: "third"},
			}
			for _, m := range msgs {
				msgCh <- m
			}
			close(msgCh)
			<-done

			// All messages must be in t.msgs after done fires.
			got := tk.Messages()
			if len(got) != len(msgs) {
				t.Fatalf("Messages() has %d items, want %d", len(got), len(msgs))
			}
			for i, m := range got {
				tm, ok := m.(*agent.TextMessage)
				if !ok {
					t.Fatalf("Messages()[%d] = %T, want *agent.TextMessage", i, m)
				}
				if tm.Text != msgs[i].Text {
					t.Errorf("Messages()[%d].Text = %q, want %q", i, tm.Text, msgs[i].Text)
				}
			}
		})
	})

	t.Run("RestartSession", func(t *testing.T) {
		t.Parallel()
		for _, startState := range []State{StateWaiting, StateAsking, StateHasPlan} {
			t.Run(startState.String(), func(t *testing.T) {
				t.Parallel()
				logDir := t.TempDir()
				backend := &testBackend{}

				r := newTestSessionRunner(newTestRepoWorkspace("", "", &stubContainer{}), logDir, map[harness.Name]agent.Backend{"test": backend})

				tk := &Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "old prompt"},
					Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
					Harness:       "test",
				}
				tk.SetRuntimeConnectionInfo("fake-instance", runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)
				tk.SetState(startState)

				h, err := r.RestartSession(t.Context(), tk, agent.Prompt{Text: "new plan"})
				if err != nil {
					t.Fatal(err)
				}
				if h == nil {
					t.Fatal("RestartSession returned nil handle")
				}
				if tk.GetState() != StateRunning {
					t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
				}
				if tk.InitialPrompt.Text != "old prompt" {
					t.Errorf("Prompt.Text = %q, want %q (must not be mutated by RestartSession)", tk.InitialPrompt.Text, "old prompt")
				}

				// The context passed to AgentBackend.Start must still be valid after
				// RestartSession returns (it must not be a request-scoped context).
				select {
				case <-backend.capturedCtx.Done():
					t.Error("context passed to AgentBackend was canceled; must use a long-lived context")
				default:
				}

				// Verify the session is functional: wait briefly and check the context
				// is still alive (not canceled by a short-lived HTTP request context).
				time.Sleep(50 * time.Millisecond)
				select {
				case <-backend.capturedCtx.Done():
					t.Error("context was canceled shortly after RestartSession returned")
				default:
				}

				// Clean up: close the session.
				tk.CloseAndDetachSession(t.Context())
			})
		}
	})

	t.Run("RestartSession/LogContainsContextCleared", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		backend := &testBackend{}

		r := newTestSessionRunner(newTestRepoWorkspace("", "", &stubContainer{}), logDir, map[harness.Name]agent.Backend{"test": backend})

		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
			Harness:       "test",
		}
		tk.SetRuntimeConnectionInfo("fake-instance", runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)

		// Create an initial session with a log writer by using the backend
		// directly (RepoWorkspace.Start needs a instance backend).
		logW, err := (&LogStore{LogDir: logDir}).Open(tk)
		if err != nil {
			t.Fatal(err)
		}
		msgCh := make(chan agent.Message, 16)
		session, err := backend.Start(t.Context(), &agent.Options{MsgCh: msgCh, LogW: logW})
		if err != nil {
			t.Fatal(err)
		}
		alreadyDone := make(chan struct{})
		close(alreadyDone)
		h1 := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: alreadyDone, LogW: logW}
		tk.AttachSession(h1)
		tk.SetState(StateRunning)

		// Simulate the agent writing a plan file.
		tk.addMessage(t.Context(), &agent.ToolUseMessage{
			ToolUseID: "tu1", Name: "Write",
			Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
		}, false)
		if snap := tk.Snapshot(); snap.PlanContent != "the plan" {
			t.Fatalf("PlanContent = %q before restart, want %q", snap.PlanContent, "the plan")
		}

		// Gracefully end the first session so we can restart.
		_ = h1.GracefulStop(t.Context(), 5*time.Second)
		tk.SetState(StateWaiting)

		// Restart: should write context_cleared to the log before closing it.
		h2, err := r.RestartSession(t.Context(), tk, agent.Prompt{Text: "execute plan"})
		if err != nil {
			t.Fatal(err)
		}
		defer tk.CloseAndDetachSession(t.Context())
		_ = h2

		// Read the raw log file and verify context_cleared is present.
		entries, err := os.ReadDir(logDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 log file, got %d", len(entries))
		}
		data, err := os.ReadFile(filepath.Join(logDir, entries[0].Name())) //nolint:gosec // test code, path is from t.TempDir()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"context_cleared"`) {
			t.Error("log file does not contain context_cleared marker")
		}
	})
}
