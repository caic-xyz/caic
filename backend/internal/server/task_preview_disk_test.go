// Tests and benchmarks disk previews for authoritative ownership, streaming content, and cancellation.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	logv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func TestRecentDiskPreview(t *testing.T) {
	t.Parallel()
	t.Run("root ownership precedes trailing child records", func(t *testing.T) {
		t.Parallel()
		records := []string{
			mustJSON(t, logv3.MetaMessage{MessageType: "caic_meta", Version: 3, Harness: "codex", Prompt: "root work"}),
			`{"t":"agent","ts":1.000,"msg":{"method":"thread/started","params":{"thread":{"id":"root"}}}}`,
			`{"t":"agent","ts":2.000,"msg":{"method":"item/completed","params":{"threadId":"root","turnId":"turn","item":{"id":"root-answer","type":"agentMessage","text":"Root explanation"}}}}`,
			`{"t":"agent","ts":3.000,"msg":{"method":"thread/started","params":{"thread":{"id":"child","parentThreadId":"root"}}}}`,
			`{"t":"agent","ts":4.000,"msg":{"method":"item/completed","params":{"threadId":"child","turnId":"child-turn","item":{"id":"child-answer","type":"agentMessage","text":"Child output must stay out of the root preview"}}}}`,
		}
		path := filepath.Join(t.TempDir(), "task.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		source, err := taskslog.LoadHistorySource(path)
		if err != nil {
			t.Fatal(err)
		}
		source.SetWireResolver(agent.Backends{harness.Codex: codex.New("", nil)})
		tracker := apiconv.NewToolTimingTracker(harness.Codex, "", nil)
		events, err := recentDiskPreview(t.Context(), source, tracker)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		var root string
		for _, event := range events {
			if event.Kind == v1.EventKindText {
				if event.Text.Text != "Root explanation" {
					t.Fatalf("wrong root text: %+v", event)
				}
				found = true
				root = event.Source.RootThreadID
			}
			if agent.MessageSource(event.Source).ThreadID == "child" {
				t.Fatalf("child leaked: %+v", event)
			}
		}
		if root != "root" {
			t.Fatalf("root ownership missing: %q", root)
		}
		if !found {
			t.Fatal("root response missing")
		}
	})
	t.Run("projected fragments use the same bounded selection", func(t *testing.T) {
		t.Parallel()
		messages := []agent.Message{&agent.UserInputMessage{Text: "Start"}, &agent.TextDeltaMessage{Text: "one "}, &agent.TextDeltaMessage{Text: "answer"}, &agent.ToolUseMessage{ToolUseID: "tool", Name: "Bash"}, &agent.ToolOutputDeltaMessage{ToolUseID: "tool", Delta: "out"}, &agent.ToolOutputDeltaMessage{ToolUseID: "tool", Delta: "put"}}
		forward := apiconv.NewToolTimingTracker(harness.Claude, "", nil)
		projected := make([]v1.EventMessage, 0, len(messages))
		for _, message := range messages {
			projected = append(projected, forward.ConvertMessage(message, time.Time{})...)
		}
		preview := newTaskPreview()
		for i := len(projected) - 1; i >= 0; i-- {
			record := projectedPreviewRecord(&projected[i])
			if !preview.addRecord(&record) {
				break
			}
		}
		events := preview.events()
		encoded, err := json.Marshal(events)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 4 || events[1].TextDelta.Text != "one answer" || events[3].ToolOutputDelta.Delta != "output" {
			t.Fatalf("bad projected preview: %s", encoded)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "task.jsonl")
		if err := os.WriteFile(path, []byte(mustJSON(t, logv3.MetaMessage{MessageType: "caic_meta", Version: 3, Harness: "codex", Prompt: "work"})+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		source, err := taskslog.LoadHistorySource(path)
		if err != nil {
			t.Fatal(err)
		}
		source.SetWireResolver(agent.Backends{harness.Codex: codex.New("", nil)})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := recentDiskPreview(ctx, source, apiconv.NewToolTimingTracker(harness.Codex, "", nil)); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error: %v", err)
		}
	})
}

func BenchmarkHandleTaskEventsBackwardDiskHistory(b *testing.B) {
	id := ksid.NewID()
	path := filepath.Join(b.TempDir(), id.String()+".jsonl")
	var data strings.Builder
	data.WriteString(benchJSON(b, logv3.MetaMessage{MessageType: "caic_meta", Version: 3, Harness: "claude", Prompt: "benchmark preview"}) + "\n")
	delta := `{"t":"agent","ts":1.000,"msg":{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}}}` + "\n"
	for range 10_000 {
		data.WriteString(delta)
	}
	final := benchJSON(b, map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]string{{"type": "text", "text": "final compact response " + strings.Repeat("x", 42<<10)}}}})
	data.WriteString(benchJSON(b, &logv3.NativeRecord{Type: "agent", Time: time.Unix(2, 0), Message: json.RawMessage(final)}) + "\n")
	data.WriteString(`{"t":"agent","ts":3.000,"msg":{"type":"result","subtype":"success","result":"done","num_turns":1}}` + "\n")
	if err := os.WriteFile(path, []byte(data.String()), 0o600); err != nil {
		b.Fatal(err)
	}
	s := newTestRouter(b, map[harness.Name]agent.Backend{harness.Claude: claudecode.New()})
	tk := mustNewTask(b, id, agent.Prompt{Text: "benchmark preview"}, harness.Claude)
	tk.SetState(taskslog.StatePurged)
	entry := s.taskMgr.NewEntry(tk, nil)
	entry.LogPath.Set(path)
	s.taskMgr.Insert(id, entry)
	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/caic/v1/tasks/"+id.String()+"/events?backward=1", http.NoBody)
		req.SetPathValue("id", id.String())
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(w, req)
		if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("event: ready")) {
			b.Fatal("canonical ready marker missing")
		}
	}
}
