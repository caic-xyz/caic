package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
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

func TestLoadLogs(t *testing.T) {
	t.Run("Valid", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repo: "r", Branch: "caic-0", Harness: "claude"})
		asst := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "terminated"})
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
	t.Run("MultipleFiles", func(t *testing.T) {
		dir := t.TempDir()

		meta1 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "first", Repo: "r", Branch: "caic-0", Harness: "claude", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		asst1 := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		writeLogFile(t, dir, "a.jsonl", meta1, asst1)

		meta2 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "second", Repo: "r", Branch: "caic-0", Harness: "claude", StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)})
		init2 := mustJSON(t, agent.SystemInitMessage{MessageType: "system", Subtype: "init", SessionID: "sid-2"})
		asst2 := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		writeLogFile(t, dir, "b.jsonl", meta2, init2, asst2)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 2 {
			t.Fatalf("len = %d, want 2", len(tasks))
		}
		// Sorted by StartedAt ascending.
		if tasks[0].Prompt != "first" {
			t.Errorf("tasks[0].Prompt = %q, want %q", tasks[0].Prompt, "first")
		}
		if tasks[1].Prompt != "second" {
			t.Errorf("tasks[1].Prompt = %q, want %q", tasks[1].Prompt, "second")
		}
		// Each task has its own messages, not merged.
		if len(tasks[0].Msgs) != 1 {
			t.Errorf("tasks[0].Msgs len = %d, want 1", len(tasks[0].Msgs))
		}
		if len(tasks[1].Msgs) != 2 {
			t.Errorf("tasks[1].Msgs len = %d, want 2", len(tasks[1].Msgs))
		}
	})
	t.Run("ContextClearedResetsPlanState", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plan task", Repo: "r", Branch: "caic-0", Harness: "claude"})
		// Old session: agent enters plan mode and writes a plan file.
		planWrite := mustJSON(t, agent.AssistantMessage{
			MessageType: "assistant",
			Message: agent.APIMessage{Content: []agent.ContentBlock{
				{Type: "tool_use", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`)},
			}},
		})
		// context_cleared written by RestartSession before starting new session.
		cleared := mustJSON(t, agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"})
		// New session header + assistant message (no plan tools).
		meta2 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plan task", Repo: "r", Branch: "caic-0", Harness: "claude"})
		asst2 := mustJSON(t, agent.AssistantMessage{MessageType: "assistant"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "terminated"})
		writeLogFile(t, dir, "task.jsonl", meta, planWrite, cleared, meta2, asst2, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		// After restore, plan state must be empty because context_cleared resets it.
		tk := &Task{InitialPrompt: agent.Prompt{Text: lt.Prompt}}
		tk.SetState(StateRunning)
		tk.RestoreMessages(lt.Msgs)
		snap := tk.Snapshot()
		if snap.InPlanMode {
			t.Error("InPlanMode = true, want false")
		}
		if snap.PlanContent != "" {
			t.Errorf("PlanContent = %q, want empty", snap.PlanContent)
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
