package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claude"
)

func setClaudeParser(tasks []*LoadedTask) {
	for _, lt := range tasks {
		lt.SetParser(claude.New().NewParser())
	}
}

func writeLogFile(t *testing.T, dir, name string, lines ...string) {
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
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// claudeAssistant builds a Claude wire-format assistant NDJSON line from
// content blocks. Each block is a map with at minimum a "type" key.
func claudeAssistant(t *testing.T, blocks ...map[string]any) string {
	msg := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": blocks,
		},
	}
	return mustJSON(t, msg)
}

// claudeInit builds a Claude wire-format system/init NDJSON line.
func claudeInit(t *testing.T, sessionID string) string {
	msg := map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessionID,
	}
	return mustJSON(t, msg)
}

func TestLoadLogs(t *testing.T) {
	t.Run("Valid", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		asst := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
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
		if tasks[0].State != StatePurged {
			t.Errorf("State = %v, want %v", tasks[0].State, StatePurged)
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

		meta1 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "first", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		asst1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		writeLogFile(t, dir, "a.jsonl", meta1, asst1)

		meta2 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "second", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude", StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)})
		init2 := claudeInit(t, "sid-2")
		asst2 := claudeAssistant(t, map[string]any{"type": "text", "text": "world"})
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
		// Msgs are nil until LoadMessages is called.
		if tasks[0].Msgs != nil {
			t.Error("tasks[0].Msgs should be nil before LoadMessages")
		}
		setClaudeParser(tasks)
		for _, lt := range tasks {
			if err := lt.LoadMessages(); err != nil {
				t.Fatal(err)
			}
		}
		// Each task has its own messages, not merged.
		// asst1 produces 1 TextMessage.
		if len(tasks[0].Msgs) != 1 {
			t.Errorf("tasks[0].Msgs len = %d, want 1", len(tasks[0].Msgs))
		}
		// init2 produces 1 InitMessage; asst2 produces 1 TextMessage = 2 total.
		if len(tasks[1].Msgs) != 2 {
			t.Errorf("tasks[1].Msgs len = %d, want 2", len(tasks[1].Msgs))
		}
	})
	t.Run("FeatureFlagsAllSet", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "feat task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
			Tailscale: true, USB: true, Display: true,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "feat.jsonl", meta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if !lt.Tailscale {
			t.Error("Tailscale = false, want true")
		}
		if !lt.USB {
			t.Error("USB = false, want true")
		}
		if !lt.Display {
			t.Error("Display = false, want true")
		}
	})
	t.Run("FeatureFlagsOmitted", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "plain task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "plain.jsonl", meta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		if lt.Tailscale {
			t.Error("Tailscale = true, want false")
		}
		if lt.USB {
			t.Error("USB = true, want false")
		}
		if lt.Display {
			t.Error("Display = true, want false")
		}
	})
	t.Run("FeatureFlagsPartial", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "usb only",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
			USB: true,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "partial.jsonl", meta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		if lt.Tailscale {
			t.Error("Tailscale = true, want false")
		}
		if !lt.USB {
			t.Error("USB = false, want true")
		}
		if lt.Display {
			t.Error("Display = true, want false")
		}
	})
	t.Run("ContextClearedResetsPlanState", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plan task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		// Old session: agent enters plan mode and writes a plan file.
		planWrite := claudeAssistant(t, map[string]any{
			"type":  "tool_use",
			"id":    "tu1",
			"name":  "Write",
			"input": map[string]any{"file_path": "/home/user/.claude/plans/p.md", "content": "the plan"},
		})
		// context_cleared written by RestartSession before starting new session.
		cleared := mustJSON(t, agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"})
		// New session header + assistant message (no plan tools).
		meta2 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plan task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		asst2 := claudeAssistant(t, map[string]any{"type": "text", "text": "done"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "task.jsonl", meta, planWrite, cleared, meta2, asst2, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		setClaudeParser(tasks)
		if err := lt.LoadMessages(); err != nil {
			t.Fatal(err)
		}
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
	t.Run("PRHeaderOnly", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "pr task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "octocat", ForgeRepo: "hello", ForgePR: 42})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "1-r-caic-1.jsonl", meta, prMsg, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if lt.ForgeOwner != "octocat" {
			t.Errorf("ForgeOwner = %q, want %q", lt.ForgeOwner, "octocat")
		}
		if lt.ForgeRepo != "hello" {
			t.Errorf("ForgeRepo = %q, want %q", lt.ForgeRepo, "hello")
		}
		if lt.ForgePR != 42 {
			t.Errorf("ForgePR = %d, want 42", lt.ForgePR)
		}
	})
	t.Run("PRFullParse", func(t *testing.T) {
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "pr task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-2"}}, Harness: "claude"})
		asst := claudeAssistant(t, map[string]any{"type": "text", "text": "done"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "org", ForgeRepo: "repo", ForgePR: 99})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, dir, "2-r-caic-2.jsonl", meta, asst, prMsg, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		// Header-only parse should find PR in tail.
		if lt.ForgePR != 99 {
			t.Errorf("ForgePR = %d, want 99 (header parse)", lt.ForgePR)
		}
		// Full parse via LoadMessages should also find it.
		setClaudeParser(tasks)
		if err := lt.LoadMessages(); err != nil {
			t.Fatal(err)
		}
		if lt.ForgePR != 99 {
			t.Errorf("ForgePR = %d, want 99 (full parse)", lt.ForgePR)
		}
	})
	t.Run("PROutsideTailWindow", func(t *testing.T) {
		// caic_pr early in the file, followed by >64 KiB of messages,
		// so the header-only tail scan cannot see it.
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "big task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-3"}}, Harness: "claude"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "acme", ForgeRepo: "widget", ForgePR: 77})

		// Build lines: header, caic_pr, then enough assistant messages
		// to push caic_pr beyond the 64 KiB tail window.
		lines := []string{meta, prMsg}
		bigText := string(make([]byte, 1024)) // 1 KiB of null bytes per message
		for i := 0; i < 80; i++ {             // 80 KiB of filler
			lines = append(lines, claudeAssistant(t, map[string]any{"type": "text", "text": bigText}))
		}
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		lines = append(lines, trailer)
		writeLogFile(t, dir, "3-r-caic-3.jsonl", lines...)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		// Header-only parse misses caic_pr (outside 64 KiB tail).
		if lt.ForgePR != 0 {
			t.Fatalf("expected header-only parse to miss caic_pr outside tail window, got ForgePR=%d", lt.ForgePR)
		}
		// Full parse via LoadMessages must recover the PR.
		setClaudeParser(tasks)
		if err := lt.LoadMessages(); err != nil {
			t.Fatal(err)
		}
		if lt.ForgePR != 77 {
			t.Errorf("ForgePR = %d after LoadMessages, want 77", lt.ForgePR)
		}
		if lt.ForgeOwner != "acme" {
			t.Errorf("ForgeOwner = %q, want %q", lt.ForgeOwner, "acme")
		}
		if lt.ForgeRepo != "widget" {
			t.Errorf("ForgeRepo = %q, want %q", lt.ForgeRepo, "widget")
		}
	})
}

func TestParseState(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want State
	}{
		{"failed", StateFailed},
		{"purged", StatePurged},
		{"unknown", StateFailed},
	} {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseState(tt.in); got != tt.want {
				t.Errorf("parseState(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
