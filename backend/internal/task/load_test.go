// Tests for task loading and configuration resolution.

package task

import (
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/harness"
)

func setClaudeParser(tasks []*LoadedTask) {
	for _, lt := range tasks {
		lt.SetParser(claudecode.New().NewWire().ParseMessage)
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

func seqOf(lines ...string) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for _, line := range lines {
			if !yield([]byte(line)) {
				return
			}
		}
	}
}

func writeCompressedLogFile(t *testing.T, dir, name string, lines iter.Seq[[]byte]) {
	if !isLogCompressed(name) {
		t.Fatalf("compressed test log name %q must end in %s", name, logCompressedExt)
	}
	out, err := os.OpenFile(filepath.Clean(filepath.Join(dir, name)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	var writeErr error
	for line := range lines {
		if _, err := enc.Write(line); err != nil {
			writeErr = err
			break
		}
		if _, err := enc.Write([]byte("\n")); err != nil {
			writeErr = err
			break
		}
	}
	if err := errors.Join(writeErr, enc.Close(), out.Close()); err != nil {
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
	t.Parallel()
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
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
	t.Run("LaunchConfigMetadata", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType:       "caic_meta",
			Version:           1,
			Prompt:            "task1",
			Repos:             []agent.MetaRepo{{Name: "r", Branch: "caic-0"}},
			Harness:           "claude",
			BaseImage:         "ghcr.io/caic/base:v1",
			ContainerPlatform: "linux/amd64",
			MaxCPUs:           6,
			CacheMounts:       []agent.MetaCacheMount{{Name: "npm", Description: "Node", HostPath: "~/.npm", MountPath: "/home/user/.npm", ReadOnly: true, Shallow: true}},
			Mounts:            []agent.MetaMount{{HostPath: "/host/work", MountPath: "/workspace/work", ReadOnly: true}},
		})
		writeLogFile(t, dir, "a.jsonl", meta)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if lt.BaseImage != "ghcr.io/caic/base:v1" || lt.ContainerPlatform != "linux/amd64" || lt.MaxCPUs != 6 {
			t.Fatalf("launch config = image %q platform %q cpus %d", lt.BaseImage, lt.ContainerPlatform, lt.MaxCPUs)
		}
		if len(lt.CacheMounts) != 1 || lt.CacheMounts[0].Name != "npm" || !lt.CacheMounts[0].ReadOnly || !lt.CacheMounts[0].Shallow {
			t.Errorf("CacheMounts = %+v", lt.CacheMounts)
		}
		if len(lt.Mounts) != 1 || lt.Mounts[0].HostPath != "/host/work" || !lt.Mounts[0].ReadOnly {
			t.Errorf("Mounts = %+v", lt.Mounts)
		}
	})
	t.Run("ResultReasoningTokens", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", ReasoningOutputTokens: 123})
		writeLogFile(t, dir, "a.jsonl", meta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].Result == nil {
			t.Fatal("Result is nil")
		}
		if tasks[0].Result.Usage.ReasoningOutputTokens != 123 {
			t.Errorf("ReasoningOutputTokens = %d, want 123", tasks[0].Result.Usage.ReasoningOutputTokens)
		}
	})
	t.Run("ValidCompressed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "compressed", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		asst := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "octo", ForgeRepo: "repo", ForgePR: 7})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(meta, asst, prMsg, trailer))

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if lt.Prompt != "compressed" {
			t.Errorf("Prompt = %q, want compressed", lt.Prompt)
		}
		if lt.State != StatePurged {
			t.Errorf("State = %v, want StatePurged", lt.State)
		}
		if lt.ForgePR != 7 {
			t.Errorf("ForgePR = %d, want 7", lt.ForgePR)
		}
		if !isLogCompressed(lt.LogPath()) {
			t.Errorf("LogPath = %q, want compressed path", lt.LogPath())
		}
	})
	t.Run("PreferCompressedDuplicate", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		plainMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plain", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		compressedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "compressed", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(compressedMeta, trailer))
		writeLogFile(t, dir, "a.jsonl", plainMeta, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].Prompt != "compressed" {
			t.Errorf("Prompt = %q, want compressed", tasks[0].Prompt)
		}
	})
	t.Run("ForTaskIDs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		wantedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "wanted", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		unrelatedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "unrelated", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
		writeLogFile(t, dir, "live1-repo-branch.jsonl", wantedMeta)
		writeLogFile(t, dir, "live10-repo-branch.jsonl", unrelatedMeta)

		tasks, err := LoadLogsForTaskIDs(dir, []string{"live1"})
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].TaskID != "live1" || tasks[0].Prompt != "wanted" {
			t.Errorf("task = (%q, %q), want (live1, wanted)", tasks[0].TaskID, tasks[0].Prompt)
		}
	})
	t.Run("NotExist", func(t *testing.T) {
		t.Parallel()
		tasks, err := LoadLogs(filepath.Join(t.TempDir(), "nope"))
		if err != nil {
			t.Fatal(err)
		}
		if tasks != nil {
			t.Error("expected nil for nonexistent dir")
		}
	})
	t.Run("BadHeader", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "feat task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
			Model: "model-1", Effort: "high",
			Tailscale: true, USB: true, Display: true, Sudo: true, GitHubToken: true,
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
		if lt.Model != "model-1" {
			t.Errorf("Model = %q, want model-1", lt.Model)
		}
		if lt.Effort != "high" {
			t.Errorf("Effort = %q, want high", lt.Effort)
		}
		if !lt.Sudo {
			t.Error("Sudo = false, want true")
		}
		if !lt.GitHubToken {
			t.Error("GitHubToken = false, want true")
		}
	})
	t.Run("FeatureFlagsOmitted", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
	t.Run("SessionMetadata", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "session task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Codex,
		})
		session := mustJSON(t, agent.MetaSessionMessage{
			MessageType:  "caic_session",
			SessionID:    "thread-1",
			Model:        "gpt-5.4",
			AgentVersion: "1.2.3",
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "session.jsonl", meta, session, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		lt := tasks[0]
		if lt.SessionID != "thread-1" {
			t.Errorf("SessionID = %q, want thread-1", lt.SessionID)
		}
		if lt.Model != "gpt-5.4" {
			t.Errorf("Model = %q, want gpt-5.4", lt.Model)
		}
		if lt.AgentVersion != "1.2.3" {
			t.Errorf("AgentVersion = %q, want 1.2.3", lt.AgentVersion)
		}
	})
	t.Run("AgentVersionMetadataWithoutSession", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "pi task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Pi,
		})
		session := mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", AgentVersion: "pi 1.2.3"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "pi.jsonl", meta, session, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("len = %d, want 1", len(tasks))
		}
		if tasks[0].AgentVersion != "pi 1.2.3" {
			t.Errorf("AgentVersion = %q, want pi 1.2.3", tasks[0].AgentVersion)
		}
	})
	t.Run("LoadSessionMetadataScansBeyondTail", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "long task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Codex,
		})
		session := mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "thread-old"})
		large := `{"text":"` + strings.Repeat("x", 70<<10) + `"}`
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "long.jsonl", meta, session, large, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		if lt.SessionID != "" {
			t.Fatalf("SessionID = %q before explicit metadata scan, want empty", lt.SessionID)
		}
		if err := lt.LoadSessionMetadata(); err != nil {
			t.Fatal(err)
		}
		if lt.SessionID != "thread-old" {
			t.Errorf("SessionID = %q, want thread-old", lt.SessionID)
		}
	})
	t.Run("LoadSessionMetadataScansLegacyInitMessage", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "legacy codex task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Codex,
		})
		init := `{"method":"thread/started","params":{"thread":{"id":"thread-from-started","cliVersion":"1.0","createdAt":1,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"","source":"user","status":{"type":"idle"},"updatedAt":2}}}`
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "legacy-codex.jsonl", meta, init, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		lt.SetParser(codex.New("", nil).NewWire().ParseMessage)
		if err := lt.LoadSessionMetadata(); err != nil {
			t.Fatal(err)
		}
		if lt.SessionID != "thread-from-started" {
			t.Errorf("SessionID = %q, want thread-from-started", lt.SessionID)
		}
	})
	t.Run("LegacyCaicInitSessionMetadata", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "legacy task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.OpenCode,
		})
		init := `{"type":"caic_init","session_id":"ses-legacy","model":"m","version":"v"}`
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
		writeLogFile(t, dir, "legacy.jsonl", meta, init, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		lt := tasks[0]
		if lt.SessionID != "ses-legacy" {
			t.Errorf("SessionID = %q, want ses-legacy", lt.SessionID)
		}
		if lt.Model != "m" || lt.AgentVersion != "v" {
			t.Errorf("model/version = %q/%q, want m/v", lt.Model, lt.AgentVersion)
		}
	})
	t.Run("ContextClearedResetsPlanState", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
		// caic_pr early in the file, followed by >64 KiB of messages,
		// so the header-only tail scan cannot see it.
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "big task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-3"}}, Harness: "claude"})
		prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "acme", ForgeRepo: "widget", ForgePR: 77})

		// Build lines: header, caic_pr, then enough assistant messages
		// to push caic_pr beyond the 64 KiB tail window.
		lines := make([]string, 0, 83)
		lines = append(lines, meta, prMsg)
		bigText := string(make([]byte, 1024)) // 1 KiB of null bytes per message
		for range 80 {                        // 80 KiB of filler
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

func TestCompressTerminalLogs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
	asst := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
	trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
	writeLogFile(t, dir, "t.jsonl", meta, asst, trailer)

	tasks, err := LoadLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompressTerminalLogs(tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "t.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("plain log stat err = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "t.jsonl.zst")); err != nil {
		t.Fatal(err)
	}
	if !isLogCompressed(tasks[0].LogPath()) {
		t.Fatalf("LogPath = %q, want compressed path", tasks[0].LogPath())
	}
	setClaudeParser(tasks)
	if err := tasks[0].LoadMessages(); err != nil {
		t.Fatal(err)
	}
	if len(tasks[0].Msgs) != 1 {
		t.Errorf("Msgs len = %d, want 1", len(tasks[0].Msgs))
	}
}

func TestTsToTime(t *testing.T) {
	t.Parallel()
	// 1735689600.5 = 2025-01-01T00:00:00.5Z (exact in float64).
	ts := 1735689600.5
	got := tsToTime(ts)
	if got.Year() != 2025 || got.Month() != time.January || got.Day() != 1 {
		t.Errorf("tsToTime(%v) = %v", ts, got)
	}
	if got.Nanosecond() != 500000000 {
		t.Errorf("tsToTime(%v).Nanosecond() = %d, want 500000000", ts, got.Nanosecond())
	}
	if got.Location() != time.UTC {
		t.Error("tsToTime should return UTC")
	}
}

func TestLoadedTask(t *testing.T) {
	t.Parallel()
	t.Run("StreamMessages", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "stream task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		a1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		pr := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "o", ForgeRepo: "r", ForgePR: 5})
		a2 := claudeAssistant(t, map[string]any{"type": "text", "text": "world"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "waiting"})
		writeLogFile(t, dir, "t.jsonl", meta, a1, pr, a2, trailer)

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)
		lt := tasks[0]

		var streamed []agent.Message
		for m, e := range lt.StreamMessages() {
			if e != nil {
				t.Fatal(e)
			}
			streamed = append(streamed, m)
		}
		if len(streamed) == 0 {
			t.Fatal("no messages streamed")
		}
		// Streaming must yield exactly the conversation messages a full load
		// produces — control records (caic_meta/pr/result) filtered out.
		if err := lt.LoadMessages(); err != nil {
			t.Fatal(err)
		}
		if len(streamed) != len(lt.Msgs) {
			t.Fatalf("streamed %d messages, full load %d", len(streamed), len(lt.Msgs))
		}
	})

	t.Run("StreamMessagesCompressed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "stream task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
		a1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
		a2 := claudeAssistant(t, map[string]any{"type": "text", "text": "world"})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "waiting"})
		writeCompressedLogFile(t, dir, "t.jsonl.zst", seqOf(meta, a1, a2, trailer))

		tasks, err := LoadLogs(dir)
		if err != nil {
			t.Fatal(err)
		}
		setClaudeParser(tasks)
		lt := tasks[0]

		var streamed []agent.Message
		for m, e := range lt.StreamMessages() {
			if e != nil {
				t.Fatal(e)
			}
			streamed = append(streamed, m)
		}
		if len(streamed) != 2 {
			t.Fatalf("streamed %d messages, want 2", len(streamed))
		}
	})

	t.Run("Primary", func(t *testing.T) {
		t.Parallel()
		t.Run("NoRepos", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{}
			if lt.Primary() != nil {
				t.Error("Primary() should be nil for no-repo task")
			}
		})
		t.Run("WithRepos", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{Repos: []RepoMount{{Name: "a/b", Branch: "caic-0"}}}
			if p := lt.Primary(); p == nil || p.Name != "a/b" {
				t.Errorf("Primary() = %+v, want a/b", p)
			}
		})
	})

	t.Run("LoadMessages", func(t *testing.T) {
		t.Parallel()
		t.Run("AlreadyLoaded", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{Msgs: []agent.Message{&agent.TextMessage{Text: "cached"}}}
			if err := lt.LoadMessages(); err != nil {
				t.Fatal(err)
			}
			if len(lt.Msgs) != 1 {
				t.Errorf("Msgs mutated when already loaded")
			}
		})
		t.Run("NoPath", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{}
			lt.SetParser(claudecode.New().NewWire().ParseMessage)
			if err := lt.LoadMessages(); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("NoParser", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{path: "/does/not/exist.jsonl"}
			if err := lt.LoadMessages(); err == nil {
				t.Fatal("expected error when no parser is set")
			}
		})
		t.Run("LoadLogFileNoParser", func(t *testing.T) {
			t.Parallel()
			_, err := loadLogFile("/does/not/exist.jsonl", nil)
			if err == nil {
				t.Fatal("expected error when parseFn is nil")
			}
			if !strings.Contains(err.Error(), "parseFn is nil") {
				t.Errorf("error = %q, want parseFn nil error", err.Error())
			}
		})
		t.Run("StreamNoParser", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{path: "/does/not/exist.jsonl"}
			var gotErr bool
			for _, e := range lt.StreamMessages() {
				if e != nil {
					gotErr = true
				}
			}
			if !gotErr {
				t.Error("StreamMessages without parser should yield an error")
			}
		})
	})

	t.Run("LoadMessagesTail", func(t *testing.T) {
		t.Parallel()
		t.Run("SmallFileFullLoad", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			a1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", CostUSD: 0.42})
			writeLogFile(t, dir, "t.jsonl", meta, a1, trailer)

			tasks, err := LoadLogs(dir)
			if err != nil {
				t.Fatal(err)
			}
			lt := tasks[0]
			setClaudeParser(tasks)
			if err := lt.LoadMessagesTail(); err != nil {
				t.Fatal(err)
			}
			if len(lt.Msgs) != 1 {
				t.Errorf("Msgs len = %d, want 1", len(lt.Msgs))
			}
			if lt.Result == nil || lt.Result.CostUSD != 0.42 {
				t.Errorf("Result = %+v", lt.Result)
			}
		})
		t.Run("TailOnlyWhenSizeExceedsThreshold", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "big", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})

			name := filepath.Join(dir, "big.jsonl")
			f, err := os.OpenFile(filepath.Clean(name), os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString(meta + "\n")
			filler := strings.Repeat("x", 1024)
			for range (maxTailLoadBytes / 1024) + 5 {
				a := claudeAssistant(t, map[string]any{"type": "text", "text": filler})
				_, _ = f.WriteString(a + "\n")
			}
			_, _ = f.WriteString(trailer + "\n")
			_ = f.Close()

			lt := &LoadedTask{
				path:    name,
				Harness: "claude",
				LogSize: maxTailLoadBytes + 1,
			}
			lt.SetParser(claudecode.New().NewWire().ParseMessage)
			if err := lt.LoadMessagesTail(); err != nil {
				t.Fatal(err)
			}
			if lt.State != StatePurged {
				t.Errorf("State = %v, want StatePurged", lt.State)
			}
			if lt.Result == nil {
				t.Fatal("Result not restored from tail")
			}
		})
		t.Run("CompressedTailKeepsBoundedRecentLines", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "compressed", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			oldMsg := claudeAssistant(t, map[string]any{"type": "text", "text": "old output"})
			recentMsg := claudeAssistant(t, map[string]any{"type": "text", "text": "recent output"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeCompressedLogFile(t, dir, "compressed.jsonl.zst", seqOf(meta, oldMsg, recentMsg, trailer))

			path := filepath.Join(dir, "compressed.jsonl.zst")
			lt, err := loadLogFileTail(path, claudecode.New().NewWire().ParseMessage, int64(len(recentMsg)+len(trailer)+2))
			if err != nil {
				t.Fatal(err)
			}
			if lt.Result == nil || lt.State != StatePurged {
				t.Fatalf("Result = %+v, State = %v", lt.Result, lt.State)
			}
			if len(lt.Msgs) != 1 {
				t.Fatalf("Msgs len = %d, want 1", len(lt.Msgs))
			}
			msg, ok := lt.Msgs[0].(*agent.TextMessage)
			if !ok {
				t.Fatalf("Msgs[0] = %T, want *agent.TextMessage", lt.Msgs[0])
			}
			if msg.Text != "recent output" {
				t.Errorf("Text = %q, want recent output", msg.Text)
			}
		})
		t.Run("AlreadyLoaded", func(t *testing.T) {
			t.Parallel()
			lt := &LoadedTask{Msgs: []agent.Message{&agent.TextMessage{Text: "cached"}}}
			lt.SetParser(claudecode.New().NewWire().ParseMessage)
			if err := lt.LoadMessagesTail(); err != nil {
				t.Fatal(err)
			}
			if len(lt.Msgs) != 1 {
				t.Errorf("Msgs mutated when already loaded")
			}
		})
	})
}

func TestParseState(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want State
	}{
		{"failed", StateFailed},
		{"crashed", StateCrashed},
		{"stopped", StateStopped},
		{"purged", StatePurged},
		{"terminated", StatePurged}, // backward compat
		{"unknown", StateFailed},
	} {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := parseState(tt.in); got != tt.want {
				t.Errorf("parseState(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
