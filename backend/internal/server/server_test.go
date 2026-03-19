package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// stubBackend implements agent.Backend for test map-membership checks.
type stubBackend struct{}

func (stubBackend) Harness() agent.Harness { return "stub" }

func (stubBackend) Start(context.Context, *agent.Options, chan<- agent.Message, io.Writer) (*agent.Session, error) {
	return nil, errors.New("stub")
}

func (stubBackend) AttachRelay(context.Context, *agent.Options, chan<- agent.Message, io.Writer) (*agent.Session, error) {
	return nil, errors.New("stub")
}

func (stubBackend) ReadRelayOutput(context.Context, string) ([]agent.Message, int64, error) {
	return nil, 0, errors.New("stub")
}

func (stubBackend) ParseMessage([]byte) ([]agent.Message, error) {
	return nil, errors.New("stub")
}

func (stubBackend) Models() []string { return []string{"m1", "m2"} }

func (stubBackend) SupportsImages() bool { return false }

func (stubBackend) ContextWindowLimit(string) int { return 180_000 }

func decodeError(t *testing.T, w *httptest.ResponseRecorder) dto.ErrorDetails {
	var resp dto.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	return resp.Error
}

func newTestPrefs(t *testing.T) *preferences.Store {
	path := filepath.Join(t.TempDir(), "preferences.json")
	store, err := preferences.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func newTestServer(t *testing.T) *Server {
	checker, err := ipgeo.NewChecker(t.Context(), "0.0.0.0/0,::/0", "")
	if err != nil {
		t.Fatalf("ipgeo.NewChecker: %v", err)
	}
	return &Server{
		ctx:          t.Context(),
		runners:      map[string]*task.Runner{},
		tasks:        make(map[string]*taskEntry),
		changed:      make(chan struct{}),
		prefs:        newTestPrefs(t),
		ipgeoChecker: checker,
		forge:        newForgeManager("", "", nil),
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

func TestHandlePurge(t *testing.T) {
	t.Run("NotWaiting", func(t *testing.T) {
		s := newTestServer(t)
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		// StatePending is the zero value, but set explicitly for clarity.
		s.tasks["t1"] = &taskEntry{
			task: tk,
			done: make(chan struct{}),
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/purge", http.NoBody)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(s, s.purgeTask)(w, req)
		if w.Code != http.StatusConflict {
			t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
		}
		e := decodeError(t, w)
		if e.Code != dto.CodeConflict {
			t.Errorf("code = %q, want %q", e.Code, dto.CodeConflict)
		}
	})

	t.Run("Waiting", func(t *testing.T) {
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []task.RepoMount{{Name: "r"}}}
		tk.SetState(task.StateWaiting)
		s := newTestServer(t)
		s.runners["r"] = &task.Runner{BaseBranch: "main", Dir: t.TempDir()}
		s.tasks["t1"] = &taskEntry{
			task: tk,
			done: make(chan struct{}),
		}

		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/purge", http.NoBody)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(s, s.purgeTask)(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		// Verify the response reports purging. Don't check tk.State
		// directly: cleanupTask runs in a goroutine and may have already
		// transitioned the state to StatePurged by now.
		var resp v1.StatusResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Status != "purging" {
			t.Errorf("status = %q, want %q", resp.Status, "purging")
		}
	})

	t.Run("CancelledContext", func(t *testing.T) {
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []task.RepoMount{{Name: "r"}}}
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
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/purge", http.NoBody)
		req = req.WithContext(ctx)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(s, s.purgeTask)(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})
}

func TestHandleContainerDeath(t *testing.T) {
	t.Run("ArchivesAsStopped", func(t *testing.T) {
		s := newTestServer(t)
		tk := &task.Task{
			InitialPrompt: agent.Prompt{Text: "test"},
			Repos:         []task.RepoMount{{Name: "r"}},
			Container:     "md-repo-caic-0",
		}
		tk.SetState(task.StateRunning)
		s.runners["r"] = &task.Runner{BaseBranch: "main", Dir: t.TempDir()}
		entry := &taskEntry{task: tk, done: make(chan struct{})}
		s.tasks["t1"] = entry

		s.handleContainerDeath("md-repo-caic-0")

		if tk.GetState() != task.StateStopped {
			t.Errorf("state = %v, want %v", tk.GetState(), task.StateStopped)
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
			prefs:   newTestPrefs(t),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test task"},"repos":[{"name":"myrepo"}],"harness":"claude"}`)
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

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"nonexistent"}],"harness":"claude"}`)
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

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"nonexistent"}`)
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

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"stub","model":"nonexistent"}`)
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
			prefs:   newTestPrefs(t),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"stub","model":"m1"}`)
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
			prefs:   newTestPrefs(t),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"claude","image":"ghcr.io/my/image:v1"}`)
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

	t.Run("NoRepoTask", func(t *testing.T) {
		// Regression: creating a task with no repos panicked with
		// "makeslice: cap out of range" because len(req.Repos)-1 == -1.
		s := &Server{
			ctx: t.Context(),
			runners: map[string]*task.Runner{
				"": {
					Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}},
				},
			},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			prefs:   newTestPrefs(t),
		}
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"no repo task"},"harness":"claude"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp v1.CreateTaskResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.ID == 0 {
			t.Error("response has zero 'id' field")
		}
	})

	t.Run("NoRepoRunnerMissing", func(t *testing.T) {
		// When no repos are specified and no "" runner is configured, return
		// a clear error instead of panicking.
		s := newTestServer(t) // no "" runner
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"no repo task"},"harness":"claude"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
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
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoadPurgedTasks(t *testing.T) {
	t.Run("OnStartup", func(t *testing.T) {
		logDir := t.TempDir()

		// Write 3 terminal task logs.
		for i, state := range []string{"purged", "failed", "purged"} {
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 1, Prompt: fmt.Sprintf("task %d", i), Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-" + strings.Repeat("0", i+1)}}, Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
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
		if err := s.loadPurgedTasks(); err != nil {
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

	t.Run("TimeLimit", func(t *testing.T) {
		logDir := t.TempDir()

		// task 0: recent (purged)
		meta0 := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "recent task", Harness: agent.Claude, StartedAt: time.Now().Add(-1 * time.Hour),
		})
		trailer0 := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, logDir, "recent.jsonl", meta0, trailer0)

		// task 1: old (purged, > 14 days)
		meta1 := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "old task", Harness: agent.Claude, StartedAt: time.Now().Add(-20 * 24 * time.Hour),
		})
		trailer1 := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		oldPath := filepath.Join(logDir, "old.jsonl")
		writeLogFile(t, logDir, "old.jsonl", meta1, trailer1)
		// Set mtime to 15 days ago.
		oldTime := time.Now().Add(-15 * 24 * time.Hour)
		if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
			t.Fatal(err)
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.tasks) != 1 {
			t.Fatalf("len(tasks) = %d, want 1 (old task should be filtered out)", len(s.tasks))
		}
		for _, e := range s.tasks {
			if e.task.InitialPrompt.Text != "recent task" {
				t.Errorf("prompt = %q, want \"recent task\"", e.task.InitialPrompt.Text)
			}
		}
	})

	t.Run("PerRepoLimit", func(t *testing.T) {
		logDir := t.TempDir()

		// Write 7 tasks for repo "a" and 3 for repo "b".
		for i := range 7 {
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 1, Prompt: fmt.Sprintf("a-%d", i),
				Repos: []agent.MetaRepo{{Name: "a", Branch: fmt.Sprintf("caic-%d", i)}}, Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
			})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeLogFile(t, logDir, fmt.Sprintf("a-%d.jsonl", i), meta, trailer)
		}
		for i := range 3 {
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 1, Prompt: fmt.Sprintf("b-%d", i),
				Repos: []agent.MetaRepo{{Name: "b", Branch: fmt.Sprintf("caic-%d", i)}}, Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, i+10, 0, 0, 0, time.UTC),
			})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeLogFile(t, logDir, fmt.Sprintf("b-%d.jsonl", i), meta, trailer)
		}

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
			t.Fatal(err)
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		// Count tasks per repo.
		counts := map[string]int{}
		for _, e := range s.tasks {
			repo := ""
			if p := e.task.Primary(); p != nil {
				repo = p.Name
			}
			counts[repo]++
		}
		if counts["a"] != 5 {
			t.Errorf("repo a: got %d tasks, want 5", counts["a"])
		}
		if counts["b"] != 3 {
			t.Errorf("repo b: got %d tasks, want 3", counts["b"])
		}
		if len(s.tasks) != 8 {
			t.Errorf("total tasks = %d, want 8 (5 from a + 3 from b)", len(s.tasks))
		}
	})

	t.Run("CostInJSON", func(t *testing.T) {
		logDir := t.TempDir()

		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix bug",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		initMsg := mustJSON(t, map[string]any{
			"type": "system", "subtype": "init", "model": "claude-opus-4-6",
			"claude_code_version": "2.0", "session_id": "s1",
		})
		result := mustJSON(t, agent.ResultMessage{
			MessageType: "result", Subtype: "success", Result: "done",
			TotalCostUSD: 1.23, Usage: agent.Usage{OutputTokens: 16400}, DurationMs: 5000, NumTurns: 3,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{
			MessageType: "caic_result", State: "purged",
			CostUSD: 1.23, Duration: 5, NumTurns: 3,
		})
		writeLogFile(t, logDir, "task.jsonl", meta, initMsg, result, trailer)

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
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
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		initMsg := mustJSON(t, map[string]any{
			"type": "system", "subtype": "init", "model": "claude-opus-4-6",
			"claude_code_version": "2.0", "session_id": "s1",
		})
		result := mustJSON(t, agent.ResultMessage{
			MessageType: "result", Subtype: "success", Result: "done",
			TotalCostUSD: 0.42, Usage: agent.Usage{OutputTokens: 5600}, DurationMs: 3000, NumTurns: 2,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{
			MessageType: "caic_result", State: "purged",
			// CostUSD intentionally zero.
		})
		writeLogFile(t, logDir, "task.jsonl", meta, initMsg, result, trailer)

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
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
			Prompt: "optimize genai provider", Repos: []agent.MetaRepo{{Name: "genai", Branch: "caic-0"}},
			Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Title: "Skip Unnecessary MD Container Build",
		})
		trailerA := mustJSON(t, agent.MetaResultMessage{
			MessageType: "caic_result", State: "purged",
			Title: "Optimize GenAI Provider",
		})
		writeLogFile(t, logDir, "a.jsonl", metaA, trailerA)

		metaB := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1,
			Prompt: "skip docker rebuilds", Repos: []agent.MetaRepo{{Name: "md", Branch: "caic-0"}},
			Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC),
			Title: "Skip Docker Rebuilds",
		})
		trailerB := mustJSON(t, agent.MetaResultMessage{
			MessageType: "caic_result", State: "purged",
			Title: "Skip Unnecessary Docker Image Rebuilds",
		})
		writeLogFile(t, logDir, "b.jsonl", metaB, trailerB)

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
			t.Fatal(err)
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.tasks) != 2 {
			t.Fatalf("len(tasks) = %d, want 2", len(s.tasks))
		}

		// Verify each task has the title matching its own repo.
		for _, e := range s.tasks {
			var repoName string
			if p := e.task.Primary(); p != nil {
				repoName = p.Name
			}
			switch repoName {
			case "genai":
				if got := e.task.Title(); got != "Optimize GenAI Provider" {
					t.Errorf("genai title = %q, want %q", got, "Optimize GenAI Provider")
				}
			case "md":
				if got := e.task.Title(); got != "Skip Unnecessary Docker Image Rebuilds" {
					t.Errorf("md title = %q, want %q", got, "Skip Unnecessary Docker Image Rebuilds")
				}
			default:
				t.Errorf("unexpected repo %q", repoName)
			}
		}

		// Verify that branchIDs scoped by repo does not lose either entry.
		// This mirrors the branchIDs construction in adoptContainers.
		branchIDs := make(map[string][]string, len(s.tasks))
		for id, e := range s.tasks {
			if p := e.task.Primary(); p != nil && p.Branch != "" {
				key := p.Name + "\x00" + p.Branch
				branchIDs[key] = append(branchIDs[key], id)
			}
		}
		if len(branchIDs) != 2 {
			t.Errorf("branchIDs has %d keys, want 2 (repo-scoped keys must not collide)", len(branchIDs))
		}
	})

	t.Run("EmptyDir", func(t *testing.T) {
		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  t.TempDir(),
		}
		if err := s.loadPurgedTasks(); err != nil {
			t.Fatal(err)
		}
		if len(s.tasks) != 0 {
			t.Errorf("len(tasks) = %d, want 0", len(s.tasks))
		}
	})

	t.Run("PROutsideTailWindow", func(t *testing.T) {
		// caic_pr early in the file with >64 KiB of messages after it.
		// The header-only tail scan cannot see caic_pr; loadPurgedTasks
		// must still restore it on the Task via LoadMessages.
		logDir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "big pr task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: agent.Claude,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		prMsg := mustJSON(t, agent.MetaPRMessage{
			MessageType: "caic_pr", ForgeOwner: "acme", ForgeRepo: "widget", ForgePR: 77,
		})

		// Build filler lines (assistant messages) totalling >64 KiB.
		bigText := strings.Repeat("x", 1024)
		filler := mustJSON(t, map[string]any{
			"type":    "assistant",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": bigText}}},
		})
		lines := make([]string, 0, 83) // meta + prMsg + 80 filler + trailer
		lines = append(lines, meta, prMsg)
		for range 80 { // 80 KiB of filler
			lines = append(lines, filler)
		}
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		lines = append(lines, trailer)
		writeLogFile(t, logDir, "task.jsonl", lines...)

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
			t.Fatal(err)
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.tasks) != 1 {
			t.Fatalf("len(tasks) = %d, want 1", len(s.tasks))
		}
		for _, e := range s.tasks {
			snap := e.task.Snapshot()
			if snap.ForgePR != 77 {
				t.Errorf("ForgePR = %d, want 77", snap.ForgePR)
			}
			if snap.ForgeOwner != "acme" {
				t.Errorf("ForgeOwner = %q, want %q", snap.ForgeOwner, "acme")
			}
			if snap.ForgeRepo != "widget" {
				t.Errorf("ForgeRepo = %q, want %q", snap.ForgeRepo, "widget")
			}
		}
	})

	t.Run("PerRepoLimit", func(t *testing.T) {
		logDir := t.TempDir()

		// Write 6 tasks for repo-a and 6 for repo-b; expect only the 5 most
		// recently active per repo (10 total) to be loaded.
		// Use times relative to now so all tasks pass the 7-day filter.
		// Task i has stoppedAt = now - (11-i)*hour so task 0 is oldest, task 11 newest.
		now := time.Now().UTC()
		for i := range 12 {
			repoName := "repo-a"
			if i >= 6 {
				repoName = "repo-b"
			}
			stoppedAt := now.Add(-time.Duration(11-i) * time.Hour)
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 1,
				Prompt:    fmt.Sprintf("task %d", i),
				Repos:     []agent.MetaRepo{{Name: repoName, Branch: fmt.Sprintf("caic-%d", i)}},
				Harness:   agent.Claude,
				StartedAt: stoppedAt,
			})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			name := fmt.Sprintf("%02d.jsonl", i)
			writeLogFile(t, logDir, name, meta, trailer)
			if err := os.Chtimes(filepath.Join(logDir, name), stoppedAt, stoppedAt); err != nil {
				t.Fatal(err)
			}
		}

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
			t.Fatal(err)
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.tasks) != 10 {
			t.Fatalf("len(tasks) = %d, want 10 (5 per repo)", len(s.tasks))
		}

		// Collect prompts and verify oldest task per repo was dropped.
		prompts := make([]string, 0, len(s.tasks))
		for _, e := range s.tasks {
			prompts = append(prompts, e.task.InitialPrompt.Text)
		}
		sort.Strings(prompts)
		// "task 0" (oldest repo-a) and "task 6" (oldest repo-b) should be excluded.
		for _, dropped := range []string{"task 0", "task 6"} {
			for _, p := range prompts {
				if p == dropped {
					t.Errorf("prompt %q should have been dropped (exceeds per-repo limit)", dropped)
				}
			}
		}
	})

	t.Run("StateInference", func(t *testing.T) {
		// Tasks without a caic_result trailer always load as "failed" —
		// we cannot distinguish purged-without-trailer from interrupted.
		// adoptContainers replaces stale entries with live state when a
		// container is still running.
		logDir := t.TempDir()

		meta := func(prompt string) string {
			return mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 1, Prompt: prompt, Harness: agent.Claude,
				Repos:     []agent.MetaRepo{{Name: "r", Branch: "caic-0"}},
				StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			})
		}
		resultMsg := mustJSON(t, agent.ResultMessage{
			MessageType: "result", Subtype: "success", Result: "done",
		})

		// no trailer + ResultMessage present → "failed" (container may be gone)
		writeLogFile(t, logDir, "waiting.jsonl", meta("waiting task"), resultMsg)

		// no trailer + no ResultMessage → "failed"
		writeLogFile(t, logDir, "interrupted.jsonl", meta("interrupted task"))

		// explicit trailer → "purged"
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, logDir, "purged.jsonl", meta("purged task"), trailer)

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
			t.Fatal(err)
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.tasks) != 3 {
			t.Fatalf("len(tasks) = %d, want 3", len(s.tasks))
		}

		states := make(map[string]string, len(s.tasks))
		for _, e := range s.tasks {
			states[e.task.InitialPrompt.Text] = e.task.Snapshot().State.String()
		}
		if got := states["waiting task"]; got != "failed" {
			t.Errorf("waiting task state = %q, want \"failed\"", got)
		}
		if got := states["interrupted task"]; got != "failed" {
			t.Errorf("interrupted task state = %q, want \"failed\"", got)
		}
		if got := states["purged task"]; got != "purged" {
			t.Errorf("purged task state = %q, want \"purged\"", got)
		}
	})
}

// parseSSEEvents extracts message-type SSE events from a response body.
func parseSSEEvents(t *testing.T, body string) []v1.EventMessage {
	var events []v1.EventMessage
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
		var ev v1.EventMessage
		if err := json.Unmarshal([]byte(after), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

func TestComputeTaskPatch(t *testing.T) {
	t.Run("ChangedFields", func(t *testing.T) {
		old := `{"id":"abc","state":"running","costUSD":0.0}`
		new_ := `{"id":"abc","state":"waiting","costUSD":1.5}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if string(patch["id"]) != `"abc"` {
			t.Errorf("id = %s, want \"abc\"", patch["id"])
		}
		if string(patch["state"]) != `"waiting"` {
			t.Errorf("state = %s, want \"waiting\"", patch["state"])
		}
		if string(patch["costUSD"]) != `1.5` {
			t.Errorf("costUSD = %s, want 1.5", patch["costUSD"])
		}
		// Unchanged field should not be in patch
		if _, ok := patch["costUSD"]; !ok {
			t.Error("costUSD should be in patch (changed from 0.0 to 1.5)")
		}
	})
	t.Run("UnchangedFieldsOmitted", func(t *testing.T) {
		old := `{"id":"abc","state":"running","repo":"myrepo"}`
		new_ := `{"id":"abc","state":"waiting","repo":"myrepo"}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := patch["repo"]; ok {
			t.Error("repo should not be in patch (unchanged)")
		}
		if _, ok := patch["state"]; !ok {
			t.Error("state should be in patch (changed)")
		}
	})
	t.Run("RemovedFieldSetToNull", func(t *testing.T) {
		old := `{"id":"abc","error":"boom"}`
		new_ := `{"id":"abc"}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if string(patch["error"]) != "null" {
			t.Errorf("removed field error = %s, want null", patch["error"])
		}
	})
	t.Run("AlwaysIncludesID", func(t *testing.T) {
		old := `{"id":"xyz","state":"running"}`
		new_ := `{"id":"xyz","state":"purged"}`
		patch, err := computeTaskPatch([]byte(old), []byte(new_))
		if err != nil {
			t.Fatal(err)
		}
		if string(patch["id"]) != `"xyz"` {
			t.Errorf("id = %s, want \"xyz\"", patch["id"])
		}
	})
}

func TestHandleTaskRawEvents(t *testing.T) {
	t.Run("PurgedTaskEvents", func(t *testing.T) {
		logDir := t.TempDir()

		// Write a purged task log with real agent messages.
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		initMsg := mustJSON(t, map[string]any{
			"type": "system", "subtype": "init", "model": "claude-opus-4-6",
			"claude_code_version": "2.0", "session_id": "s1",
		})
		assistant := mustJSON(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"model": "claude-opus-4-6",
				"content": []map[string]any{
					{"type": "text", "text": "I found the bug"},
				},
				"usage": map[string]any{
					"input_tokens": 100, "output_tokens": 50,
				},
			},
		})
		result := mustJSON(t, agent.ResultMessage{
			MessageType: "result", Subtype: "success", Result: "done", TotalCostUSD: 0.05, DurationMs: 1000, NumTurns: 1,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{
			MessageType: "caic_result", State: "purged", CostUSD: 0.05, Duration: 1,
		})
		writeLogFile(t, logDir, "task.jsonl", meta, initMsg, assistant, result, trailer)

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
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
		// purged tasks instead of blocking until context deadline.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/"+taskID+"/raw_events", http.NoBody).WithContext(ctx)
		req.SetPathValue("id", taskID)
		w := httptest.NewRecorder()
		start := time.Now()
		s.handleTaskRawEvents(w, req)
		elapsed := time.Since(start)
		if elapsed > 200*time.Millisecond {
			t.Errorf("handleTaskRawEvents blocked for %v; purged tasks should return immediately after history replay", elapsed)
		}

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		events := parseSSEEvents(t, w.Body.String())
		if len(events) == 0 {
			t.Fatal("no SSE events received for purged task with messages")
		}

		kinds := make([]v1.EventKind, len(events))
		for i, ev := range events {
			kinds[i] = ev.Kind
		}
		wantKinds := []v1.EventKind{v1.EventKindInit, v1.EventKindText, v1.EventKindUsage, v1.EventKindResult}
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

		// Write a purged task log with stream events (text deltas) followed
		// by the final assistant message, simulating --include-partial-messages output.
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "explain streaming",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		initMsg := mustJSON(t, map[string]any{
			"type": "system", "subtype": "init", "model": "claude-opus-4-6",
			"claude_code_version": "2.0", "session_id": "s1",
		})
		delta1 := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}}`
		delta2 := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}}`
		msgStart := `{"type":"stream_event","event":{"type":"message_start"}}`
		assistant := mustJSON(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"model": "claude-opus-4-6",
				"content": []map[string]any{
					{"type": "text", "text": "Hello world"},
				},
				"usage": map[string]any{
					"input_tokens": 50, "output_tokens": 20,
				},
			},
		})
		result := mustJSON(t, agent.ResultMessage{
			MessageType: "result", Subtype: "success", Result: "done", TotalCostUSD: 0.02, DurationMs: 200, NumTurns: 1,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{
			MessageType: "caic_result", State: "purged", CostUSD: 0.02, Duration: 0.2,
		})
		writeLogFile(t, logDir, "task.jsonl", meta, initMsg, msgStart, delta1, delta2, assistant, result, trailer)

		s := &Server{
			runners: map[string]*task.Runner{},
			tasks:   make(map[string]*taskEntry),
			changed: make(chan struct{}),
			logDir:  logDir,
		}
		if err := s.loadPurgedTasks(); err != nil {
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
		kinds := make([]v1.EventKind, len(events))
		for i, ev := range events {
			kinds[i] = ev.Kind
		}
		// Expect: init + text + usage + result. The two textDelta messages are
		// filtered by filterHistoryForReplay because a final TextMessage follows.
		wantKinds := []v1.EventKind{v1.EventKindInit, v1.EventKindText, v1.EventKindUsage, v1.EventKindResult}
		if len(kinds) != len(wantKinds) {
			t.Fatalf("event kinds = %v, want %v", kinds, wantKinds)
		}
		for i := range wantKinds {
			if kinds[i] != wantKinds[i] {
				t.Errorf("kinds[%d] = %q, want %q", i, kinds[i], wantKinds[i])
			}
		}
		if events[1].Text == nil || events[1].Text.Text != "Hello world" {
			t.Errorf("text event = %+v, want text 'Hello world'", events[1].Text)
		}
	})
}

func TestConfigValidate(t *testing.T) {
	t.Run("both empty is valid", func(t *testing.T) {
		if err := (&Config{}).Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("PAT only is valid", func(t *testing.T) {
		c := &Config{GitHubToken: "ghp_abc", GitLabToken: "glpat-abc"}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth with ExternalURL and allowlist is valid", func(t *testing.T) {
		c := &Config{GitHubOAuthClientID: "id", GitHubOAuthClientSecret: "sec", ExternalURL: "https://caic.example.com", GitHubOAuthAllowedUsers: "alice,bob"}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("ExternalURL auto is valid", func(t *testing.T) {
		c := &Config{ExternalURL: "auto"}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth with ExternalURL auto is valid", func(t *testing.T) {
		c := &Config{GitHubOAuthClientID: "id", GitHubOAuthClientSecret: "sec", GitHubOAuthAllowedUsers: "alice", ExternalURL: "auto"}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth without ExternalURL is invalid", func(t *testing.T) {
		c := &Config{GitHubOAuthClientID: "id", GitHubOAuthClientSecret: "sec", GitHubOAuthAllowedUsers: "alice"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub OAuth without allowlist is invalid", func(t *testing.T) {
		c := &Config{GitHubOAuthClientID: "id", GitHubOAuthClientSecret: "sec", ExternalURL: "https://caic.example.com"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLab OAuth without allowlist is invalid", func(t *testing.T) {
		c := &Config{GitLabOAuthClientID: "id", GitLabOAuthClientSecret: "sec", ExternalURL: "https://caic.example.com"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("OAuth with http ExternalURL is invalid", func(t *testing.T) {
		c := &Config{GitHubOAuthClientID: "id", GitHubOAuthClientSecret: "sec", GitHubOAuthAllowedUsers: "alice", ExternalURL: "http://caic.example.com"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("invalid ExternalURL is invalid", func(t *testing.T) {
		c := &Config{ExternalURL: "not a url"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("ExternalURL with subpath is invalid", func(t *testing.T) {
		c := &Config{ExternalURL: "https://caic.example.com/sub"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("ExternalURL with trailing slash is valid", func(t *testing.T) {
		c := &Config{ExternalURL: "https://caic.example.com/"}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("invalid GitLabURL is invalid", func(t *testing.T) {
		c := &Config{GitLabURL: "not a url"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLabURL with subpath is invalid", func(t *testing.T) {
		c := &Config{GitLabURL: "https://gitlab.example.com/sub"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub OAuth ID without secret is invalid", func(t *testing.T) {
		c := &Config{GitHubOAuthClientID: "id"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub OAuth secret without ID is invalid", func(t *testing.T) {
		c := &Config{GitHubOAuthClientSecret: "sec"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLab OAuth ID without secret is invalid", func(t *testing.T) {
		c := &Config{GitLabOAuthClientID: "id"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLab OAuth secret without ID is invalid", func(t *testing.T) {
		c := &Config{GitLabOAuthClientSecret: "sec"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub PAT and OAuth together is invalid", func(t *testing.T) {
		c := &Config{GitHubToken: "ghp_abc", GitHubOAuthClientID: "id", GitHubOAuthClientSecret: "sec", GitHubOAuthAllowedUsers: "alice", ExternalURL: "https://caic.example.com"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLab PAT and OAuth together is invalid", func(t *testing.T) {
		c := &Config{GitLabToken: "glpat-abc", GitLabOAuthClientID: "id", GitLabOAuthClientSecret: "sec", GitLabOAuthAllowedUsers: "alice", ExternalURL: "https://caic.example.com"}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
}

func TestBuildHandler(t *testing.T) {
	t.Run("auth disabled", func(t *testing.T) {
		s := newTestServer(t)
		if _, err := s.buildHandler(); err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
	})

	t.Run("auth enabled", func(t *testing.T) {
		// Regression: adding /api/v1/auth/ (unqualified) alongside GET / (qualified)
		// caused a pattern conflict panic in Go 1.22+ ServeMux.
		s := newTestServer(t)
		secret := make([]byte, 32)
		s.sessionSecret = secret
		usersPath := filepath.Join(t.TempDir(), "users.json")
		store, err := auth.Open(usersPath)
		if err != nil {
			t.Fatalf("open auth store: %v", err)
		}
		s.authStore = store
		if _, err := s.buildHandler(); err != nil {
			t.Fatalf("buildHandler() with auth error = %v", err)
		}
	})

	t.Run("static handler rejects non-GET", func(t *testing.T) {
		s := newTestServer(t)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequest(method, "/", http.NoBody)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s / = %d, want %d", method, w.Code, http.StatusMethodNotAllowed)
			}
		}
	})

	t.Run("host check rejects wrong host", func(t *testing.T) {
		s := newTestServer(t)
		s.allowedHost = "caic.example.com"
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "evil.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("wrong host: status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("host check allows matching host", func(t *testing.T) {
		s := newTestServer(t)
		s.allowedHost = "caic.example.com"
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("matching host should not be forbidden")
		}
	})

	t.Run("host check strips port", func(t *testing.T) {
		s := newTestServer(t)
		s.allowedHost = "caic.example.com"
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com:8080"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("matching host with port should not be forbidden")
		}
	})

	t.Run("host check is case insensitive", func(t *testing.T) {
		s := newTestServer(t)
		s.allowedHost = "caic.example.com"
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "CAIC.Example.COM"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("case-insensitive host should not be forbidden")
		}
	})

	t.Run("no host check when allowedHost empty", func(t *testing.T) {
		s := newTestServer(t)
		// allowedHost is empty by default
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "anything.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("no host check should not reject any host")
		}
	})

	t.Run("auto host check locks first FQDN", func(t *testing.T) {
		s := newTestServer(t)
		s.autoHostLock = &autoHostState{}
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		// First FQDN request locks the host.
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("first FQDN request should not be forbidden")
		}

		// Same host is allowed.
		req = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("same host should not be forbidden")
		}

		// Different FQDN is rejected.
		req = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "evil.example.com"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("different FQDN: status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("auto host check allows IP before and after lock", func(t *testing.T) {
		s := newTestServer(t)
		s.autoHostLock = &autoHostState{}
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		// IP request before any FQDN — allowed.
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "192.168.1.1:8080"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("IP request before lock should not be forbidden")
		}

		// Lock a FQDN.
		req = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)

		// IP request after lock — still allowed.
		req = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "192.168.1.1:8080"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("IP request after lock should not be forbidden")
		}
	})

	t.Run("auto host check allows localhost", func(t *testing.T) {
		s := newTestServer(t)
		s.autoHostLock = &autoHostState{}
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("localhost should not be forbidden")
		}
	})

	t.Run("auto host check is case insensitive", func(t *testing.T) {
		s := newTestServer(t)
		s.autoHostLock = &autoHostState{}
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "CAIC.Example.COM"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("first FQDN should not be forbidden")
		}

		req = httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("case-insensitive match should not be forbidden")
		}
	})

	for _, tc := range []struct {
		name string
		host string
		xfp  string // X-Forwarded-Proto
		want string
	}{
		{"non-default HTTP port", "caic.example.com:8080", "", "http://caic.example.com:8080"},
		{"default HTTP port omitted", "caic.example.com:80", "", "http://caic.example.com"},
		{"default HTTPS port omitted", "caic.example.com:443", "https", "https://caic.example.com"},
		{"non-default HTTPS port", "quick.giraffe-cobra.ts.net:8443", "https", "https://quick.giraffe-cobra.ts.net:8443"},
		{"HTTPS without port", "caic.example.com", "https", "https://caic.example.com"},
		{"HTTP without port", "caic.example.com", "", "http://caic.example.com"},
	} {
		t.Run("auto host lock "+tc.name, func(t *testing.T) {
			state := &autoHostState{}
			r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			r.Host = tc.host
			if tc.xfp != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xfp)
			}
			state.lock(r)
			if got := state.ExternalURL(); got != tc.want {
				t.Errorf("ExternalURL = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("auto host oauth config resolves redirect URI", func(t *testing.T) {
		s := newTestServer(t)
		s.autoHostLock = &autoHostState{}
		s.githubOAuth = &auth.ProviderConfig{
			ClientID:     "id",
			ClientSecret: "sec",
		}

		// Before lock, oauthConfigFor returns nil.
		if cfg := s.oauthConfigFor("github"); cfg != nil {
			t.Fatal("oauthConfigFor should return nil before lock")
		}

		// Lock via a request.
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		r.Host = "caic.example.com"
		r.Header.Set("X-Forwarded-Proto", "https")
		s.autoHostLock.lock(r)

		// After lock, redirect URI is resolved.
		cfg := s.oauthConfigFor("github")
		if cfg == nil {
			t.Fatal("oauthConfigFor should not return nil after lock")
		}
		want := "https://caic.example.com/api/v1/auth/github/callback"
		if cfg.RedirectURI != want {
			t.Errorf("RedirectURI = %q, want %q", cfg.RedirectURI, want)
		}

		// Original config is not modified.
		if s.githubOAuth.RedirectURI != "" {
			t.Errorf("original config should not be modified, got RedirectURI = %q", s.githubOAuth.RedirectURI)
		}
	})
}

func TestOAuthCallbackStateValidation(t *testing.T) {
	// Spin up a fake OAuth token endpoint that returns a valid access token,
	// and a fake userinfo endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"bearer"}`))
	}))
	defer tokenServer.Close()
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"login":"testuser","avatar_url":"https://example.com/avatar.png"}`))
	}))
	defer userServer.Close()

	secret := make([]byte, 32)
	usersPath := filepath.Join(t.TempDir(), "users.json")
	store, err := auth.Open(usersPath)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	s := newTestServer(t)
	s.sessionSecret = secret
	s.authStore = store
	s.githubOAuth = &auth.ProviderConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		AuthEndpoint: "https://github.com/login/oauth/authorize",
		TokenURL:     tokenServer.URL,
		UserInfoURL:  userServer.URL,
		Scopes:       []string{"repo"},
		RedirectURI:  "http://localhost/api/v1/auth/github/callback",
	}

	t.Run("valid state round-trip succeeds", func(t *testing.T) {
		// Simulate the start handler to get a valid state cookie.
		startW := httptest.NewRecorder()
		startReq := httptest.NewRequest(http.MethodGet, "/api/v1/auth/github/start", http.NoBody)
		s.handleAuthStart("github")(startW, startReq)
		if startW.Code != http.StatusFound {
			t.Fatalf("start status = %d, want %d", startW.Code, http.StatusFound)
		}

		// Extract the state cookie and the state query param from the redirect URL.
		var stateCookie *http.Cookie
		for _, c := range startW.Result().Cookies() {
			if c.Name == auth.StateCookieName {
				stateCookie = c
				break
			}
		}
		if stateCookie == nil {
			t.Fatal("no state cookie set")
		}
		loc := startW.Header().Get("Location")
		redirectURL, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("parse redirect URL: %v", err)
		}
		rawState := redirectURL.Query().Get("state")

		// Build callback request with the state echoed back (as GitHub would).
		cbReq := httptest.NewRequest(http.MethodGet,
			"/api/v1/auth/github/callback?code=testcode&state="+url.QueryEscape(rawState), http.NoBody)
		cbReq.AddCookie(stateCookie)
		cbW := httptest.NewRecorder()
		s.handleAuthCallback("github")(cbW, cbReq)

		if cbW.Code != http.StatusFound {
			body, _ := io.ReadAll(cbW.Result().Body)
			t.Fatalf("callback status = %d, want %d; body = %s", cbW.Code, http.StatusFound, body)
		}
	})
}

func TestForgeFor(t *testing.T) {
	t.Run("PAT", func(t *testing.T) {
		s := newTestServer(t)
		s.forge.githubToken = "pat-token"
		f := s.forge.forgeFor(t.Context(), forge.KindGitHub)
		if f == nil {
			t.Fatal("forgeFor returned nil with PAT set")
		}
	})

	t.Run("no token returns nil", func(t *testing.T) {
		s := newTestServer(t)
		f := s.forge.forgeFor(t.Context(), forge.KindGitHub)
		if f != nil {
			t.Fatal("forgeFor should return nil when no tokens available")
		}
	})

	t.Run("no token without user context returns nil even with auth store", func(t *testing.T) {
		usersPath := filepath.Join(t.TempDir(), "users.json")
		store, err := auth.Open(usersPath)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		s := newTestServer(t)
		s.authStore = store
		// OAuth mode but no user in context and no PAT — returns nil.
		// CI polling is driven by SSE handlers which have user context.
		f := s.forge.forgeFor(t.Context(), forge.KindGitHub)
		if f != nil {
			t.Fatal("forgeFor should return nil without user context or PAT")
		}
	})
}

func TestPrefsPerUser(t *testing.T) {
	t.Run("separate users get separate preferences", func(t *testing.T) {
		s := newTestServer(t)

		if err := s.prefs.Update("user-alice", func(p *preferences.Preferences) {
			p.Settings.AutoFixOnCIFailure = true
		}); err != nil {
			t.Fatalf("update alice: %v", err)
		}

		p1 := s.prefs.Get("user-alice")
		p2 := s.prefs.Get("user-bob")
		if !p1.Settings.AutoFixOnCIFailure {
			t.Error("alice: AutoFixOnCIFailure should be true")
		}
		if p2.Settings.AutoFixOnCIFailure {
			t.Error("bob: AutoFixOnCIFailure should be false (independent of alice)")
		}
	})

	t.Run("all users stored in single file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "preferences.json")
		store, err := preferences.Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := store.Update("alice", func(p *preferences.Preferences) {
			p.Settings.AutoFixOnCIFailure = true
		}); err != nil {
			t.Fatalf("update alice: %v", err)
		}
		if err := store.Update("bob", func(p *preferences.Preferences) {
			p.Harness = "codex"
		}); err != nil {
			t.Fatalf("update bob: %v", err)
		}
		// Reload from disk to verify both users persisted in the same file.
		store2, err := preferences.Open(path)
		if err != nil {
			t.Fatalf("Open reload: %v", err)
		}
		if !store2.Get("alice").Settings.AutoFixOnCIFailure {
			t.Error("alice: AutoFixOnCIFailure not persisted")
		}
		if store2.Get("bob").Harness != "codex" {
			t.Error("bob: Harness not persisted")
		}
	})

	t.Run("default user in no-auth mode", func(t *testing.T) {
		s := newTestServer(t)
		// No auth in context — userIDFromCtx returns "default".
		id := userIDFromCtx(t.Context())
		if id != "default" {
			t.Errorf("expected 'default', got %q", id)
		}
		// Prefs for default user are usable.
		p := s.prefs.Get(id)
		if p.Version == 0 {
			t.Error("default prefs should have non-zero version")
		}
	})
}
