package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/server/dto"
	"github.com/maruel/caic/backend/internal/task"
)

func decodeError(t *testing.T, w *httptest.ResponseRecorder) dto.ErrorDetails {
	t.Helper()
	var resp dto.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	return resp.Error
}

func newTestServer() *Server {
	return &Server{
		runners: map[string]*task.Runner{},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
	}
}

func TestHandleTaskEventsNotFound(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/99/events", http.NoBody)
	req.SetPathValue("id", "99")
	w := httptest.NewRecorder()
	s.handleTaskEvents(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeNotFound {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeNotFound)
	}
}

func TestHandleTaskEventsNonexistentID(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/abc/events", http.NoBody)
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	s.handleTaskEvents(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeNotFound {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeNotFound)
	}
}

func TestHandleTaskInputNotRunning(t *testing.T) {
	s := newTestServer()
	s.tasks["t1"] = &taskEntry{
		task: &task.Task{Prompt: "test"},
		done: make(chan struct{}),
	}

	body := strings.NewReader(`{"prompt":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/input", body)
	req.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	handleWithTask(s, s.sendInput)(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeConflict {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeConflict)
	}
}

func TestHandleTaskInputEmptyPrompt(t *testing.T) {
	s := newTestServer()
	s.tasks["t1"] = &taskEntry{
		task: &task.Task{Prompt: "test"},
		done: make(chan struct{}),
	}

	body := strings.NewReader(`{"prompt":""}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/input", body)
	req.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	handleWithTask(s, s.sendInput)(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeBadRequest {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeBadRequest)
	}
}

func TestHandleTerminateNotWaiting(t *testing.T) {
	s := newTestServer()
	s.tasks["t1"] = &taskEntry{
		task: &task.Task{Prompt: "test", State: task.StatePending},
		done: make(chan struct{}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/terminate", http.NoBody)
	req.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	handleWithTask(s, s.terminateTask)(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeConflict {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeConflict)
	}
}

func TestHandleTerminateWaiting(t *testing.T) {
	tk := &task.Task{Prompt: "test", State: task.StateWaiting}
	tk.InitDoneCh()
	s := newTestServer()
	s.tasks["t1"] = &taskEntry{
		task: tk,
		done: make(chan struct{}),
	}

	// Simulate the Kill goroutine that runs in production.
	runner := &task.Runner{Dir: t.TempDir(), BaseBranch: "main"}
	done := s.tasks["t1"].done
	go func() {
		defer close(done)
		runner.Kill(t.Context(), tk)
	}()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/terminate", http.NoBody)
	req.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	handleWithTask(s, s.terminateTask)(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify doneCh is closed.
	select {
	case <-tk.Done():
	default:
		t.Error("doneCh not closed after terminate")
	}
}

func TestHandleCreateTaskReturnsID(t *testing.T) {
	s := &Server{
		runners: map[string]*task.Runner{
			"myrepo": {BaseBranch: "main", Dir: t.TempDir()},
		},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
	}
	handler := s.handleCreateTask(t.Context())

	body := strings.NewReader(`{"prompt":"test task","repo":"myrepo"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	var resp dto.CreateTaskResp
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID == 0 {
		t.Error("response has zero 'id' field")
	}
}

func TestHandleCreateTaskMissingRepo(t *testing.T) {
	s := newTestServer()
	handler := s.handleCreateTask(t.Context())

	body := strings.NewReader(`{"prompt":"test task"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeBadRequest {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeBadRequest)
	}
}

func TestHandleCreateTaskUnknownRepo(t *testing.T) {
	s := newTestServer()
	handler := s.handleCreateTask(t.Context())

	body := strings.NewReader(`{"prompt":"test","repo":"nonexistent"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeBadRequest {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeBadRequest)
	}
}

func TestHandleCreateTaskUnknownField(t *testing.T) {
	s := newTestServer()
	handler := s.handleCreateTask(t.Context())

	body := strings.NewReader(`{"prompt":"test","repo":"r","bogus":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeBadRequest {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeBadRequest)
	}
}

func TestHandleListRepos(t *testing.T) {
	s := &Server{
		repos: []repoInfo{
			{RelPath: "org/repoA", AbsPath: "/src/org/repoA", BaseBranch: "main"},
			{RelPath: "repoB", AbsPath: "/src/repoB", BaseBranch: "develop"},
		},
		runners: map[string]*task.Runner{},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos", http.NoBody)
	w := httptest.NewRecorder()
	handle(s.listRepos)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var repos []dto.RepoJSON
	if err := json.NewDecoder(w.Body).Decode(&repos); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("len = %d, want 2", len(repos))
	}
	if repos[0].Path != "org/repoA" {
		t.Errorf("repos[0].Path = %q, want %q", repos[0].Path, "org/repoA")
	}
	if repos[1].BaseBranch != "develop" {
		t.Errorf("repos[1].BaseBranch = %q, want %q", repos[1].BaseBranch, "develop")
	}
}

func writeLogFile(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	data := make([]byte, 0, len(lines)*64)
	for _, l := range lines {
		data = append(data, l...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoadTerminatedTasksOnStartup(t *testing.T) {
	logDir := t.TempDir()

	// Write 3 terminal task logs.
	for i, state := range []string{"terminated", "failed", "terminated"} {
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: fmt.Sprintf("task %d", i), Repo: "r",
			Branch: "caic/w" + strings.Repeat("0", i+1), StartedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: state, CostUSD: float64(i + 1)})
		writeLogFile(t, logDir, fmt.Sprintf("%d.jsonl", i), meta, trailer)
	}

	s := &Server{
		runners: map[string]*task.Runner{},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
		logDir:  logDir,
	}
	if err := s.loadTerminatedTasks(); err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3", len(s.tasks))
	}

	// Collect prompts sorted by ksid (time-sortable) to verify all loaded.
	prompts := make([]string, 0, len(s.tasks))
	var anyEntry *taskEntry
	for _, e := range s.tasks {
		prompts = append(prompts, e.task.Prompt)
		if anyEntry == nil {
			anyEntry = e
		}
	}
	sort.Strings(prompts)
	if prompts[0] != "task 0" || prompts[1] != "task 1" || prompts[2] != "task 2" {
		t.Errorf("prompts = %v, want [task 0, task 1, task 2]", prompts)
	}

	// Verify result is populated on at least one entry.
	if anyEntry.result == nil {
		t.Fatal("result is nil on a loaded entry")
	}

	// Verify done channel is closed (task is terminal).
	for _, e := range s.tasks {
		select {
		case <-e.done:
		default:
			t.Error("done channel not closed on a loaded entry")
		}
	}
}

func TestTerminatedTaskEventsAfterRestart(t *testing.T) {
	logDir := t.TempDir()

	// Write a terminated task log with real agent messages.
	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
		Repo: "r", Branch: "caic/w0", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	initMsg := mustJSON(t, agent.SystemInitMessage{
		MessageType: "system", Subtype: "init", Model: "claude-opus-4-6", Version: "2.0", SessionID: "s1",
	})
	assistant := mustJSON(t, agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Model:   "claude-opus-4-6",
			Content: []agent.ContentBlock{{Type: "text", Text: "I found the bug"}},
			Usage:   agent.Usage{InputTokens: 100, OutputTokens: 50},
		},
	})
	result := mustJSON(t, agent.ResultMessage{
		MessageType: "result", Subtype: "success", Result: "done", TotalCostUSD: 0.05, DurationMs: 1000, NumTurns: 1,
	})
	trailer := mustJSON(t, agent.MetaResultMessage{
		MessageType: "caic_result", State: "terminated", CostUSD: 0.05, DurationMs: 1000,
	})
	writeLogFile(t, logDir, "task.jsonl", meta, initMsg, assistant, result, trailer)

	// Simulate server restart: load terminated tasks from logs.
	s := &Server{
		runners: map[string]*task.Runner{},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
		logDir:  logDir,
	}
	if err := s.loadTerminatedTasks(); err != nil {
		t.Fatal(err)
	}

	// Find the task ID.
	s.mu.Lock()
	if len(s.tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(s.tasks))
	}
	var taskID string
	for id := range s.tasks {
		taskID = id
	}
	s.mu.Unlock()

	// Subscribe to events via SSE. The handler should return immediately for
	// terminated tasks instead of blocking until context deadline.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+taskID+"/events", http.NoBody).WithContext(ctx)
	req.SetPathValue("id", taskID)
	w := httptest.NewRecorder()
	start := time.Now()
	s.handleTaskEvents(w, req)
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("handleTaskEvents blocked for %v; terminated tasks should return immediately after history replay", elapsed)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	// Parse SSE events from the response body.
	body := w.Body.String()
	var events []dto.EventMessage
	for _, line := range strings.Split(body, "\n") {
		after, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev dto.EventMessage
		if err := json.Unmarshal([]byte(after), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatal("no SSE events received for terminated task with messages")
	}

	// Verify we got the expected event kinds.
	kinds := make([]string, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	wantKinds := []string{"init", "text", "usage", "result"}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("event kinds = %v, want %v", kinds, wantKinds)
	}
	for i := range wantKinds {
		if kinds[i] != wantKinds[i] {
			t.Errorf("kinds[%d] = %q, want %q", i, kinds[i], wantKinds[i])
		}
	}
	// Verify text content.
	if events[1].Text == nil || events[1].Text.Text != "I found the bug" {
		t.Errorf("text event = %+v, want text 'I found the bug'", events[1].Text)
	}
}

func TestLoadTerminatedTasksCostInJSON(t *testing.T) {
	logDir := t.TempDir()

	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta", Version: 1, Prompt: "fix bug",
		Repo: "r", Branch: "caic/w0", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	initMsg := mustJSON(t, agent.SystemInitMessage{
		MessageType: "system", Subtype: "init", Model: "claude-opus-4-6", Version: "2.0", SessionID: "s1",
	})
	result := mustJSON(t, agent.ResultMessage{
		MessageType: "result", Subtype: "success", Result: "done",
		TotalCostUSD: 1.23, DurationMs: 5000, NumTurns: 3,
	})
	trailer := mustJSON(t, agent.MetaResultMessage{
		MessageType: "caic_result", State: "terminated",
		CostUSD: 1.23, DurationMs: 5000, NumTurns: 3,
	})
	writeLogFile(t, logDir, "task.jsonl", meta, initMsg, result, trailer)

	s := &Server{
		runners: map[string]*task.Runner{},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
		logDir:  logDir,
	}
	if err := s.loadTerminatedTasks(); err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(s.tasks))
	}
	for _, e := range s.tasks {
		j := s.toJSON(e)
		if j.CostUSD != 1.23 {
			t.Errorf("CostUSD = %f, want 1.23", j.CostUSD)
		}
		if j.DurationMs != 5000 {
			t.Errorf("DurationMs = %d, want 5000", j.DurationMs)
		}
		if j.NumTurns != 3 {
			t.Errorf("NumTurns = %d, want 3", j.NumTurns)
		}
		if j.Model != "claude-opus-4-6" {
			t.Errorf("Model = %q, want %q", j.Model, "claude-opus-4-6")
		}
		if j.ClaudeCodeVersion != "2.0" {
			t.Errorf("ClaudeCodeVersion = %q, want %q", j.ClaudeCodeVersion, "2.0")
		}
	}
}

func TestLoadTerminatedTasksBackfillsCostFromMessages(t *testing.T) {
	logDir := t.TempDir()

	// Trailer has zero cost (e.g. session exited without final ResultMessage),
	// but the messages contain a ResultMessage with cost.
	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta", Version: 1, Prompt: "fix bug",
		Repo: "r", Branch: "caic/w0", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	initMsg := mustJSON(t, agent.SystemInitMessage{
		MessageType: "system", Subtype: "init", Model: "claude-opus-4-6", Version: "2.0", SessionID: "s1",
	})
	result := mustJSON(t, agent.ResultMessage{
		MessageType: "result", Subtype: "success", Result: "done",
		TotalCostUSD: 0.42, DurationMs: 3000, NumTurns: 2,
	})
	trailer := mustJSON(t, agent.MetaResultMessage{
		MessageType: "caic_result", State: "terminated",
		// CostUSD intentionally zero.
	})
	writeLogFile(t, logDir, "task.jsonl", meta, initMsg, result, trailer)

	s := &Server{
		runners: map[string]*task.Runner{},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
		logDir:  logDir,
	}
	if err := s.loadTerminatedTasks(); err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(s.tasks))
	}
	for _, e := range s.tasks {
		j := s.toJSON(e)
		if j.CostUSD != 0.42 {
			t.Errorf("CostUSD = %f, want 0.42 (should be backfilled from ResultMessage)", j.CostUSD)
		}
		if j.NumTurns != 2 {
			t.Errorf("NumTurns = %d, want 2", j.NumTurns)
		}
		if j.DurationMs != 3000 {
			t.Errorf("DurationMs = %d, want 3000", j.DurationMs)
		}
	}
}

func TestLoadTerminatedTasksEmptyDir(t *testing.T) {
	s := &Server{
		runners: map[string]*task.Runner{},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
		logDir:  t.TempDir(),
	}
	if err := s.loadTerminatedTasks(); err != nil {
		t.Fatal(err)
	}
	if len(s.tasks) != 0 {
		t.Errorf("len(tasks) = %d, want 0", len(s.tasks))
	}
}
