// Tests for Store: queued prompts, log authority, result trailers, compression, and startup scans.

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

	"github.com/caic-xyz/caic/backend/internal/tokens"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

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
	repos := make([]v3.MetaRepo, len(t.Repos))
	for i, repo := range t.Repos {
		repos[i] = v3.MetaRepo{Name: repo.Name, BaseBranch: repo.BaseBranch, Branch: repo.Branch, ContainerPath: repo.ContainerPath}
	}
	return &agent.MetaMessage{MessageType: "caic_meta", Prompt: t.InitialPrompt.Text, Repos: repos, Harness: string(t.Harness), RequestedModel: t.Model, RequestedEffort: t.Effort}
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
	t.Run("Initialize", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			s := NewStore(testLogger(), t.TempDir())
			id := ksid.NewID().String()
			h := &agent.MetaMessage{Prompt: "prepared", Harness: string(harness.Claude), StartedAt: time.Now().UTC()}
			p, err := s.Initialize(id+"-repo-caic-0.jsonl", h)
			if err != nil {
				t.Fatal(err)
			}
			lt, err := LoadHistorySource(p.Path)
			if err != nil || lt.TaskID != id {
				t.Fatalf("initialized log=%+v %v", lt, err)
			}
			if err := p.Rollback(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
				t.Fatalf("rollback left log: %v", err)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			s := NewStore(testLogger(), t.TempDir())
			h := &agent.MetaMessage{Prompt: "prepared", Harness: string(harness.Claude)}
			for _, name := range []string{"../outside.jsonl", ""} {
				if _, err := s.Initialize(name, h); err == nil {
					t.Fatalf("Initialize(%q) accepted", name)
				}
			}
			path := filepath.Join(s.LogDir, "existing.jsonl")
			if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Initialize("existing.jsonl", h); !errors.Is(err, os.ErrExist) {
				t.Fatalf("Initialize existing=%v", err)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != "existing" { //nolint:gosec // Test-owned pre-existing log.
				t.Fatalf("existing file changed: %q %v", b, err)
			}
			h.StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			if _, err := s.Initialize("bad.jsonl", h); err == nil {
				t.Fatal("invalid metadata accepted")
			}
			if _, err := os.Stat(filepath.Join(s.LogDir, "bad.jsonl")); !os.IsNotExist(err) {
				t.Fatalf("bad log retained: %v", err)
			}
		})
		t.Run("replacement_survives_rollback", func(t *testing.T) {
			t.Parallel()
			s := NewStore(testLogger(), t.TempDir())
			p, err := s.Initialize("prepared.jsonl", &agent.MetaMessage{Prompt: "prepared", Harness: string(harness.Claude)})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(p.Path, p.Path+".original"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.Path, []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := p.Rollback(); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(p.Path); err != nil || string(b) != "replacement" {
				t.Fatalf("replacement=%q %v", b, err)
			}
		})
	})

	t.Run("OpenRemovesFailedNewHeader", func(t *testing.T) {
		t.Parallel()
		s := NewStore(testLogger(), t.TempDir())
		h := &agent.MetaMessage{Prompt: "prepared", Harness: string(harness.Claude), StartedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
		if _, _, err := s.Open("failed.jsonl", h); err == nil {
			t.Fatal("Open accepted unserializable metadata")
		}
		files, err := os.ReadDir(s.LogDir)
		if err != nil || len(files) != 0 {
			t.Fatalf("failed header left files: %v, %v", files, err)
		}
	})
	t.Run("LoadInputQueue", func(t *testing.T) {
		t.Parallel()
		for _, fixture := range []struct{ name, raw string }{
			{"truncated", "["},
			{"trailing", "[] {}"},
			{"invalid_id", `[{"id":"/","taskID":"/","text":"hello"}]`},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "input-queue.json"), []byte(fixture.raw), 0o600); err != nil {
					t.Fatal(err)
				}
				store := NewStore(testLogger(), dir)
				if _, err := store.LoadInputQueue(); err == nil {
					t.Fatal("corrupt queue accepted")
				}
			})
		}
	})
	t.Run("WriteResultTrailerReasoningTokens", func(t *testing.T) {
		t.Parallel()
		b := &agenttest.LogSink{}
		res := &Result{
			State:         StateWaiting,
			DiskUsedBytes: new(int64(456)),
			Usage: agent.Usage{
				ReasoningOutputTokens:    123,
				CacheCreationInputTokens: 110,
				CacheWrites:              tokens.CacheWrites{{TTL: 0, Tokens: 10}, {TTL: time.Hour, Tokens: 100}},
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
		var loaded LoadedTask
		applyMetaResult(&loaded, &got)
		if !loaded.LastTrailer.Usage.Equal(res.Usage) {
			t.Fatalf("trailer usage = %+v, want %+v", loaded.LastTrailer.Usage, res.Usage)
		}
		if got.DiskUsedBytes == nil || *got.DiskUsedBytes != 456 {
			t.Errorf("DiskUsedBytes = %v, want 456", got.DiskUsedBytes)
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
		if meta.Version != int(agent.LogVersionV3) || meta.Prompt != "test prompt" || meta.RequestedModel != "model-1" || meta.RequestedEffort != "high" {
			t.Fatalf("unexpected metadata: %+v", meta)
		}
		if len(meta.Repos) != 1 || meta.Repos[0].ContainerPath != "~/src/org/repo" {
			t.Fatalf("metadata repos = %+v", meta.Repos)
		}
	})
	t.Run("OpenRejectsNonBaseFilename", func(t *testing.T) {
		t.Parallel()
		store := NewStore(testLogger(), t.TempDir())
		header := agent.MetaMessage{MessageType: "caic_meta", Harness: string(harness.Codex)}
		for _, name := range []string{"", ".", "..", "/etc/passwd", "sub/task.jsonl", "../task.jsonl"} {
			if _, _, err := store.Open(name, &header); err == nil {
				t.Errorf("Open(%q) = nil error, want rejection", name)
			}
		}
	})
	t.Run("ReopenRejectsNonBaseFilename", func(t *testing.T) {
		t.Parallel()
		store := NewStore(testLogger(), t.TempDir())
		header := agent.MetaMessage{MessageType: "caic_meta", Harness: string(harness.Codex)}
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
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
		entries := logLines(t, second.path)
		if len(entries) != 1 {
			t.Fatalf("log lines = %d, want unchanged header", len(entries))
		}
	})
	t.Run("OpenCreatesCanonicalV3Log", func(t *testing.T) {
		t.Parallel()
		store := NewStore(testLogger(), t.TempDir())
		tk := &testTask{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Codex}
		log, err := openTaskLog(store, tk)
		if err != nil {
			t.Fatal(err)
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
			t.Fatalf("v3 log = %s, want canonical metadata and control", got)
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
			Version:     int(agent.LogVersionV3),
			Prompt:      "test",
			Repos:       []v3.MetaRepo{{Name: "org/repo", Branch: "caic-0"}},
			Harness:     string(harness.Claude),
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
		header := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV3), Prompt: "test", Harness: string(harness.Codex)}) + "\n"
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

	t.Run("ReopenCompressedRejectsInvalidArchive", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"corrupt", "harness_mismatch"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				store := NewStore(testLogger(), t.TempDir())
				tk := &testTask{ID: ksid.NewID(), Harness: harness.Codex}
				log, err := openTaskLog(store, tk)
				if err != nil {
					t.Fatal(err)
				}
				plain := log.path
				archive, err := store.Compress(plain, log, StateFailed)
				if err != nil {
					t.Fatal(err)
				}
				header := tk.logHeader()
				if name == "corrupt" {
					if err := os.WriteFile(archive, []byte("invalid zstd"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					header.Harness = string(harness.Claude)
				}
				if log, _, err := store.Reopen(filepath.Base(archive), header); err == nil {
					_ = log.Close()
					t.Fatal("Reopen succeeded, want validation error")
				}
				if _, err := os.Stat(archive); err != nil {
					t.Fatalf("archive removed on failed restore: %v", err)
				}
				if _, err := os.Stat(plain); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("plain log published on failed restore: %v", err)
				}
			})
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV3), Harness: string(harness.Claude), Prompt: "task"})
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
			staleMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV3), Harness: string(harness.Claude), Prompt: "stale source"})
			writeLogFile(t, dir, filepath.Base(path), staleMeta, trailer)
			// An interrupted compression left a stale compressed copy behind.
			if _, err := (&Store{}).compressPath(path); err != nil {
				t.Fatal(err)
			}
			authMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV3), Harness: string(harness.Claude), Prompt: "authoritative source"})
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV3), Harness: string(harness.Claude), Prompt: "task"})
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV3), Harness: string(harness.Claude), Prompt: "task"})
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
				meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: int(agent.LogVersionV3), Harness: string(harness.Claude), Prompt: fmt.Sprintf("race %d", i)})
				writeLogFile(t, dir, name, meta, trailer)
				path := filepath.Join(dir, name)
				// The two production shapes of terminal compression settle the
				// same log at the same time: the startup pass and a task's own
				// terminal path. Serialization in compressPath must make both
				// converge on one compressed log.
				var errs [2]error
				var wg sync.WaitGroup
				wg.Go(func() {
					errs[0] = st.SettleTerminal(nil)
				})
				wg.Go(func() {
					_, errs[1] = st.Compress(path, nil, StatePurged)
				})
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
		t.Run("RelationshipMetadata", func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, false, mustJSON(t, agent.MetaMessage{
				MessageType:      "caic_meta",
				Version:          3,
				Prompt:           "forked task",
				Repos:            []v3.MetaRepo{{Name: "r", Branch: "caic-1"}},
				Harness:          string(harness.Claude),
				ForkedFromTaskID: "3BL0EKDTO000",
				ParentTaskID:     "3BL0EKDTO001",
				OwnerID:          "user-1",
				CaicMCP:          true,
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
			if tasks[0].ParentTaskID != "3BL0EKDTO001" {
				t.Fatalf("ParentTaskID = %q, want 3BL0EKDTO001", tasks[0].ParentTaskID)
			}
			if tasks[0].OwnerID != "user-1" || !tasks[0].CaicMCP {
				t.Fatalf("CAIC MCP metadata = owner %q, enabled %v", tasks[0].OwnerID, tasks[0].CaicMCP)
			}
		})

		t.Run("OrdinaryForkIsRoot", func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, false, mustJSON(t, agent.MetaMessage{
				MessageType:      "caic_meta",
				Version:          3,
				Prompt:           "ordinary fork",
				Repos:            []v3.MetaRepo{{Name: "r", Branch: "caic-1"}},
				Harness:          string(harness.Claude),
				ForkedFromTaskID: "3BL0EKDTO000",
			}))

			tasks, err := NewStore(testLogger(), filepath.Dir(path)).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(tasks) != 1 {
				t.Fatalf("len(tasks) = %d, want 1", len(tasks))
			}
			if got := tasks[0].ParentTaskID; got != "" {
				t.Errorf("ParentTaskID = %q, want root task", got)
			}
		})

		t.Run("LazySemanticScanAllowsAppendWithSameRawHeader", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "task.jsonl")
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     int(agent.LogVersionV3),
				Prompt:      "original",
				Harness:     string(harness.Claude),
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
			if err := tasks[0].LoadMessagesWithResolver(newTestWireResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return claudecode.New().NewWire().ParseMessage, nil
			})); err != nil {
				t.Fatal(err)
			}
			if len(tasks[0].Timeline) != 1 {
				t.Fatalf("appended semantic messages = %#v, want one message", tasks[0].Timeline)
			}
		})
		t.Run("PhysicalAuthority", func(t *testing.T) {
			t.Parallel()
			meta := func(version int, h harness.Name) string {
				return mustJSON(t, agent.MetaMessage{
					MessageType: "caic_meta",
					Version:     version,
					Prompt:      "task",
					Repos:       []v3.MetaRepo{{Name: "r", Branch: "caic-0"}},
					Harness:     string(h),
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
					lines := []string{"", "  ", meta(agent.LogVersionV3, harness.Claude), assistant, meta(agent.LogVersionV3, harness.Claude)}
					if compressed {
						writeCompressedLogFile(t, dir, name, seqOf(lines...))
					} else {
						writeLogFile(t, dir, name, lines...)
					}
					path := filepath.Join(dir, name)
					if _, err := loadLogHeader(testLogger(), path, true); err != nil {
						t.Fatalf("loadLogHeader: %v", err)
					}
					if _, err := loadSemanticTask(path, newTestWireResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return claudecode.New().NewWire().ParseMessage, nil
					})); err != nil {
						t.Fatalf("loadLogFile: %v", err)
					}
				})
				for _, mismatch := range []struct {
					name      string
					line      string
					errorText string
				}{
					{name: "ChangedVersion", line: meta(2, harness.Claude), errorText: "unsupported log version"},
					{name: "ChangedHarness", line: meta(agent.LogVersionV3, harness.Codex), errorText: "harness changed"},
				} {
					t.Run(label+mismatch.name, func(t *testing.T) {
						t.Parallel()
						dir := t.TempDir()
						lines := []string{meta(agent.LogVersionV3, harness.Claude), assistant, mismatch.line}
						if compressed {
							writeCompressedLogFile(t, dir, name, seqOf(lines...))
						} else {
							writeLogFile(t, dir, name, lines...)
						}
						path := filepath.Join(dir, name)
						if _, err := loadLogHeader(testLogger(), path, true); err == nil || !strings.Contains(err.Error(), mismatch.errorText) {
							t.Fatalf("loadLogHeader error = %v, want %s", err, mismatch.errorText)
						}
						if _, err := loadSemanticTask(path, newTestWireResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
							return claudecode.New().NewWire().ParseMessage, nil
						})); err == nil || !strings.Contains(err.Error(), mismatch.errorText) {
							t.Fatalf("loadLogFile error = %v, want %s", err, mismatch.errorText)
						}
					})
				}
			}
		})
		t.Run("Valid", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "task1", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
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
		t.Run("ControlsRestoreHeaderAndMessages", func(t *testing.T) {
			t.Parallel()
			lines := []string{
				`{"t":"caic_meta","version":3,"prompt":"control task","repos":[],"harness":"codex"}`,
				`{"t":"session","session_id":"session-1","model":"model-1","agent_version":"agent-1"}`,
				`{"t":"pr","forge_owner":"owner","forge_repo":"repo","forge_pr":7}`,
				`{"t":"git_summary","diff_stat":[{"path":"main.go","added":2,"deleted":1}],"ts":2000000000}`,
				`{"t":"agent","ts":1.000,"msg":{"type":"assistant","text":"conversation"}}`,
				`{"t":"result","state":"purged","title":"done","cost_usd":1.25,"duration":2.5,"num_turns":3}`,
			}
			for _, compressed := range []bool{false, true} {
				t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
					t.Parallel()
					path := writePhysicalTestLog(t, compressed, lines...)
					loaded, err := loadLogHeader(testLogger(), path, true)
					if err != nil {
						t.Fatal(err)
					}
					if loaded.Prompt != "control task" || loaded.Harness != harness.Codex || loaded.State != StatePurged {
						t.Fatalf("task metadata=%+v", loaded)
					}
					if loaded.SessionID != "session-1" || loaded.ReportedModel != "model-1" || loaded.AgentVersion != "agent-1" {
						t.Fatalf("session metadata=%+v", loaded)
					}
					if loaded.ForgeOwner != "owner" || loaded.ForgeRepo != "repo" || loaded.ForgePR != 7 || !loaded.DiffCreated {
						t.Fatalf("repository metadata=%+v", loaded)
					}
					if loaded.LastTrailer == nil || loaded.Title != "done" || loaded.LastTrailer.CostUSD != 1.25 || loaded.LastTrailer.Duration != 2500*time.Millisecond || loaded.LastTrailer.NumTurns != 3 {
						t.Fatalf("result=%+v", loaded.LastTrailer)
					}
					if loaded.Timeline != nil {
						t.Fatalf("inventory eagerly retained messages=%#v", loaded.Timeline)
					}
					calls := 0
					if err := loaded.LoadMessagesWithResolver(newTestWireResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return func(raw []byte) ([]agent.Message, error) {
							calls++
							if string(raw) != `{"type":"assistant","text":"conversation"}` {
								return nil, fmt.Errorf("unexpected native payload: %s", raw)
							}
							return []agent.Message{&agent.TextMessage{Text: "conversation"}}, nil
						}, nil
					})); err != nil {
						t.Fatal(err)
					}
					if calls != 1 || len(loaded.Timeline) != 2 {
						t.Fatalf("native parses=%d,timeline=%#v", calls, loaded.Timeline)
					}
					if message, ok := loaded.Timeline[1].Message.(*agent.TextMessage); !ok || message.Text != "conversation" {
						t.Fatalf("conversation=%#v", loaded.Timeline[1].Message)
					}
					if compressed {
						cached, err := loadLogHeader(testLogger(), path, true)
						if err != nil {
							t.Fatal(err)
						}
						if cached.SessionID != loaded.SessionID || cached.ForgePR != 7 || cached.LastTrailer == nil || cached.LastTrailer.CostUSD != 1.25 || cached.Timeline != nil {
							t.Fatalf("cached metadata=%+v", cached)
						}
					}
				})
			}
		})

		t.Run("MalformedControlsFailClosed", func(t *testing.T) {
			t.Parallel()
			header := mustJSON(t, agent.MetaMessage{Version: 3, Harness: string(harness.Codex), Prompt: "task"})
			for _, tc := range []struct{ name, line string }{
				{"session type", `{"t":"session","session_id":1}`},
				{"session unknown field", `{"t":"session","session_id":"session-1","bogus":true}`},
				{"PR type", `{"t":"pr","forge_pr":"bad"}`},
				{"PR unknown field", `{"t":"pr","forge_pr":7,"bogus":true}`},
				{"diff type", `{"t":"git_summary","diff_stat":true}`},
				{"diff unknown field", `{"t":"git_summary","diff_stat":[],"bogus":true}`},
				{"result type", `{"t":"result","state":"purged","cost_usd":"bad"}`},
				{"result unknown field", `{"t":"result","state":"purged","bogus":true}`},
			} {
				for _, compressed := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/compressed=%t", tc.name, compressed), func(t *testing.T) {
						t.Parallel()
						path := writePhysicalTestLog(t, compressed, header, tc.line)
						if _, err := loadLogHeader(testLogger(), path, true); err == nil {
							t.Fatal("malformed control accepted")
						}
					})
				}
			}
		})

		t.Run("MalformedNativeMessagesFailClosed", func(t *testing.T) {
			t.Parallel()
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: int(agent.LogVersionV3), Prompt: "native task", Harness: string(harness.Codex),
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
						t.Fatalf("loadLogHeader error = %v, want malformed native message rejection", err)
					}
					tasks, err := NewStore(testLogger(), filepath.Dir(path)).LoadUnsettled()
					if err != nil {
						t.Fatal(err)
					}
					if len(tasks) != 0 {
						t.Fatalf("persistent inventory accepted malformed native message: %#v", tasks)
					}
				})
			}
		})
		t.Run("LaunchConfigMetadata", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{
				MessageType:       "caic_meta",
				Version:           3,
				Prompt:            "task1",
				Repos:             []v3.MetaRepo{{Name: "r", Branch: "caic-0"}},
				Harness:           "claude",
				BaseImage:         "ghcr.io/caic/base:v1",
				ContainerPlatform: "linux/amd64",
				MaxCPUs:           6,
				CacheMounts:       []v3.MetaCacheMount{{Name: "npm", Description: "Node", HostPath: "~/.npm", ContainerPath: "/home/user/.npm", ReadOnly: true, Shallow: true}},
				Mounts:            []v3.MetaMount{{HostPath: "/host/work", ContainerPath: "/workspace/work", ReadOnly: true}},
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "task1", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			withDiff := mustJSON(t, agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: v3.DiffStat{{Path: "a.go", LinesAdded: 3, LinesDeleted: 1}}, Ts: 1})
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "task1", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "task1", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "compressed", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
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
			plainMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "plain", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			compressedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "compressed", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
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

			meta1 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "first", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
			asst1 := claudeAssistant(t, map[string]any{"type": "text", "text": "hello"})
			writeLogFile(t, dir, "a.jsonl", meta1, asst1)

			meta2 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "second", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude", StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)})
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
			if tasks[0].Timeline != nil {
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
			if len(tasks[0].Timeline) != 1 {
				t.Errorf("tasks[0].Msgs len = %d, want 1", len(tasks[0].Timeline))
			}
			// init2 produces 1 InitMessage; asst2 produces 1 TextMessage = 2 total.
			if len(tasks[1].Timeline) != 2 {
				t.Errorf("tasks[1].Msgs len = %d, want 2", len(tasks[1].Timeline))
			}
		})
		t.Run("FeatureFlagsAllSet", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 3, Prompt: "feat task",
				Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
				RequestedModel: "model-1", RequestedEffort: "high",
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
			if lt.RequestedModel != "model-1" {
				t.Errorf("Model = %q, want model-1", lt.RequestedModel)
			}
			if lt.RequestedEffort != "high" {
				t.Errorf("Effort = %q, want high", lt.RequestedEffort)
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
				MessageType: "caic_meta", Version: 3, Prompt: "plain task",
				Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
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
				MessageType: "caic_meta", Version: 3, Prompt: "usb only",
				Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude",
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
				MessageType: "caic_meta", Version: 3, Prompt: "session task",
				Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: string(harness.Codex),
			})
			session := mustJSON(t, agent.MetaSessionMessage{
				MessageType:   "caic_session",
				SessionID:     "thread-1",
				ReportedModel: "gpt-5.4",
				AgentVersion:  "1.2.3",
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
			if lt.ReportedModel != "gpt-5.4" {
				t.Errorf("ReportedModel = %q, want gpt-5.4", lt.ReportedModel)
			}
			if lt.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", lt.AgentVersion)
			}
		})
		t.Run("AgentVersionMetadataWithoutSession", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 3, Prompt: "pi task",
				Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: string(harness.Pi),
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
				MessageType: "caic_meta", Version: 3, Prompt: "long task",
				Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: string(harness.Codex),
			})
			session := mustJSON(t, agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "thread-old"})
			large := nativeLogLine(`{"text":"` + strings.Repeat("x", 70<<10) + `"}`)
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "stopped"})
			writeLogFile(t, dir, "long.jsonl", meta, session, large, trailer)

			tasks, err := NewStore(testLogger(), dir).LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			lt := tasks[0]
			lt.SetWireResolver(newTestWireResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return codex.New("", nil).NewWire().ParseMessage, nil
			}))

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
				MessageType: "caic_meta", Version: 3, Prompt: "session task", Harness: string(harness.Claude),
			})
			mismatch := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 2, Prompt: "session task", Harness: string(harness.Claude),
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
					if _, err := loadSemanticSessionMetadata(path, newTestWireResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
					})); err == nil || !strings.Contains(err.Error(), "invalid first log header") {
						t.Fatalf("error = %v, want invalid first header", err)
					}
				})
				t.Run(format+" mixed authority after metadata", func(t *testing.T) {
					t.Parallel()
					path := writePhysicalTestLog(t, compressed, meta, session, mismatch)
					if _, err := loadSemanticSessionMetadata(path, newTestWireResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
					})); err == nil || !strings.Contains(err.Error(), "unsupported log version") {
						t.Fatalf("error = %v, want wrong t discriminator", err)
					}
				})
				t.Run(format+" leading empty lines", func(t *testing.T) {
					t.Parallel()
					path := writePhysicalTestLog(t, compressed, "", "  ", meta, session)
					lt, err := loadSemanticSessionMetadata(path, newTestWireResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
						return func([]byte) ([]agent.Message, error) { return nil, nil }, nil
					}))

					if err != nil {
						t.Fatal(err)
					}
					if lt.SessionID != "session-1" {
						t.Fatalf("SessionID = %q, want session-1", lt.SessionID)
					}
				})
			}
		})
		t.Run("ContextClearedResetsPlanState", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "plan task", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
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
			meta2 := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "plan task", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
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
			for _, entry := range lt.Timeline {
				if marker, ok := entry.Message.(*agent.SystemMessage); ok && marker.Subtype == "context_cleared" {
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "pr task", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "pr task", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-2"}}, Harness: "claude"})
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "big task", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-3"}}, Harness: "claude"})
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
				Version:     int(agent.LogVersionV3),
				Prompt:      "task",
				Harness:     string(harness.Claude),
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "settle", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: string(harness.Claude)})
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
					meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: prefix + strconv.Itoa(i), Repos: []v3.MetaRepo{{Name: repo, Branch: "caic-0"}}, Harness: string(harness.Claude), StartedAt: mtime})
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
			metaNew := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "new", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: string(harness.Claude)})
			metaOld := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "old", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: string(harness.Claude)})
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
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "twin", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: string(harness.Claude)})
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
			wantedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "wanted", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: "claude"})
			unrelatedMeta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Prompt: "unrelated", Repos: []v3.MetaRepo{{Name: "r", Branch: "caic-1"}}, Harness: "claude"})
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
		t.Run("ArchivedRetainedTask", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			meta := mustJSON(t, agent.MetaMessage{MessageType: "caic_meta", Version: 3, Harness: "pi", Prompt: "retained"})
			writeCompressedLogFile(t, dir, "retained-repo-branch.jsonl.zst", seqOf(meta))
			tasks, err := NewStore(testLogger(), dir).LoadForTaskIDs([]string{"retained"})
			if err != nil || len(tasks) != 1 {
				t.Fatalf("archived retained task: tasks=%v err=%v", tasks, err)
			}
			if tasks[0].Prompt != "retained" || !strings.HasSuffix(tasks[0].LogPath(), ".zst") {
				t.Fatalf("archived source = %#v", tasks[0])
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
