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

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// stubBackend implements agent.Backend for test map-membership checks.
type stubBackend struct{}

func (stubBackend) Harness() agent.Harness { return "stub" }

func (stubBackend) Start(context.Context, *agent.Options, chan<- agent.Message, io.Writer) (*agent.Session, error) {
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

func (stubBackend) SupportsImages() bool { return false }

func (stubBackend) ContextWindowLimit(string) int { return 180_000 }

func decodeError(t *testing.T, w *httptest.ResponseRecorder) dto.ErrorDetails {
	t.Helper()
	var resp dto.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	return resp.Error
}

func testPrefs(t *testing.T) *preferences.Store {
	s, err := preferences.Open(filepath.Join(t.TempDir(), "preferences.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		ctx:     t.Context(),
		runners: map[string]*task.Runner{},
		tasks:   make(map[string]*taskEntry),
		changed: make(chan struct{}),
		prefs:   testPrefs(t),
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
			task: &task.Task{InitialPrompt: agent.Prompt{Text: "test"}},
			done: make(chan struct{}),
		}

		body := strings.NewReader(`{"prompt":{"text":"hello"}}`)
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
			task: &task.Task{InitialPrompt: agent.Prompt{Text: "test"}},
			done: make(chan struct{}),
		}

		body := strings.NewReader(`{"prompt":{"text":""}}`)
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
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(task.StateRunning)
		s.tasks["t1"] = &taskEntry{
			task: tk,
			done: make(chan struct{}),
		}

		body := strings.NewReader(`{"prompt":{"text":"new plan"}}`)
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
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.SetState(task.StateWaiting)
		s.tasks["t1"] = &taskEntry{
			task: tk,
			done: make(chan struct{}),
		}

		body := strings.NewReader(`{"prompt":{"text":""}}`)
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
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		// StatePending is the zero value, but set explicitly for clarity.
		s.tasks["t1"] = &taskEntry{
			task: tk,
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
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repo: "r"}
		tk.SetState(task.StateWaiting)
		s := newTestServer(t)
		s.runners["r"] = &task.Runner{BaseBranch: "main", Dir: t.TempDir()}
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

		// Verify the response reports terminating. Don't check tk.State
		// directly: cleanupTask runs in a goroutine and may have already
		// transitioned the state to StateTerminated by now.
		var resp v1.StatusResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Status != "terminating" {
			t.Errorf("status = %q, want %q", resp.Status, "terminating")
		}
	})

	t.Run("CancelledContext", func(t *testing.T) {
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repo: "r"}
		tk.SetState(task.StateRunning)
		s := newTestServer(t)
		s.runners["r"] = &task.Runner{BaseBranch: "main", Dir: t.TempDir()}
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

func TestHandleContainerDeath(t *testing.T) {
	t.Run("TriggersCleanup", func(t *testing.T) {
		s := newTestServer(t)
		tk := &task.Task{
			InitialPrompt: agent.Prompt{Text: "test"},
			Repo:          "r",
			Container:     "md-repo-caic-0",
		}
		tk.SetState(task.StateRunning)
		s.runners["r"] = &task.Runner{BaseBranch: "main", Dir: t.TempDir()}
		entry := &taskEntry{task: tk, done: make(chan struct{})}
		s.tasks["t1"] = entry

		s.handleContainerDeath("md-repo-caic-0")

		// Wait for the async cleanup goroutine to complete.
		select {
		case <-entry.done:
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup did not complete in time")
		}

		if tk.GetState() != task.StateFailed {
			t.Errorf("state = %v, want %v", tk.GetState(), task.StateFailed)
		}

		s.mu.Lock()
		result := entry.result
		s.mu.Unlock()
		if result == nil {
			t.Fatal("result is nil after container death cleanup")
		}
	})

	t.Run("UnknownContainer", func(t *testing.T) {
		s := newTestServer(t)
		// Should not panic or cause errors.
		s.handleContainerDeath("unknown-container")
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
			prefs:   testPrefs(t),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test task"},"repo":"myrepo","harness":"claude"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp v1.CreateTaskResp
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

		body := strings.NewReader(`{"initialPrompt":{"text":"test task"}}`)
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

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repo":"nonexistent","harness":"claude"}`)
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

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repo":"myrepo","harness":"nonexistent"}`)
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

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repo":"myrepo","harness":"stub","model":"nonexistent"}`)
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
			prefs:   testPrefs(t),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repo":"myrepo","harness":"stub","model":"m1"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp v1.CreateTaskResp
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
			prefs:   testPrefs(t),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repo":"myrepo","harness":"claude","image":"ghcr.io/my/image:v1"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp v1.CreateTaskResp
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
		if entry.task.DockerImage != "ghcr.io/my/image:v1" {
			t.Errorf("Image = %q, want %q", entry.task.DockerImage, "ghcr.io/my/image:v1")
		}
	})

	t.Run("UnknownField", func(t *testing.T) {
		s := newTestServer(t)
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repo":"r","harness":"claude","bogus":true}`)
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

	req := httptest.NewRequest(http.MethodGet, "/api/v1/server/repos", http.NoBody)
	w := httptest.NewRecorder()
	handle(s.listRepos)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var repos []v1.Repo
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
				Branch: "caic-" + strings.Repeat("0", i+1), Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
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
			prompts = append(prompts, e.task.InitialPrompt.Text)
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
			Repo: "r", Branch: "caic-0", Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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
			CostUSD: 1.23, Duration: 5, NumTurns: 3,
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
			if j.Duration != 5 {
				t.Errorf("Duration = %f, want 5", j.Duration)
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
			Repo: "r", Branch: "caic-0", Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		initMsg := mustJSON(t, agent.SystemInitMessage{
			MessageType: "system", Subtype: "init", Model: "claude-opus-4-6", Version: "2.0", SessionID: "s1",
		})
		result := mustJSON(t, agent.ResultMessage{
			MessageType: "result", Subtype: "success", Result: "done",
			TotalCostUSD: 0.42, DurationMs: 3000, NumTurns: 2, // DurationMs is agent wire format (int64 ms).
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
			if j.Duration != 3 {
				t.Errorf("Duration = %f, want 3", j.Duration)
			}
		}
	})

	t.Run("SameBranchDifferentRepos", func(t *testing.T) {
		logDir := t.TempDir()

		// Two logs from different repos share the same branch name.
		// Each must retain its own title and prompt.
		metaA := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1,
			Prompt: "optimize genai provider", Repo: "genai", Branch: "caic-0",
			Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Title: "Skip Unnecessary MD Container Build",
		})
		trailerA := mustJSON(t, agent.MetaResultMessage{
			MessageType: "caic_result", State: "terminated",
			Title: "Optimize GenAI Provider",
		})
		writeLogFile(t, logDir, "a.jsonl", metaA, trailerA)

		metaB := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1,
			Prompt: "skip docker rebuilds", Repo: "md", Branch: "caic-0",
			Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC),
			Title: "Skip Docker Rebuilds",
		})
		trailerB := mustJSON(t, agent.MetaResultMessage{
			MessageType: "caic_result", State: "terminated",
			Title: "Skip Unnecessary Docker Image Rebuilds",
		})
		writeLogFile(t, logDir, "b.jsonl", metaB, trailerB)

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
		if len(s.tasks) != 2 {
			t.Fatalf("len(tasks) = %d, want 2", len(s.tasks))
		}

		// Verify each task has the title matching its own repo.
		for _, e := range s.tasks {
			switch e.task.Repo {
			case "genai":
				if got := e.task.Title(); got != "Optimize GenAI Provider" {
					t.Errorf("genai title = %q, want %q", got, "Optimize GenAI Provider")
				}
			case "md":
				if got := e.task.Title(); got != "Skip Unnecessary Docker Image Rebuilds" {
					t.Errorf("md title = %q, want %q", got, "Skip Unnecessary Docker Image Rebuilds")
				}
			default:
				t.Errorf("unexpected repo %q", e.task.Repo)
			}
		}

		// Verify that branchID scoped by repo does not lose either entry.
		// This mirrors the branchID construction in adoptContainers.
		branchID := make(map[string]string, len(s.tasks))
		for id, e := range s.tasks {
			if e.task.Branch != "" {
				key := e.task.Repo + "\x00" + e.task.Branch
				branchID[key] = id
			}
		}
		if len(branchID) != 2 {
			t.Errorf("branchID has %d entries, want 2 (repo-scoped keys must not collide)", len(branchID))
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

// parseSSEEvents extracts message-type SSE events from a response body.
func parseSSEEvents(t *testing.T, body string) []v1.ClaudeEventMessage {
	var events []v1.ClaudeEventMessage
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
		var ev v1.ClaudeEventMessage
		if err := json.Unmarshal([]byte(after), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

func TestHandleTaskRawEvents(t *testing.T) {
	t.Run("TerminatedTaskEvents", func(t *testing.T) {
		logDir := t.TempDir()

		// Write a terminated task log with real agent messages.
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
			Repo: "r", Branch: "caic-0", Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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
			MessageType: "caic_result", State: "terminated", CostUSD: 0.05, Duration: 1,
		})
		writeLogFile(t, logDir, "task.jsonl", meta, initMsg, assistant, result, trailer)

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
		events := parseSSEEvents(t, w.Body.String())
		if len(events) == 0 {
			t.Fatal("no SSE events received for terminated task with messages")
		}

		kinds := make([]v1.ClaudeEventKind, len(events))
		for i, ev := range events {
			kinds[i] = ev.Kind
		}
		wantKinds := []v1.ClaudeEventKind{v1.ClaudeEventKindInit, v1.ClaudeEventKindText, v1.ClaudeEventKindUsage, v1.ClaudeEventKindResult}
		if len(kinds) != len(wantKinds) {
			t.Fatalf("event kinds = %v, want %v", kinds, wantKinds)
		}
		for i := range wantKinds {
			if kinds[i] != wantKinds[i] {
				t.Errorf("kinds[%d] = %q, want %q", i, kinds[i], wantKinds[i])
			}
		}
		if events[1].Text == nil || events[1].Text.Text != "I found the bug" {
			t.Errorf("text event = %+v, want text 'I found the bug'", events[1].Text)
		}
	})

	t.Run("StreamEventTextDelta", func(t *testing.T) {
		logDir := t.TempDir()

		// Write a terminated task log with stream events (text deltas) followed
		// by the final assistant message, simulating --include-partial-messages output.
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "explain streaming",
			Repo: "r", Branch: "caic-0", Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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
			MessageType: "caic_result", State: "terminated", CostUSD: 0.02, Duration: 0.2,
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

		events := parseSSEEvents(t, w.Body.String())
		kinds := make([]v1.ClaudeEventKind, len(events))
		for i, ev := range events {
			kinds[i] = ev.Kind
		}
		// Expect: init + 2 textDelta (message_start filtered) + text + usage + result
		wantKinds := []v1.ClaudeEventKind{v1.ClaudeEventKindInit, v1.ClaudeEventKindTextDelta, v1.ClaudeEventKindTextDelta, v1.ClaudeEventKindText, v1.ClaudeEventKindUsage, v1.ClaudeEventKindResult}
		if len(kinds) != len(wantKinds) {
			t.Fatalf("event kinds = %v, want %v", kinds, wantKinds)
		}
		for i := range wantKinds {
			if kinds[i] != wantKinds[i] {
				t.Errorf("kinds[%d] = %q, want %q", i, kinds[i], wantKinds[i])
			}
		}

		if events[1].TextDelta == nil || events[1].TextDelta.Text != "Hello " {
			t.Errorf("textDelta[0] = %+v, want text 'Hello '", events[1].TextDelta)
		}
		if events[2].TextDelta == nil || events[2].TextDelta.Text != "world" {
			t.Errorf("textDelta[1] = %+v, want text 'world'", events[2].TextDelta)
		}
		if events[3].Text == nil || events[3].Text.Text != "Hello world" {
			t.Errorf("text event = %+v, want text 'Hello world'", events[3].Text)
		}
	})
}
