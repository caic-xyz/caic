// Tests for the on-disk EventMessage replay cache for terminal task logs.

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

type failAfterSSEWriter struct {
	strings.Builder

	remaining int
}

func (w *failAfterSSEWriter) Write(data []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, os.ErrClosed
	}
	if len(data) > w.remaining {
		n, _ := w.WriteString(string(data[:w.remaining]))
		w.remaining -= n
		return n, os.ErrClosed
	}
	n, err := w.WriteString(string(data))
	w.remaining -= n
	return n, err
}

func newLiveReplayWriter(logPath string, prove eventreplay.ProofProvider) (*eventreplay.MessageWriter, error) {
	proof, err := prove(logPath)
	if err != nil {
		return nil, err
	}
	return eventreplay.NewMessageWriter(logPath, proof, prove)
}

func TestReplayCache(t *testing.T) {
	t.Parallel()

	t.Run("TerminalHistoryRegenerationFailurePublishesExplicitError", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		logs, err := task.LoadLogs(dir)
		if err != nil || len(logs) != 1 {
			t.Fatalf("load terminal log = (%#v, %v)", logs, err)
		}
		cache, err := eventreplay.NewCacheWriter(logPath, task.CacheProofForLog)
		if err != nil {
			t.Fatal(err)
		}
		if err := cache.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		entry := taskmgr.NewEntry(&task.Task{}, logs[0])
		(&taskHandlers{}).streamHistoryFromDisk(t.Context(), w, w, entry)
		if body := w.Body.String(); body != "event: error\ndata: {\"message\":\"task history is unavailable\"}\n\n" {
			t.Fatalf("terminal unservable replay body = %q, want explicit error", body)
		}
	})
	t.Run("MidStreamWriteFailureDoesNotRegenerateDuplicateHistory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		logs, err := task.LoadLogs(dir)
		if err != nil || len(logs) != 1 {
			t.Fatalf("load log = (%#v, %v)", logs, err)
		}
		cache, err := eventreplay.NewCacheWriter(logPath, task.CacheProofForLog)
		if err != nil {
			t.Fatal(err)
		}
		cache.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"first"}}`))
		cache.WriteEventData([]byte(`{"kind":"text","ts":2,"text":{"text":"second"}}`))
		if err := cache.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		out := &failAfterSSEWriter{remaining: 1}
		idx := 0
		err = (&taskHandlers{}).streamReplayStore(t.Context(), out, httptest.NewRecorder(), taskmgr.NewEntry(&task.Task{}, logs[0]), &idx)
		if err == nil || !strings.Contains(err.Error(), "after history publication") || out.Len() != 1 {
			t.Fatalf("stream replay = (%v, %q), want one partial write without regeneration", err, out.String())
		}
	})
	t.Run("CancelledReplayDoesNotPublishCachedHistory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "task.jsonl")
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		logs, err := task.LoadLogs(dir)
		if err != nil || len(logs) != 1 {
			t.Fatalf("load log = (%#v, %v)", logs, err)
		}
		cache, err := eventreplay.NewCacheWriter(logPath, task.CacheProofForLog)
		if err != nil {
			t.Fatal(err)
		}
		cache.WriteEventData([]byte(`{"kind":"text","ts":1,"text":{"text":"must not publish"}}`))
		if err := cache.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		out := httptest.NewRecorder()
		idx := 0
		err = (&taskHandlers{}).streamReplayStore(ctx, out, out, taskmgr.NewEntry(&task.Task{}, logs[0]), &idx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream replay error = %v, want context cancellation", err)
		}
		if out.Body.Len() != 0 || idx != 0 {
			t.Fatalf("cancelled replay published body=%q index=%d", out.Body.String(), idx)
		}
	})

	// serveEvents drives handleTaskEvents once and returns the SSE body.
	serveEvents := func(t *testing.T, s *testRouter, taskID string) string {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID)
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(w, req)
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
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
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

	t.Run("ContextClearedV1V2RegenerationParity", func(t *testing.T) {
		t.Parallel()
		replayEvents := make([][]v1.EventMessage, 0, 2)
		for _, version := range []agent.LogVersion{agent.LogVersionV1, agent.LogVersionV2} {
			logDir := t.TempDir()
			logPath := filepath.Join(logDir, "task.jsonl")
			var lines []string
			if version == agent.LogVersionV1 {
				lines = []string{
					`{"type":"caic_meta","version":1,"prompt":"test","repos":[],"harness":"claude"}`,
					`{"type":"system","subtype":"context_cleared"}`,
					`{"type":"caic_result","state":"purged"}`,
				}
			} else {
				lines = []string{
					`{"t":"caic_meta","version":2,"prompt":"test","repos":[],"harness":"claude"}`,
					`{"t":"context_cleared"}`,
					`{"t":"result","state":"purged"}`,
				}
			}
			if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			s, taskID := newServer(t, logDir)
			body := serveEvents(t, s, taskID)
			if !strings.Contains(body, `"subtype":"context_cleared"`) {
				t.Fatalf("v%d replay omitted context_cleared:\n%s", version, body)
			}
			events := readReplayCacheEvents(t, eventreplay.CachePath(logPath)).events
			if len(events) != 1 || events[0].System == nil || events[0].System.Subtype != "context_cleared" {
				t.Fatalf("v%d replay events = %#v, want one context_cleared system event", version, events)
			}
			events[0].Ts = 0 // Replay generation time is not part of v1/v2 record semantics.
			replayEvents = append(replayEvents, events)
		}
		if !reflect.DeepEqual(replayEvents[0], replayEvents[1]) {
			t.Fatalf("v1/v2 replay events differ:\nv1: %#v\nv2: %#v", replayEvents[0], replayEvents[1])
		}
	})
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
		if err := os.WriteFile(logPath, []byte("{\"type\":\"caic_meta\",\"version\":1,\"harness\":\"claude\",\"prompt\":\"test\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		w, err := newLiveReplayWriter(logPath, task.CacheProofForLog)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.WriteMessage(t.Context(), agent.ParsedMessage{Message: &agent.TextDeltaMessage{Text: "partial"}}); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteMessage(t.Context(), agent.ParsedMessage{Message: &agent.TextMessage{Text: "final"}}); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}

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
		cache, err := eventreplay.NewCacheWriter(logPath, task.CacheProofForLog)
		if err != nil {
			t.Fatal(err)
		}
		if err := cache.CommitContext(t.Context(), logPath); err != nil {
			t.Fatal(err)
		}
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
