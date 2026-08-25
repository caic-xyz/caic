// Tests for Store: log segment open/reopen authority, result trailers, terminal log compression, and unsettled/settled/task-ID scans.

package taskslog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
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

func openTaskLog(s *Store, tk *testTask) (*testTaskLog, error) {
	if s.LogDir == "" {
		return nil, errors.New("no log dir")
	}
	log, path, err := s.Open(tk.logFilename(), tk.logHeader())
	if err != nil {
		return nil, err
	}
	return &testTaskLog{LogSink: log, path: path}, nil
}

func reopenTaskLog(s *Store, tk *testTask, path string) (*testTaskLog, error) {
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

func TestStore(t *testing.T) {
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
		if err := (&Store{}).WriteResultTrailer(b, "title", res); err != nil {
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
		store := NewStore(testLogger(), t.TempDir())
		tk := &testTask{InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		log, err := openTaskLog(store, tk)
		if err != nil {
			t.Fatal(err)
		}
		plainPath := log.path
		summary := &LoadedTask{State: StateFailed, LastTrailer: &Result{State: StateFailed}}
		compressed, err := store.CompressTerminal(log.path, log, summary)
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
		cached, ok := readHeaderCache(compressed)
		if !ok {
			t.Fatal("Compress did not publish a terminal header cache")
		}
		if cached.LastTrailer == nil || cached.LastTrailer.State != StateFailed {
			t.Fatalf("cached result = %#v, want failed", cached.LastTrailer)
		}
		if err := log.AppendMessage(&agent.LogMessage{MessageType: "caic_log", Line: "after compression"}); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("append after compression error = %v, want os.ErrClosed", err)
		}
	})
	t.Run("OpenWritesMetadata", func(t *testing.T) {
		t.Parallel()
		store := NewStore(testLogger(), t.TempDir())
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
		store := NewStore(testLogger(), t.TempDir())
		header := agent.MetaMessage{MessageType: "caic_meta", Harness: harness.Codex}
		for _, name := range []string{"", ".", "..", "/etc/passwd", "sub/task.jsonl", "../task.jsonl"} {
			if _, _, err := store.Open(name, &header); err == nil {
				t.Errorf("Open(%q) = nil error, want rejection", name)
			}
		}
	})
	t.Run("ReopenRejectsNonBaseFilename", func(t *testing.T) {
		t.Parallel()
		store := NewStore(testLogger(), t.TempDir())
		header := agent.MetaMessage{MessageType: "caic_meta", Harness: harness.Codex}
		for _, name := range []string{"", ".", "..", "/etc/passwd", "sub/task.jsonl", "../task.jsonl"} {
			if _, _, err := store.Reopen(name, &header); err == nil {
				t.Errorf("Reopen(%q) = nil error, want rejection", name)
			}
		}
	})
	t.Run("OpenPreservesExistingLogAuthority", func(t *testing.T) {
		t.Parallel()
		store := NewStore(testLogger(), t.TempDir())
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
		store := NewStore(testLogger(), t.TempDir())
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
		store := NewStore(testLogger(), dir)
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
		store := NewStore(testLogger(), dir)
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
		store := NewStore(testLogger(), dir)
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
		w, err := reopenTaskLog(NewStore(testLogger(), dir), tk, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("CompressPath", func(t *testing.T) {
		t.Parallel()
		t.Run("Success", func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "task.jsonl")
			const contents = "first record\nsecond record\n"
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}

			compressed, err := (&Store{}).compressPath(path)
			if err != nil {
				t.Fatal(err)
			}
			if compressed != compressedLogPath(path) {
				t.Fatalf("compressed path = %q, want %q", compressed, compressedLogPath(path))
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("plain log stat = %v, want os.ErrNotExist", err)
			}
			r, err := openLogReader(compressed)
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
			if string(data) != contents {
				t.Fatalf("compressed contents = %q, want %q", data, contents)
			}
		})

		t.Run("FailurePreservesPlainSource", func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "task.jsonl")
			const contents = "task log\n"
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(compressedLogPath(path), 0o700); err != nil {
				t.Fatal(err)
			}

			if _, err := (&Store{}).compressPath(path); err == nil {
				t.Fatal("compressPath returned nil error")
			}
			data, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != contents {
				t.Fatalf("plain contents = %q, want %q", data, contents)
			}
			if _, err := os.Stat(compressedLogTempPath(path)); !os.IsNotExist(err) {
				t.Fatalf("temporary compressed log stat = %v, want os.ErrNotExist", err)
			}
		})
	})
	t.Run("SettleTerminal", func(t *testing.T) {
		t.Parallel()
		t.Run("CompressesPurged", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "purged.jsonl")
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: "task"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeLogFile(t, dir, filepath.Base(path), meta, trailer)
			updated := time.Date(2024, time.June, 1, 2, 3, 4, 0, time.UTC)
			if err := os.Chtimes(path, updated, updated); err != nil {
				t.Fatal(err)
			}
			if err := (NewStore(testLogger(), dir)).SettleTerminal(nil); err != nil {
				t.Fatal(err)
			}
			compressed := compressedLogPath(path)
			info, err := os.Stat(compressed)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(updated) {
				t.Fatalf("compressed log mtime = %s, want preserved %s", info.ModTime(), updated)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("plain log stat = %v, want os.ErrNotExist", err)
			}
			cached, ok := readHeaderCache(compressed)
			if !ok {
				t.Fatal("SettleTerminal did not publish a terminal header cache")
			}
			if cached.LastTrailer == nil || cached.LastTrailer.State != StatePurged {
				t.Fatalf("cached result = %#v, want purged", cached.LastTrailer)
			}
			st := NewStore(testLogger(), dir)
			st.cutoff, st.maxSettledPerRepo = time.Time{}, 0
			reloaded, err := st.LoadSettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(reloaded) != 1 {
				t.Fatalf("reloaded logs = %d, want 1", len(reloaded))
			}
			if reloaded[0].LogSize != info.Size() {
				t.Fatalf("reloaded log size = %d, want %d", reloaded[0].LogSize, info.Size())
			}
			if !reloaded[0].LastStateUpdateAt.Equal(updated) {
				t.Fatalf("reloaded update time = %s, want %s", reloaded[0].LastStateUpdateAt, updated)
			}
		})

		t.Run("RetriesPlainSourceAfterInterruptedReplacement", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "purged.jsonl")
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			staleMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: "stale source"})
			writeLogFile(t, dir, filepath.Base(path), staleMeta, trailer)
			// An interrupted compression left a stale compressed copy behind.
			if _, err := (&Store{}).compressPath(path); err != nil {
				t.Fatal(err)
			}
			authMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: "authoritative source"})
			writeLogFile(t, dir, filepath.Base(path), authMeta, trailer)
			paths, err := logPaths(testLogger(), dir, nil, false, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != 1 || paths[0] != path {
				t.Fatalf("log paths = %q, want plain source %q", paths, path)
			}
			if err := (NewStore(testLogger(), dir)).SettleTerminal(nil); err != nil {
				t.Fatal(err)
			}
			r, err := openLogReader(compressedLogPath(path))
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
			var gotMeta agent.MetaMessage
			if err := json.Unmarshal(bytes.SplitN(data, []byte("\n"), 2)[0], &gotMeta); err != nil {
				t.Fatal(err)
			}
			if gotMeta.Prompt != "authoritative source" {
				t.Fatalf("retried compressed prompt = %q, want authoritative source", gotMeta.Prompt)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("plain source stat = %v, want os.ErrNotExist", err)
			}
		})

		t.Run("SkipsStopped", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "stopped.jsonl")
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: "task"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
			writeLogFile(t, dir, filepath.Base(path), meta, trailer)
			if err := (NewStore(testLogger(), dir)).SettleTerminal(nil); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("stopped plain log stat = %v", err)
			}
			if _, err := os.Stat(compressedLogPath(path)); !os.IsNotExist(err) {
				t.Fatalf("stopped compressed log stat = %v, want os.ErrNotExist", err)
			}
		})

		t.Run("SkipsExcluded", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "purged.jsonl")
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: "task"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeLogFile(t, dir, filepath.Base(path), meta, trailer)
			updated := time.Date(2024, time.June, 1, 2, 3, 4, 0, time.UTC)
			if err := os.Chtimes(path, updated, updated); err != nil {
				t.Fatal(err)
			}
			exclude := map[string]struct{}{filepath.Clean(path): {}}
			if err := (NewStore(testLogger(), dir)).SettleTerminal(exclude); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("excluded plain log stat = %v", err)
			}
			if _, err := os.Stat(compressedLogPath(path)); !os.IsNotExist(err) {
				t.Fatalf("excluded compressed log stat = %v, want os.ErrNotExist", err)
			}
		})
		t.Run("ConcurrentSettleSameLog", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			st := NewStore(testLogger(), dir)
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			for i := range 20 {
				name := fmt.Sprintf("race%d.jsonl", i)
				meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Harness: harness.Claude, Prompt: fmt.Sprintf("race %d", i)})
				writeLogFile(t, dir, name, meta, trailer)
				path := filepath.Join(dir, name)
				// The two production shapes of terminal compression settle the
				// same log at the same time: the startup pass and a task's own
				// terminal path. Serialization in compressPath must make both
				// converge on one compressed log.
				var errs [2]error
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					errs[0] = st.SettleTerminal(nil)
				}()
				go func() {
					defer wg.Done()
					_, errs[1] = st.Compress(path, nil, StatePurged)
				}()
				wg.Wait()
				if errs[0] != nil || errs[1] != nil {
					t.Fatalf("iteration %d: SettleTerminal = %v, Compress = %v, want both nil", i, errs[0], errs[1])
				}
				compressed := compressedLogPath(path)
				if _, err := os.Stat(compressed); err != nil {
					t.Fatalf("iteration %d: compressed log stat = %v", i, err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("iteration %d: plain log stat = %v, want os.ErrNotExist", i, err)
				}
				r, err := openLogReader(compressed)
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
				var gotMeta agent.MetaMessage
				if err := json.Unmarshal(bytes.SplitN(data, []byte("\n"), 2)[0], &gotMeta); err != nil {
					t.Fatal(err)
				}
				if want := fmt.Sprintf("race %d", i); gotMeta.Prompt != want {
					t.Fatalf("iteration %d: compressed prompt = %q, want %q", i, gotMeta.Prompt, want)
				}
			}
		})
	})

	t.Run("LoadUnsettled", func(t *testing.T) {
		t.Parallel()
		t.Run("ForkedFromTaskIDMetadata", func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, false, mustJSON(t, agent.MetaMessage{
				MessageType:      "caic_meta",
				Version:          1,
				Prompt:           "forked task",
				Repos:            []agent.MetaRepo{{Name: "r", Branch: "caic-1"}},
				Harness:          harness.Claude,
				ForkedFromTaskID: "3BL0EKDTO000",
			}))

			tasks, err := NewStore(testLogger(), filepath.Dir(path)).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("len(tasks) = %d, want 1", len(tasks))
			}
			if tasks[0].ForkedFromTaskID != "3BL0EKDTO000" {
				t.Fatalf("ForkedFromTaskID = %q, want 3BL0EKDTO000", tasks[0].ForkedFromTaskID)
			}
		})

		t.Run("LazySemanticScanAllowsAppendWithSameRawHeader", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "task.jsonl")
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     int(agent.LogVersionV1),
				Prompt:      "original",
				Harness:     harness.Claude,
			})
			writeLogFile(t, dir, filepath.Base(path), meta)
			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // path is test-controlled.
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteString(claudeAssistant(t, map[string]any{"type": "text", "text": "appended"}) + "\n")
			if closeErr := file.Close(); writeErr != nil || closeErr != nil {
				t.Fatalf("append task log = %v, %v", writeErr, closeErr)
			}
			if err := tasks[0].LoadMessagesWithResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return claudecode.New().NewWire().ParseMessage, nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(tasks[0].Msgs) != 1 {
				t.Fatalf("appended semantic messages = %#v, want one message", tasks[0].Msgs)
			}
		})
		t.Run("PhysicalAuthority", func(t *testing.T) {
			t.Parallel()
			meta := func(version agent.LogVersion, h harness.Name) string {
				return mustJSON(t, agent.MetaMessage{
					MessageType: "caic_meta",
					Version:     int(version),
					Prompt:      "task",
					Repos:       []agent.MetaRepo{{Name: "r", Branch: "caic-0"}},
					Harness:     h,
				})
			}
			assistant := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
			for _, compressed := range []bool{false, true} {
				label := "Plain"
				name := "authority.jsonl"
				if compressed {
					label = "Compressed"
					name += ".zst"
				}
				t.Run(label+"LeadingBlankLines", func(t *testing.T) {
					t.Parallel()
					dir := t.TempDir()
					lines := []string{"", "  ", meta(agent.LogVersionV1, harness.Claude), assistant, meta(agent.LogVersionV1, harness.Claude)}
					if compressed {
						writeCompressedLogFile(t, dir, name, seqOf(lines...))
					} else {
						writeLogFile(t, dir, name, lines...)
					}
					path := filepath.Join(dir, name)
					if _, err := loadLogHeader(testLogger(), path, true); err != nil {
						t.Fatalf("loadLogHeader: %v", err)
					}
					if _, err := loadSemanticLog(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return claudecode.New().NewWire().ParseMessage, nil
					}); err != nil {
						t.Fatalf("loadLogFile: %v", err)
					}
				})
				for _, mismatch := range []struct {
					name string
					line string
				}{
					{name: "ChangedVersion", line: meta(agent.LogVersionV2, harness.Claude)},
					{name: "ChangedHarness", line: meta(agent.LogVersionV1, harness.Codex)},
				} {
					t.Run(label+mismatch.name, func(t *testing.T) {
						t.Parallel()
						dir := t.TempDir()
						lines := []string{meta(agent.LogVersionV1, harness.Claude), assistant, mismatch.line}
						if compressed {
							writeCompressedLogFile(t, dir, name, seqOf(lines...))
						} else {
							writeLogFile(t, dir, name, lines...)
						}
						path := filepath.Join(dir, name)
						want := "authority changed"
						if mismatch.name == "ChangedVersion" {
							want = "wrong t discriminator"
						}
						if _, err := loadLogHeader(testLogger(), path, true); err == nil || !strings.Contains(err.Error(), want) {
							t.Fatalf("loadLogHeader error = %v, want %s", err, want)
						}
						if _, err := loadSemanticLog(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
							return claudecode.New().NewWire().ParseMessage, nil
						}); err == nil || !strings.Contains(err.Error(), want) {
							t.Fatalf("loadLogFile error = %v, want %s", err, want)
						}
					})
				}
			}
		})
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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
		t.Run("V2ControlsMatchV1HeaderSemantics", func(t *testing.T) {
			t.Parallel()
			v1Meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "control task", Harness: harness.Codex,
			})
			v2Meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: int(agent.LogVersionV2), Prompt: "control task", Harness: harness.Codex,
			})
			v1Lines := []string{
				v1Meta,
				mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "session-1", Model: "model-1", AgentVersion: "agent-1"}),
				mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "owner", ForgeRepo: "repo", ForgePR: 7}),
				mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: agent.DiffStat{{Path: "main.go", Added: 2, Deleted: 1}}, Ts: 2_000_000_000}),
				`{"type":"assistant","text":"conversation"}`,
				mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", Title: "done", CostUSD: 1.25, Duration: 2.5, NumTurns: 3}),
			}
			v2Lines := []string{
				v2Meta,
				`{"t":"session","session_id":"session-1","model":"model-1","agent_version":"agent-1"}`,
				`{"t":"pr","forge_owner":"owner","forge_repo":"repo","forge_pr":7}`,
				`{"t":"diff_stat","diff_stat":[{"path":"main.go","added":2,"deleted":1}],"ts":2000000000}`,
				`{"t":"agent","ts":1.000,"msg":{"type":"assistant","text":"conversation"}}`,
				`{"t":"result","state":"purged","title":"done","cost_usd":1.25,"duration":2.5,"num_turns":3}`,
			}
			for _, compressed := range []bool{false, true} {
				format := "plain"
				if compressed {
					format = "zstd"
				}
				t.Run(format, func(t *testing.T) {
					t.Parallel()
					load := func(t *testing.T, name string, lines []string) *LoadedTask {
						dir := t.TempDir()
						if compressed {
							name += logCompressedExt
							writeCompressedLogFile(t, dir, name, seqOf(lines...))
						} else {
							writeLogFile(t, dir, name, lines...)
						}
						loaded, err := loadLogHeader(testLogger(), filepath.Join(dir, name), true)
						if err != nil {
							t.Fatal(err)
						}
						return loaded
					}
					v1 := load(t, "v1.jsonl", v1Lines)
					v2 := load(t, "v2.jsonl", v2Lines)
					if v2.SessionID != v1.SessionID || v2.Model != v1.Model || v2.AgentVersion != v1.AgentVersion {
						t.Fatalf("session metadata v2 = (%q, %q, %q), v1 = (%q, %q, %q)", v2.SessionID, v2.Model, v2.AgentVersion, v1.SessionID, v1.Model, v1.AgentVersion)
					}
					if v2.ForgeOwner != v1.ForgeOwner || v2.ForgeRepo != v1.ForgeRepo || v2.ForgePR != v1.ForgePR {
						t.Fatalf("PR metadata v2 = (%q, %q, %d), v1 = (%q, %q, %d)", v2.ForgeOwner, v2.ForgeRepo, v2.ForgePR, v1.ForgeOwner, v1.ForgeRepo, v1.ForgePR)
					}
					if !v2.DiffCreated || v2.LastStateUpdateAt != v1.LastStateUpdateAt {
						t.Fatalf("diff metadata v2 = (%t, %v), v1 = (%t, %v)", v2.DiffCreated, v2.LastStateUpdateAt, v1.DiffCreated, v1.LastStateUpdateAt)
					}
					if v2.LastTrailer == nil || v1.LastTrailer == nil || v2.LastTrailer.State != v1.LastTrailer.State || v2.LastTrailer.CostUSD != v1.LastTrailer.CostUSD || v2.LastTrailer.Duration != v1.LastTrailer.Duration || v2.LastTrailer.NumTurns != v1.LastTrailer.NumTurns {
						t.Fatalf("result metadata v2 = %#v, v1 = %#v", v2.LastTrailer, v1.LastTrailer)
					}
					for _, loaded := range []*LoadedTask{v1, v2} {
						if loaded.Msgs != nil {
							t.Fatalf("inventory messages = %#v, want nil for lazy semantic loading", loaded.Msgs)
						}
						calls := 0
						if err := loaded.LoadMessagesWithResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
							return func(raw []byte) ([]agent.Message, error) {
								calls++
								if !json.Valid(raw) {
									return nil, errors.New("invalid test conversation")
								}
								return []agent.Message{&agent.TextMessage{Text: "conversation"}}, nil
							}, nil
						}); err != nil {
							t.Fatal(err)
						}
						if calls != 1 || len(loaded.Msgs) != 1 {
							t.Fatalf("lazy semantic load calls/messages = %d/%#v, want 1/one conversation", calls, loaded.Msgs)
						}
					}
					if compressed {
						cached, err := loadLogHeader(testLogger(), v2.path, true)
						if err != nil {
							t.Fatal(err)
						}
						if cached.SessionID != v2.SessionID || cached.AgentVersion != v2.AgentVersion || cached.ForgePR != v2.ForgePR || cached.DiffCreated != v2.DiffCreated || cached.LastTrailer == nil || cached.LastTrailer.State != v2.LastTrailer.State {
							t.Fatalf("cached v2 control metadata = %#v, want %#v", cached, v2)
						}
						if cached.Msgs != nil {
							t.Fatalf("cached inventory messages = %#v, want nil", cached.Msgs)
						}
					}
				})
			}
		})
		t.Run("V2NativeTailBackfillMatchesV1", func(t *testing.T) {
			t.Parallel()
			v1Meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV1), Prompt: "native metadata", Harness: harness.Claude})
			v2Meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV2), Prompt: "native metadata", Harness: harness.Claude})
			v1Lines := []string{
				v1Meta,
				`{"type":"system","subtype":"init","model":"native-model","claude_code_version":"native-version"}`,
				`{"type":"result","total_cost_usd":1.25,"duration_ms":2500,"num_turns":3,"usage":{"input_tokens":4}}`,
				`{"type":"caic_result","state":"purged"}`,
			}
			v2Lines := []string{
				v2Meta,
				`{"t":"agent","ts":1.000,"msg":{"type":"system","subtype":"init","model":"native-model","claude_code_version":"native-version"}}`,
				`{"t":"agent","ts":2.000,"msg":{"type":"result","total_cost_usd":1.25,"duration_ms":2500,"num_turns":3,"usage":{"input_tokens":4}}}`,
				`{"t":"result","state":"purged"}`,
			}
			for _, compressed := range []bool{false, true} {
				format := "plain"
				if compressed {
					format = "zstd"
				}
				t.Run(format, func(t *testing.T) {
					t.Parallel()
					load := func(t *testing.T, name string, lines []string) *LoadedTask {
						dir := t.TempDir()
						if compressed {
							name += logCompressedExt
							writeCompressedLogFile(t, dir, name, seqOf(lines...))
						} else {
							writeLogFile(t, dir, name, lines...)
						}
						loaded, err := loadLogHeader(testLogger(), filepath.Join(dir, name), true)
						if err != nil {
							t.Fatal(err)
						}
						return loaded
					}
					v1 := load(t, "v1.jsonl", v1Lines)
					v2 := load(t, "v2.jsonl", v2Lines)
					if v2.Model != v1.Model || v2.AgentVersion != v1.AgentVersion {
						t.Fatalf("native init metadata v2 = (%q, %q), v1 = (%q, %q)", v2.Model, v2.AgentVersion, v1.Model, v1.AgentVersion)
					}
					if v2.LastTrailer == nil || v1.LastTrailer == nil || v2.LastTrailer.CostUSD != v1.LastTrailer.CostUSD || v2.LastTrailer.Duration != v1.LastTrailer.Duration || v2.LastTrailer.NumTurns != v1.LastTrailer.NumTurns || v2.LastTrailer.Usage != v1.LastTrailer.Usage {
						t.Fatalf("native result backfill v2 = %#v, v1 = %#v", v2.LastTrailer, v1.LastTrailer)
					}
					if v2.Msgs != nil {
						t.Fatalf("inventory messages = %#v, want nil", v2.Msgs)
					}
				})
			}
		})
		t.Run("MalformedControlsFailClosed", func(t *testing.T) {
			t.Parallel()
			for _, version := range []agent.LogVersion{agent.LogVersionV1, agent.LogVersionV2} {
				meta := mustJSON(t, agent.MetaMessage{
					MessageType: "caic_meta", Version: int(version), Prompt: "control task", Harness: harness.Codex,
				})
				for _, tc := range []struct {
					name string
					v1   string
					v2   string
				}{
					{name: "session", v1: `{"type":"caic_session","session_id":1}`, v2: `{"t":"session","session_id":"session-1","bogus":true}`},
					{name: "PR", v1: `{"type":"caic_pr","forge_pr":"bad"}`, v2: `{"t":"pr","forge_pr":7,"bogus":true}`},
					{name: "diff", v1: `{"type":"caic_diff_stat","diff_stat":true}`, v2: `{"t":"diff_stat","diff_stat":[],"bogus":true}`},
					{name: "result", v1: `{"type":"caic_result","state":"purged","cost_usd":"bad"}`, v2: `{"t":"result","state":"purged","bogus":true}`},
				} {
					control := tc.v1
					if version == agent.LogVersionV2 {
						control = tc.v2
					}
					for _, compressed := range []bool{false, true} {
						format := "plain"
						if compressed {
							format = "zstd"
						}
						t.Run(fmt.Sprintf("v%d/%s/%s", version, format, tc.name), func(t *testing.T) {
							t.Parallel()
							path := writePhysicalTestLog(t, compressed, meta, control)
							if _, err := loadLogHeader(testLogger(), path, true); err == nil {
								t.Fatal("loadLogHeader accepted malformed control")
							}
						})
					}
				}
			}
		})

		t.Run("V2MalformedNativeMessagesFailClosed", func(t *testing.T) {
			t.Parallel()
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: int(agent.LogVersionV2), Prompt: "native task", Harness: harness.Codex,
			})
			for _, compressed := range []bool{false, true} {
				format := "plain"
				if compressed {
					format = "zstd"
				}
				t.Run(format, func(t *testing.T) {
					t.Parallel()
					path := writePhysicalTestLog(t, compressed, meta, `{"t":"agent","ts":1700000000.123,"msg":{"type":"assistant"} trailing}`)
					if _, err := loadLogHeader(testLogger(), path, true); err == nil || !strings.Contains(err.Error(), "invalid or noncanonical msg envelope") {
						t.Fatalf("loadLogHeader error = %v, want malformed v2 native message rejection", err)
					}
					tasks, err := NewStore(testLogger(), filepath.Dir(path)).LoadUnsettled()
					if err != nil {
						t.Fatal(err)
					}
					if len(tasks) != 0 {
						t.Fatalf("persistent inventory accepted malformed v2 native message: %#v", tasks)
					}
				})
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
				CacheMounts:       []agent.MetaCacheMount{{Name: "npm", Description: "Node", HostPath: "~/.npm", ContainerPath: "/home/user/.npm", ReadOnly: true, Shallow: true}},
				Mounts:            []agent.MetaMount{{HostPath: "/host/work", ContainerPath: "/workspace/work", ReadOnly: true}},
			})
			writeLogFile(t, dir, "a.jsonl", meta)

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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
		t.Run("DiffCreatedStickyAcrossEmptyTail", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			withDiff := mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: agent.DiffStat{{Path: "a.go", Added: 3, Deleted: 1}}, Ts: 1})
			// A later empty diff (agent committed, working tree clean) must not clear it.
			emptyDiff := mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", Ts: 2})
			writeLogFile(t, dir, "a.jsonl", meta, withDiff, emptyDiff)

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("len = %d, want 1", len(tasks))
			}
			if !tasks[0].DiffCreated {
				t.Error("DiffCreated = false, want true after a non-empty diff followed by an empty one")
			}
		})
		t.Run("DiffCreatedFalseWithoutDiff", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			emptyDiff := mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", Ts: 1})
			writeLogFile(t, dir, "a.jsonl", meta, emptyDiff)

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("len = %d, want 1", len(tasks))
			}
			if tasks[0].DiffCreated {
				t.Error("DiffCreated = true, want false when only empty diffs were recorded")
			}
		})
		t.Run("ResultReasoningTokens", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "task1", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", ReasoningOutputTokens: 123})
			writeLogFile(t, dir, "a.jsonl", meta, trailer)

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("len = %d, want 1", len(tasks))
			}
			if tasks[0].LastTrailer == nil {
				t.Fatal("Result is nil")
			}
			if tasks[0].LastTrailer.Usage.ReasoningOutputTokens != 123 {
				t.Errorf("ReasoningOutputTokens = %d, want 123", tasks[0].LastTrailer.Usage.ReasoningOutputTokens)
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

			st := NewStore(testLogger(), dir)
			st.cutoff, st.maxSettledPerRepo = time.Time{}, 0
			tasks, err := st.LoadSettled()
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

		t.Run("PreferPlainSourceAfterInterruptedCompression", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			plainMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "plain", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			compressedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "compressed", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(compressedMeta, trailer))
			writeLogFile(t, dir, "a.jsonl", plainMeta, trailer)

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("len = %d, want 1", len(tasks))
			}
			if tasks[0].Prompt != "plain" {
				t.Errorf("Prompt = %q, want plain", tasks[0].Prompt)
			}
		})
		t.Run("NotExist", func(t *testing.T) {
			t.Parallel()
			tasks, err := NewStore(testLogger(), filepath.Join(t.TempDir(), "nope")).LoadUnsettled()
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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
		t.Run("V1NativeSessionTypeIsNotTaskMetadata", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 1, Prompt: "session task", Harness: harness.Claude,
			})
			nativeSession := `{"type":"session","session_id":"native-session","version":"native"}`
			caicSession := mustJSON(t, agent.MetaSessionMessage{
				MessageType: "caic_session", SessionID: "caic-session", AgentVersion: "1.2.3",
			})
			path := filepath.Join(dir, "session.jsonl")
			writeLogFile(t, dir, filepath.Base(path), meta, nativeSession, caicSession)

			parsedNative := false
			parseNativeSession := func(line []byte) ([]agent.Message, error) {
				if len(line) == 0 {
					return nil, errors.New("empty native record")
				}
				if bytes.Contains(line, []byte(`"type":"session"`)) {
					parsedNative = true
				}
				return []agent.Message{&agent.TextMessage{Text: "native session record"}}, nil
			}
			lt, err := loadSemanticSessionMetadata(path, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
				if h != harness.Claude {
					t.Fatalf("resolver harness = %q, want claude", h)
				}
				return parseNativeSession, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !parsedNative {
				t.Fatal("v1 native session record did not reach the harness parser")
			}
			if lt.SessionID != "caic-session" || lt.AgentVersion != "1.2.3" {
				t.Fatalf("session metadata = (%q, %q), want (caic-session, 1.2.3)", lt.SessionID, lt.AgentVersion)
			}
		})
		t.Run("V2CaicSessionAliasesAreNotTaskMetadata", func(t *testing.T) {
			t.Parallel()
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 2, Prompt: "session task", Harness: harness.Claude,
			})
			caicSession := `{"t":"caic_session","session_id":"wrong-session","model":"wrong-model","version":"wrong-version"}`
			caicInit := `{"t":"caic_init","session_id":"wrong-init","model":"wrong-model","version":"wrong-version"}`
			path := writePhysicalTestLog(t, false, meta, caicSession, caicInit)

			_, err := loadSemanticSessionMetadata(path, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
				if h != harness.Claude {
					t.Fatalf("resolver harness = %q, want claude", h)
				}
				return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
			})
			if err == nil || !strings.Contains(err.Error(), "unknown top-level t") {
				t.Fatalf("v2 caic_session alias error = %v, want strict unknown-token rejection", err)
			}
			tasks, err := NewStore(testLogger(), filepath.Dir(path)).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 0 {
				t.Fatalf("persistent inventory accepted malformed v2 controls: %#v", tasks)
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			lt := tasks[0]
			lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return codex.New("", nil).NewWire().ParseMessage, nil
			})
			if lt.SessionID != "thread-old" {
				t.Fatalf("SessionID = %q after authority scan, want thread-old", lt.SessionID)
			}
			if err := lt.LoadSessionMetadata(); err != nil {
				t.Fatal(err)
			}
			if lt.SessionID != "thread-old" {
				t.Errorf("SessionID = %q, want thread-old", lt.SessionID)
			}
		})
		t.Run("LoadSessionMetadataEnforcesAuthority", func(t *testing.T) {
			t.Parallel()
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 1, Prompt: "session task", Harness: harness.Claude,
			})
			mismatch := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 2, Prompt: "session task", Harness: harness.Claude,
			})
			session := mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "session-1"})
			for _, compressed := range []bool{false, true} {
				format := "plain"
				if compressed {
					format = "zstd"
				}
				t.Run(format+" missing header", func(t *testing.T) {
					t.Parallel()
					path := writePhysicalTestLog(t, compressed, session)
					if _, err := loadSemanticSessionMetadata(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
					}); err == nil || !strings.Contains(err.Error(), "invalid first log header") {
						t.Fatalf("error = %v, want invalid first header", err)
					}
				})
				t.Run(format+" mixed authority after metadata", func(t *testing.T) {
					t.Parallel()
					path := writePhysicalTestLog(t, compressed, meta, session, mismatch)
					if _, err := loadSemanticSessionMetadata(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
					}); err == nil || !strings.Contains(err.Error(), "wrong t discriminator") {
						t.Fatalf("error = %v, want wrong t discriminator", err)
					}
				})
				t.Run(format+" leading empty lines", func(t *testing.T) {
					t.Parallel()
					path := writePhysicalTestLog(t, compressed, "", "  ", meta, session)
					lt, err := loadSemanticSessionMetadata(path, func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
					})
					if err != nil {
						t.Fatal(err)
					}
					if lt.SessionID != "session-1" {
						t.Fatalf("SessionID = %q, want session-1", lt.SessionID)
					}
				})
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			lt := tasks[0]
			lt.SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return codex.New("", nil).NewWire().ParseMessage, nil
			})
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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
			// The context-clear marker remains present for Task to apply during restoration.
			found := false
			for _, message := range lt.Msgs {
				if marker, ok := message.(*agent.SystemMessage); ok && marker.Subtype == "context_cleared" {
					found = true
				}
			}
			if !found {
				t.Error("context-cleared marker missing from loaded messages")
			}
		})
		t.Run("PRHeaderOnly", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "pr task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
			prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "octocat", ForgeRepo: "hello", ForgePR: 42})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeLogFile(t, dir, "1-r-caic-1.jsonl", meta, prMsg, trailer)

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
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
			// caic_pr is early in the file, followed by >64 KiB of messages.
			// Full parser traversal derives its metadata even though tail messages
			// remain bounded.
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "big task", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-3"}}, Harness: "claude"})
			prMsg := mustJSON(t, agent.MetaPRMessage{MessageType: "caic_pr", ForgeOwner: "acme", ForgeRepo: "widget", ForgePR: 77})

			// Build lines: header, caic_pr, then enough assistant messages
			// to place caic_pr outside the retained tail window.
			lines := make([]string, 0, 83)
			lines = append(lines, meta, prMsg)
			bigText := string(make([]byte, 1024)) // 1 KiB of null bytes per message
			for range 80 {                        // 80 KiB of filler
				lines = append(lines, claudeAssistant(t, map[string]any{"type": "text", "text": bigText}))
			}
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			lines = append(lines, trailer)
			writeLogFile(t, dir, "3-r-caic-3.jsonl", lines...)

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("len = %d, want 1", len(tasks))
			}
			lt := tasks[0]
			// The full traversal derives PR metadata before tail messages are retained.
			if lt.ForgePR != 77 {
				t.Fatalf("ForgePR = %d, want 77", lt.ForgePR)
			}
			if lt.ForgeOwner != "acme" {
				t.Fatalf("ForgeOwner = %q, want acme", lt.ForgeOwner)
			}
			if lt.ForgeRepo != "widget" {
				t.Fatalf("ForgeRepo = %q, want widget", lt.ForgeRepo)
			}
			// Full parse via LoadMessages retains the same PR metadata.
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

		t.Run("ManyFiles", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			line := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     int(agent.LogVersionV1),
				Prompt:      "task",
				Harness:     harness.Claude,
			})
			for i := range 512 {
				writeLogFile(t, dir, fmt.Sprintf("task-%03d.jsonl", i), line)
			}

			loaded, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != 512 {
				t.Fatalf("loaded %d task logs, want 512", len(loaded))
			}
		})
	})

	// LoadSettled covers the settled (compressed) scan: it loads .zst logs,
	// applies the mtime retention cutoff before decoding, and skips a .zst
	// whose plain source is authoritative (plain wins).
	t.Run("LoadSettled", func(t *testing.T) {
		t.Parallel()
		t.Run("LoadsCompressed", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "settle", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeCompressedLogFile(t, dir, "s.jsonl.zst", seqOf(meta, trailer))
			st := NewStore(testLogger(), dir)
			st.cutoff, st.maxSettledPerRepo = time.Time{}, 0
			tasks, err := st.LoadSettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("len(tasks) = %d, want 1", len(tasks))
			}
			if tasks[0].Prompt != "settle" {
				t.Errorf("Prompt = %q, want settle", tasks[0].Prompt)
			}
		})
		t.Run("CapsPerRepoMostRecentFirst", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			now := time.Now().UTC()
			for r, repo := range []string{"org/a", "org/b"} {
				prefix := repo[len("org/"):]
				for i := range 6 {
					name := prefix + fmt.Sprintf("%d.jsonl.zst", i)
					// Distinct mtime per log (i=0 oldest, i=5 newest), all inside
					// the production retention cutoff.
					mtime := now.Add(time.Duration((i+1)*10+r) * time.Minute)
					meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: prefix + strconv.Itoa(i), Repos: []agent.MetaRepo{{Name: repo, Branch: "caic-0"}}, Harness: harness.Claude, StartedAt: mtime})
					path := filepath.Join(dir, name)
					writeCompressedLogFile(t, dir, name, seqOf(meta, trailer))
					if err := os.Chtimes(path, mtime, mtime); err != nil {
						t.Fatal(err)
					}
				}
			}
			st := NewStore(testLogger(), dir)
			tasks, err := st.LoadSettled()
			if err != nil {
				t.Fatal(err)
			}
			// The production cap keeps the five newest logs per repo,
			// independently: each repo loses exactly its oldest log.
			want := map[string]bool{}
			for _, repo := range []string{"a", "b"} {
				for i := 1; i < 6; i++ {
					want[repo+strconv.Itoa(i)] = true
				}
			}
			check := func(run string, gotTasks []*LoadedTask) {
				got := map[string]bool{}
				for _, lt := range gotTasks {
					got[lt.Prompt] = true
				}
				if len(got) != len(want) {
					t.Fatalf("%s: len(tasks) = %d, want %d (five newest per repo)", run, len(got), len(want))
				}
				for p := range got {
					if !want[p] {
						t.Fatalf("%s: unexpected settled task %q (oldest per repo must be capped)", run, p)
					}
				}
			}
			check("cold", tasks)
			// A warm pass resolves repo attribution from the header cache and
			// must select the same logs.
			warm, err := st.LoadSettled()
			if err != nil {
				t.Fatal(err)
			}
			check("warm", warm)
		})
		t.Run("AgeFilterSkipsOld", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			metaNew := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "new", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude})
			metaOld := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "old", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: harness.Claude})
			writeCompressedLogFile(t, dir, "new.jsonl.zst", seqOf(metaNew, trailer))
			writeCompressedLogFile(t, dir, "old.jsonl.zst", seqOf(metaOld, trailer))
			oldTime := time.Now().Add(-15 * 24 * time.Hour)
			if err := os.Chtimes(filepath.Join(dir, "old.jsonl.zst"), oldTime, oldTime); err != nil {
				t.Fatal(err)
			}
			cutoff := time.Now().Add(-14 * 24 * time.Hour)
			st := NewStore(testLogger(), dir)
			st.cutoff, st.maxSettledPerRepo = cutoff, 0
			tasks, err := st.LoadSettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("len(tasks) = %d, want 1 (old filtered by mtime)", len(tasks))
			}
			if tasks[0].Prompt != "new" {
				t.Errorf("Prompt = %q, want new", tasks[0].Prompt)
			}
		})
		t.Run("SkipsPlainTwin", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "twin", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			// A plain source and its compressed twin for the same base: the plain
			// source is authoritative, so the compressed scan skips the twin.
			writeLogFile(t, dir, "a.jsonl", meta, trailer)
			writeCompressedLogFile(t, dir, "a.jsonl.zst", seqOf(meta, trailer))
			st := NewStore(testLogger(), dir)
			st.cutoff, st.maxSettledPerRepo = time.Time{}, 0
			tasks, err := st.LoadSettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 0 {
				t.Fatalf("len(tasks) = %d, want 0 (plain twin wins)", len(tasks))
			}
		})
	})

	t.Run("LoadForTaskIDs", func(t *testing.T) {
		t.Parallel()
		t.Run("Valid", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			wantedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "wanted", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			unrelatedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 1, Prompt: "unrelated", Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
			writeLogFile(t, dir, "live1-repo-branch.jsonl", wantedMeta)
			writeLogFile(t, dir, "live10-repo-branch.jsonl", unrelatedMeta)

			tasks, err := NewStore(testLogger(), dir).LoadForTaskIDs([]string{"live1"})
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
		t.Run("Missing", func(t *testing.T) {
			t.Parallel()
			if _, err := NewStore(testLogger(), t.TempDir()).LoadForTaskIDs([]string{"missing"}); err == nil || !strings.Contains(err.Error(), "missing task logs") {
				t.Fatalf("LoadLogsForTaskIDs error = %v, want missing-log error", err)
			}
		})
		t.Run("Invalid", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeLogFile(t, dir, "broken-repo-branch.jsonl", `{"type":"assistant"}`)
			_, err := NewStore(testLogger(), dir).LoadForTaskIDs([]string{"broken"})
			if err == nil || !strings.Contains(err.Error(), "load task log") {
				t.Fatalf("LoadForTaskIDs error = %v, want invalid-log error", err)
			}
		})
	})
}
