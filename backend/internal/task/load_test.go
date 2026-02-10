package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maruel/wmao/backend/internal/agent"
)

func writeLogFile(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	data := make([]byte, 0, len(lines)*64)
	for _, l := range lines {
		data = append(data, l...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoadBranchLogs(t *testing.T) {
	t.Run("EmptyDir", func(t *testing.T) {
		if lt := LoadBranchLogs("", "wmao/w0"); lt != nil {
			t.Error("expected nil for empty logDir")
		}
	})
	t.Run("NoMatch", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "other", Branch: "wmao/w9"})
		writeLogFile(t, dir, "a.jsonl", meta)

		if lt := LoadBranchLogs(dir, "wmao/w0"); lt != nil {
			t.Error("expected nil when no files match branch")
		}
	})
	t.Run("SingleFile", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "fix bug", Repo: "test", Branch: "wmao/w0"})
		init := mustJSON(t, agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "sid-1"})
		asst := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		result := mustJSON(t, agent.ResultMessage{MessageType: "result", Result: "done"})
		writeLogFile(t, dir, "a.jsonl", meta, init, asst, result)

		lt := LoadBranchLogs(dir, "wmao/w0")
		if lt == nil {
			t.Fatal("expected non-nil LoadedTask")
		}
		if lt.Prompt != "fix bug" {
			t.Errorf("Prompt = %q, want %q", lt.Prompt, "fix bug")
		}
		if len(lt.Msgs) != 3 {
			t.Fatalf("Msgs len = %d, want 3", len(lt.Msgs))
		}
		if lt.Msgs[0].Type() != "system" {
			t.Errorf("Msgs[0].Type() = %q, want %q", lt.Msgs[0].Type(), "system")
		}
	})
	t.Run("MultipleFiles", func(t *testing.T) {
		dir := t.TempDir()

		// First session.
		meta1 := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "fix bug", Repo: "test", Branch: "wmao/w0", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		asst1 := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		writeLogFile(t, dir, "a.jsonl", meta1, asst1)

		// Second session (later StartedAt so it sorts after).
		meta2 := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "fix bug", Repo: "test", Branch: "wmao/w0", StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)})
		init2 := mustJSON(t, agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "sid-2"})
		asst2 := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		writeLogFile(t, dir, "b.jsonl", meta2, init2, asst2)

		lt := LoadBranchLogs(dir, "wmao/w0")
		if lt == nil {
			t.Fatal("expected non-nil LoadedTask")
		}
		if len(lt.Msgs) != 3 {
			t.Fatalf("Msgs len = %d, want 3", len(lt.Msgs))
		}
		if lt.Prompt != "fix bug" {
			t.Errorf("Prompt = %q, want %q", lt.Prompt, "fix bug")
		}
	})
	t.Run("BranchReusedPromptUpdated", func(t *testing.T) {
		dir := t.TempDir()

		// First session with original prompt.
		meta1 := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "old stale prompt", Repo: "test", Branch: "wmao/w0", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		asst1 := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		writeLogFile(t, dir, "a.jsonl", meta1, asst1)

		// Second session reuses same branch with a new prompt.
		meta2 := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "new current prompt", Repo: "test", Branch: "wmao/w0", StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)})
		asst2 := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		writeLogFile(t, dir, "b.jsonl", meta2, asst2)

		lt := LoadBranchLogs(dir, "wmao/w0")
		if lt == nil {
			t.Fatal("expected non-nil LoadedTask")
		}
		if lt.Prompt != "new current prompt" {
			t.Errorf("Prompt = %q, want %q (got stale prompt from earlier session)", lt.Prompt, "new current prompt")
		}
	})
	t.Run("NonexistentDir", func(t *testing.T) {
		if lt := LoadBranchLogs("/nonexistent/path", "wmao/w0"); lt != nil {
			t.Error("expected nil for nonexistent dir")
		}
	})
	t.Run("NoFalseMatch", func(t *testing.T) {
		dir := t.TempDir()
		// Log file for wmao/w10 should NOT match wmao/w1.
		meta := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "task", Branch: "wmao/w10"})
		asst := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		writeLogFile(t, dir, "a.jsonl", meta, asst)

		if lt := LoadBranchLogs(dir, "wmao/w1"); lt != nil {
			t.Error("wmao/w10 log should not match wmao/w1")
		}
	})
}

func TestLoadLogs(t *testing.T) {
	t.Run("Valid", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "task1", Repo: "r", Branch: "wmao/w0"})
		asst := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "wmao_result", State: "terminated"})
		writeLogFile(t, dir, "a.jsonl", meta, asst, trailer)

		// Non-jsonl file should be ignored.
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].Prompt != "task1" {
			t.Errorf("Prompt = %q, want %q", tasks[0].Prompt, "task1")
		}
		if tasks[0].State != StateTerminated {
			t.Errorf("State = %v, want %v", tasks[0].State, StateTerminated)
		}
	})
	t.Run("NotExist", func(t *testing.T) {
		tasks, err := LoadLogs(filepath.Join(t.TempDir(), "nope"))
		if err != nil {
			t.Fatal(err)
		}
		if tasks != nil {
			t.Error("expected nil for nonexistent dir")
		}
	})
	t.Run("BadHeader", func(t *testing.T) {
		dir := t.TempDir()
		writeLogFile(t, dir, "bad.jsonl", `{"type":"not_meta"}`)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 0 {
			t.Errorf("len = %d, want 0", len(tasks))
		}
	})
}

func TestLoadTerminated(t *testing.T) {
	t.Run("EmptyDir", func(t *testing.T) {
		if got := LoadTerminated("", 10); got != nil {
			t.Errorf("expected nil, got %d tasks", len(got))
		}
	})
	t.Run("FiltersTerminalOnly", func(t *testing.T) {
		dir := t.TempDir()
		// Task with done trailer.
		meta0 := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "t0", Repo: "r", Branch: "wmao/w0", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		trailer0 := mustJSON(t, agent.MetaResultMessage{MessageType: "wmao_result", State: "terminated"})
		writeLogFile(t, dir, "a.jsonl", meta0, trailer0)

		// Task without trailer (still running — must NOT be loaded).
		meta1 := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "t1", Repo: "r", Branch: "wmao/w1", StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)})
		writeLogFile(t, dir, "b.jsonl", meta1)

		// Task with terminated trailer.
		meta2 := mustJSON(t, agent.MetaMessage{MessageType: "wmao_meta", Version: 1, Prompt: "t2", Repo: "r", Branch: "wmao/w2", StartedAt: time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)})
		trailer2 := mustJSON(t, agent.MetaResultMessage{MessageType: "wmao_result", State: "terminated"})
		writeLogFile(t, dir, "c.jsonl", meta2, trailer2)

		got := LoadTerminated(dir, 10)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		// Most recent first.
		if got[0].Prompt != "t2" {
			t.Errorf("got[0].Prompt = %q, want %q", got[0].Prompt, "t2")
		}
		if got[1].Prompt != "t0" {
			t.Errorf("got[1].Prompt = %q, want %q", got[1].Prompt, "t0")
		}
	})
	t.Run("LimitsToN", func(t *testing.T) {
		dir := t.TempDir()
		for i := range 5 {
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "wmao_meta", Version: 1, Prompt: fmt.Sprintf("t%d", i), Repo: "r",
				Branch: fmt.Sprintf("wmao/w%d", i), StartedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
			})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "wmao_result", State: "terminated"})
			writeLogFile(t, dir, fmt.Sprintf("%d.jsonl", i), meta, trailer)
		}

		got := LoadTerminated(dir, 3)
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		// Most recent first: t4, t3, t2.
		if got[0].Prompt != "t4" {
			t.Errorf("got[0].Prompt = %q, want %q", got[0].Prompt, "t4")
		}
		if got[2].Prompt != "t2" {
			t.Errorf("got[2].Prompt = %q, want %q", got[2].Prompt, "t2")
		}
	})
}

func TestParseState(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want State
	}{
		{"failed", StateFailed},
		{"terminated", StateTerminated},
		{"unknown", StateFailed},
	} {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseState(tt.in); got != tt.want {
				t.Errorf("parseState(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
