package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/ksid"
)

// testBackend implements agent.Backend for tests. It launches a "cat" process
// that blocks until stdin is closed. capturedCtx records the context passed
// to Start so tests can assert context lifetime.
type testBackend struct {
	capturedCtx context.Context
}

func (b *testBackend) Harness() agent.Harness { return "test" }

func (b *testBackend) Start(ctx context.Context, _ *agent.Options, msgCh chan<- agent.Message, _ io.Writer) (*agent.Session, error) {
	b.capturedCtx = ctx
	cmd := exec.CommandContext(ctx, "cat")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return agent.NewSession(cmd, stdin, stdout, msgCh, nil, &testWire{}, nil), nil
}

func (b *testBackend) AttachRelay(context.Context, string, int64, chan<- agent.Message, io.Writer) (*agent.Session, error) {
	return nil, errors.New("test backend does not support relay")
}

func (b *testBackend) ReadRelayOutput(context.Context, string) ([]agent.Message, int64, error) {
	return nil, 0, errors.New("test backend does not support relay")
}

func (b *testBackend) ParseMessage(line []byte) (agent.Message, error) {
	return agent.ParseMessage(line)
}

func (b *testBackend) Models() []string { return []string{"test-model"} }

// SupportsImages always returns false in the test backend.
func (b *testBackend) SupportsImages() bool { return false }

func (b *testBackend) ContextWindowLimit(string) int { return 180_000 }

// testWire implements agent.WireFormat for testing.
type testWire struct{}

func (*testWire) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	msg := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{Type: "user"}
	msg.Message.Role = "user"
	msg.Message.Content = p.Text
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	if logW != nil {
		_, _ = logW.Write(data)
	}
	return nil
}

func (*testWire) ParseMessage(line []byte) (agent.Message, error) {
	return agent.ParseMessage(line)
}

func TestRunner(t *testing.T) {
	t.Run("Init", func(t *testing.T) {
		t.Run("Basic", func(t *testing.T) {
			clone := initTestRepo(t, "main")
			r := &Runner{
				BaseBranch: "main",
				Dir:        clone,
			}
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r.nextID != 0 {
				t.Errorf("nextID = %d, want 0", r.nextID)
			}
		})
		t.Run("SkipsExisting", func(t *testing.T) {
			clone := initTestRepo(t, "main")
			// Pre-create branches and push to remote.
			runGit(t, clone, "branch", "caic-0")
			runGit(t, clone, "push", "origin", "caic-0")
			runGit(t, clone, "branch", "caic-3")
			runGit(t, clone, "push", "origin", "caic-3")

			r := &Runner{
				BaseBranch: "main",
				Dir:        clone,
			}
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r.nextID != 4 {
				t.Errorf("nextID = %d, want 4", r.nextID)
			}
		})
	})

	t.Run("Setup", func(t *testing.T) {
		t.Run("CustomBaseBranch", func(t *testing.T) {
			// Verify that setup creates the task branch from t.BaseBranch
			// when it differs from the runner's default BaseBranch.
			clone := initTestRepo(t, "main")
			// Create a feature branch on origin with a distinct commit.
			runGit(t, clone, "checkout", "-b", "feature")
			if err := os.WriteFile(filepath.Join(clone, "feature.txt"), []byte("feat\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "feature commit")
			runGit(t, clone, "push", "origin", "feature")
			runGit(t, clone, "checkout", "main")

			logDir := t.TempDir()
			stub := &stubContainer{}
			r := &Runner{
				BaseBranch: "main",
				Dir:        clone,
				LogDir:     logDir,
				Container:  stub,
			}
			r.initDefaults()

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repo:          "org/repo",
				BaseBranch:    "feature",
				Harness:       agent.Claude,
			}

			r.branchMu.Lock()
			sr, err := r.setup(t.Context(), tk, nil)
			r.branchMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}

			// The task branch must contain the feature commit (feature.txt).
			out, execErr := exec.Command("git", "-C", clone, "show", sr.Branch+":feature.txt").Output() //nolint:gosec // controlled test args
			if execErr != nil {
				t.Fatalf("feature.txt not in task branch %s: %v", sr.Branch, execErr)
			}
			if string(out) != "feat\n" {
				t.Errorf("feature.txt content = %q, want %q", string(out), "feat\n")
			}
		})
		t.Run("LocalOnlyBaseBranch", func(t *testing.T) {
			// Verify that setup works when BaseBranch exists only locally
			// (not pushed to origin).
			clone := initTestRepo(t, "main")
			runGit(t, clone, "checkout", "-b", "local-only")
			if err := os.WriteFile(filepath.Join(clone, "local.txt"), []byte("local\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "local commit")
			// Do NOT push to origin — branch exists only locally.
			runGit(t, clone, "checkout", "main")

			logDir := t.TempDir()
			stub := &stubContainer{}
			r := &Runner{
				BaseBranch: "main",
				Dir:        clone,
				LogDir:     logDir,
				Container:  stub,
			}
			r.initDefaults()

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repo:          "org/repo",
				BaseBranch:    "local-only",
				Harness:       agent.Claude,
			}

			r.branchMu.Lock()
			sr, err := r.setup(t.Context(), tk, nil)
			r.branchMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}

			// The task branch must contain the local commit (local.txt).
			out, execErr := exec.Command("git", "-C", clone, "show", sr.Branch+":local.txt").Output() //nolint:gosec // controlled test args
			if execErr != nil {
				t.Fatalf("local.txt not in task branch %s: %v", sr.Branch, execErr)
			}
			if string(out) != "local\n" {
				t.Errorf("local.txt content = %q, want %q", string(out), "local\n")
			}
		})
	})

	t.Run("Cleanup", func(t *testing.T) {
		t.Run("NoSessionUsesLiveStats", func(t *testing.T) {
			// Simulate an adopted task after server restart: no active session, but
			// live stats were restored from log messages. Cleanup should fall back to
			// LiveStats for the result cost.
			clone := initTestRepo(t, "main")
			r := &Runner{
				BaseBranch: "main",
				Dir:        clone,
			}

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repo:          "org/repo",
				Branch:        "main",
			}
			tk.SetState(StateRunning)

			// Restore messages with cost info (simulates RestoreMessages from logs).
			tk.RestoreMessages([]agent.Message{
				&agent.ResultMessage{
					MessageType:  "result",
					TotalCostUSD: 0.42,
					NumTurns:     5,
					DurationMs:   12345,
				},
			})

			result := r.Cleanup(t.Context(), tk, StateTerminated)
			if result.State != StateTerminated {
				t.Errorf("state = %v, want %v", result.State, StateTerminated)
			}
			if result.CostUSD != 0.42 {
				t.Errorf("CostUSD = %f, want 0.42", result.CostUSD)
			}
			if result.NumTurns != 5 {
				t.Errorf("NumTurns = %d, want 5", result.NumTurns)
			}
			if result.Duration != 12345*time.Millisecond {
				t.Errorf("Duration = %v, want %v", result.Duration, 12345*time.Millisecond)
			}
		})

		t.Run("UsesLiveDiffStat", func(t *testing.T) {
			clone := initTestRepo(t, "main")
			r := &Runner{
				BaseBranch: "main",
				Dir:        clone,
			}

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repo:          "org/repo",
				Branch:        "main",
			}
			tk.SetState(StateRunning)

			// Restore messages including a DiffStatMessage (simulates relay output).
			tk.RestoreMessages([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat: agent.DiffStat{
						{Path: "a.go", Added: 10, Deleted: 3},
						{Path: "b.go", Added: 5, Deleted: 0},
					},
				},
			})

			result := r.Cleanup(t.Context(), tk, StateTerminated)
			if len(result.DiffStat) != 2 {
				t.Fatalf("DiffStat has %d entries, want 2", len(result.DiffStat))
			}
			if result.DiffStat[0].Path != "a.go" || result.DiffStat[0].Added != 10 {
				t.Errorf("DiffStat[0] = %+v, want {a.go 10 3}", result.DiffStat[0])
			}
		})

		t.Run("BackupCreated", func(t *testing.T) {
			// Container has a commit not reachable from origin. Cleanup should
			// create a caic-backup/ branch.
			clone := initTestRepo(t, "main")

			// Set up a "container" bare repo with a commit on caic/w0.
			containerBare := filepath.Join(filepath.Dir(clone), "container.git")
			runGit(t, "", "init", "--bare", containerBare)
			containerName := "md-repo-caic-w0"
			runGit(t, clone, "remote", "add", containerName, containerBare)

			// Create a commit only reachable via the container remote.
			runGit(t, clone, "checkout", "-b", "caic/w0")
			if err := os.WriteFile(filepath.Join(clone, "work.txt"), []byte("work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "container work")
			runGit(t, clone, "push", containerName, "caic/w0")
			runGit(t, clone, "checkout", "main")
			runGit(t, clone, "branch", "-D", "caic/w0")

			// The fetchFn simulates md fetch by running git fetch on the container remote.
			stub := &stubContainer{
				fetchFn: func(_ string) error {
					cmd := exec.Command("git", "fetch", containerName)
					cmd.Dir = clone
					out, err := cmd.CombinedOutput()
					if err != nil {
						return fmt.Errorf("git fetch %s: %w: %s", containerName, err, out)
					}
					return nil
				},
			}

			r := &Runner{
				BaseBranch: "main",
				Dir:        clone,
				Container:  stub,
			}

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repo:          "org/repo",
				Branch:        "caic/w0",
				Container:     containerName,
			}
			tk.SetState(StateRunning)

			r.Cleanup(t.Context(), tk, StateTerminated)

			// Verify the backup branch was created.
			cmd := exec.Command("git", "rev-parse", "--verify", "caic-backup/caic/w0")
			cmd.Dir = clone
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("backup branch not created: %v\n%s", err, out)
			}
		})

		t.Run("BackupSkippedWhenReachable", func(t *testing.T) {
			// Container commit was already pushed to origin. No backup needed.
			clone := initTestRepo(t, "main")

			containerBare := filepath.Join(filepath.Dir(clone), "container.git")
			runGit(t, "", "init", "--bare", containerBare)
			containerName := "md-repo-caic-w1"
			runGit(t, clone, "remote", "add", containerName, containerBare)

			// Create a commit and push to BOTH origin and container.
			runGit(t, clone, "checkout", "-b", "caic/w1")
			if err := os.WriteFile(filepath.Join(clone, "synced.txt"), []byte("synced\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "synced work")
			runGit(t, clone, "push", "origin", "caic/w1")
			runGit(t, clone, "push", containerName, "caic/w1")
			runGit(t, clone, "checkout", "main")
			runGit(t, clone, "branch", "-D", "caic/w1")

			stub := &stubContainer{
				fetchFn: func(_ string) error {
					cmd := exec.Command("git", "fetch", containerName)
					cmd.Dir = clone
					out, err := cmd.CombinedOutput()
					if err != nil {
						return fmt.Errorf("git fetch %s: %w: %s", containerName, err, out)
					}
					return nil
				},
			}

			r := &Runner{
				BaseBranch: "main",
				Dir:        clone,
				Container:  stub,
			}

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repo:          "org/repo",
				Branch:        "caic/w1",
				Container:     containerName,
			}
			tk.SetState(StateRunning)

			r.Cleanup(t.Context(), tk, StateTerminated)

			// Verify no backup branch was created.
			cmd := exec.Command("git", "rev-parse", "--verify", "caic-backup/caic/w1")
			cmd.Dir = clone
			if err := cmd.Run(); err == nil {
				t.Error("backup branch should not exist when commit is reachable from origin")
			}
		})

		t.Run("BackupFetchFails", func(t *testing.T) {
			// Fetch fails — Cleanup must still kill the container.
			clone := initTestRepo(t, "main")

			stub := &stubContainer{
				fetchErr: errors.New("container unreachable"),
			}

			r := &Runner{
				BaseBranch: "main",
				Dir:        clone,
				Container:  stub,
			}

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repo:          "org/repo",
				Branch:        "caic/w0",
				Container:     "md-repo-caic-w0",
			}
			tk.SetState(StateRunning)

			result := r.Cleanup(t.Context(), tk, StateTerminated)
			if result.State != StateTerminated {
				t.Errorf("state = %v, want %v", result.State, StateTerminated)
			}
			// No backup branch should exist (fetch failed).
			cmd := exec.Command("git", "rev-parse", "--verify", "caic-backup/caic/w0")
			cmd.Dir = clone
			if err := cmd.Run(); err == nil {
				t.Error("backup branch should not exist when fetch fails")
			}
		})
	})

	t.Run("openLog", func(t *testing.T) {
		t.Run("CreatesFile", func(t *testing.T) {
			dir := t.TempDir()
			logDir := filepath.Join(dir, "logs")
			r := &Runner{LogDir: logDir}
			tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Repo: "org/repo", Branch: "caic-0"}
			w, err := r.openLog(tk)
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
		})
	})

	t.Run("ContainerDir", func(t *testing.T) {
		tests := []struct {
			dir  string
			want string
		}{
			{"/home/maruel/src/caic", "/home/user/src/caic"},
			{"/home/alice/projects/myrepo", "/home/user/src/myrepo"},
			{"/opt/repos/foo", "/home/user/src/foo"},
		}
		for _, tc := range tests {
			r := &Runner{Dir: tc.dir}
			got := r.containerDir()
			if got != tc.want {
				t.Errorf("containerDir(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		}
	})

	t.Run("StartMessageDispatch", func(t *testing.T) {
		t.Run("ResultMessage", func(t *testing.T) {
			stub := &stubContainer{}
			r := &Runner{Container: stub}
			r.initDefaults()

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Branch: "caic-0"}
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh := r.startMessageDispatch(t.Context(), tk)

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

		t.Run("MutatingToolEmitsDiffStat", func(t *testing.T) {
			for _, tool := range []string{"Edit", "Bash", "Write", "NotebookEdit"} {
				t.Run(tool, func(t *testing.T) {
					stub := &stubContainer{}
					r := &Runner{Container: stub}
					r.initDefaults()

					tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Branch: "caic-0"}
					tk.SetState(StateRunning)
					_, ch, unsub := tk.Subscribe(t.Context())
					defer unsub()

					msgCh := r.startMessageDispatch(t.Context(), tk)

					// Send an AssistantMessage with a mutating tool_use block.
					toolID := "tool_edit_1"
					msgCh <- &agent.AssistantMessage{
						MessageType: "assistant",
						Message: agent.APIMessage{
							Content: []agent.ContentBlock{
								{Type: "tool_use", ID: toolID, Name: tool, Input: json.RawMessage(`{}`)},
							},
						},
					}
					// Drain the assistant message from the subscriber.
					recvMsg(t, ch)

					// Send the tool result (UserMessage with ParentToolUseID).
					msgCh <- &agent.UserMessage{
						MessageType:     "user",
						ParentToolUseID: &toolID,
					}

					// Expect two messages: the UserMessage and a DiffStatMessage.
					var gotDiffStat bool
					for range 2 {
						msg := recvMsg(t, ch)
						if ds, ok := msg.(*agent.DiffStatMessage); ok {
							gotDiffStat = true
							if len(ds.DiffStat) != 1 || ds.DiffStat[0].Path != "main.go" {
								t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", ds.DiffStat)
							}
						}
					}
					if !gotDiffStat {
						t.Error("no DiffStatMessage emitted after mutating tool result")
					}
					if !stub.fetched {
						t.Error("Fetch was not called on mutating tool result")
					}
					close(msgCh)
				})
			}
		})

		t.Run("NonMutatingToolNoDiffStat", func(t *testing.T) {
			stub := &stubContainer{}
			r := &Runner{Container: stub}
			r.initDefaults()

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Branch: "caic-0"}
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh := r.startMessageDispatch(t.Context(), tk)

			toolID := "tool_read_1"
			msgCh <- &agent.AssistantMessage{
				MessageType: "assistant",
				Message: agent.APIMessage{
					Content: []agent.ContentBlock{
						{Type: "tool_use", ID: toolID, Name: "Read", Input: json.RawMessage(`{}`)},
					},
				},
			}
			recvMsg(t, ch) // drain assistant

			msgCh <- &agent.UserMessage{
				MessageType:     "user",
				ParentToolUseID: &toolID,
			}
			// Only expect the UserMessage, no DiffStatMessage.
			msg := recvMsg(t, ch)
			if _, ok := msg.(*agent.DiffStatMessage); ok {
				t.Error("unexpected DiffStatMessage emitted for non-mutating tool")
			}
			if stub.fetched {
				t.Error("Fetch was called for non-mutating tool")
			}
			close(msgCh)
		})
	})

	t.Run("RestartSession", func(t *testing.T) {
		logDir := t.TempDir()
		backend := &testBackend{}

		r := &Runner{
			LogDir:   logDir,
			Backends: map[agent.Harness]agent.Backend{"test": backend},
		}

		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "old prompt"},
			Repo:          "org/repo",
			Harness:       "test",
			Branch:        "caic-0",
			Container:     "fake-container",
		}
		tk.SetState(StateWaiting)

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
		tk.CloseAndDetachSession()
	})

	t.Run("RestartSession/LogContainsContextCleared", func(t *testing.T) {
		logDir := t.TempDir()
		backend := &testBackend{}

		r := &Runner{
			LogDir:   logDir,
			Backends: map[agent.Harness]agent.Backend{"test": backend},
		}

		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
			Repo:          "org/repo",
			Harness:       "test",
			Branch:        "caic-0",
			Container:     "fake-container",
		}

		// Create an initial session with a log writer by using the backend
		// directly (Runner.Start needs a container backend).
		logW, err := r.openLog(tk)
		if err != nil {
			t.Fatal(err)
		}
		msgCh := make(chan agent.Message, 16)
		session, err := backend.Start(t.Context(), nil, msgCh, logW)
		if err != nil {
			t.Fatal(err)
		}
		h1 := &SessionHandle{Session: session, MsgCh: msgCh, LogW: logW}
		tk.AttachSession(h1)
		tk.SetState(StateRunning)

		// Simulate the agent writing a plan file.
		tk.addMessage(t.Context(), &agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{Content: []agent.ContentBlock{
				{Type: "tool_use", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`)},
			}},
		})
		if snap := tk.Snapshot(); snap.PlanContent != "the plan" {
			t.Fatalf("PlanContent = %q before restart, want %q", snap.PlanContent, "the plan")
		}

		// Gracefully end the first session so we can restart.
		h1.Session.Close()
		<-h1.Session.Done()
		tk.SetState(StateWaiting)

		// Restart: should write context_cleared to the log before closing it.
		h2, err := r.RestartSession(t.Context(), tk, agent.Prompt{Text: "execute plan"})
		if err != nil {
			t.Fatal(err)
		}
		defer tk.CloseAndDetachSession()
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

	t.Run("BranchDiffStat", func(t *testing.T) {
		sc := &stubContainer{}
		r := &Runner{Container: sc, Dir: "/repo"}
		ds := r.BranchDiffStat(t.Context(), "feature")
		if !sc.fetched {
			t.Error("BranchDiffStat did not call Fetch")
		}
		if len(ds) != 1 || ds[0].Path != "main.go" || ds[0].Added != 5 || ds[0].Deleted != 1 {
			t.Errorf("BranchDiffStat = %+v, want [{main.go +5 -1}]", ds)
		}
	})
}

// stubContainer implements ContainerBackend for testing. Diff returns a fixed
// numstat line; Fetch records that it was called.
type stubContainer struct {
	fetched  bool
	fetchErr error              // If set, Fetch returns this error.
	fetchFn  func(string) error // If set, called with the branch during Fetch.
}

func (s *stubContainer) Start(_ context.Context, _, _ string, _ []string, _ StartOptions) (_, _ string, _ error) {
	return "stub", "", nil
}

func (s *stubContainer) Diff(_ context.Context, _, _ string, _ ...string) (string, error) {
	return "5\t1\tmain.go\n", nil
}

func (s *stubContainer) Fetch(_ context.Context, _, branch string) error {
	s.fetched = true
	if s.fetchErr != nil {
		return s.fetchErr
	}
	if s.fetchFn != nil {
		return s.fetchFn(branch)
	}
	return nil
}

func (s *stubContainer) Kill(context.Context, string, string) error { return nil }

// recvMsg reads a single message from ch, respecting the test context and a
// 1-second safety timeout.
func recvMsg(t *testing.T, ch <-chan agent.Message) agent.Message {
	select {
	case m := <-ch:
		return m
	case <-t.Context().Done():
		t.Fatal("test context canceled waiting for message")
		return nil
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
		return nil
	}
}

// initTestRepo creates a bare "remote" and a local clone with one commit on
// baseBranch. Returns the clone directory. origin points to the bare repo so
// git fetch/push work locally.
func initTestRepo(t *testing.T, baseBranch string) string { //nolint:unparam // baseBranch is parameterized for clarity.
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	runGit(t, "", "init", "--bare", bare)
	runGit(t, "", "init", clone)
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "checkout", "-b", baseBranch)

	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-m", "init")
	runGit(t, clone, "remote", "add", "origin", bare)
	runGit(t, clone, "push", "-u", "origin", baseBranch)
	return clone
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // test helper with controlled args
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
