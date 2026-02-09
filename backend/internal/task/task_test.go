package task

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maruel/wmao/backend/internal/agent"
)

func TestSlugify(t *testing.T) {
	t.Run("LowerCase", func(t *testing.T) {
		got := slugify("fix the auth bug")
		if got != "fix-the-auth-bug" {
			t.Errorf("got %q, want %q", got, "fix-the-auth-bug")
		}
	})
	t.Run("SpecialChars", func(t *testing.T) {
		got := slugify("Add pagination to /api/users")
		if got != "add-pagination-to-ap" {
			t.Errorf("got %q, want %q", got, "add-pagination-to-ap")
		}
	})
	t.Run("UpperCase", func(t *testing.T) {
		got := slugify("UPPER CASE")
		if got != "upper-case" {
			t.Errorf("got %q, want %q", got, "upper-case")
		}
	})
	t.Run("Truncation", func(t *testing.T) {
		got := slugify("a " + string(make([]byte, 100)))
		if len(got) > 20 {
			t.Errorf("len = %d, want <= 20", len(got))
		}
	})
	t.Run("NoTrailingHyphenAfterTruncation", func(t *testing.T) {
		got := slugify("tell a joke about Montréal and friends")
		if got[len(got)-1] == '-' {
			t.Errorf("trailing hyphen: %q", got)
		}
		if len(got) > 20 {
			t.Errorf("len = %d, want <= 20", len(got))
		}
	})
}

func TestBranchName(t *testing.T) {
	// Branch names must be valid Docker container name components:
	// only [a-zA-Z0-9_.-] are allowed in Docker container names.
	branch := branchName("tell a joke about Montréal")
	want := "wmao/tell-a-joke-about-mo"
	if branch != want {
		t.Errorf("got %q, want %q", branch, want)
	}
}

func TestOpenLog(t *testing.T) {
	t.Run("EmptyDir", func(t *testing.T) {
		r := &Runner{}
		w, closeFn := r.openLog("test")
		defer closeFn()
		if w != nil {
			t.Error("expected nil writer when LogDir is empty")
		}
	})
	t.Run("CreatesFile", func(t *testing.T) {
		dir := t.TempDir()
		logDir := filepath.Join(dir, "logs")
		r := &Runner{LogDir: logDir}
		w, closeFn := r.openLog("fix the auth bug")
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
		if filepath.Ext(name) != ".jsonl" {
			t.Errorf("expected .jsonl extension, got %q", name)
		}
		if len(name) < len("20060102T150405-x.jsonl") {
			t.Errorf("filename too short: %q", name)
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

	ch, unsub := task.Subscribe(t.Context())
	defer unsub()

	// Should receive replayed history.
	timeout := time.After(time.Second)
	for range 2 {
		select {
		case <-ch:
		case <-timeout:
			t.Fatal("timed out waiting for replay message")
		}
	}
}

func TestSubscribeLive(t *testing.T) {
	task := &Task{Prompt: "test"}

	ch, unsub := task.Subscribe(t.Context())
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

func TestTaskFinish(t *testing.T) {
	tk := &Task{Prompt: "test"}
	tk.InitDoneCh()

	// Done should not be closed yet.
	select {
	case <-tk.Done():
		t.Fatal("doneCh closed prematurely")
	default:
	}

	tk.Finish()

	// Done should be closed now.
	select {
	case <-tk.Done():
	default:
		t.Fatal("doneCh not closed after Finish")
	}

	// Idempotent.
	tk.Finish()
}

func TestAddMessageTransitionsToWaiting(t *testing.T) {
	tk := &Task{Prompt: "test", State: StateRunning}
	result := &agent.ResultMessage{MessageType: "result"}
	tk.addMessage(result)
	if tk.State != StateWaiting {
		t.Errorf("state = %v, want %v", tk.State, StateWaiting)
	}
}

func TestStateEndedString(t *testing.T) {
	if got := StateEnded.String(); got != "ended" {
		t.Errorf("StateEnded.String() = %q, want %q", got, "ended")
	}
}

func TestTaskEnd(t *testing.T) {
	tk := &Task{Prompt: "test"}
	tk.InitDoneCh()

	// Not ended yet.
	if tk.IsEnded() {
		t.Fatal("IsEnded() true before End()")
	}

	tk.End()

	// Flag set and channel closed.
	if !tk.IsEnded() {
		t.Fatal("IsEnded() false after End()")
	}
	select {
	case <-tk.Done():
	default:
		t.Fatal("doneCh not closed after End()")
	}
}

func TestTaskEndIdempotent(t *testing.T) {
	tk := &Task{Prompt: "test"}
	tk.InitDoneCh()
	tk.End()
	tk.End() // must not panic
	if !tk.IsEnded() {
		t.Fatal("IsEnded() false after double End()")
	}
}
