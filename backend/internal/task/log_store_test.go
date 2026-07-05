// Tests for LogStore log segment creation, trailers, and replay attachment.

package task

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
)

type fakeReplayWriter struct {
	messages []agent.Message
	commits  []string
}

func (w *fakeReplayWriter) WriteMessage(msg agent.Message) {
	w.messages = append(w.messages, msg)
}

func (w *fakeReplayWriter) Commit(logPath string) {
	w.commits = append(w.commits, logPath)
}

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
		replay := &fakeReplayWriter{}
		var gotPath string
		var gotHarness harness.Name
		store := &LogStore{
			LogDir: t.TempDir(),
			EventReplayFactory: func(logPath string, h harness.Name) EventReplayWriter {
				gotPath = logPath
				gotHarness = h
				return replay
			},
		}
		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test prompt"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0", MountedPath: "~/src/org/repo"}},
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
		if meta.MessageType != "caic_meta" || meta.Prompt != "test prompt" || meta.Model != "model-1" || meta.Effort != "high" {
			t.Fatalf("unexpected metadata: %+v", meta)
		}
		if len(meta.Repos) != 1 || meta.Repos[0].MountedPath != "~/src/org/repo" {
			t.Fatalf("metadata repos = %+v", meta.Repos)
		}

		tk.addMessage(t.Context(), &agent.TextMessage{Text: "hello"}, false)
		if len(replay.messages) != 1 {
			t.Fatalf("replay messages = %d, want 1", len(replay.messages))
		}
	})
}
