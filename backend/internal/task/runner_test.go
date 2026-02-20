package task

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
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
			runGit(t, clone, "branch", "caic/w0")
			runGit(t, clone, "push", "origin", "caic/w0")
			runGit(t, clone, "branch", "caic/w3")
			runGit(t, clone, "push", "origin", "caic/w3")

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
				state:         StateRunning,
			}

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
				State:         StateRunning,
			}

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
	})

	t.Run("openLog", func(t *testing.T) {
		t.Run("CreatesFile", func(t *testing.T) {
			dir := t.TempDir()
			logDir := filepath.Join(dir, "logs")
			r := &Runner{LogDir: logDir}
			tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Repo: "org/repo", Branch: "caic/w0"}
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
			want := tk.ID.String() + "-org-repo-caic-w0.jsonl"
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
		stub := &stubContainer{}
		r := &Runner{Container: stub}
		r.initDefaults()

		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, state: StateRunning, Branch: "caic/w0"}
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
			Branch:        "caic/w0",
			Container:     "fake-container",
			state:         StateWaiting,
		}

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
}

// stubContainer implements ContainerBackend for testing. Diff returns a fixed
// numstat line; Fetch records that it was called.
type stubContainer struct {
	fetched bool
}

func (s *stubContainer) Start(_ context.Context, _, _ string, _ []string, _ StartOptions) (_, _ string, _ error) {
	return "stub", "", nil
}

func (s *stubContainer) Diff(_ context.Context, _, _ string, _ ...string) (string, error) {
	return "5\t1\tmain.go\n", nil
}

func (s *stubContainer) Fetch(context.Context, string, string) error {
	s.fetched = true
	return nil
}

func (s *stubContainer) Kill(context.Context, string, string) error { return nil }

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
