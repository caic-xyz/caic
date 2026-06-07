// Tests for the HTTP server request handling and routing.

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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/tasktest"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/voicegateway/voicertc"
)

// stubBackend implements agent.Backend for test map-membership checks.
type stubBackend struct{}

func (stubBackend) Harness() agent.Harness { return "stub" }

func (stubBackend) Start(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("stub")
}

func (stubBackend) AttachRelay(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("stub")
}

func (stubBackend) ReadRelayOutput(context.Context, string) ([]agent.Message, int64, error) {
	return nil, 0, errors.New("stub")
}

func (stubBackend) Models() []string   { return []string{"m1", "m2"} }
func (stubBackend) SetModels([]string) {}

func (stubBackend) SupportsImages() bool { return false }

func (stubBackend) AgentArgs(agent.HarnessArgs) []string { return nil }

func (stubBackend) NewWire() agent.WireFormat { return claudecode.New() }

func (stubBackend) SupportsCompact() bool { return false }

func (stubBackend) ContextWindowLimit(string) int { return 180_000 }

func decodeError(t *testing.T, w *httptest.ResponseRecorder) api.ErrorDetails {
	var resp api.ErrorResponse
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

// newTestServer creates a Server for tests.
func newTestServer(t *testing.T) *Server {
	checker, err := ipgeo.NewChecker(t.Context(), "0.0.0.0/0,::/0", "", "")
	if err != nil {
		t.Fatalf("ipgeo.NewChecker: %v", err)
	}
	s := &Server{
		ctx:            t.Context(),
		runtimeBackend: &mdruntime.Backend{},
		prefs:          newTestPrefs(t),
		ipgeoChecker:   checker,
		forge:          newForgeManager("", "", nil),
	}
	s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
	s.initConcernAdapters()
	s.webhooks = s.newWebhookHandlers(nil, nil, nil)
	return s
}

// insertTestTask registers a task in the test server's manager and returns the
// entry. It registers the entry under the supplied path id as well as the
// task's own ID string, so handlers that re-resolve via the Manager by
// entry.Task().ID.String() find the same entry (in production the two always
// coincide because Insert keys on t.ID.String()).
func insertTestTask(t *testing.T, s *Server, id string, tk *task.Task) *tasks.Entry { //nolint:unparam // id is constant today; keep generic
	e := tasks.NewEntry(tk)
	s.taskMgr.Insert(id, e)
	if taskID := tk.ID.String(); taskID != id {
		s.taskMgr.Insert(taskID, e)
	}
	return e
}

// registerTestRunner registers a runner under relPath in the task Manager's
// registry, the single owner of the runner set.
func registerTestRunner(s *Server, relPath string, r *task.Runner) {
	s.taskMgr.RegisterRunner(relPath, r)
}

// testEntries returns a snapshot of every registered task entry (test-only).
func testEntries(s *Server) []*tasks.Entry {
	var out []*tasks.Entry
	s.taskMgr.Range(func(_ string, e *tasks.Entry) bool {
		out = append(out, e)
		return true
	})
	return out
}

// loadPurgedTasksForTest reads logs from disk and registers purged tasks via
// the manager. Replaces the deleted Server.loadPurgedTasks helper.
func loadPurgedTasksForTest(s *Server) error {
	logs, err := task.LoadLogs(s.logDir)
	if err != nil {
		return err
	}
	return s.taskMgr.LoadPurgedTasks(logs)
}

func newRunnerConstructionTestServer(t *testing.T, root string) *Server {
	harnessEnv := map[string][]string{string(agent.Codex): {"CODEX_HOME=/tmp/codex"}}
	backend := &mdruntime.Backend{HarnessEnv: harnessEnv}
	logDir := filepath.Join(t.TempDir(), "logs")
	cacheDir := filepath.Join(t.TempDir(), "cache")
	s := &Server{
		ctx:            t.Context(),
		absRoot:        root,
		logDir:         logDir,
		cacheDir:       cacheDir,
		runtimeBackend: backend,
		agentBackends:  map[agent.Harness]agent.Backend{agent.Codex: stubBackend{}},
		harnessEnv:     harnessEnv,
		repoReg:        newRepoRegistry(nil),
	}
	s.taskMgr = tasks.New(tasks.Config{
		ServerCtx:  t.Context(),
		LogDir:     logDir,
		CacheDir:   cacheDir,
		Backend:    backend,
		Backends:   s.agentBackends,
		HarnessEnv: harnessEnv,
	})
	s.initConcernAdapters()
	return s
}

func initCloneSourceRepo(t *testing.T) string {
	repo := filepath.Join(t.TempDir(), "source")
	runServerGit(t, "", "init", repo)
	runServerGit(t, repo, "config", "user.email", "test@example.com")
	runServerGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runServerGit(t, repo, "add", "README.md")
	runServerGit(t, repo, "commit", "-m", "init")
	runServerGit(t, repo, "branch", "-M", "main")
	return repo
}

func runServerGit(t *testing.T, dir string, args ...string) {
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-only fixed arguments and temp paths
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestCloneRepo(t *testing.T) {
	t.Parallel()

	t.Run("valid runner construction and watcher overlap", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		source := initCloneSourceRepo(t)
		s := newRunnerConstructionTestServer(t, root)

		repo, err := s.cloneRepo(t.Context(), &v1.CloneRepoReq{URL: source, Path: "./cloned"})
		if err != nil {
			t.Fatalf("cloneRepo: %v", err)
		}
		if repo.Path != "cloned" {
			t.Fatalf("repo path = %q, want cloned", repo.Path)
		}
		runner, ok := s.taskMgr.Runner("cloned")
		if !ok {
			t.Fatal("cloned runner not registered")
		}
		if runner.RepoName != "cloned" {
			t.Fatalf("RepoName = %q, want cloned", runner.RepoName)
		}
		if runner.Dir != filepath.Join(root, "cloned") {
			t.Fatalf("Dir = %q, want cloned path", runner.Dir)
		}
		if runner.LogDir != s.logDir || runner.CacheDir != s.cacheDir {
			t.Fatalf("runner dirs = log %q cache %q, want log %q cache %q", runner.LogDir, runner.CacheDir, s.logDir, s.cacheDir)
		}
		if runner.Runtime != s.runtimeBackend {
			t.Fatal("runner instance backend was not wired")
		}
		if len(runner.HarnessEnv[string(agent.Codex)]) != 1 || runner.HarnessEnv[string(agent.Codex)][0] != "CODEX_HOME=/tmp/codex" {
			t.Fatalf("HarnessEnv = %#v, want configured codex env", runner.HarnessEnv)
		}
		if len(runner.Backends) == 0 {
			t.Fatal("runner backends were not initialized")
		}

		s.syncReposInDir(t.Context(), root)
		if got := s.repoReg.snapshot(); len(got) != 1 || got[0].RelPath != "cloned" {
			t.Fatalf("repo registry after watcher sync = %+v, want one cloned repo", got)
		}
		after, ok := s.taskMgr.Runner("cloned")
		if !ok {
			t.Fatal("runner disappeared after watcher sync")
		}
		if after != runner {
			t.Fatal("watcher replaced an already registered clone runner")
		}
	})

	t.Run("error cleans partial clone", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		parent := initCloneSourceRepo(t)
		submodule := initCloneSourceRepo(t)
		runServerGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", submodule, "deps/sub")
		runServerGit(t, parent, "commit", "-m", "add submodule")
		if err := os.RemoveAll(submodule); err != nil {
			t.Fatalf("remove submodule source: %v", err)
		}
		s := newRunnerConstructionTestServer(t, root)

		if _, err := s.cloneRepo(t.Context(), &v1.CloneRepoReq{URL: parent, Path: "broken"}); err == nil {
			t.Fatal("cloneRepo succeeded, want submodule clone failure")
		}
		if _, err := os.Stat(filepath.Join(root, "broken")); !os.IsNotExist(err) {
			t.Fatalf("partial clone path still exists: %v", err)
		}
		if got := s.repoReg.snapshot(); len(got) != 0 {
			t.Fatalf("repo registry = %+v, want empty after failed clone", got)
		}
		if _, ok := s.taskMgr.Runner("broken"); ok {
			t.Fatal("failed clone registered a runner")
		}
	})
}

func TestHandleTaskEvents(t *testing.T) {
	t.Parallel()
	t.Run("NotFound", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/tasks/99/raw_events", http.NoBody)
		req.SetPathValue("id", "99")
		w := httptest.NewRecorder()
		s.handleTaskRawEvents(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		e := decodeError(t, w)
		if e.Code != api.CodeNotFound {
			t.Errorf("code = %q, want %q", e.Code, api.CodeNotFound)
		}
	})

	t.Run("NonexistentID", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/tasks/abc/raw_events", http.NoBody)
		req.SetPathValue("id", "abc")
		w := httptest.NewRecorder()
		s.handleTaskRawEvents(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		e := decodeError(t, w)
		if e.Code != api.CodeNotFound {
			t.Errorf("code = %q, want %q", e.Code, api.CodeNotFound)
		}
	})
}

func TestHandleTaskInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		bodyJSON   string
		wantStatus int
		wantCode   api.ErrorCode
	}{
		{
			name:       "NotRunning",
			bodyJSON:   `{"prompt":{"text":"hello"}}`,
			wantStatus: http.StatusConflict,
			wantCode:   api.CodeConflict,
		},
		{
			name:       "EmptyPrompt",
			bodyJSON:   `{"prompt":{"text":""}}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   api.CodeBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestServer(t)
			insertTestTask(t, s, "t1", &task.Task{InitialPrompt: agent.Prompt{Text: "test"}})

			body := strings.NewReader(tt.bodyJSON)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks/t1/input", body)
			req.SetPathValue("id", "t1")
			w := httptest.NewRecorder()
			handleWithTask(s, s.sendInput)(w, req)
			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			e := decodeError(t, w)
			if e.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}
}

// testRestart is a helper for TestHandleRestart subtests.
func testRestart(t *testing.T, state task.State, bodyJSON string, wantStatus int, wantCode api.ErrorCode) {
	s := newTestServer(t)
	tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
	tk.SetState(state)
	insertTestTask(t, s, "t1", tk)

	body := strings.NewReader(bodyJSON)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks/t1/restart", body)
	req.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	handleWithTask(s, s.restartTask)(w, req)
	if w.Code != wantStatus {
		t.Errorf("status = %d, want %d", w.Code, wantStatus)
	}
	e := decodeError(t, w)
	if e.Code != wantCode {
		t.Errorf("code = %q, want %q", e.Code, wantCode)
	}
}

func TestHandleRestart(t *testing.T) {
	t.Parallel()
	t.Run("NotWaiting", func(t *testing.T) {
		t.Parallel()
		testRestart(t, task.StateRunning, `{"prompt":{"text":"new plan"}}`, http.StatusConflict, api.CodeConflict)
	})

	t.Run("EmptyPrompt", func(t *testing.T) {
		t.Parallel()
		testRestart(t, task.StateWaiting, `{"prompt":{"text":""}}`, http.StatusBadRequest, api.CodeBadRequest)
	})
}

func TestHandlePurge(t *testing.T) {
	t.Parallel()
	t.Run("NotWaiting", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		// StatePending is the zero value, but set explicitly for clarity.
		insertTestTask(t, s, "t1", tk)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks/t1/purge", http.NoBody)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(s, s.purgeTask)(w, req)
		if w.Code != http.StatusConflict {
			t.Errorf("status = %d, want %d", w.Code, http.StatusConflict)
		}
		e := decodeError(t, w)
		if e.Code != api.CodeConflict {
			t.Errorf("code = %q, want %q", e.Code, api.CodeConflict)
		}
	})

	t.Run("Waiting", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []task.RepoMount{{Name: "r"}}}
		tk.SetState(task.StateWaiting)
		s := newTestServer(t)
		registerTestRunner(s, "r", &task.Runner{BaseBranch: "main", Dir: t.TempDir()})
		insertTestTask(t, s, "t1", tk)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks/t1/purge", http.NoBody)
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
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []task.RepoMount{{Name: "r"}}}
		tk.SetState(task.StateRunning)
		s := newTestServer(t)
		registerTestRunner(s, "r", &task.Runner{BaseBranch: "main", Dir: t.TempDir()})
		insertTestTask(t, s, "t1", tk)

		// Use an already-cancelled context to simulate shutdown scenario
		// where BaseContext is cancelled before the handler completes.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks/t1/purge", http.NoBody)
		req = req.WithContext(ctx)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(s, s.purgeTask)(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})
}

func TestHandleCreateTask(t *testing.T) {
	t.Parallel()
	t.Run("ReturnsID", func(t *testing.T) {
		t.Parallel()
		s := &Server{
			ctx:   t.Context(),
			prefs: newTestPrefs(t),
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}},
		})
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test task"},"repos":[{"name":"myrepo"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
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
		t.Parallel()
		s := newTestServer(t)
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test task"}}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		e := decodeError(t, w)
		if e.Code != api.CodeBadRequest {
			t.Errorf("code = %q, want %q", e.Code, api.CodeBadRequest)
		}
	})

	t.Run("UnknownRepo", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"nonexistent"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		e := decodeError(t, w)
		if e.Code != api.CodeBadRequest {
			t.Errorf("code = %q, want %q", e.Code, api.CodeBadRequest)
		}
	})

	t.Run("UnknownHarness", func(t *testing.T) {
		t.Parallel()
		s := &Server{
			ctx:   t.Context(),
			prefs: newTestPrefs(t),
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "myrepo", &task.Runner{BaseBranch: "main", Dir: t.TempDir()})
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"nonexistent"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		e := decodeError(t, w)
		if e.Code != api.CodeBadRequest {
			t.Errorf("code = %q, want %q", e.Code, api.CodeBadRequest)
		}
		if !strings.Contains(e.Message, "nonexistent") {
			t.Errorf("message = %q, want it to mention the unknown harness", e.Message)
		}
	})

	t.Run("InvalidModel", func(t *testing.T) {
		t.Parallel()
		s := &Server{
			ctx:   t.Context(),
			prefs: newTestPrefs(t),
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[agent.Harness]agent.Backend{"stub": stubBackend{}},
		})
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"stub","model":"nonexistent"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		e := decodeError(t, w)
		if e.Code != api.CodeBadRequest {
			t.Errorf("code = %q, want %q", e.Code, api.CodeBadRequest)
		}
		if !strings.Contains(e.Message, "nonexistent") {
			t.Errorf("message = %q, want it to mention the invalid model", e.Message)
		}
	})

	t.Run("ValidModel", func(t *testing.T) {
		t.Parallel()
		s := &Server{
			ctx:   t.Context(),
			prefs: newTestPrefs(t),
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[agent.Harness]agent.Backend{"stub": stubBackend{}},
		})
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"stub","model":"m1"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
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
		t.Parallel()
		s := &Server{
			ctx:   t.Context(),
			prefs: newTestPrefs(t),
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}},
		})
		handler := handle(s.createTask)

		// Set docker image in user preferences.
		if err := s.prefs.Update("default", func(p *preferences.Preferences) {
			p.Settings.BaseImage = "ghcr.io/my/image:v1"
			p.Settings.ContainerPlatform = "linux/amd64"
		}); err != nil {
			t.Fatal(err)
		}

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
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

		// Verify the task uses the image from preferences.
		entry, _ := s.taskMgr.GetEntry(resp.ID.String())
		if entry == nil {
			t.Fatal("task not found")
		}
		if entry.Task().BaseImage != "ghcr.io/my/image:v1" {
			t.Errorf("Image = %q, want %q", entry.Task().BaseImage, "ghcr.io/my/image:v1")
		}
		if entry.Task().ContainerPlatform != "linux/amd64" {
			t.Errorf("ContainerPlatform = %q, want linux/amd64", entry.Task().ContainerPlatform)
		}
	})

	t.Run("WithCachePreferences", func(t *testing.T) {
		t.Parallel()
		s := &Server{
			ctx:   t.Context(),
			prefs: newTestPrefs(t),
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}},
		})
		if err := s.prefs.Update("default", func(p *preferences.Preferences) {
			p.Settings.UseDefaultCaches = true
			p.Settings.WellKnownCaches = map[string]bool{"go-mod": false, "npm": true}
			p.Settings.CacheMappings = []preferences.CacheMapping{{HostPath: "/host/custom", ContainerPath: "/home/user/.custom"}}
			p.Settings.CustomMounts = []preferences.MountMapping{{HostPath: "/host/work", ContainerPath: "/workspace/external"}}
		}); err != nil {
			t.Fatal(err)
		}

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handle(s.createTask)(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp v1.CreateTaskResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		entry, _ := s.taskMgr.GetEntry(resp.ID.String())
		if entry == nil {
			t.Fatal("task not found")
		}
		var gotCustom, gotCustomMount, gotNPM, gotGoMod bool
		for _, cm := range entry.Task().CacheMounts {
			switch cm.Name {
			case "custom-cache-0":
				gotCustom = cm.HostPath == "/host/custom" && cm.MountPath == "/home/user/.custom"
			case "custom-mount-0":
				gotCustomMount = cm.HostPath == "/host/work" && cm.MountPath == "/workspace/external"
			case "npm":
				gotNPM = true
			case "go-mod":
				gotGoMod = true
			}
		}
		if !gotCustom {
			t.Errorf("custom cache mapping missing from %+v", entry.Task().CacheMounts)
		}
		if !gotCustomMount {
			t.Errorf("custom mount missing from %+v", entry.Task().CacheMounts)
		}
		if !gotNPM {
			t.Errorf("enabled npm cache missing from %+v", entry.Task().CacheMounts)
		}
		if gotGoMod {
			t.Errorf("disabled go-mod cache present in %+v", entry.Task().CacheMounts)
		}
	})

	t.Run("NoRepoTask", func(t *testing.T) {
		t.Parallel()
		// Regression: creating a task with no repos panicked with
		// "makeslice: cap out of range" because len(req.Repos)-1 == -1.
		s := &Server{
			ctx:   t.Context(),
			prefs: newTestPrefs(t),
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{
			Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}},
		})
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"no repo task"},"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
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

	t.Run("NoRepoRunnerNoBackend", func(t *testing.T) {
		t.Parallel()
		// Creating a no-repo task with no registered harness backends returns
		// a clear 400 instead of panicking.
		s := newTestServer(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{}})
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"no repo task"},"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("UnknownField", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		handler := handle(s.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repo":"r","harness":"claude","bogus":true}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		e := decodeError(t, w)
		if e.Code != api.CodeBadRequest {
			t.Errorf("code = %q, want %q", e.Code, api.CodeBadRequest)
		}
	})
}

func TestSignalProcess(t *testing.T) {
	t.Parallel()

	t.Run("strictDecodeRejectsUnknownField", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []task.RepoMount{{Name: "r"}}}
		tk.SetRuntimeInstanceInfo("ctr", "", "", 0)
		insertTestTask(t, s, "t1", tk)
		s.runtimeBackend = &tasktest.FakeRuntimeBackend{
			SignalFunc: func(context.Context, runtime.InstanceID, int, string) error {
				t.Fatal("Signal should not be called")
				return nil
			},
		}
		processes := &RuntimeProcesses{
			taskMgr:      s.taskMgr,
			backend:      s.runtimeBackend,
			authEnabled:  s.authEnabled,
			notifyChange: s.taskMgr.NotifyTaskChange,
		}

		body := strings.NewReader(`{"signal":"SIGTERM","extra":true}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks/t1/processes/123/signal", body)
		req.SetPathValue("id", "t1")
		req.SetPathValue("pid", "123")
		w := httptest.NewRecorder()
		processes.HandleSignalProcess(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []task.RepoMount{{Name: "r"}}}
		tk.SetRuntimeInstanceInfo("ctr", "", "", 0)
		insertTestTask(t, s, "t1", tk)
		var gotPID int
		var gotSignal string
		s.runtimeBackend = &tasktest.FakeRuntimeBackend{
			SignalFunc: func(_ context.Context, id runtime.InstanceID, pid int, sig string) error {
				if id != "ctr" {
					t.Errorf("instance = %q, want ctr", id)
				}
				gotPID = pid
				gotSignal = sig
				return nil
			},
		}
		processes := &RuntimeProcesses{
			taskMgr:      s.taskMgr,
			backend:      s.runtimeBackend,
			authEnabled:  s.authEnabled,
			notifyChange: s.taskMgr.NotifyTaskChange,
		}

		body := strings.NewReader(`{"signal":"SIGKILL"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/tasks/t1/processes/123/signal", body)
		req.SetPathValue("id", "t1")
		req.SetPathValue("pid", "123")
		w := httptest.NewRecorder()
		processes.HandleSignalProcess(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if gotPID != 123 {
			t.Errorf("pid = %d, want 123", gotPID)
		}
		if gotSignal != "SIGKILL" {
			t.Errorf("signal = %q, want SIGKILL", gotSignal)
		}
	})
}

func TestHandleListRepos(t *testing.T) {
	t.Parallel()
	s := &Server{
		repoReg: newRepoRegistry([]RepoInfo{
			{RelPath: "org/repoA", AbsPath: "/src/org/repoA", BaseBranch: "main"},
			{RelPath: "repoB", AbsPath: "/src/repoB", BaseBranch: "develop"},
		}),
	}
	s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/server/repos", http.NoBody)
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
	if repos[1].BaseBranch.Name != "develop" {
		t.Errorf("repos[1].BaseBranch = %q, want %q", repos[1].BaseBranch.Name, "develop")
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
	t.Parallel()
	t.Run("OnStartup", func(t *testing.T) {
		t.Parallel()
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 3 {
			t.Fatalf("len(entries) = %d, want 3", len(entries))
		}

		// Collect prompts sorted by ksid (time-sortable) to verify all loaded.
		prompts := make([]string, 0, len(entries))
		var anyEntry *tasks.Entry
		for _, e := range entries {
			prompts = append(prompts, e.Task().InitialPrompt.Text)
			if anyEntry == nil {
				anyEntry = e
			}
		}
		sort.Strings(prompts)
		if prompts[0] != "task 0" || prompts[1] != "task 1" || prompts[2] != "task 2" {
			t.Errorf("prompts = %v, want [task 0, task 1, task 2]", prompts)
		}

		// Verify result is populated on at least one entry.
		if anyEntry.Result() == nil {
			t.Fatal("result is nil on a loaded entry")
		}

		// Verify done channel is closed (task is terminal).
		for _, e := range entries {
			select {
			case <-e.Done():
			default:
				t.Error("done channel not closed on a loaded entry")
			}
		}
	})

	t.Run("TimeLimit", func(t *testing.T) {
		t.Parallel()
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1 (old task should be filtered out)", len(entries))
		}
		for _, e := range entries {
			if e.Task().InitialPrompt.Text != "recent task" {
				t.Errorf("prompt = %q, want \"recent task\"", e.Task().InitialPrompt.Text)
			}
		}
	})

	t.Run("PerRepoLimit", func(t *testing.T) {
		t.Parallel()
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		// Count tasks per repo.
		counts := map[string]int{}
		for _, e := range entries {
			repo := ""
			if p := e.Task().Primary(); p != nil {
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
		if len(entries) != 8 {
			t.Errorf("total tasks = %d, want 8 (5 from a + 3 from b)", len(entries))
		}
	})

	t.Run("CostInJSON", func(t *testing.T) {
		t.Parallel()
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		for _, e := range entries {
			j := v1conv.Task(t.Context(), e, s.taskResolvers())
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
		t.Parallel()
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		for _, e := range entries {
			j := v1conv.Task(t.Context(), e, s.taskResolvers())
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
		t.Parallel()
		logDir := t.TempDir()

		// Two logs from different repos share the same branch name.
		// Each must retain its own title and prompt.
		metaA := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1,
			Prompt: "optimize genai provider", Repos: []agent.MetaRepo{{Name: "genai", Branch: "caic-0"}},
			Harness: agent.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Title: "Skip Unnecessary MD Runtime Build",
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 2 {
			t.Fatalf("len(entries) = %d, want 2", len(entries))
		}

		// Verify each task has the title matching its own repo.
		for _, e := range entries {
			var repoName string
			if p := e.Task().Primary(); p != nil {
				repoName = p.Name
			}
			switch repoName {
			case "genai":
				if got := e.Task().Title(); got != "Optimize GenAI Provider" {
					t.Errorf("genai title = %q, want %q", got, "Optimize GenAI Provider")
				}
			case "md":
				if got := e.Task().Title(); got != "Skip Unnecessary Docker Image Rebuilds" {
					t.Errorf("md title = %q, want %q", got, "Skip Unnecessary Docker Image Rebuilds")
				}
			default:
				t.Errorf("unexpected repo %q", repoName)
			}
		}

		// Verify that branchIDs scoped by repo does not lose either entry.
		// This mirrors the branchIDs construction in adoptContainers.
		branchIDs := make(map[string][]string)
		s.taskMgr.Range(func(id string, e *tasks.Entry) bool {
			if p := e.Task().Primary(); p != nil && p.Branch != "" {
				key := p.Name + "\x00" + p.Branch
				branchIDs[key] = append(branchIDs[key], id)
			}
			return true
		})
		if len(branchIDs) != 2 {
			t.Errorf("branchIDs has %d keys, want 2 (repo-scoped keys must not collide)", len(branchIDs))
		}
	})

	t.Run("EmptyDir", func(t *testing.T) {
		t.Parallel()
		s := &Server{
			logDir: t.TempDir(),
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 0 {
			t.Errorf("len(tasks) = %d, want 0", len(entries))
		}
	})

	t.Run("PROutsideTailWindow", func(t *testing.T) {
		t.Parallel()
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		for _, e := range entries {
			snap := e.Task().Snapshot()
			// caic_pr is outside the 64 KiB tail window; without full
			// message parse, PR metadata is not recovered.
			if snap.ForgePR != 0 {
				t.Errorf("ForgePR = %d, want 0 (prMsg outside tail window)", snap.ForgePR)
			}
			if snap.ForgeOwner != "" {
				t.Errorf("ForgeOwner = %q, want empty (prMsg outside tail window)", snap.ForgeOwner)
			}
			if snap.ForgeRepo != "" {
				t.Errorf("ForgeRepo = %q, want empty (prMsg outside tail window)", snap.ForgeRepo)
			}
		}
	})

	t.Run("PerRepoLimit", func(t *testing.T) {
		t.Parallel()
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 10 {
			t.Fatalf("len(entries) = %d, want 10 (5 per repo)", len(entries))
		}

		// Collect prompts and verify oldest task per repo was dropped.
		prompts := make([]string, 0, len(entries))
		for _, e := range entries {
			prompts = append(prompts, e.Task().InitialPrompt.Text)
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
		t.Parallel()
		// Tasks without a caic_result trailer always load as "failed" —
		// we cannot distinguish purged-without-trailer from interrupted.
		// adoptContainers replaces stale entries with live state when a
		// instance is still running.
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

		// no trailer + ResultMessage present → "failed" (instance may be gone)
		writeLogFile(t, logDir, "waiting.jsonl", meta("waiting task"), resultMsg)

		// no trailer + no ResultMessage → "failed"
		writeLogFile(t, logDir, "interrupted.jsonl", meta("interrupted task"))

		// explicit trailer → "purged"
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, logDir, "purged.jsonl", meta("purged task"), trailer)

		s := &Server{
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 3 {
			t.Fatalf("len(entries) = %d, want 3", len(entries))
		}

		states := make(map[string]string, len(entries))
		for _, e := range entries {
			states[e.Task().InitialPrompt.Text] = e.Task().Snapshot().State.String()
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
	t.Run("FeatureFlags", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "feat task",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: agent.Claude,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Tailscale: true, USB: true, Display: true,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, logDir, "feat.jsonl", meta, trailer)

		s := &Server{
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		for _, e := range entries {
			if !e.Task().Tailscale {
				t.Error("Tailscale = false, want true")
			}
			if !e.Task().USB {
				t.Error("USB = false, want true")
			}
			if !e.Task().Display {
				t.Error("Display = false, want true")
			}
		}
	})
}

func TestComputeTaskPatch(t *testing.T) {
	t.Parallel()
	t.Run("ChangedFields", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
	t.Parallel()
	t.Run("PurgedTaskEvents", func(t *testing.T) {
		t.Parallel()
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		var taskID string
		s.taskMgr.Range(func(id string, _ *tasks.Entry) bool {
			taskID = id
			return false
		})

		// Subscribe to events via SSE. The handler should return immediately for
		// purged tasks instead of blocking until context deadline.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/tasks/"+taskID+"/raw_events", http.NoBody).WithContext(ctx)
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
		// With lazy loading, purged task messages are loaded on demand
		// when the events endpoint is accessed. The handler replays the
		// full message history, then emits "ready" and closes the stream.
		body := w.Body.String()
		if !strings.Contains(body, "event: ready") {
			t.Error("expected 'ready' event for purged task")
		}
		if !strings.Contains(body, "I found the bug") {
			t.Error("expected text message 'I found the bug' to be replayed for purged task")
		}
	})

	t.Run("StreamEventTextDelta", func(t *testing.T) {
		t.Parallel()
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
			logDir: logDir,
		}
		s.taskMgr = tasks.New(tasks.Config{ServerCtx: t.Context()})
		registerTestRunner(s, "", &task.Runner{Backends: map[agent.Harness]agent.Backend{agent.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		var taskID string
		s.taskMgr.Range(func(id string, _ *tasks.Entry) bool {
			taskID = id
			return false
		})

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/tasks/"+taskID+"/raw_events", http.NoBody).WithContext(ctx)
		req.SetPathValue("id", taskID)
		w := httptest.NewRecorder()
		s.handleTaskRawEvents(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		// With lazy loading, purged task messages are loaded on demand
		// when the events endpoint is accessed. The handler replays the
		// full message history, then emits "ready" and closes the stream.
		body := w.Body.String()
		if !strings.Contains(body, "event: ready") {
			t.Error("expected 'ready' event for purged task")
		}
		if !strings.Contains(body, "Hello world") {
			t.Error("expected text message 'Hello world' to be replayed for purged task")
		}
	})
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	t.Run("both empty is valid", func(t *testing.T) {
		t.Parallel()
		if err := (&Config{}).Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("PAT only is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitHub: GitHubConfig{Token: "ghp_abc"}, GitLab: GitLabConfig{Token: "glpat-abc"}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth with ExternalURL and allowlist is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: "alice,bob"},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("ExternalURL auto is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{Auth: AuthConfig{ExternalURL: "auto"}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth with ExternalURL auto is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: "alice"},
			Auth:   AuthConfig{ExternalURL: "auto"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("OAuth without ExternalURL is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: "alice"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub OAuth without allowlist is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec"},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLab OAuth without allowlist is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitLab: GitLabConfig{OAuthClientID: "id", OAuthClientSecret: "sec"},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("OAuth with http ExternalURL is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: "alice"},
			Auth:   AuthConfig{ExternalURL: "http://caic.example.com"},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("invalid ExternalURL is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{Auth: AuthConfig{ExternalURL: "not a url"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("ExternalURL with subpath is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{Auth: AuthConfig{ExternalURL: "https://caic.example.com/sub"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("ExternalURL with trailing slash is valid and stripped", func(t *testing.T) {
		t.Parallel()
		c := &Config{Auth: AuthConfig{ExternalURL: "https://caic.example.com/"}}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
		if c.Auth.ExternalURL != "https://caic.example.com" {
			t.Fatalf("ExternalURL trailing slash not stripped: %q", c.Auth.ExternalURL)
		}
	})
	t.Run("invalid GitLabURL is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitLab: GitLabConfig{URL: "not a url"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLabURL with subpath is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitLab: GitLabConfig{URL: "https://gitlab.example.com/sub"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub OAuth ID without secret is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitHub: GitHubConfig{OAuthClientID: "id"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub OAuth secret without ID is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitHub: GitHubConfig{OAuthClientSecret: "sec"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLab OAuth ID without secret is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitLab: GitLabConfig{OAuthClientID: "id"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitLab OAuth secret without ID is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{GitLab: GitLabConfig{OAuthClientSecret: "sec"}}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("GitHub PAT and OAuth together is valid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitHub: GitHubConfig{Token: "ghp_abc", OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: "alice"},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
	t.Run("GitLab PAT and OAuth together is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			GitLab: GitLabConfig{Token: "glpat-abc", OAuthClientID: "id", OAuthClientSecret: "sec", OAuthAllowedUsers: "alice"},
			Auth:   AuthConfig{ExternalURL: "https://caic.example.com"},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
	t.Run("invalid voice gateway is invalid", func(t *testing.T) {
		t.Parallel()
		c := &Config{
			Voice: VoiceConfig{
				Gateway: VoiceGatewayConfig{Mode: VoiceGatewayModeExternal},
			},
		}
		if err := c.Validate(); err == nil {
			t.Fatal("Validate() expected error, got nil")
		}
	})
}

func TestVoiceGatewayMetadata(t *testing.T) {
	t.Parallel()
	t.Run("default disabled", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		got := s.voiceGatewayMetadata()
		if got.Mode != v1.VoiceGatewayModeDisabled {
			t.Fatalf("Mode = %q, want disabled", got.Mode)
		}
	})

	t.Run("external", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.voiceGateway = VoiceGatewayConfig{
			Mode: VoiceGatewayModeExternal,
			URL:  "https://voice.example.com",
		}
		got := s.voiceGatewayMetadata()
		if got.Mode != v1.VoiceGatewayModeExternal {
			t.Fatalf("Mode = %q, want external", got.Mode)
		}
		if got.URL != "https://voice.example.com" {
			t.Fatalf("URL = %q, want https://voice.example.com", got.URL)
		}
		if got.TokenEndpoint != "/api/v1/voice/token" {
			t.Fatalf("TokenEndpoint = %q, want /api/v1/voice/token", got.TokenEndpoint)
		}
	})

	t.Run("embedded", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.voiceGateway = VoiceGatewayConfig{Mode: VoiceGatewayModeEmbedded}
		s.voiceBridge = &voicertc.Bridge{}
		got := s.voiceGatewayMetadata()
		if got.Mode != v1.VoiceGatewayModeEmbedded {
			t.Fatalf("Mode = %q, want embedded", got.Mode)
		}
		if got.TokenEndpoint != "/api/v1/voice/token" {
			t.Fatalf("TokenEndpoint = %q, want /api/v1/voice/token", got.TokenEndpoint)
		}
	})
}

func TestBuildHandler(t *testing.T) {
	t.Parallel()
	t.Run("auth disabled", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		if _, err := s.buildHandler(); err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
	})

	t.Run("auth enabled", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
		s := newTestServer(t)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequestWithContext(t.Context(), method, "/", http.NoBody)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s / = %d, want %d", method, w.Code, http.StatusMethodNotAllowed)
			}
		}
	})

	t.Run("static host check rejects wrong host", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.hostState = auth.NewHostState("https://caic.example.com")
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "evil.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("wrong host: status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("static host check allows matching host", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.hostState = auth.NewHostState("https://caic.example.com")
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("matching host should not be forbidden")
		}
	})

	t.Run("static host check is case insensitive", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.hostState = auth.NewHostState("https://caic.example.com")
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "CAIC.Example.COM"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("case-insensitive host should not be forbidden")
		}
	})

	t.Run("no host check when hostState nil", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "anything.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("no host check should not reject any host")
		}
	})

	t.Run("auto host check locks first FQDN", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.hostState = &auth.HostState{}
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		// First FQDN request locks the host.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("first FQDN request should not be forbidden")
		}

		// Same host is allowed.
		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("same host should not be forbidden")
		}

		// Different FQDN is rejected.
		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "evil.example.com"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("different FQDN: status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("auto host check rejects same host on different port", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.hostState = &auth.HostState{}
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		// Lock on port 8080.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com:8080"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("first FQDN request should not be forbidden")
		}

		// Same host, different port is rejected.
		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com:9090"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("different port: status = %d, want %d", w.Code, http.StatusForbidden)
		}

		// Same host without port is also rejected.
		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("missing port: status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("auto host check allows IP before and after lock", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.hostState = &auth.HostState{}
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		// IP request before any FQDN — allowed.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "192.168.1.1:8080"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("IP request before lock should not be forbidden")
		}

		// Lock a FQDN.
		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)

		// IP request after lock — still allowed.
		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "192.168.1.1:8080"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("IP request after lock should not be forbidden")
		}
	})

	t.Run("auto host check allows localhost", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.hostState = &auth.HostState{}
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("localhost should not be forbidden")
		}
	})

	t.Run("auto host check is case insensitive", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.hostState = &auth.HostState{}
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "CAIC.Example.COM"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("first FQDN should not be forbidden")
		}

		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = "caic.example.com"
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("case-insensitive match should not be forbidden")
		}
	})
}

func TestOAuthCallbackStateValidation(t *testing.T) {
	t.Parallel()
	// Spin up a fake OAuth token endpoint that returns a valid access token,
	// and a fake userinfo endpoint.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"bearer"}`))
	}))
	t.Cleanup(tokenServer.Close)
	userServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"login":"testuser","avatar_url":"https://example.com/avatar.png"}`))
	}))
	t.Cleanup(userServer.Close)

	secret := make([]byte, 32)
	usersPath := filepath.Join(t.TempDir(), "users.json")
	store, err := auth.Open(usersPath)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}

	host := auth.NewHostState("http://localhost")
	s := newTestServer(t)
	s.sessionSecret = secret
	s.authStore = store
	s.hostState = host
	s.githubOAuth = &auth.ProviderConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		AuthEndpoint: "https://github.com/login/oauth/authorize",
		TokenURL:     tokenServer.URL,
		UserInfoURL:  userServer.URL,
		Scopes:       []string{"repo"},
		Provider:     "github",
		Host:         host,
	}

	t.Run("valid state round-trip succeeds", func(t *testing.T) {
		t.Parallel()
		// Simulate the start handler to get a valid state cookie.
		startW := httptest.NewRecorder()
		startReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/auth/github/start", http.NoBody)
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
		cbReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
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
	t.Parallel()
	t.Run("PAT", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		s.forge.githubToken = "pat-token"
		f := s.forge.forgeFor(t.Context(), forge.KindGitHub)
		if f == nil {
			t.Fatal("forgeFor returned nil with PAT set")
		}
	})

	t.Run("no token returns nil", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		f := s.forge.forgeFor(t.Context(), forge.KindGitHub)
		if f != nil {
			t.Fatal("forgeFor should return nil when no tokens available")
		}
	})

	t.Run("no token without user context returns nil even with auth store", func(t *testing.T) {
		t.Parallel()
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
	t.Parallel()
	t.Run("separate users get separate preferences", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
