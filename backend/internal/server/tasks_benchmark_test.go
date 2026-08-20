// Benchmarks for task SSE event replay performance.

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
)

func BenchmarkHandleTaskRawEventsPurgedReplay(b *testing.B) {
	b.Run("ClaudeSmallOutputManyDeltas", func(b *testing.B) {
		const deltaCount = 10_000
		taskID, s := benchmarkPurgedTaskEventServer(b, deltaCount)

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody)
			req.SetPathValue("id", taskID)
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

	b.Run("PiAccumulatedDeltas", func(b *testing.B) {
		const deltaCount = 2_000
		taskID, s := benchmarkPurgedPiTaskEventServer(b, deltaCount)

		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			req := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody)
			req.SetPathValue("id", taskID)
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

func benchmarkPurgedTaskEventServer(b *testing.B, deltaCount int) (string, *testRouter) {
	logDir := b.TempDir()
	taskID := ksid.NewID().String()
	finalText := "final compact response " + strings.Repeat("x", 42<<10)

	path := filepath.Join(logDir, taskID+".jsonl")
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

func benchmarkPurgedPiTaskEventServer(b *testing.B, deltaCount int) (string, *testRouter) {
	logDir := b.TempDir()
	taskID := ksid.NewID().String()
	finalText := "final compact response " + strings.Repeat("x", 42<<10)

	path := filepath.Join(logDir, taskID+".jsonl")
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
