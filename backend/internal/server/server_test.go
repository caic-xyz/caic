package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// stubBackend implements agent.Backend for test map-membership checks.
type stubBackend struct{}

func (stubBackend) Harness() agent.Harness { return "stub" }

func (stubBackend) Start(context.Context, agent.Options, chan<- agent.Message, io.Writer) (*agent.Session, error) {
	return nil, errors.New("stub")
}

func (stubBackend) AttachRelay(context.Context, string, int64, chan<- agent.Message, io.Writer) (*agent.Session, error) {
	return nil, errors.New("stub")
}

func (stubBackend) ReadRelayOutput(context.Context, string) ([]agent.Message, int64, error) {
	return nil, 0, errors.New("stub")
}

func (stubBackend) ParseMessage([]byte) (agent.Message, error) {
	return nil, errors.New("stub")
}

func (stubBackend) Models() []string { return []string{"m1", "m2"} }

func decodeError(t *testing.T, w *httptest.ResponseRecorder) dto.ErrorDetails {
	t.Helper()
	var resp dto.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	return resp.Error
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		ctx:     t.Context(),
		runners: map[string]*task.Runner{},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
	}
}

func TestHandleTaskEvents(t *testing.T) {
	t.Run("NotFound", func(t *testing.T) {
		s := newTestServer(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/99/raw_events", http.NoBody)
		req.SetPathValue("id", "99")
		w := httptest.NewRecorder()
		s.handleTaskRawEvents(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		e := decodeError(t, w)
		if e.Code != dto.CodeNotFound {
			t.Errorf("code = %q, want %q", e.Code, dto.CodeNotFound)
		}
	})

	t.Run("NonexistentID", func(t *testing.T) {
		s := newTestServer(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/abc/raw_events", http.NoBody)
		req.SetPathValue("id", "abc")
		w := httptest.NewRecorder()
		s.handleTaskRawEvents(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		e := decodeError(t, w)
		if e.Code != dto.CodeNotFound {
			t.Errorf("code = %q, want %q", e.Code, dto.CodeNotFound)
		}
	})
}

func TestHandleTaskInput(t *testing.T) {
	t.Run("NotRunning", func(t *testing.T) {
		s := newTestServer(t)
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
	})

	t.Run("EmptyPrompt", func(t *testing.T) {
		s := newTestServer(t)
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
	})
}

func TestHandleRestart(t *testing.T) {
	t.Run("NotWaiting", func(t *testing.T) {
		s := newTestServer(t)
		s.tasks["t1"] = &taskEntry{
			task: &task.Task{Prompt: "test", State: task.StateRunning},
			done: make(chan struct{}),
		}

		body := strings.NewReader(`{"prompt":"new plan"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/restart", body)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(s, s.restartTask)(w, req)
		if w.Code != http.StatusConflict {
			t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
		}
		e := decodeError(t, w)
		if e.Code != dto.CodeConflict {
			t.Errorf("code = %q, want %q", e.Code, dto.CodeConflict)
		}
	})

	t.Run("EmptyPrompt", func(t *testing.T) {
		s := newTestServer(t)
		s.tasks["t1"] = &taskEntry{
			task: &task.Task{Prompt: "test", State: task.StateWaiting},
			done: make(chan struct{}),
		}

		body := strings.NewReader(`{"prompt":""}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/restart", body)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(s, s.restartTask)(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		e := decodeError(t, w)
		if e.Code != dto.CodeBadRequest {
			t.Errorf("code = %q, want %q", e.Code, dto.CodeBadRequest)
		}
	})
}

func TestHandleTerminate(t *testing.T) {
	t.Run("NotWaiting", func(t *testing.T) {
		s := newTestServer(t)
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
	})

	t.Run("Waiting", func(t *testing.T) {
		tk := &task.Task{Prompt: "test", State: task.StateWaiting}
		tk.InitDoneCh()
		s := newTestServer(t)
		s.tasks["t1"] = &taskEntry{
			task: tk,
			done: make(chan struct{}),
		}

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
	})

	t.Run("CancelledContext", func(t *testing.T) {
		tk := &task.Task{Prompt: "test", State: task.StateRunning}
		tk.InitDoneCh()
		s := newTestServer(t)
		s.tasks["t1"] = &taskEntry{
			task: tk,
			done: make(chan struct{}),
		}

		// Use an already-cancelled context to simulate shutdown scenario
		// where BaseContext is cancelled before the handler completes.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/terminate", http.NoBody)
		req = req.WithContext(ctx)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(s, s.terminateTask)(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})
}

func TestHandleCreateTask(t *testing.T) {
	t.Run("ReturnsID", func(t *testing.T) {
		s := &Server{
			ctx: t.Context(),
			runners: map[string]*task.Runner{
				"myrepo": {
					BaseBranch: "main",
					Dir:        t.TempDir(),
					Backends:   map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}},
				},
			},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"prompt":"test task","repo":"myrepo","harness":"claude"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp dto.CreateTaskResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.ID == 0 {
			t.Error("response has zero 'id' field")
		}
	})

	t.Run("MissingRepo", func(t *testing.T) {
		s := newTestServer(t)
		handler := handle(s.createTask)

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
	})

	t.Run("UnknownRepo", func(t *testing.T) {
		s := newTestServer(t)
		handler := handle(s.createTask)

		body := strings.NewReader(`{"prompt":"test","repo":"nonexistent","harness":"claude"}`)
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
	})

	t.Run("UnknownHarness", func(t *testing.T) {
		s := &Server{
			ctx: t.Context(),
			runners: map[string]*task.Runner{
				"myrepo": {BaseBranch: "main", Dir: t.TempDir()},
			},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"prompt":"test","repo":"myrepo","harness":"nonexistent"}`)
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
		if !strings.Contains(e.Message, "nonexistent") {
			t.Errorf("message = %q, want it to mention the unknown harness", e.Message)
		}
	})

	t.Run("InvalidModel", func(t *testing.T) {
		s := &Server{
			ctx: t.Context(),
			runners: map[string]*task.Runner{
				"myrepo": {
					BaseBranch: "main",
					Dir:        t.TempDir(),
					Backends:   map[agent.Harness]agent.Backend{"stub": stubBackend{}},
				},
			},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"prompt":"test","repo":"myrepo","harness":"stub","model":"nonexistent"}`)
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
		if !strings.Contains(e.Message, "nonexistent") {
			t.Errorf("message = %q, want it to mention the invalid model", e.Message)
		}
	})

	t.Run("ValidModel", func(t *testing.T) {
		s := &Server{
			ctx: t.Context(),
			runners: map[string]*task.Runner{
				"myrepo": {
					BaseBranch: "main",
					Dir:        t.TempDir(),
					Backends:   map[agent.Harness]agent.Backend{"stub": stubBackend{}},
				},
			},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"prompt":"test","repo":"myrepo","harness":"stub","model":"m1"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp dto.CreateTaskResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.ID == 0 {
			t.Error("response has zero 'id' field")
		}
	})

	t.Run("WithImage", func(t *testing.T) {
		s := &Server{
			ctx: t.Context(),
			runners: map[string]*task.Runner{
				"myrepo": {
					BaseBranch: "main",
					Dir:        t.TempDir(),
					Backends:   map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}},
				},
			},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"prompt":"test","repo":"myrepo","harness":"claude","image":"ghcr.io/my/image:v1"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp dto.CreateTaskResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.ID == 0 {
			t.Error("response has zero 'id' field")
		}

		// Verify the task has the image set.
		s.mu.Lock()
		entry := s.tasks[resp.ID.String()]
		s.mu.Unlock()
		if entry == nil {
			t.Fatal("task not found")
		}
		if entry.task.Image != "ghcr.io/my/image:v1" {
			t.Errorf("Image = %q, want %q", entry.task.Image, "ghcr.io/my/image:v1")
		}
	})

	t.Run("UnknownField", func(t *testing.T) {
		s := newTestServer(t)
		handler := handle(s.createTask)

		body := strings.NewReader(`{"prompt":"test","repo":"r","harness":"claude","bogus":true}`)
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
	})
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

func TestLoadTerminatedTasks(t *testing.T) {
	t.Run("OnStartup", func(t *testing.T) {
		logDir := t.TempDir()

		// Write 3 terminal task logs.
		for i, state := range []string{"terminated", "failed", "terminated"} {
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 1, Prompt: fmt.Sprintf("task %d", i), Repo: "r",
				Branch: "caic/w" + strings.Repeat("0", i+1), Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
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
	})

	t.Run("CostInJSON", func(t *testing.T) {
		logDir := t.TempDir()

		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix bug",
			Repo: "r", Branch: "caic/w0", Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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
			if j.AgentVersion != "2.0" {
				t.Errorf("AgentVersion = %q, want %q", j.AgentVersion, "2.0")
			}
		}
	})

	t.Run("BackfillsCostFromMessages", func(t *testing.T) {
		logDir := t.TempDir()

		// Trailer has zero cost (e.g. session exited without final ResultMessage),
		// but the messages contain a ResultMessage with cost.
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix bug",
			Repo: "r", Branch: "caic/w0", Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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
	})

	t.Run("EmptyDir", func(t *testing.T) {
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
	})
}

func TestTerminatedTaskEventsAfterRestart(t *testing.T) {
	logDir := t.TempDir()

	// Write a terminated task log with real agent messages.
	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
		Repo: "r", Branch: "caic/w0", Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+taskID+"/raw_events", http.NoBody).WithContext(ctx)
	req.SetPathValue("id", taskID)
	w := httptest.NewRecorder()
	start := time.Now()
	s.handleTaskRawEvents(w, req)
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("handleTaskRawEvents blocked for %v; terminated tasks should return immediately after history replay", elapsed)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	// Parse SSE events from the response body. Only collect "message"
	// events; skip control events like "ready".
	body := w.Body.String()
	var events []dto.ClaudeEventMessage
	eventType := "message" // SSE default
	for _, line := range strings.Split(body, "\n") {
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			eventType = after
			continue
		}
		after, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			if line == "" {
				eventType = "message" // reset after blank line
			}
			continue
		}
		if eventType != "message" {
			continue
		}
		var ev dto.ClaudeEventMessage
		if err := json.Unmarshal([]byte(after), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatal("no SSE events received for terminated task with messages")
	}

	// Verify we got the expected event kinds.
	kinds := make([]dto.ClaudeEventKind, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	wantKinds := []dto.ClaudeEventKind{dto.ClaudeEventKindInit, dto.ClaudeEventKindText, dto.ClaudeEventKindUsage, dto.ClaudeEventKindResult}
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

func TestStreamEventTextDeltaInSSE(t *testing.T) {
	logDir := t.TempDir()

	// Write a terminated task log with stream events (text deltas) followed
	// by the final assistant message, simulating --include-partial-messages output.
	meta := mustJSON(t, agent.MetaMessage{
		MessageType: "caic_meta", Version: 1, Prompt: "explain streaming",
		Repo: "r", Branch: "caic/w0", Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	initMsg := mustJSON(t, agent.SystemInitMessage{
		MessageType: "system", Subtype: "init", Model: "claude-opus-4-6", Version: "2.0", SessionID: "s1",
	})
	delta1 := mustJSON(t, agent.StreamEvent{
		MessageType: "stream_event",
		Event: agent.StreamEventData{
			Type: "content_block_delta", Index: 0,
			Delta: &agent.StreamDelta{Type: "text_delta", Text: "Hello "},
		},
	})
	delta2 := mustJSON(t, agent.StreamEvent{
		MessageType: "stream_event",
		Event: agent.StreamEventData{
			Type: "content_block_delta", Index: 0,
			Delta: &agent.StreamDelta{Type: "text_delta", Text: "world"},
		},
	})
	// Non-text stream events (message_start, input_json_delta) should be filtered.
	msgStart := mustJSON(t, agent.StreamEvent{
		MessageType: "stream_event",
		Event:       agent.StreamEventData{Type: "message_start"},
	})
	assistant := mustJSON(t, agent.AssistantMessage{
		MessageType: "assistant",
		Message: agent.APIMessage{
			Model:   "claude-opus-4-6",
			Content: []agent.ContentBlock{{Type: "text", Text: "Hello world"}},
			Usage:   agent.Usage{InputTokens: 50, OutputTokens: 20},
		},
	})
	result := mustJSON(t, agent.ResultMessage{
		MessageType: "result", Subtype: "success", Result: "done", TotalCostUSD: 0.02, DurationMs: 200, NumTurns: 1,
	})
	trailer := mustJSON(t, agent.MetaResultMessage{
		MessageType: "caic_result", State: "terminated", CostUSD: 0.02, DurationMs: 200,
	})
	writeLogFile(t, logDir, "task.jsonl", meta, initMsg, msgStart, delta1, delta2, assistant, result, trailer)

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
	if len(s.tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(s.tasks))
	}
	var taskID string
	for id := range s.tasks {
		taskID = id
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+taskID+"/raw_events", http.NoBody).WithContext(ctx)
	req.SetPathValue("id", taskID)
	w := httptest.NewRecorder()
	s.handleTaskRawEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	var events []dto.ClaudeEventMessage
	eventType := "message"
	for _, line := range strings.Split(body, "\n") {
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			eventType = after
			continue
		}
		after, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			if line == "" {
				eventType = "message"
			}
			continue
		}
		if eventType != "message" {
			continue
		}
		var ev dto.ClaudeEventMessage
		if err := json.Unmarshal([]byte(after), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		events = append(events, ev)
	}

	kinds := make([]dto.ClaudeEventKind, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	// Expect: init + 2 textDelta (message_start filtered) + text + usage + result
	wantKinds := []dto.ClaudeEventKind{dto.ClaudeEventKindInit, dto.ClaudeEventKindTextDelta, dto.ClaudeEventKindTextDelta, dto.ClaudeEventKindText, dto.ClaudeEventKindUsage, dto.ClaudeEventKindResult}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("event kinds = %v, want %v", kinds, wantKinds)
	}
	for i := range wantKinds {
		if kinds[i] != wantKinds[i] {
			t.Errorf("kinds[%d] = %q, want %q", i, kinds[i], wantKinds[i])
		}
	}

	// Verify textDelta payloads.
	if events[1].TextDelta == nil || events[1].TextDelta.Text != "Hello " {
		t.Errorf("textDelta[0] = %+v, want text 'Hello '", events[1].TextDelta)
	}
	if events[2].TextDelta == nil || events[2].TextDelta.Text != "world" {
		t.Errorf("textDelta[1] = %+v, want text 'world'", events[2].TextDelta)
	}
	// Final text event should have the complete text.
	if events[3].Text == nil || events[3].Text.Text != "Hello world" {
		t.Errorf("text event = %+v, want text 'Hello world'", events[3].Text)
	}
}
