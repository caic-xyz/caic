// Tests for LogStore log segment creation, trailers, and replay attachment.

package task

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task/tasktest"
)

func logLines(t *testing.T, path string) []string {
	data, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestLogStore(t *testing.T) {
	t.Parallel()
	t.Run("WriteResultTrailerReasoningTokens", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		res := &Result{
			State: StateWaiting,
			Usage: agent.Usage{
				ReasoningOutputTokens: 123,
			},
		}
		if err := (&LogStore{}).WriteResultTrailer(&b, "title", res); err != nil {
			t.Fatal(err)
		}
		var got agent.MetaResultMessage
		if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
			t.Fatal(err)
		}
		if got.ReasoningOutputTokens != 123 {
			t.Errorf("ReasoningOutputTokens = %d, want 123", got.ReasoningOutputTokens)
		}
	})
	t.Run("OpenAttachesReplayAndWritesMetadata", func(t *testing.T) {
		t.Parallel()
		replay := &tasktest.FakeEventReplayWriter{}
		var gotPath string
		var gotHarness harness.Name
		store := &LogStore{
			LogDir: t.TempDir(),
			EventReplayFactory: func(logPath string, h harness.Name) (EventReplayWriter, error) {
				gotPath = logPath
				gotHarness = h
				return replay, nil
			},
		}
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
		if gotPath == "" || gotPath != tk.LogPath() {
			t.Fatalf("replay path = %q, task path = %q", gotPath, tk.LogPath())
		}
		if gotHarness != harness.Codex {
			t.Fatalf("replay harness = %q, want %q", gotHarness, harness.Codex)
		}

		entries := logLines(t, tk.LogPath())
		if len(entries) != 1 {
			t.Fatalf("log lines = %d, want 1", len(entries))
		}
		var meta agent.MetaMessage
		if err := json.Unmarshal([]byte(entries[0]), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.MessageType != "caic_meta" || meta.Version != int(agent.LogVersionV1) || meta.Prompt != "test prompt" || meta.Model != "model-1" || meta.Effort != "high" {
			t.Fatalf("unexpected metadata: %+v", meta)
		}
		if len(meta.Repos) != 1 || meta.Repos[0].ContainerPath != "~/src/org/repo" {
			t.Fatalf("metadata repos = %+v", meta.Repos)
		}

		tk.addMessage(t.Context(), &agent.TextMessage{Text: "hello"}, false)
		if len(replay.Messages) != 1 {
			t.Fatalf("replay messages = %d, want 1", len(replay.Messages))
		}
	})
	t.Run("ReopenRejectsV2WithoutMutation", func(t *testing.T) {
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
			Version:     int(agent.LogVersionV2),
			Prompt:      "test",
			Repos:       []agent.MetaRepo{{Name: "org/repo", Branch: "caic-0"}},
			Harness:     harness.Codex,
		}) + "\n"
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := store.Reopen(tk); err == nil || !strings.Contains(err.Error(), "requires versioned log sink") {
			t.Fatalf("Reopen error = %v, want v2 sink error", err)
		}
		if _, err := store.Open(tk); err == nil || !strings.Contains(err.Error(), "requires versioned log sink") {
			t.Fatalf("Open error = %v, want v2 sink error", err)
		}
		got, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != line {
			t.Fatalf("v2 log mutated:\n%s", got)
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
	t.Run("ReopenWritesValidatedInodeAfterPathReplacement", func(t *testing.T) {
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
		header := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV1),
			Prompt:      "test",
			Repos:       []agent.MetaRepo{{Name: "org/repo", Branch: "caic-0"}},
			Harness:     harness.Codex,
		}) + "\n"
		if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := store.Reopen(tk)
		if err != nil {
			t.Fatal(err)
		}
		validatedPath := path + ".validated"
		if err := os.Rename(path, validatedPath); err != nil {
			t.Fatal(err)
		}
		const replacement = "unvalidated replacement\n"
		if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
			t.Fatal(err)
		}
		const appendLine = "{\"type\":\"caic_result\",\"state\":\"waiting\"}\n"
		if _, err := w.Write([]byte(appendLine)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		gotReplacement, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if string(gotReplacement) != replacement {
			t.Fatalf("replacement was mutated: %q", gotReplacement)
		}
		gotValidated, err := os.ReadFile(validatedPath) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		if string(gotValidated) != header+appendLine {
			t.Fatalf("validated inode = %q, want header plus append", gotValidated)
		}
	})
	t.Run("OpenSurfacesReplayFactoryError", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("replay unavailable")
		store := &LogStore{
			LogDir: t.TempDir(),
			EventReplayFactory: func(string, harness.Name) (EventReplayWriter, error) {
				return nil, wantErr
			},
		}
		tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test prompt"}, Harness: harness.Codex}

		w, err := store.Open(tk)
		if w != nil {
			t.Fatal("Open returned writer with replay factory error")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("Open error = %v, want %v", err, wantErr)
		}
	})
}
