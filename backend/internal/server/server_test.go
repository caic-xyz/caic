package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maruel/wmao/backend/internal/agent"
	"github.com/maruel/wmao/backend/internal/server/dto"
	"github.com/maruel/wmao/backend/internal/task"
)

func decodeError(t *testing.T, w *httptest.ResponseRecorder) dto.ErrorDetails {
	t.Helper()
	var resp dto.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	return resp.Error
}

func TestHandleTaskEventsNotFound(t *testing.T) {
	s := &Server{runners: map[string]*task.Runner{}, changed: make(chan struct{})}
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

func TestHandleTaskEventsInvalidID(t *testing.T) {
	s := &Server{runners: map[string]*task.Runner{}, changed: make(chan struct{})}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/abc/events", http.NoBody)
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()
	s.handleTaskEvents(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeBadRequest {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeBadRequest)
	}
}

func TestHandleTaskInputNotRunning(t *testing.T) {
	s := &Server{runners: map[string]*task.Runner{}, changed: make(chan struct{})}
	s.tasks = append(s.tasks, &taskEntry{
		task: &task.Task{Prompt: "test"},
		done: make(chan struct{}),
	})

	body := strings.NewReader(`{"prompt":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/0/input", body)
	req.SetPathValue("id", "0")
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
	s := &Server{runners: map[string]*task.Runner{}, changed: make(chan struct{})}
	s.tasks = append(s.tasks, &taskEntry{
		task: &task.Task{Prompt: "test"},
		done: make(chan struct{}),
	})

	body := strings.NewReader(`{"prompt":""}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/0/input", body)
	req.SetPathValue("id", "0")
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
	s := &Server{runners: map[string]*task.Runner{}, changed: make(chan struct{})}
	s.tasks = append(s.tasks, &taskEntry{
		task: &task.Task{Prompt: "test", State: task.StatePending},
		done: make(chan struct{}),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/0/terminate", http.NoBody)
	req.SetPathValue("id", "0")
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
	s := &Server{runners: map[string]*task.Runner{}, changed: make(chan struct{})}
	s.tasks = append(s.tasks, &taskEntry{
		task: tk,
		done: make(chan struct{}),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/0/terminate", http.NoBody)
	req.SetPathValue("id", "0")
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
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["id"]; !ok {
		t.Error("response missing 'id' field")
	}
}

func TestHandleCreateTaskMissingRepo(t *testing.T) {
	s := &Server{runners: map[string]*task.Runner{}, changed: make(chan struct{})}
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
	s := &Server{runners: map[string]*task.Runner{}, changed: make(chan struct{})}
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
	s := &Server{runners: map[string]*task.Runner{}, changed: make(chan struct{})}
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
			MessageType: "wmao_meta", Prompt: fmt.Sprintf("task %d", i), Repo: "r",
			Branch: "wmao/w" + strings.Repeat("0", i+1), StartedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "wmao_result", State: state, CostUSD: float64(i + 1)})
		writeLogFile(t, logDir, "20260101T0"+strings.Repeat("0", i+1)+"0000-wmao-w"+strings.Repeat("0", i+1)+".jsonl", meta, trailer)
	}

	s := &Server{
		runners: map[string]*task.Runner{},
		changed: make(chan struct{}),
		logDir:  logDir,
	}
	s.loadTerminatedTasks()

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3", len(s.tasks))
	}

	// Verify tasks are in ascending StartedAt order (oldest first).
	if s.tasks[0].task.Prompt != "task 0" {
		t.Errorf("tasks[0].Prompt = %q, want %q", s.tasks[0].task.Prompt, "task 0")
	}
	if s.tasks[2].task.Prompt != "task 2" {
		t.Errorf("tasks[2].Prompt = %q, want %q", s.tasks[2].task.Prompt, "task 2")
	}

	// Verify result is populated.
	if s.tasks[0].result == nil {
		t.Fatal("tasks[0].result is nil")
	}
	if s.tasks[0].result.CostUSD != 1.0 {
		t.Errorf("tasks[0].result.CostUSD = %f, want 1.0", s.tasks[0].result.CostUSD)
	}

	// Verify done channel is closed (task is terminal).
	select {
	case <-s.tasks[0].done:
	default:
		t.Error("tasks[0].done not closed")
	}
}

func TestLoadTerminatedTasksEmptyLogDir(t *testing.T) {
	s := &Server{
		runners: map[string]*task.Runner{},
		changed: make(chan struct{}),
		logDir:  "",
	}
	s.loadTerminatedTasks()
	if len(s.tasks) != 0 {
		t.Errorf("len(tasks) = %d, want 0", len(s.tasks))
	}
}
