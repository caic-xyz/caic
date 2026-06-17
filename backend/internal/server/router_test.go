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

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/gomode"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
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
	"github.com/caic-xyz/caic/oauth/oauthclient"
)

// stubBackend implements agent.Backend for test map-membership checks.
type stubBackend struct{}

func (stubBackend) Harness() harness.Name { return "stub" }

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

func (stubBackend) NewWire() agent.WireFormat { return claudecode.New().NewWire() }

func (stubBackend) SupportsCompact() bool { return false }

func (stubBackend) ContextWindowLimit(string) int { return 180_000 }

func decodeError(t *testing.T, w *httptest.ResponseRecorder) api.ErrorDetails {
	var resp api.ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	return resp.Error
}

func newTestPrefs(t testing.TB) *preferences.Store {
	path := filepath.Join(t.TempDir(), "preferences.json")
	store, err := preferences.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

// testRouter bundles a Router with the dependencies tests poke directly. The
// production Router exposes these only through handler concerns; the wrapper
// keeps test access ergonomic without re-adding fields to Router.
type testRouter struct {
	*Router

	taskMgr               *tasks.Manager
	repos                 *repos.Service
	prefs                 *preferences.Store
	forge                 *forgemanager.Manager
	oauthRefreshTokenPath string
}

// newTestRouter creates a Router for tests. Tests commonly mutate auth/host
// fields (authStore, sessionSecret, hostState) on the returned Router and then
// call buildHandler; buildHandler re-syncs hostState into the MCP concern, and
// the authHandlers copies must be synced by the test if exercised.
func newTestRouter(t testing.TB) *testRouter {
	checker, err := ipgeo.NewChecker(t.Context(), "0.0.0.0/0,::/0", "", "")
	if err != nil {
		t.Fatalf("ipgeo.NewChecker: %v", err)
	}
	backend := &mdruntime.Backend{}
	taskMgr := tasks.New(tasks.Config{ServerCtx: t.Context()})
	repoSvc := repos.NewService("", "", "", nil, repos.NewRegistry(nil), taskMgr, backend, nil)
	prefs := newTestPrefs(t)
	forgeManager := forgemanager.New("", "", nil)
	s, err := New(t.Context(), Dependencies{
		Repos:          repoSvc,
		ProcessBackend: backend,
		TaskManager:    taskMgr,
		Preferences:    prefs,
		IPGeoChecker:   checker,
		Forge:          forgeManager,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testRouter{Router: s, taskMgr: taskMgr, repos: repoSvc, prefs: prefs, forge: forgeManager}
}

// newTestRouterWithAuthHost creates a Router with an auth store, suitable for
// OAuth tests that need s.oauthServer to be non-nil at construction time.
// If refreshTokenPath is non-empty, it is used as the OAuth refresh token store.
func newTestRouterWithAuthHost(t testing.TB, authStore *auth.Store, refreshTokenPath string, hostState *auth.HostState) *testRouter {
	checker, err := ipgeo.NewChecker(t.Context(), "0.0.0.0/0,::/0", "", "")
	if err != nil {
		t.Fatalf("ipgeo.NewChecker: %v", err)
	}
	backend := &mdruntime.Backend{}
	taskMgr := tasks.New(tasks.Config{ServerCtx: t.Context()})
	repoSvc := repos.NewService("", "", "", nil, repos.NewRegistry(nil), taskMgr, backend, nil)
	prefs := newTestPrefs(t)
	forgeManager := forgemanager.New("", "", nil)
	s, err := New(t.Context(), Dependencies{
		Repos:                      repoSvc,
		ProcessBackend:             backend,
		TaskManager:                taskMgr,
		Preferences:                prefs,
		IPGeoChecker:               checker,
		Forge:                      forgeManager,
		AuthStore:                  authStore,
		OAuthPrivateKeyPEM:         testMCPOAuthSigningKeyPEM(t),
		OAuthRefreshTokenStorePath: refreshTokenPath,
		HostState:                  hostState,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testRouter{Router: s, taskMgr: taskMgr, repos: repoSvc, prefs: prefs, forge: forgeManager, oauthRefreshTokenPath: refreshTokenPath}
}

// newTestOAuthRouter creates a Router configured with OAuth host state and a
// session secret, suitable for tests that hit OAuth endpoints or issue tokens.
func newTestOAuthRouter(t testing.TB, authStore *auth.Store) *testRouter {
	s := newTestRouterWithAuthHost(t, authStore, "", auth.NewHostState("https://caic.example.com"))
	s.sessionSecret = []byte("0123456789abcdef0123456789abcdef")
	return s
}

func testTaskHandlers(s *testRouter) *taskHandlers {
	return s.taskHandlers
}

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		if _, err := New(t.Context(), Dependencies{}); err == nil {
			t.Fatal("New() error = nil, want process backend required")
		}
	})
}

func TestHostIsLoopback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr     string
		loopback bool
	}{
		{"127.0.0.1:2242", true},
		{"[::1]:2242", true},
		{"localhost:2242", true},
		{"0.0.0.0:2242", false},
		{"192.168.1.10:2242", false},
		{"caic.example.com:2242", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			if got := hostIsLoopback(hostOnly(tc.addr)); got != tc.loopback {
				t.Fatalf("hostIsLoopback(hostOnly(%q)) = %v, want %v", tc.addr, got, tc.loopback)
			}
		})
	}
}

// insertTestTask registers a task in the test server's manager and returns the
// entry. It registers the entry under the supplied path id as well as the
// task's own ID string, so handlers that re-resolve via the Manager by
// entry.Task().ID.String() find the same entry (in production the two always
// coincide because Insert keys on t.ID.String()).
func insertTestTask(t *testing.T, s *testRouter, id string, tk *task.Task) *tasks.Entry { //nolint:unparam // id is constant today; keep generic
	e := tasks.NewEntry(tk, nil)
	s.taskMgr.Insert(id, e)
	if taskID := tk.ID.String(); taskID != id {
		s.taskMgr.Insert(taskID, e)
	}
	return e
}

// registerTestRunner registers a runner under relPath in the task Manager's
// registry, the single owner of the runner set.
func registerTestRunner(s *testRouter, relPath string, r *task.Runner) {
	s.taskMgr.RegisterRunner(relPath, r)
}

// testEntries returns a snapshot of every registered task entry (test-only).
func testEntries(s *testRouter) []*tasks.Entry {
	var out []*tasks.Entry
	s.taskMgr.Range(func(_ string, e *tasks.Entry) bool {
		out = append(out, e)
		return true
	})
	return out
}

// loadPurgedTasksForTest reads logs from disk and registers purged tasks via
// the manager. Replaces the deleted Server.loadPurgedTasks helper.
func loadPurgedTasksForTest(s *testRouter, logDir string) error {
	logs, err := task.LoadLogs(logDir)
	if err != nil {
		return err
	}
	return s.taskMgr.LoadPurgedTasks(logs)
}

type runnerConstructionTestFixture struct {
	server   *testRouter
	logDir   string
	cacheDir string
	backend  *mdruntime.Backend
}

func newRunnerConstructionTestServer(t *testing.T, root string) runnerConstructionTestFixture {
	harnessEnv := map[string][]string{string(harness.Codex): {"CODEX_HOME=/tmp/codex"}}
	backend := &mdruntime.Backend{HarnessEnv: harnessEnv}
	backends := map[harness.Name]agent.Backend{harness.Codex: stubBackend{}}
	logDir := filepath.Join(t.TempDir(), "logs")
	cacheDir := filepath.Join(t.TempDir(), "cache")
	taskMgr := tasks.New(tasks.Config{
		ServerCtx:  t.Context(),
		LogDir:     logDir,
		CacheDir:   cacheDir,
		Backend:    backend,
		Backends:   backends,
		HarnessEnv: harnessEnv,
	})
	repoSvc := repos.NewService(root, logDir, cacheDir, harnessEnv, repos.NewRegistry(nil), taskMgr, backend, backends)
	s, err := New(t.Context(), Dependencies{
		Repos:          repoSvc,
		ProcessBackend: backend,
		TaskManager:    taskMgr,
		Forge:          forgemanager.New("", "", nil),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return runnerConstructionTestFixture{
		server:   &testRouter{Router: s, taskMgr: taskMgr, repos: repoSvc},
		logDir:   logDir,
		cacheDir: cacheDir,
		backend:  backend,
	}
}

func newTestRepoWatcher(t *testing.T, root string, s *testRouter) *repos.Watcher {
	return repos.NewWatcher(&repos.WatcherConfig{
		Ctx:          t.Context(),
		AbsRoot:      root,
		Repos:        func() []repos.Info { return testWatchedRepos(s.repos) },
		RelPath:      s.repos.RelPath,
		RunnerExists: s.repos.RunnerRegistered,
		OnDiscovered: func(ctx context.Context, abs string) {
			result, err := s.repos.DiscoverRunner(ctx, abs)
			if err != nil {
				t.Errorf("DiscoverRunner(%q): %v", abs, err)
				return
			}
			if s.repos.RunnerRegistered(result.Info.RelPath) {
				return
			}
			s.repos.RegisterRunner(&result)
		},
		OnRemoved: s.repos.DeregisterRunner,
	})
}

func testWatchedRepos(repoService *repos.Service) []repos.Info {
	snap := repoService.Snapshot()
	out := make([]repos.Info, len(snap))
	for i := range snap {
		out[i] = repos.Info{
			RelPath:    snap[i].RelPath,
			AbsPath:    snap[i].AbsPath,
			BaseBranch: snap[i].BaseBranch,
		}
	}
	return out
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
		fixture := newRunnerConstructionTestServer(t, root)
		s := fixture.server

		repo, err := s.serverHandlers.cloneRepo(t.Context(), &v1.CloneRepoReq{URL: source, Path: "./cloned"})
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
		if runner.LogDir != fixture.logDir || runner.CacheDir != fixture.cacheDir {
			t.Fatalf("runner dirs = log %q cache %q, want log %q cache %q", runner.LogDir, runner.CacheDir, fixture.logDir, fixture.cacheDir)
		}
		if runner.Runtime != fixture.backend {
			t.Fatal("runner instance backend was not wired")
		}
		if len(runner.HarnessEnv[string(harness.Codex)]) != 1 || runner.HarnessEnv[string(harness.Codex)][0] != "CODEX_HOME=/tmp/codex" {
			t.Fatalf("HarnessEnv = %#v, want configured codex env", runner.HarnessEnv)
		}
		if len(runner.Backends) == 0 {
			t.Fatal("runner backends were not initialized")
		}

		newTestRepoWatcher(t, root, s).SyncReposInDir(t.Context(), root)
		if got := s.repos.Snapshot(); len(got) != 1 || got[0].RelPath != "cloned" {
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
		s := newRunnerConstructionTestServer(t, root).server

		if _, err := s.serverHandlers.cloneRepo(t.Context(), &v1.CloneRepoReq{URL: parent, Path: "broken"}); err == nil {
			t.Fatal("cloneRepo succeeded, want submodule clone failure")
		}
		if _, err := os.Stat(filepath.Join(root, "broken")); !os.IsNotExist(err) {
			t.Fatalf("partial clone path still exists: %v", err)
		}
		if got := s.repos.Snapshot(); len(got) != 0 {
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
		s := newTestRouter(t)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/99/raw_events", http.NoBody)
		req.SetPathValue("id", "99")
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskRawEvents(w, req)
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
		s := newTestRouter(t)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/abc/raw_events", http.NoBody)
		req.SetPathValue("id", "abc")
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskRawEvents(w, req)
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
			s := newTestRouter(t)
			insertTestTask(t, s, "t1", &task.Task{InitialPrompt: agent.Prompt{Text: "test"}})

			body := strings.NewReader(tt.bodyJSON)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks/t1/input", body)
			req.SetPathValue("id", "t1")
			w := httptest.NewRecorder()
			handleWithTask(testTaskHandlers(s), testTaskHandlers(s).service.sendInput)(w, req)
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
	s := newTestRouter(t)
	tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
	tk.SetState(state)
	insertTestTask(t, s, "t1", tk)

	body := strings.NewReader(bodyJSON)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks/t1/restart", body)
	req.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	handleWithTask(testTaskHandlers(s), testTaskHandlers(s).service.restartTask)(w, req)
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
		s := newTestRouter(t)
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		// StatePending is the zero value, but set explicitly for clarity.
		insertTestTask(t, s, "t1", tk)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks/t1/purge", http.NoBody)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(testTaskHandlers(s), testTaskHandlers(s).service.purgeTask)(w, req)
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
		s := newTestRouter(t)
		registerTestRunner(s, "r", &task.Runner{BaseBranch: "main", Dir: t.TempDir()})
		insertTestTask(t, s, "t1", tk)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks/t1/purge", http.NoBody)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(testTaskHandlers(s), testTaskHandlers(s).service.purgeTask)(w, req)
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
		s := newTestRouter(t)
		registerTestRunner(s, "r", &task.Runner{BaseBranch: "main", Dir: t.TempDir()})
		insertTestTask(t, s, "t1", tk)

		// Use an already-cancelled context to simulate shutdown scenario
		// where BaseContext is cancelled before the handler completes.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks/t1/purge", http.NoBody)
		req = req.WithContext(ctx)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(testTaskHandlers(s), testTaskHandlers(s).service.purgeTask)(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})
}

func TestHandleCreateTask(t *testing.T) {
	t.Parallel()
	t.Run("ReturnsID", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[harness.Name]agent.Backend{harness.Claude: stubBackend{}},
		})
		handler := handle(testTaskHandlers(s).service.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test task"},"repos":[{"name":"myrepo"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp v1.Task
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.ID == 0 {
			t.Error("response has zero 'id' field")
		}
	})

	t.Run("MissingRepo", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		handler := handle(testTaskHandlers(s).service.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test task"}}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t)
		handler := handle(testTaskHandlers(s).service.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"nonexistent"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t)
		registerTestRunner(s, "myrepo", &task.Runner{BaseBranch: "main", Dir: t.TempDir()})
		handler := handle(testTaskHandlers(s).service.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"nonexistent"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t)
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[harness.Name]agent.Backend{"stub": stubBackend{}},
		})
		handler := handle(testTaskHandlers(s).service.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"stub","model":"nonexistent"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t)
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[harness.Name]agent.Backend{"stub": stubBackend{}},
		})
		handler := handle(testTaskHandlers(s).service.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"stub","model":"m1"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp v1.Task
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.ID == 0 {
			t.Error("response has zero 'id' field")
		}
	})

	t.Run("WithImage", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[harness.Name]agent.Backend{harness.Claude: stubBackend{}},
		})
		handler := handle(testTaskHandlers(s).service.createTask)

		// Set docker image in user preferences.
		if err := s.prefs.Update("default", func(p *preferences.Preferences) {
			p.Settings.BaseImage = "ghcr.io/my/image:v1"
			p.Settings.ContainerPlatform = "linux/amd64"
		}); err != nil {
			t.Fatal(err)
		}

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp v1.Task
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
		s := newTestRouter(t)
		registerTestRunner(s, "myrepo", &task.Runner{
			BaseBranch: "main",
			Dir:        t.TempDir(),
			Backends:   map[harness.Name]agent.Backend{harness.Claude: stubBackend{}},
		})
		if err := s.prefs.Update("default", func(p *preferences.Preferences) {
			p.Settings.WellKnownCaches = map[string]bool{"go-mod": false, "npm": true}
			p.Settings.CacheMappings = []preferences.CacheMapping{
				{HostPath: "/host/custom", ContainerPath: "/home/user/.custom", Enabled: true},
				{HostPath: "/host/disabled-cache", ContainerPath: "/home/user/.disabled-cache", Enabled: false},
			}
			p.Settings.CustomMounts = []preferences.MountMapping{
				{HostPath: "/host/work", ContainerPath: "/workspace/external", Enabled: true, ReadOnly: true},
				{HostPath: "/host/disabled-work", ContainerPath: "/workspace/disabled", Enabled: false},
			}
		}); err != nil {
			t.Fatal(err)
		}

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
		w := httptest.NewRecorder()
		handle(testTaskHandlers(s).service.createTask)(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp v1.Task
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		entry, _ := s.taskMgr.GetEntry(resp.ID.String())
		if entry == nil {
			t.Fatal("task not found")
		}
		var gotCustom, gotNPM, gotGoMod bool
		for _, cm := range entry.Task().CacheMounts {
			switch cm.Name {
			case "custom-cache-0":
				gotCustom = cm.HostPath == "/host/custom" && cm.MountPath == "/home/user/.custom"
			case "npm":
				gotNPM = true
			case "go-mod":
				gotGoMod = true
			case "custom-mount-0":
				t.Errorf("custom mount was stored as a cache: %+v", cm)
			}
		}
		gotCustomMount := false
		for _, m := range entry.Task().Mounts {
			if m.HostPath == "/host/work" && m.MountPath == "/workspace/external" && m.ReadOnly {
				gotCustomMount = true
			}
			if m.HostPath == "/host/disabled-work" || m.MountPath == "/workspace/disabled" {
				t.Errorf("disabled custom mount present: %+v", m)
			}
		}
		if !gotCustom {
			t.Errorf("custom cache mapping missing from %+v", entry.Task().CacheMounts)
		}
		if !gotCustomMount {
			t.Errorf("custom mount missing from %+v", entry.Task().CacheMounts)
		}
		for _, cm := range entry.Task().CacheMounts {
			if cm.HostPath == "/host/disabled-cache" || cm.MountPath == "/home/user/.disabled-cache" {
				t.Errorf("disabled custom cache present: %+v", cm)
			}
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
		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{
			Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}},
		})
		handler := handle(testTaskHandlers(s).service.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"no repo task"},"harness":"claude","model":"m1","effort":"high"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp v1.Task
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.ID == 0 {
			t.Error("response has zero 'id' field")
		}
		prefs := s.prefs.Get("default")
		if prefs.Harness != "claude" {
			t.Errorf("preferences harness = %q, want claude", prefs.Harness)
		}
		if prefs.Models["claude"] != "m1" {
			t.Errorf("preferences models[claude] = %q, want m1", prefs.Models["claude"])
		}
		if prefs.Efforts["claude"]["m1"] != "high" {
			t.Errorf("preferences efforts[claude][m1] = %q, want high", prefs.Efforts["claude"]["m1"])
		}
	})

	t.Run("NoRepoRunnerNoBackend", func(t *testing.T) {
		t.Parallel()
		// Creating a no-repo task with no registered harness backends returns
		// a clear 400 instead of panicking.
		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{}})
		handler := handle(testTaskHandlers(s).service.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"no repo task"},"harness":"claude"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("UnknownField", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		handler := handle(testTaskHandlers(s).service.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repo":"r","harness":"claude","bogus":true}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t)
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []task.RepoMount{{Name: "r"}}}
		tk.SetRuntimeConnectionInfo("ctr", runtime.ConnectionTarget{SSHHost: "ctr"}, "", "", 0)
		insertTestTask(t, s, "t1", tk)
		backend := &tasktest.FakeRuntimeBackend{
			SignalFunc: func(context.Context, runtime.InstanceID, int, string) error {
				t.Fatal("Signal should not be called")
				return nil
			},
		}
		processes := &runtimeProcessHandlers{
			taskMgr:     s.taskMgr,
			backend:     backend,
			authEnabled: false,
		}

		body := strings.NewReader(`{"signal":"SIGTERM","extra":true}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/processes/t1/123/signal", body)
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
		s := newTestRouter(t)
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []task.RepoMount{{Name: "r"}}}
		tk.SetRuntimeConnectionInfo("ctr", runtime.ConnectionTarget{SSHHost: "ctr"}, "", "", 0)
		insertTestTask(t, s, "t1", tk)
		var gotPID int
		var gotSignal string
		backend := &tasktest.FakeRuntimeBackend{
			SignalFunc: func(_ context.Context, id runtime.InstanceID, pid int, sig string) error {
				if id != "ctr" {
					t.Errorf("instance = %q, want ctr", id)
				}
				gotPID = pid
				gotSignal = sig
				return nil
			},
		}
		processes := &runtimeProcessHandlers{
			taskMgr:     s.taskMgr,
			backend:     backend,
			authEnabled: false,
		}

		body := strings.NewReader(`{"signal":"SIGKILL"}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/processes/t1/123/signal", body)
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
	s := newTestRouter(t)
	s.repos = repos.NewService("", "", "", nil, repos.NewRegistry([]repos.Info{
		{RelPath: "org/repoA", AbsPath: "/src/org/repoA", BaseBranch: "main"},
		{RelPath: "repoB", AbsPath: "/src/repoB", BaseBranch: "develop"},
	}), s.taskMgr, nil, nil)
	s.serverHandlers.repos = s.repos

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/server/repos", http.NoBody)
	w := httptest.NewRecorder()
	handle(s.serverHandlers.listRepos)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var repoList []v1.Repo
	if err := json.NewDecoder(w.Body).Decode(&repoList); err != nil {
		t.Fatal(err)
	}
	if len(repoList) != 2 {
		t.Fatalf("len = %d, want 2", len(repoList))
	}
	if repoList[0].Path != "org/repoA" {
		t.Errorf("repos[0].Path = %q, want %q", repoList[0].Path, "org/repoA")
	}
	if repoList[1].BaseBranch.Name != "develop" {
		t.Errorf("repos[1].BaseBranch = %q, want %q", repoList[1].BaseBranch.Name, "develop")
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
				MessageType: "caic_meta", Version: 1, Prompt: fmt.Sprintf("task %d", i), Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-" + strings.Repeat("0", i+1)}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
			})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: state, CostUSD: float64(i + 1)})
			writeLogFile(t, logDir, fmt.Sprintf("%d.jsonl", i), meta, trailer)
		}

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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
			MessageType: "caic_meta", Version: 1, Prompt: "recent task", Harness: harness.Claude, StartedAt: time.Now().Add(-1 * time.Hour),
		})
		trailer0 := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, logDir, "recent.jsonl", meta0, trailer0)

		// task 1: old (purged, > 14 days)
		meta1 := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "old task", Harness: harness.Claude, StartedAt: time.Now().Add(-20 * 24 * time.Hour),
		})
		trailer1 := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		oldPath := filepath.Join(logDir, "old.jsonl")
		writeLogFile(t, logDir, "old.jsonl", meta1, trailer1)
		// Set mtime to 15 days ago.
		oldTime := time.Now().Add(-15 * 24 * time.Hour)
		if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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
				Repos: []agent.MetaRepo{{Name: "a", Branch: fmt.Sprintf("caic-%d", i)}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
			})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeLogFile(t, logDir, fmt.Sprintf("a-%d.jsonl", i), meta, trailer)
		}
		for i := range 3 {
			meta := mustJSON(t, agent.MetaMessage{
				MessageType: "caic_meta", Version: 1, Prompt: fmt.Sprintf("b-%d", i),
				Repos: []agent.MetaRepo{{Name: "b", Branch: fmt.Sprintf("caic-%d", i)}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, i+10, 0, 0, 0, time.UTC),
			})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeLogFile(t, logDir, fmt.Sprintf("b-%d.jsonl", i), meta, trailer)
		}

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		for _, e := range entries {
			j := v1conv.Task(t.Context(), e, testTaskHandlers(s).service.taskResolvers())
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
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		for _, e := range entries {
			j := v1conv.Task(t.Context(), e, testTaskHandlers(s).service.taskResolvers())
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
			Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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
			Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC),
			Title: "Skip Docker Rebuilds",
		})
		trailerB := mustJSON(t, agent.MetaResultMessage{
			MessageType: "caic_result", State: "purged",
			Title: "Skip Unnecessary Docker Image Rebuilds",
		})
		writeLogFile(t, logDir, "b.jsonl", metaB, trailerB)

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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
		logDir := t.TempDir()
		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude,
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

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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
				Harness:   harness.Claude,
				StartedAt: stoppedAt,
			})
			trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			name := fmt.Sprintf("%02d.jsonl", i)
			writeLogFile(t, logDir, name, meta, trailer)
			if err := os.Chtimes(filepath.Join(logDir, name), stoppedAt, stoppedAt); err != nil {
				t.Fatal(err)
			}
		}

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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
				MessageType: "caic_meta", Version: 1, Prompt: prompt, Harness: harness.Claude,
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

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Tailscale: true, USB: true, Display: true,
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, logDir, "feat.jsonl", meta, trailer)

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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

func TestHandleTaskRawEvents(t *testing.T) {
	t.Parallel()
	t.Run("PurgedTaskEvents", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()

		// Write a purged task log with real agent messages.
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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

		// Subscribe to events via SSE. The handler should return after history
		// replay instead of waiting for the request context deadline.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		t.Cleanup(cancel)
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID)
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskRawEvents(w, req)
		if err := ctx.Err(); err != nil {
			t.Fatalf("handleTaskRawEvents blocked until context deadline: %v", err)
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

	t.Run("AdoptedRunningTaskEventsUseInMemoryHistory", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		taskID := ksid.NewID()

		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		diskMsg := mustJSON(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"model":   "claude-opus-4-6",
				"content": []map[string]any{{"type": "text", "text": "slow disk replay should not be used"}},
			},
		})
		writeLogFile(t, logDir, taskID.String()+".jsonl", meta, diskMsg)
		logs, err := task.LoadLogs(logDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) != 1 {
			t.Fatalf("logs len = %d, want 1", len(logs))
		}
		logs[0].SetParser(claudecode.New().NewWire().ParseMessage)

		tk := &task.Task{ID: taskID, InitialPrompt: agent.Prompt{Text: "fix the bug"}, Harness: harness.Claude}
		tk.RestoreMessages([]agent.Message{&agent.TextMessage{Text: "fast in-memory history"}})
		tk.SetState(task.StateRunning)

		s := newTestRouter(t)
		s.taskMgr.Insert(taskID.String(), tasks.NewEntry(tk, logs[0]))

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID.String())
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskRawEvents(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		body := w.Body.String()
		if !strings.Contains(body, "fast in-memory history") {
			t.Fatalf("expected in-memory history in SSE body:\n%s", body)
		}
		if strings.Contains(body, "slow disk replay should not be used") {
			t.Fatalf("disk history leaked into running task SSE body:\n%s", body)
		}
	})

	t.Run("AdoptedStoppedTaskEventsUseFullDiskLog", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		taskID := ksid.NewID()

		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		initMsg := mustJSON(t, map[string]any{
			"type": "system", "subtype": "init", "model": "claude-opus-4-6",
			"claude_code_version": "2.0", "session_id": "s1",
		})
		early := mustJSON(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"model":   "claude-opus-4-6",
				"content": []map[string]any{{"type": "text", "text": "early full log message"}},
			},
		})
		result := mustJSON(t, agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"})
		staleExit := `{"type":"caic_exit","exit_code":2,"error":"stale crash"}`
		writeLogFile(t, logDir, taskID.String()+".jsonl", meta, initMsg, early, result, staleExit)
		logs, err := task.LoadLogs(logDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) != 1 {
			t.Fatalf("logs len = %d, want 1", len(logs))
		}
		logs[0].SetParser(claudecode.New().NewWire().ParseMessage)

		tk := &task.Task{
			ID:            taskID,
			InitialPrompt: agent.Prompt{Text: "fix the bug"},
			Repos:         []task.RepoMount{{Name: "r", Branch: "caic-0"}},
			Harness:       harness.Claude,
		}
		tk.RestoreMessages([]agent.Message{&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"}})
		tk.SetState(task.StateStopped)

		s := newTestRouter(t)
		s.taskMgr.Insert(taskID.String(), tasks.NewEntry(tk, logs[0]))

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID.String())
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskRawEvents(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		body := w.Body.String()
		if !strings.Contains(body, "early full log message") {
			t.Fatalf("expected full disk history in SSE body:\n%s", body)
		}
		if strings.Contains(body, "stale crash") {
			t.Fatalf("stale relay exit leaked into SSE body:\n%s", body)
		}
	})

	t.Run("StreamEventTextDelta", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()

		// Write a purged task log with stream events (text deltas) followed
		// by the final assistant message, simulating --include-partial-messages output.
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "explain streaming",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
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

		s := newTestRouter(t)
		registerTestRunner(s, "", &task.Runner{Backends: map[harness.Name]agent.Backend{harness.Claude: stubBackend{}}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
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
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody).WithContext(ctx)
		req.SetPathValue("id", taskID)
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskRawEvents(w, req)

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

func TestVoiceGatewayMetadata(t *testing.T) {
	t.Parallel()
	t.Run("default disabled", func(t *testing.T) {
		t.Parallel()
		h := &voiceHandlers{}
		got := h.metadata()
		if got.Mode != v1.VoiceGatewayModeDisabled {
			t.Fatalf("Mode = %q, want disabled", got.Mode)
		}
	})

	t.Run("external", func(t *testing.T) {
		t.Parallel()
		h := &voiceHandlers{
			gateway: VoiceGatewayConfig{
				Mode: VoiceGatewayModeExternal,
				URL:  "https://voice.example.com",
			},
		}
		got := h.metadata()
		if got.Mode != v1.VoiceGatewayModeExternal {
			t.Fatalf("Mode = %q, want external", got.Mode)
		}
		if got.URL != "https://voice.example.com" {
			t.Fatalf("URL = %q, want https://voice.example.com", got.URL)
		}
		if !got.AuthRequired {
			t.Fatal("AuthRequired = false, want true")
		}
	})

	t.Run("embedded", func(t *testing.T) {
		t.Parallel()
		h := &voiceHandlers{
			bridge:  &voicertc.Bridge{},
			gateway: VoiceGatewayConfig{Mode: VoiceGatewayModeEmbedded},
		}
		got := h.metadata()
		if got.Mode != v1.VoiceGatewayModeEmbedded {
			t.Fatalf("Mode = %q, want embedded", got.Mode)
		}
		if got.AuthRequired {
			t.Fatal("AuthRequired = true, want false")
		}
	})
}

func TestGoModeSettings(t *testing.T) {
	t.Parallel()
	voice := v1.VoiceGatewayMetadata{
		Mode:         v1.VoiceGatewayModeExternal,
		URL:          "https://voice.example.com",
		AuthRequired: true,
		Capabilities: []string{"voice.gatewayGeminiLive"},
	}
	got := newGoModeSettings(voice, true)
	if got.Service != "caic" {
		t.Fatalf("Service = %q, want caic", got.Service)
	}
	if got.APIVersion != 1 {
		t.Fatalf("APIVersion = %d, want 1", got.APIVersion)
	}
	if got.WebShell.BridgeVersion != 1 {
		t.Fatalf("WebShell = %+v", got.WebShell)
	}
	if len(got.WebShell.ToolGroups) != 1 {
		t.Fatalf("ToolGroups = %+v, want 1", got.WebShell.ToolGroups)
	}
	if grp := got.WebShell.ToolGroups[0]; grp.Name != "tasks" || grp.Endpoint != "/api/caic/v1/mcp" || grp.ProtocolVersion != mcp.ProtocolVersion || !grp.AuthRequired {
		t.Fatalf("ToolGroup = %+v", got.WebShell.ToolGroups[0])
	}
	if got.WebShell.VoiceGateway.Required {
		t.Fatal("VoiceGateway.Required = true, want false")
	}
	if got.WebShell.VoiceGateway.URL != "https://voice.example.com" || !got.WebShell.VoiceGateway.AuthRequired {
		t.Fatalf("VoiceGateway = %+v", got.WebShell.VoiceGateway)
	}

	embedded := newGoModeSettings(v1.VoiceGatewayMetadata{Mode: v1.VoiceGatewayModeEmbedded}, false)
	if embedded.WebShell.VoiceGateway.URL != "/" || embedded.WebShell.VoiceGateway.AuthRequired {
		t.Fatalf("Embedded VoiceGateway = %+v", embedded.WebShell.VoiceGateway)
	}
}

func TestBuildHandler(t *testing.T) {
	t.Parallel()
	t.Run("unknown API path returns 404 not SPA", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/does/not/exist", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		if strings.Contains(w.Body.String(), "<html") {
			t.Fatalf("body = %q, want 404 not SPA HTML", w.Body.String())
		}
	})

	t.Run("blocked origin error includes IP and category", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		checker, err := ipgeo.NewChecker(t.Context(), "tailscale", "", "")
		if err != nil {
			t.Fatalf("ipgeo.NewChecker: %v", err)
		}
		s.ipgeoChecker = checker
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/server/config", http.NoBody)
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
		if got, want := strings.TrimSpace(w.Body.String()), "forbidden: origin 127.0.0.1 (local) not allowed"; got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
	})

	t.Run("MCP accepts caic bearer token", func(t *testing.T) {
		t.Parallel()
		_, h, user, registered := newMCPOAuthLifecycleRouter(t)
		tokenResp := authorizeMCPClient(t, h, &user, &registered)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/call", `"name":"tasks_list","arguments":{}`)))
		req.Host = "caic.example.com"
		req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "tasks_list")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp mcp.JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Error != nil {
			t.Fatalf("JSON-RPC error = %+v", resp.Error)
		}
	})

	t.Run("MCP rejects caic session JWT in authorization header", func(t *testing.T) {
		t.Parallel()
		usersPath := filepath.Join(t.TempDir(), "users.json")
		store, err := auth.Open(usersPath)
		if err != nil {
			t.Fatalf("open auth store: %v", err)
		}
		user, err := store.UpsertUser(&auth.User{Provider: forge.KindGitHub, ProviderID: "1", Username: "alice", AccessToken: "forge-token"})
		if err != nil {
			t.Fatalf("upsert user: %v", err)
		}
		s := newTestOAuthRouter(t, store)
		secret := s.sessionSecret
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
		sessionJWT, err := auth.IssueToken(&user, secret, sessionTTL)
		if err != nil {
			t.Fatalf("issue session token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/call", `"name":"tasks_list","arguments":{}`)))
		req.Host = "caic.example.com"
		req.Header.Set("Authorization", "Bearer "+sessionJWT)
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "tasks_list")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("MCP rejects invalid origin before handler", func(t *testing.T) {
		t.Parallel()
		_, h, user, registered := newMCPOAuthLifecycleRouter(t)
		tokenResp := authorizeMCPClient(t, h, &user, &registered)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader("not-json"))
		req.Host = "caic.example.com"
		req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
		req.Header.Set("Origin", "https://evil.example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
		}
	})

	t.Run("MCP tool scope policy covers every advertised tool", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		specs, err := registry.specs(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range specs {
			if _, ok := mcpToolScopes[spec.Name]; !ok {
				t.Fatalf("tool %q has no MCP scope policy", spec.Name)
			}
		}
	})

	t.Run("MCP scope denial includes auth metadata and audit", func(t *testing.T) {
		t.Parallel()
		s, h, user, registered := newMCPOAuthLifecycleRouter(t)
		tokenResp := authorizeMCPClient(t, h, &user, &registered)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/call", `"name":"task_create","arguments":{"prompt":"OPENAI_API_KEY=sk-secret","repos":["repo"]}`)))
		req.Host = "caic.example.com"
		req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "task_create")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var raw map[string]any
		if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		result, ok := raw["result"].(map[string]any)
		if !ok || result["isError"] != true || result["_meta"] == nil {
			t.Fatalf("result = %#v", raw["result"])
		}
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		events := registry.audit.snapshot()
		if len(events) == 0 || events[len(events)-1].Decision == "allow" {
			t.Fatalf("audit events = %+v", events)
		}
	})

	t.Run("MCP rate limiter rejects excess requests", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		s.hostState = auth.NewHostState("https://caic.example.com")
		s.mcpHandlers.rateLimiter = newRateLimiter(1, time.Minute)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
		for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/list", `{}`)))
			req.Host = "caic.example.com"
			req.RemoteAddr = fmt.Sprintf("192.0.2.55:%d", 1234+i)
			req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
			req.Header.Set("Mcp-Method", "tools/list")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != want {
				t.Fatalf("request %d status = %d, want %d", i+1, w.Code, want)
			}
		}
	})

	t.Run("MCP redacts secrets", func(t *testing.T) {
		t.Parallel()
		secretURL := "https://user:" + "pass@example.com/repo.git"
		got := redactForJSON(map[string]any{"accessToken": "ghp_" + "secret", "url": secretURL, "text": "OPENAI_API_KEY=" + "sk-secret", "log": "clone " + secretURL})
		data, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, secret := range []string{"ghp_secret", "pass", "sk-secret"} {
			if strings.Contains(text, secret) {
				t.Fatalf("redacted JSON still contains %q: %s", secret, text)
			}
		}
	})

	t.Run("go mode settings are public", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		secret := make([]byte, 32)
		s.sessionSecret = secret
		usersPath := filepath.Join(t.TempDir(), "users.json")
		store, err := auth.Open(usersPath)
		if err != nil {
			t.Fatalf("open auth store: %v", err)
		}
		s.authStore = store
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/gomode.json", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp gomode.Settings
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Service != "caic" || resp.APIVersion != 1 {
			t.Fatalf("settings = %+v", resp)
		}
	})

	t.Run("auth disabled", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		if _, err := s.buildHandler(); err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
	})

	t.Run("auth enabled", func(t *testing.T) {
		t.Parallel()
		// Regression: adding /auth/ (unqualified) alongside GET / (qualified)
		// caused a pattern conflict panic in Go 1.22+ ServeMux.
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
		s := newTestRouter(t)
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
	s := newTestRouter(t)
	s.sessionSecret = secret
	s.authStore = store
	s.hostState = host
	githubCfg := oauthclient.NewGitHubConfig("cid", "csec", "http://localhost/api/caic/v1/auth/github/callback")
	githubCfg.TokenURL = tokenServer.URL
	githubCfg.UserInfoURL = userServer.URL
	githubCfg.Scopes = []string{"repo"}
	githubOAuth := &githubCfg
	s.authHandlers.store = s.authStore
	s.authHandlers.sessionSecret = s.sessionSecret
	s.authHandlers.hostState = s.hostState
	s.authHandlers.githubOAuth = githubOAuth

	t.Run("valid state round-trip succeeds", func(t *testing.T) {
		t.Parallel()
		// Simulate the start handler to get a valid state cookie.
		startW := httptest.NewRecorder()
		startReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/github/start", http.NoBody)
		s.authHandlers.handleStart("github")(startW, startReq)
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
			"/auth/github/callback?code=testcode&state="+url.QueryEscape(rawState), http.NoBody)
		cbReq.AddCookie(stateCookie)
		cbW := httptest.NewRecorder()
		s.authHandlers.handleCallback("github")(cbW, cbReq)

		if cbW.Code != http.StatusFound {
			body, _ := io.ReadAll(cbW.Result().Body)
			t.Fatalf("callback status = %d, want %d; body = %s", cbW.Code, http.StatusFound, body)
		}
	})

	t.Run("rejects cross-origin web next", func(t *testing.T) {
		t.Parallel()
		startW := httptest.NewRecorder()
		startReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/github/start?next="+url.QueryEscape("https://evil.example/callback"), http.NoBody)
		s.authHandlers.handleStart("github")(startW, startReq)
		if startW.Code != http.StatusBadRequest {
			t.Fatalf("start status = %d, want %d", startW.Code, http.StatusBadRequest)
		}
	})

	t.Run("rejects backslash web next", func(t *testing.T) {
		t.Parallel()
		for _, next := range []string{`/\evil.example/callback`, `/oauth/authorize\evil`} {
			startW := httptest.NewRecorder()
			startReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/github/start?next="+url.QueryEscape(next), http.NoBody)
			s.authHandlers.handleStart("github")(startW, startReq)
			if startW.Code != http.StatusBadRequest {
				t.Fatalf("next %q start status = %d, want %d", next, startW.Code, http.StatusBadRequest)
			}
		}
	})

	t.Run("web next state redirects back after login", func(t *testing.T) {
		t.Parallel()
		next := "/oauth/authorize?client_id=caic_test&state=client-state"
		startW := httptest.NewRecorder()
		startReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/github/start?next="+url.QueryEscape(next), http.NoBody)
		s.authHandlers.handleStart("github")(startW, startReq)
		if startW.Code != http.StatusFound {
			t.Fatalf("start status = %d, want %d", startW.Code, http.StatusFound)
		}
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

		cbReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			"/auth/github/callback?code=testcode&state="+url.QueryEscape(rawState), http.NoBody)
		cbReq.AddCookie(stateCookie)
		cbW := httptest.NewRecorder()
		s.authHandlers.handleCallback("github")(cbW, cbReq)
		if cbW.Code != http.StatusFound {
			body, _ := io.ReadAll(cbW.Result().Body)
			t.Fatalf("callback status = %d, want %d; body = %s", cbW.Code, http.StatusFound, body)
		}
		if cbW.Header().Get("Location") != next {
			t.Fatalf("Location = %q, want %q", cbW.Header().Get("Location"), next)
		}
	})
}

func TestWebFetchHandlers(t *testing.T) {
	t.Parallel()
	h := &webFetchHandlers{}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><head><title>Example</title><script>ignore()</script></head><body><nav>skip</nav><main>Hello <strong>Marc-Antoine</strong></main></body></html>`))
		}))
		t.Cleanup(upstream.Close)

		resp, err := h.webFetch(t.Context(), &v1.WebFetchReq{URL: upstream.URL})
		if err != nil {
			t.Fatalf("webFetch: %v", err)
		}
		if resp.Title != "Example" {
			t.Fatalf("Title = %q, want %q", resp.Title, "Example")
		}
		if !strings.Contains(resp.Content, "Hello Marc-Antoine") {
			t.Fatalf("Content = %q, want fetched body text", resp.Content)
		}
		if strings.Contains(resp.Content, "ignore") || strings.Contains(resp.Content, "skip") {
			t.Fatalf("Content = %q, want script and nav text omitted", resp.Content)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		t.Cleanup(upstream.Close)

		_, err := h.webFetch(t.Context(), &v1.WebFetchReq{URL: upstream.URL})
		if err == nil {
			t.Fatal("webFetch returned nil error for non-200 response")
		}
		if !strings.Contains(err.Error(), "HTTP 503") {
			t.Fatalf("error = %v, want HTTP 503", err)
		}
	})
}

func TestPrefsPerUser(t *testing.T) {
	t.Parallel()
	t.Run("separate users get separate preferences", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)

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
		s := newTestRouter(t)
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
