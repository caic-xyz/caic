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

func TestTaskCacheProofForLog(t *testing.T) {
	t.Parallel()
	newTask := func(t *testing.T) (*Task, string, *ValidatedLogSnapshot) {
		dir := t.TempDir()
		tk := &Task{ID: ksid.NewID(), Harness: harness.Codex}
		path := filepath.Join(dir, taskLogFileName(tk))
		header := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "proof", Harness: harness.Codex})
		if err := os.WriteFile(path, []byte(header+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := loadSemanticLogSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tk.SetLogPath(path)
		return tk, path, snapshot
	}

	t.Run("rejects replacement identity", func(t *testing.T) {
		t.Parallel()
		tk, path, snapshot := newTask(t)
		tk.SetLogValidationSnapshot(snapshot)
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(snapshot.RawHeader+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := tk.CacheProofForLog(path); err == nil {
			t.Fatal("replacement identity was accepted by retained task proof")
		}
	})
	t.Run("rejects header mutation", func(t *testing.T) {
		t.Parallel()
		tk, path, snapshot := newTask(t)
		tk.SetLogValidationSnapshot(snapshot)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		mutated := strings.Replace(snapshot.RawHeader, "proof", "other", 1)
		if len(mutated) != len(snapshot.RawHeader) {
			t.Fatal("test header mutation changed length")
		}
		if err := os.WriteFile(path, []byte(mutated+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		if _, err := tk.CacheProofForLog(path); err == nil {
			t.Fatal("same-stat header mutation was accepted by retained task proof")
		}
	})
	t.Run("rejects snapshot without EOF", func(t *testing.T) {
		t.Parallel()
		tk, path, snapshot := newTask(t)
		invalid := *snapshot
		invalid.EOFValidated = false
		tk.SetLogValidationSnapshot(&invalid)
		if _, err := tk.CacheProofForLog(path); err == nil {
			t.Fatal("non-EOF snapshot was accepted by task proof provider")
		}
	})
}

func TestLogStore(t *testing.T) {
	t.Parallel()
	t.Run("WriteResultTrailerReasoningTokens", func(t *testing.T) {
		t.Parallel()
		b := &testLogSink{}
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
	t.Run("OpenAttachesReplayAndWritesMetadata", func(t *testing.T) {
		t.Parallel()
		replay := &tasktest.FakeEventReplayWriter{}
		var gotPath string
		store := &LogStore{
			LogDir: t.TempDir(),
			EventReplayFactory: func(logPath string, _ CacheProof, _ CacheProofProvider) (EventReplayWriter, error) {
				gotPath = logPath
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
		snapshot, err := loadSemanticLogSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tk.SetLogPath(path)
		tk.SetLogValidationSnapshot(snapshot)
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
		if err := w.AppendNative([]byte(appendLine)); err != nil {
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
	t.Run("ReopenSnapshotRejectsPathReplacementBeforeReturn", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tk := &Task{ID: ksid.NewID(), Harness: harness.Codex}
		path := filepath.Join(dir, taskLogFileName(tk))
		header := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     int(agent.LogVersionV1),
			Prompt:      "test",
			Harness:     harness.Codex,
		}) + "\n"
		if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := loadSemanticLogSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		appendFile, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600) //nolint:gosec // path is test-controlled.
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = appendFile.Close() })
		if err := os.Rename(path, path+".validated"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := validateRawLogAppendSnapshot(path, appendFile, tk, snapshot); err == nil || !strings.Contains(err.Error(), "replaced") {
			t.Fatalf("snapshot append validation error = %v, want path replacement", err)
		}
	})
	t.Run("ReopenPassesInitialProofAndFreshProvider", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tk := &Task{ID: ksid.NewID(), Harness: harness.Codex}
		path := filepath.Join(dir, taskLogFileName(tk))
		header := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "test", Harness: harness.Codex}) + "\n"
		if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := loadSemanticLogSnapshot(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tk.SetLogPath(path)
		tk.SetLogValidationSnapshot(snapshot)
		var initial CacheProof
		var fresh CacheProofProvider
		store := &LogStore{LogDir: dir, EventReplayFactory: func(_ string, proof CacheProof, provider CacheProofProvider) (EventReplayWriter, error) {
			initial = proof
			fresh = provider
			return &tasktest.FakeEventReplayWriter{}, nil
		}}
		w, err := store.Reopen(tk)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if initial != snapshot.cacheProof() {
			t.Fatalf("initial replay proof = %#v, want Reopen observation %#v", initial, snapshot.cacheProof())
		}
		if fresh == nil {
			t.Fatal("Reopen did not provide a fresh task proof provider")
		}
		proof, err := fresh(path)
		if err != nil {
			t.Fatal(err)
		}
		if proof != initial {
			t.Fatalf("fresh replay proof = %#v, want %#v", proof, initial)
		}
	})
	t.Run("OpenSurfacesReplayFactoryError", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("replay unavailable")
		store := &LogStore{
			LogDir: t.TempDir(),
			EventReplayFactory: func(string, CacheProof, CacheProofProvider) (EventReplayWriter, error) {
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
