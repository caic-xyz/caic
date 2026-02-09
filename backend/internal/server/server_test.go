package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	s := &Server{runners: map[string]*task.Runner{}}
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
	s := &Server{runners: map[string]*task.Runner{}}
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
	s := &Server{runners: map[string]*task.Runner{}}
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
	s := &Server{runners: map[string]*task.Runner{}}
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

func TestHandleFinishNotWaiting(t *testing.T) {
	s := &Server{runners: map[string]*task.Runner{}}
	s.tasks = append(s.tasks, &taskEntry{
		task: &task.Task{Prompt: "test", State: task.StatePending},
		done: make(chan struct{}),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/0/finish", http.NoBody)
	req.SetPathValue("id", "0")
	w := httptest.NewRecorder()
	handleWithTask(s, s.finishTask)(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeConflict {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeConflict)
	}
}

func TestHandleFinishWaiting(t *testing.T) {
	tk := &task.Task{Prompt: "test", State: task.StateWaiting}
	tk.InitDoneCh()
	s := &Server{runners: map[string]*task.Runner{}}
	s.tasks = append(s.tasks, &taskEntry{
		task: tk,
		done: make(chan struct{}),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/0/finish", http.NoBody)
	req.SetPathValue("id", "0")
	w := httptest.NewRecorder()
	handleWithTask(s, s.finishTask)(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Verify doneCh is closed.
	select {
	case <-tk.Done():
	default:
		t.Error("doneCh not closed after finish")
	}
}

func TestHandleCreateTaskReturnsID(t *testing.T) {
	s := &Server{
		runners: map[string]*task.Runner{
			"myrepo": {BaseBranch: "main", Dir: t.TempDir()},
		},
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
	s := &Server{runners: map[string]*task.Runner{}}
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
	s := &Server{runners: map[string]*task.Runner{}}
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
	s := &Server{runners: map[string]*task.Runner{}}
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

func TestHandleTakeoverNotWaiting(t *testing.T) {
	s := &Server{runners: map[string]*task.Runner{}}
	s.tasks = append(s.tasks, &taskEntry{
		task: &task.Task{Prompt: "test", State: task.StateRunning},
		done: make(chan struct{}),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/0/takeover", http.NoBody)
	req.SetPathValue("id", "0")
	w := httptest.NewRecorder()
	handleWithTask(s, s.takeoverTask)(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	e := decodeError(t, w)
	if e.Code != dto.CodeConflict {
		t.Errorf("code = %q, want %q", e.Code, dto.CodeConflict)
	}
}

func TestHandleListRepos(t *testing.T) {
	s := &Server{
		repos: []repoInfo{
			{RelPath: "org/repoA", AbsPath: "/src/org/repoA", BaseBranch: "main"},
			{RelPath: "repoB", AbsPath: "/src/repoB", BaseBranch: "develop"},
		},
		runners: map[string]*task.Runner{},
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
