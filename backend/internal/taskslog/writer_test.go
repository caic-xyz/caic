// Tests for Writer log segment creation, trailers, and validated reopening.

package taskslog

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

type testTask struct {
	ID            ksid.ID
	InitialPrompt agent.Prompt
	Repos         []RepoMount
	Harness       harness.Name
	Model         string
	Effort        string
}

func (t *testTask) logFilename() string {
	repo, branch := "", ""
	if len(t.Repos) > 0 {
		repo = strings.ReplaceAll(t.Repos[0].Name, "/", "-")
		branch = strings.ReplaceAll(t.Repos[0].Branch, "/", "-")
	}
	return t.ID.String() + "-" + repo + "-" + branch + ".jsonl"
}

func (t *testTask) logHeader() *agent.MetaMessage {
	repos := make([]agent.MetaRepo, len(t.Repos))
	for i, repo := range t.Repos {
		repos[i] = agent.MetaRepo{Name: repo.Name, BaseBranch: repo.BaseBranch, Branch: repo.Branch, ContainerPath: repo.ContainerPath}
	}
	return &agent.MetaMessage{MessageType: "caic_meta", Prompt: t.InitialPrompt.Text, Repos: repos, Harness: t.Harness, Model: t.Model, Effort: t.Effort}
}

type testTaskLog struct {
	agent.LogSink

	path string
}

func openTaskLog(s *Writer, tk *testTask) (*testTaskLog, error) {
	if s.LogDir == "" {
		return nil, errors.New("no log dir")
	}
	log, path, err := s.Open(tk.logFilename(), tk.logHeader())
	if err != nil {
		return nil, err
	}
	return &testTaskLog{LogSink: log, path: path}, nil
}

func reopenTaskLog(s *Writer, tk *testTask, path string) (*testTaskLog, error) {
	name := tk.logFilename()
	if path != "" {
		name = filepath.Base(path)
	} else if s.LogDir == "" {
		return nil, errors.New("no log dir")
	}
	log, resolved, err := s.Reopen(name, tk.logHeader())
	if err != nil {
		return nil, err
	}
	return &testTaskLog{LogSink: log, path: resolved}, nil
}

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

func TestWriter(t *testing.T) {
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
		if err := (&Writer{}).WriteResultTrailer(b, "title", res); err != nil {
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
		store := &Writer{LogDir: t.TempDir()}
		tk := &testTask{InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		log, err := openTaskLog(store, tk)
		if err != nil {
			t.Fatal(err)
		}
		plainPath := log.path
		compressed, err := store.Compress(log.path, log, StateFailed)
		if err != nil {
			t.Fatal(err)
		}
		log.path = compressed
		if !isLogCompressed(log.path) {
			t.Fatalf("compressed log path = %q, want compressed", log.path)
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
		store := &Writer{LogDir: t.TempDir()}
		tk := &testTask{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test prompt"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0", ContainerPath: "~/src/org/repo"}},
			Harness:       harness.Codex,
			Model:         "model-1",
			Effort:        "high",
		}

		w, err := openTaskLog(store, tk)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		entries := logLines(t, w.path)
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
	t.Run("OpenRejectsNonBaseFilename", func(t *testing.T) {
		t.Parallel()
		store := &Writer{LogDir: t.TempDir()}
		header := agent.MetaMessage{MessageType: "caic_meta", Harness: harness.Codex}
		for _, name := range []string{"", ".", "..", "/etc/passwd", "sub/task.jsonl", "../task.jsonl"} {
			if _, _, err := store.Open(name, &header); err == nil {
				t.Errorf("Open(%q) = nil error, want rejection", name)
			}
		}
	})
	t.Run("ReopenRejectsNonBaseFilename", func(t *testing.T) {
		t.Parallel()
		store := &Writer{LogDir: t.TempDir()}
		header := agent.MetaMessage{MessageType: "caic_meta", Harness: harness.Codex}
		for _, name := range []string{"", ".", "..", "/etc/passwd", "sub/task.jsonl", "../task.jsonl"} {
			if _, _, err := store.Reopen(name, &header); err == nil {
				t.Errorf("Reopen(%q) = nil error, want rejection", name)
			}
		}
	})
	t.Run("OpenPreservesExistingLogAuthority", func(t *testing.T) {
		t.Parallel()
		store := &Writer{LogDir: t.TempDir()}
		tk := &testTask{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		first, err := openTaskLog(store, tk)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		second, err := openTaskLog(store, tk)
		if err != nil {
			t.Fatal(err)
		}
		if got := second.LogVersion(); got != agent.LogVersionV2 {
			t.Fatalf("LogVersion = %d, want %d", got, agent.LogVersionV2)
		}
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
		entries := logLines(t, second.path)
		if len(entries) != 1 {
			t.Fatalf("log lines = %d, want unchanged header", len(entries))
		}
	})
	t.Run("OpenCreatesCanonicalV2Log", func(t *testing.T) {
		t.Parallel()
		store := &Writer{LogDir: t.TempDir()}
		tk := &testTask{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		log, err := openTaskLog(store, tk)
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
		got, err := os.ReadFile(log.path)
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
		store := &Writer{LogDir: dir}
		tk := &testTask{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
			Harness:       harness.Codex,
		}
		path := filepath.Join(dir, tk.logFilename())
		line := `{"t":"caic_meta","version":2,"prompt":"test","repos":[{"name":"org/repo","branch":"caic-0"}],"harness":"codex"}` + "\n"
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}

		log, err := reopenTaskLog(store, tk, path)
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
		store := &Writer{LogDir: dir}
		tk := &testTask{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		path := filepath.Join(dir, tk.logFilename())
		header := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "test", Harness: harness.Codex}) + "\n"
		if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
		log, err := reopenTaskLog(store, tk, path)
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
		store := &Writer{LogDir: dir}
		tk := &testTask{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
			Harness:       harness.Codex,
		}
		path := filepath.Join(dir, tk.logFilename())
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

		if _, err := reopenTaskLog(store, tk, path); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("Reopen error = %v, want harness mismatch", err)
		}
	})

	t.Run("Reopen", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tk := &testTask{ID: ksid.NewID(), Harness: harness.Codex}
		path := filepath.Join(dir, tk.logFilename())
		header := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "test", Harness: harness.Codex}) + "\n"
		if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := reopenTaskLog(&Writer{LogDir: dir}, tk, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
