// Benchmarks task snapshots, bounded MCP results, and task-list SSE publication.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/maruel/gomode/mcp"
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
		out, replays, _ := taskSvc.taskListSnapshotWithReplay(b.Context(), cursors)
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
	registry := &mcpTools{taskSvc: testTaskHandlers(s).taskSvc}
	b.ReportAllocs()
	b.ReportMetric(mcpTaskPageSizeDefault, "tasks/op")
	b.ResetTimer()
	for b.Loop() {
		result := registry.handleTasksList(b.Context(), &mcp.ToolRequest[mcpTaskListArgs]{Arguments: mcpTaskListArgs{}})
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
	registry := &mcpTools{serverConfig: s.serverHandlers}
	b.ReportAllocs()
	b.ReportMetric(mcpRepoPageSizeDefault, "repos/op")
	b.ResetTimer()
	for b.Loop() {
		result := registry.handleReposList(b.Context(), &mcp.ToolRequest[mcpRepoListArgs]{Arguments: mcpRepoListArgs{}})
		output, ok := result.Structured.(mcpRepoListOutput)
		if result.IsError || !ok || len(output.Repositories) != mcpRepoPageSizeDefault {
			b.Fatal("repository-list page has unexpected size")
		}
	}
}

func BenchmarkTaskListStreamInitialBatch(b *testing.B) {
	for _, count := range []int{1, 100} {
		b.Run(fmt.Sprintf("tasks_%d", count), func(b *testing.B) {
			s := newTestRouter(b, nil)
			for range count {
				id := ksid.NewID()
				insertTestTask(s, id, mustNewTask(b, id, agent.Prompt{Text: "benchmark task"}, harness.Claude))
			}
			s.taskMgr.CompleteRestoration(nil)
			ctx, cancel := context.WithCancel(context.WithValue(b.Context(), httpLoggerKey{}, slog.New(slog.DiscardHandler)))
			cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/tasks/events", nil)
			handler := testTaskHandlers(s)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				w := httptest.NewRecorder()
				handler.handleTaskListEvents(w, req)
				b.ReportMetric(float64(w.Body.Len()), "wire-B/op")
			}
		})
	}
}
