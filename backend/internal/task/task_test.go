package task

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"
	"github.com/maruel/wmao/backend/internal/agent"
)

func TestOpenLog(t *testing.T) {
	t.Run("EmptyDir", func(t *testing.T) {
		r := &Runner{}
		w, closeFn := r.openLog(&Task{Prompt: "test"})
		defer closeFn()
		if w != nil {
			t.Error("expected nil writer when LogDir is empty")
		}
	})
	t.Run("CreatesFile", func(t *testing.T) {
		dir := t.TempDir()
		logDir := filepath.Join(dir, "logs")
		r := &Runner{LogDir: logDir}
		tk := &Task{ID: ksid.NewID(), Prompt: "test", Repo: "org/repo", Branch: "wmao/w0"}
		w, closeFn := r.openLog(tk)
		defer closeFn()
		if w == nil {
			t.Fatal("expected non-nil writer")
		}
		// Write something and close.
		_, _ = w.Write([]byte("test\n"))
		closeFn()

		entries, err := os.ReadDir(logDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected 1 file, got %d", len(entries))
		}
		name := entries[0].Name()
		want := tk.ID.String() + "-org-repo-wmao-w0.jsonl"
		if name != want {
			t.Errorf("filename = %q, want %q", name, want)
		}
	})
}

func TestSubscribeReplay(t *testing.T) {
	task := &Task{Prompt: "test"}
	// Add messages before subscribing.
	msg1 := &agent.SystemMessage{MessageType: "system", Subtype: "status"}
	msg2 := &agent.AssistantMessage{MessageType: "assistant"}
	task.addMessage(msg1)
	task.addMessage(msg2)

	history, ch, unsub := task.Subscribe(t.Context())
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
}

func TestSubscribeReplayLargeHistory(t *testing.T) {
	task := &Task{Prompt: "test"}
	// Add more messages than any reasonable channel buffer to verify no deadlock.
	const n = 1000
	for range n {
		task.addMessage(&agent.AssistantMessage{MessageType: "assistant"})
	}

	history, ch, unsub := task.Subscribe(t.Context())
	defer unsub()
	_ = ch

	if len(history) != n {
		t.Fatalf("history len = %d, want %d", len(history), n)
	}
}

func TestSubscribeMultipleListeners(t *testing.T) {
	task := &Task{Prompt: "test"}
	task.addMessage(&agent.SystemMessage{MessageType: "system", Subtype: "init"})

	// Start two subscribers.
	h1, ch1, unsub1 := task.Subscribe(t.Context())
	defer unsub1()
	h2, ch2, unsub2 := task.Subscribe(t.Context())
	defer unsub2()

	// Both get the same history.
	if len(h1) != 1 || len(h2) != 1 {
		t.Fatalf("history lens = %d, %d; want 1, 1", len(h1), len(h2))
	}

	// Send a live message — both channels should receive it.
	task.addMessage(&agent.AssistantMessage{MessageType: "assistant"})

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
}

func TestSubscribeLive(t *testing.T) {
	task := &Task{Prompt: "test"}

	_, ch, unsub := task.Subscribe(t.Context())
	defer unsub()

	// Send a live message after subscribing.
	msg := &agent.AssistantMessage{MessageType: "assistant"}
	task.addMessage(msg)

	timeout := time.After(time.Second)
	select {
	case got := <-ch:
		if got.Type() != "assistant" {
			t.Errorf("type = %q, want %q", got.Type(), "assistant")
		}
	case <-timeout:
		t.Fatal("timed out waiting for live message")
	}
}

func TestSendInputNotRunning(t *testing.T) {
	task := &Task{Prompt: "test"}
	err := task.SendInput("hello")
	if err == nil {
		t.Error("expected error when no session is active")
	}
}

func TestTaskTerminate(t *testing.T) {
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
}

func TestAddMessageTransitionsToWaiting(t *testing.T) {
	tk := &Task{Prompt: "test", State: StateRunning}
	result := &agent.ResultMessage{MessageType: "result"}
	tk.addMessage(result)
	if tk.State != StateWaiting {
		t.Errorf("state = %v, want %v", tk.State, StateWaiting)
	}
}

func TestAddMessageTransitionsToAsking(t *testing.T) {
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
}

func TestStateStrings(t *testing.T) {
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
		{StateFailed, "failed"},
		{StateTerminated, "terminated"},
	} {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

// --- Runner e2e test helpers ---

// initTestRepo creates a bare "remote" and a local clone with one commit on
// baseBranch. Returns the clone directory. origin points to the bare repo so
// git fetch/push work locally.
func initTestRepo(t *testing.T, baseBranch string) string {
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
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestRunnerInit(t *testing.T) {
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
}

func TestRunnerInitSkipsExisting(t *testing.T) {
	clone := initTestRepo(t, "main")
	// Pre-create branches.
	runGit(t, clone, "branch", "wmao/w0")
	runGit(t, clone, "branch", "wmao/w3")

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
}

func TestRestoreMessages(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		tk := &Task{Prompt: "test"}
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
}
