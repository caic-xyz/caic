// Tests for LogStore log segment creation, trailers, and validated reopening.

package task

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func logLines(t *testing.T, path string) []string {
	r, err := openLogReader(path)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestLogStore(t *testing.T) {
	t.Parallel()
	t.Run("WriteResultTrailerReasoningTokens", func(t *testing.T) {
		t.Parallel()
		b := &agenttest.LogSink{Version: agent.LogVersionV2}
		res := &Result{
			State: StateWaiting,
			Usage: agent.Usage{
				ReasoningOutputTokens: 123,
			},
		}
		if err := (&LogStore{}).WriteResultTrailer(b, "title", res); err != nil {
			t.Fatal(err)
		}
		var got agent.MetaResultMessage
		if err := json.Unmarshal(b.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ReasoningOutputTokens != 123 {
			t.Errorf("ReasoningOutputTokens = %d, want 123", got.ReasoningOutputTokens)
		}
	})
	t.Run("CompressClosesWriterBeforeCompression", func(t *testing.T) {
		t.Parallel()
		store := &LogStore{LogDir: t.TempDir()}
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		log, err := store.Open(tk)
		if err != nil {
			t.Fatal(err)
		}
		plainPath := tk.LogPath()
		if err := store.Compress(tk, log, StateFailed); err != nil {
			t.Fatal(err)
		}
		if !isLogCompressed(tk.LogPath()) {
			t.Fatalf("compressed log path = %q, want compressed", tk.LogPath())
		}
		if _, err := os.Stat(plainPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plain log stat = %v, want os.ErrNotExist", err)
		}
		if err := log.AppendMessage(&agent.LogMessage{MessageType: "caic_log", Line: "after compression"}); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("append after compression error = %v, want os.ErrClosed", err)
		}
	})
	t.Run("OpenWritesMetadata", func(t *testing.T) {
		t.Parallel()
		store := &LogStore{LogDir: t.TempDir()}
		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test prompt"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0", ContainerPath: "~/src/org/repo"}},
			Harness:       harness.Codex,
			Model:         "model-1",
			Effort:        "high",
		}

		w, err := store.Open(tk)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		entries := logLines(t, tk.LogPath())
		if len(entries) != 1 {
			t.Fatalf("log lines = %d, want 1", len(entries))
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(entries[0]), &raw); err != nil {
			t.Fatal(err)
		}
		if string(raw["t"]) != `"caic_meta"` {
			t.Fatalf("metadata discriminator = %s, want caic_meta", raw["t"])
		}
		var meta agent.MetaMessage
		if err := json.Unmarshal([]byte(entries[0]), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Version != int(agent.LogVersionV2) || meta.Prompt != "test prompt" || meta.Model != "model-1" || meta.Effort != "high" {
			t.Fatalf("unexpected metadata: %+v", meta)
		}
		if len(meta.Repos) != 1 || meta.Repos[0].ContainerPath != "~/src/org/repo" {
			t.Fatalf("metadata repos = %+v", meta.Repos)
		}
	})
	t.Run("OpenPreservesExistingLogAuthority", func(t *testing.T) {
		t.Parallel()
		store := &LogStore{LogDir: t.TempDir()}
		tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		first, err := store.Open(tk)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		second, err := store.Open(tk)
		if err != nil {
			t.Fatal(err)
		}
		if got := second.LogVersion(); got != agent.LogVersionV2 {
			t.Fatalf("LogVersion = %d, want %d", got, agent.LogVersionV2)
		}
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
		entries := logLines(t, tk.LogPath())
		if len(entries) != 1 {
			t.Fatalf("log lines = %d, want unchanged header", len(entries))
		}
	})
	t.Run("OpenCreatesCanonicalV2Log", func(t *testing.T) {
		t.Parallel()
		store := &LogStore{LogDir: t.TempDir()}
		tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		log, err := store.Open(tk)
		if err != nil {
			t.Fatal(err)
		}
		if got := log.LogVersion(); got != agent.LogVersionV2 {
			t.Fatalf("LogVersion = %d, want %d", got, agent.LogVersionV2)
		}
		if err := log.AppendMessage(&agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "acme", ForgeRepo: "repo", ForgePR: 1}); err != nil {
			t.Fatal(err)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(tk.LogPath())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), `"t":"caic_meta"`) || !strings.Contains(string(got), `"t":"pr"`) {
			t.Fatalf("v2 log = %s, want canonical metadata and control", got)
		}
	})
	t.Run("ReopenPreservesV2Authority", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		store := &LogStore{LogDir: dir}
		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
			Harness:       harness.Codex,
		}
		path := filepath.Join(dir, taskLogFileName(tk))
		line := `{"t":"caic_meta","version":2,"prompt":"test","repos":[{"name":"org/repo","branch":"caic-0"}],"harness":"codex"}` + "\n"
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}

		log, err := store.Reopen(tk)
		if err != nil {
			t.Fatal(err)
		}
		if got := log.LogVersion(); got != agent.LogVersionV2 {
			t.Fatalf("LogVersion = %d, want %d", got, agent.LogVersionV2)
		}
		if err := log.AppendMessage(&agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "acme", ForgeRepo: "repo", ForgePR: 1}); err != nil {
			t.Fatal(err)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), `"t":"pr"`) {
			t.Fatalf("v2 log = %s, want canonical pr control", got)
		}
	})
	t.Run("ReopenPreservesV1Authority", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		store := &LogStore{LogDir: dir}
		tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		path := filepath.Join(dir, taskLogFileName(tk))
		header := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "test", Harness: harness.Codex}) + "\n"
		if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
		log, err := store.Reopen(tk)
		if err != nil {
			t.Fatal(err)
		}
		if got := log.LogVersion(); got != agent.LogVersionV1 {
			t.Fatalf("LogVersion = %d, want %d", got, agent.LogVersionV1)
		}
		if err := log.AppendMessage(&agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "acme", ForgeRepo: "repo", ForgePR: 1}); err != nil {
			t.Fatal(err)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), `"type":"caic_pr"`) {
			t.Fatalf("v1 log = %s, want legacy pr control", got)
		}
	})
	t.Run("ReopenRejectsHarnessMismatch", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		store := &LogStore{LogDir: dir}
		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
			Harness:       harness.Codex,
		}
		path := filepath.Join(dir, taskLogFileName(tk))
		line := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV1),
			Prompt:      "test",
			Repos:       []agent.MetaRepo{{Name: "org/repo", Branch: "caic-0"}},
			Harness:     harness.Claude,
		}) + "\n"
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := store.Reopen(tk); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("Reopen error = %v, want harness mismatch", err)
		}
	})

	t.Run("Reopen", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tk := &Task{ID: ksid.NewID(), Harness: harness.Codex}
		path := filepath.Join(dir, taskLogFileName(tk))
		header := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "test", Harness: harness.Codex}) + "\n"
		if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
		tk.SetLogPath(path)
		w, err := (&LogStore{LogDir: dir}).Reopen(tk)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
