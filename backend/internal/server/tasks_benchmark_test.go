// Benchmarks for task snapshots, MCP result bounding, and SSE event replay performance.

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	capipi "github.com/caic-xyz/caic/backend/internal/agent/pi"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func BenchmarkTaskListSnapshot(b *testing.B) {
	s := newTestRouter(b, nil)
	for i := range 10 {
		id := ksid.NewID()
		task := mustNewTask(b, id, agent.Prompt{Text: "benchmark task"}, harness.Claude)
		if i%2 == 0 {
			task.SetState(taskslog.StateStopped)
		}
		insertTestTask(s, id, task)
	}

	taskSvc := testTaskHandlers(s).taskSvc
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := len(taskSvc.taskListSnapshot(b.Context())); got != 10 {
			b.Fatalf("task count = %d, want 10", got)
		}
	}
}

func BenchmarkTaskListSnapshotWithReplay(b *testing.B) {
	s := newTestRouter(b, nil)
	for i := range 10 {
		id := ksid.NewID()
		task := mustNewTask(b, id, agent.Prompt{Text: "benchmark task"}, harness.Claude)
		if i%2 == 0 {
			task.SetState(taskslog.StateStopped)
		}
		insertTestTask(s, id, task)
	}

	taskSvc := testTaskHandlers(s).taskSvc
	// Seed every cursor to the current sequence so the benchmark measures the
	// steady state where no task has pending transitions to replay.
	cursors := map[string]uint64{}
	s.taskMgr.Range(func(id ksid.ID, e *taskmgr.Entry) bool {
		_, seq, _ := e.Task().SnapshotWithStateHistory(0)
		cursors[id.String()] = seq
		return true
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		out, replays := taskSvc.taskListSnapshotWithReplay(b.Context(), cursors)
		if len(out) != 10 || len(replays) != 10 {
			b.Fatalf("task count = %d, replay count = %d, want 10", len(out), len(replays))
		}
	}
}

func BenchmarkBoundedTextToolResult(b *testing.B) {
	text := strings.Repeat("a", mcpTextOutputMaxBytes*2)
	b.ReportAllocs()
	b.ReportMetric(float64(len(text)), "input-B/op")
	b.ResetTimer()
	for b.Loop() {
		result := boundedTextToolResult(text)
		output, ok := result.Structured.(mcp.TextOutput)
		if result.IsError || !ok || len(output.Result) != mcpTextOutputMaxBytes {
			b.Fatal("bounded result has unexpected size")
		}
	}
}

func BenchmarkMCPTaskListPage(b *testing.B) {
	s := newTestRouter(b, nil)
	for range 100 {
		id := ksid.NewID()
		task := mustNewTask(b, id, agent.Prompt{Text: "benchmark task"}, harness.Claude)
		insertTestTask(s, id, task)
	}
	registry := &mcpRegistry{taskSvc: testTaskHandlers(s).taskSvc}
	b.ReportAllocs()
	b.ReportMetric(mcpTaskPageSizeDefault, "tasks/op")
	b.ResetTimer()
	for b.Loop() {
		result := registry.handleTasksList(b.Context(), mcpTaskListArgs{})
		output, ok := result.Structured.(mcpTaskListOutput)
		if result.IsError || !ok || len(output.Tasks) != mcpTaskPageSizeDefault {
			b.Fatal("task-list page has unexpected size")
		}
	}
}

func BenchmarkMCPRepoListPage(b *testing.B) {
	s := newTestRouter(b, nil)
	checks := make([]forge.Check, 100)
	for i := range checks {
		checks[i].Name = fmt.Sprintf("check-%03d", i)
	}
	for i := range mcpRepoPageSizeDefault {
		path := fmt.Sprintf("repo-%03d", i)
		registerRouterCheckout(b, s.checkouts, path, &repo.Checkout{BaseBranch: "main", Dir: b.TempDir()})
		s.repoStatus.SetResultIfChanged(path, "sha", forgecache.Result{Status: forge.CIStatusSuccess, Checks: checks})
	}
	registry := &mcpRegistry{serverConfig: s.serverHandlers}
	b.ReportAllocs()
	b.ReportMetric(mcpRepoPageSizeDefault, "repos/op")
	b.ResetTimer()
	for b.Loop() {
		result := registry.handleReposList(b.Context(), mcpRepoListArgs{})
		output, ok := result.Structured.(mcpRepoListOutput)
		if result.IsError || !ok || len(output.Repositories) != mcpRepoPageSizeDefault {
			b.Fatal("repository-list page has unexpected size")
		}
	}
}

func BenchmarkHandleTaskRawEventsPurgedReplay(b *testing.B) {
	b.Run("ClaudeSmallOutputManyDeltas", func(b *testing.B) {
		const deltaCount = 10_000
		taskID, s := benchmarkPurgedTaskEventServer(b, deltaCount)

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
			req.SetPathValue("id", taskID.String())
			w := httptest.NewRecorder()

			testTaskHandlers(s).handleTaskEvents(w, req)

			if w.Code != http.StatusOK {
				b.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("final compact response")) {
				b.Fatal("SSE body did not contain final response")
			}
			b.ReportMetric(float64(w.Body.Len()), "response-B/op")
		}
	})

	b.Run("ClaudeResumeAfterAll", func(b *testing.B) {
		const deltaCount = 10_000
		taskID, s := benchmarkPurgedTaskEventServer(b, deltaCount)
		entry, ok := s.taskMgr.GetEntry(taskID)
		if !ok {
			b.Fatal("benchmark task disappeared")
		}
		lastEventID := taskEventID{
			timeline: entry.Task().TimelineID(),
			source:   taskEventSourceDisk,
			message:  deltaCount + 4,
		}.String()

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
			req.SetPathValue("id", taskID.String())
			req.Header.Set("Last-Event-ID", lastEventID)
			w := httptest.NewRecorder()

			testTaskHandlers(s).handleTaskEvents(w, req)

			if w.Code != http.StatusOK {
				b.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
			}
			if bytes.Contains(w.Body.Bytes(), []byte("final compact response")) || !bytes.Contains(w.Body.Bytes(), []byte("event: ready")) {
				b.Fatal("resumed SSE body did not contain only the ready tail")
			}
			b.ReportMetric(float64(w.Body.Len()), "response-B/op")
		}
	})

	b.Run("PiAccumulatedDeltas", func(b *testing.B) {
		const deltaCount = 2_000
		taskID, s := benchmarkPurgedPiTaskEventServer(b, deltaCount)

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
			req.SetPathValue("id", taskID.String())
			w := httptest.NewRecorder()

			testTaskHandlers(s).handleTaskEvents(w, req)

			if w.Code != http.StatusOK {
				b.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("final compact response")) {
				b.Fatal("SSE body did not contain final response")
			}
		}
	})
}

func benchmarkPurgedTaskEventServer(b *testing.B, deltaCount int) (ksid.ID, *testRouter) {
	logDir := b.TempDir()
	taskID := ksid.NewID()
	finalText := "final compact response " + strings.Repeat("x", 42<<10)

	path := filepath.Join(logDir, taskID.String()+".jsonl")
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	lines := []string{
		benchJSON(b, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     1,
			Prompt:      "benchmark replay",
			Repos:       []agent.MetaRepo{{Name: "r", Branch: "caic-0"}},
			Harness:     harness.Claude,
			StartedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}),
		benchJSON(b, map[string]any{
			"type":                "system",
			"subtype":             "init",
			"model":               "claude-opus-4-6",
			"claude_code_version": "2.0",
			"session_id":          "s1",
		}),
	}
	for _, line := range lines {
		_, _ = f.WriteString(line + "\n")
	}
	for i := range deltaCount {
		_, _ = fmt.Fprintf(f, `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"delta-%05d "}}}`+"\n", i)
	}
	for _, line := range []string{
		benchJSON(b, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"model": "claude-opus-4-6",
				"content": []map[string]any{
					{"type": "text", "text": finalText},
				},
				"usage": map[string]any{
					"input_tokens": 100, "output_tokens": 50,
				},
			},
		}),
		benchJSON(b, agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done", TotalCostUSD: 0.05, DurationMs: 1000, NumTurns: 1}),
		benchJSON(b, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", CostUSD: 0.05, Duration: 1}),
	} {
		_, _ = f.WriteString(line + "\n")
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}

	s := newTestRouter(b, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
	if err := loadPurgedTasksForTest(s, logDir); err != nil {
		b.Fatal(err)
	}
	return taskID, s
}

func benchmarkPurgedPiTaskEventServer(b *testing.B, deltaCount int) (ksid.ID, *testRouter) {
	logDir := b.TempDir()
	taskID := ksid.NewID()
	finalText := "final compact response " + strings.Repeat("x", 42<<10)

	path := filepath.Join(logDir, taskID.String()+".jsonl")
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	for _, line := range []string{
		benchJSON(b, agent.MetaMessage{
			MessageType: "caic_meta",
			Version:     1,
			Prompt:      "benchmark pi replay",
			Repos:       []agent.MetaRepo{{Name: "r", Branch: "caic-0"}},
			Harness:     harness.Pi,
			StartedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}),
		`{"type":"message_start","message":{"role":"assistant","model":"gpt-5.5"}}`,
	} {
		_, _ = f.WriteString(line + "\n")
	}
	var accumulated strings.Builder
	for i := range deltaCount {
		delta := fmt.Sprintf("delta-%05d ", i)
		accumulated.WriteString(delta)
		_, _ = fmt.Fprintf(f, `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":%q},"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`+"\n", delta, accumulated.String())
	}
	for _, line := range []string{
		fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%q}],"stopReason":"stop"}}`, finalText),
		benchJSON(b, agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done", TotalCostUSD: 0.05, DurationMs: 1000, NumTurns: 1}),
		benchJSON(b, agent.MetaResultMessage{MessageType: "caic_result", State: "purged", CostUSD: 0.05, Duration: 1}),
	} {
		_, _ = f.WriteString(line + "\n")
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}

	s := newTestRouter(b, map[harness.Name]agent.Backend{harness.Pi: capipi.New("", nil)})
	if err := loadPurgedTasksForTest(s, logDir); err != nil {
		b.Fatal(err)
	}
	return taskID, s
}

func benchJSON(b *testing.B, v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		b.Fatal(err)
	}
	return string(data)
}
