// Tests for the on-disk EventMessage replay cache for terminal task logs.

package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

func TestReplayCache(t *testing.T) {
	t.Parallel()
	// serveEvents drives handleTaskRawEvents once and returns the SSE body.
	serveEvents := func(t *testing.T, s *testRouter, taskID string) string {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID)
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskRawEvents(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		return w.Body.String()
	}

	writePurged := func(t *testing.T, logDir, text string) string {
		t.Helper()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		initMsg := mustJSON(t, map[string]any{
			"type": "system", "subtype": "init", "model": "claude-opus-4-6",
			"claude_code_version": "2.0", "session_id": "s1",
		})
		assistant := mustJSON(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"model":   "claude-opus-4-6",
				"content": []map[string]any{{"type": "text", "text": text}},
				"usage":   map[string]any{"input_tokens": 100, "output_tokens": 50},
			},
		})
		result := mustJSON(t, agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done", DurationMs: 1000, NumTurns: 1})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", Duration: 1})
		writeLogFile(t, logDir, "task.jsonl", meta, initMsg, assistant, result, trailer)
		return filepath.Join(logDir, "task.jsonl")
	}

	newServer := func(t *testing.T, logDir string) (*testRouter, string) {
		t.Helper()
		s := newTestRouter(t)
		s.taskMgr.RegisterBackends(map[harness.Name]agent.Backend{harness.Claude: stubBackend{}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}
		var taskID string
		s.taskMgr.Range(func(id string, _ *taskmgr.Entry) bool { taskID = id; return false })
		if taskID == "" {
			t.Fatal("no task registered")
		}
		return s, taskID
	}

	t.Run("MissThenHitIsByteIdentical", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		logPath := writePurged(t, logDir, "I found the bug")
		s, taskID := newServer(t, logDir)

		first := serveEvents(t, s, taskID) // miss: renders and populates cache
		cachePath := eventreplay.CachePath(logPath)
		if _, err := os.Stat(cachePath); err != nil {
			t.Fatalf("cache file not written: %v", err)
		}
		contents := readReplayCacheEvents(t, cachePath)
		if len(contents.events) == 0 {
			t.Fatal("cache has no events")
		}
		if strings.Contains(strings.Join(contents.rawLines, "\n"), "event: ") {
			t.Fatalf("cache stores SSE framing instead of EventMessage JSONL: %q", contents.rawLines)
		}
		if !replayCacheHasText(contents.events, "I found the bug") {
			t.Fatalf("cache EventMessages missing assistant text: %#v", contents.events)
		}
		second := serveEvents(t, s, taskID) // hit: served from cache

		if first != second {
			t.Fatalf("cache hit body differs from first render:\nfirst:\n%s\nsecond:\n%s", first, second)
		}
		for _, want := range []string{"I found the bug", "event: ready"} {
			if !strings.Contains(second, want) {
				t.Fatalf("cached body missing %q:\n%s", want, second)
			}
		}
	})

	t.Run("LiveWriterCompactsEventMessages", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		logPath := filepath.Join(logDir, "task.jsonl")
		writeLogFile(t, logDir, "task.jsonl", mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}))

		w := eventreplay.NewMessageWriter(logPath, harness.Claude)
		w.WriteMessage(&agent.TextDeltaMessage{Text: "partial"})
		w.WriteMessage(&agent.TextMessage{Text: "final"})
		w.Commit(logPath)

		contents := readReplayCacheEvents(t, eventreplay.CachePath(logPath))
		if len(contents.events) != 1 {
			t.Fatalf("cache event count = %d, want 1: %#v", len(contents.events), contents.events)
		}
		if contents.events[0].Kind != v1.EventKindText || contents.events[0].Text == nil || contents.events[0].Text.Text != "final" {
			t.Fatalf("cache event = %#v, want final text", contents.events[0])
		}
	})

	t.Run("CorruptCacheRegeneratesFromRaw", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		logPath := writePurged(t, logDir, "raw truth")
		cachePath := eventreplay.CachePath(logPath)
		if err := os.WriteFile(cachePath, []byte("not zstd"), 0o600); err != nil {
			t.Fatal(err)
		}
		s, taskID := newServer(t, logDir)

		body := serveEvents(t, s, taskID)
		if !strings.Contains(body, "raw truth") {
			t.Fatalf("body missing regenerated raw content:\n%s", body)
		}
		contents := readReplayCacheEvents(t, cachePath)
		if !replayCacheHasText(contents.events, "raw truth") {
			t.Fatalf("regenerated cache missing raw content: %#v", contents.events)
		}
	})

	t.Run("EmptyCacheRegeneratesFromRaw", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		logPath := writePurged(t, logDir, "empty cache raw truth")
		cache := eventreplay.NewCacheWriter(logPath)
		cache.Commit(logPath)
		s, taskID := newServer(t, logDir)

		body := serveEvents(t, s, taskID)
		if !strings.Contains(body, "empty cache raw truth") {
			t.Fatalf("body missing regenerated raw content:\n%s", body)
		}
		contents := readReplayCacheEvents(t, eventreplay.CachePath(logPath))
		if !replayCacheHasText(contents.events, "empty cache raw truth") {
			t.Fatalf("regenerated cache missing raw content: %#v", contents.events)
		}
	})

	t.Run("StaleLogInvalidatesCache", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		logPath := writePurged(t, logDir, "original message")
		s, taskID := newServer(t, logDir)

		if body := serveEvents(t, s, taskID); !strings.Contains(body, "original message") {
			t.Fatalf("first body missing original message:\n%s", body)
		}

		// Rewrite the log with new content and a later mtime. The size+mtime
		// binding in the cache header must force a fresh render.
		writePurged(t, logDir, "rewritten message")
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(logPath, future, future); err != nil {
			t.Fatal(err)
		}
		// Reload so the entry's LoadedTask points at the rewritten log.
		s2, taskID2 := newServer(t, logDir)
		body := serveEvents(t, s2, taskID2)
		if !strings.Contains(body, "rewritten message") {
			t.Fatalf("stale cache served; expected rewritten message:\n%s", body)
		}
		if strings.Contains(body, "original message") {
			t.Fatalf("stale cache leaked original message:\n%s", body)
		}
	})
}

type replayCacheContents struct {
	events   []v1.EventMessage
	rawLines []string
}

func readReplayCacheEvents(t *testing.T, path string) replayCacheContents {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dec.Close)

	sc := bufio.NewScanner(dec)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	if !sc.Scan() {
		t.Fatal("cache missing header")
	}
	var header eventreplay.CacheHeader
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		t.Fatal(err)
	}
	if header.Version != eventreplay.CacheVersion {
		t.Fatalf("cache version = %d, want %d", header.Version, eventreplay.CacheVersion)
	}

	var events []v1.EventMessage
	var rawLines []string
	for sc.Scan() {
		rawLine := sc.Text()
		rawLines = append(rawLines, rawLine)
		var ev v1.EventMessage
		if err := json.Unmarshal([]byte(rawLine), &ev); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return replayCacheContents{events: events, rawLines: rawLines}
}

func replayCacheHasText(events []v1.EventMessage, text string) bool {
	for i := range events {
		if events[i].Text != nil && strings.Contains(events[i].Text.Text, text) {
			return true
		}
	}
	return false
}
