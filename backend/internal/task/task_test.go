package task

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maruel/wmao/backend/internal/agent"
)

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
		w, closeFn := r.openLog("wmao/w0")
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
