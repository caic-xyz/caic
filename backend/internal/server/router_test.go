// Tests for the HTTP server request handling and routing.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/gomode"
	"github.com/caic-xyz/caic/gomode/voicegateway/voicertc"
	"github.com/caic-xyz/caic/oauth/oauthclient"
)

type reviveDuringStoppedScanWriter struct {
	*httptest.ResponseRecorder

	revived bool
	revive  func()
}

func (w *reviveDuringStoppedScanWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	if !w.revived && bytes.Contains(data, []byte("event: message")) {
		w.revived = true
		w.revive()
	}
	return n, err
}

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
func newRouterTestCheckout(dir string) *repo.Checkout {
	return &repo.Checkout{
		BaseBranch: "main",
		Dir:        dir,
		RelPath:    filepath.Base(dir),
		GitTimeout: time.Minute,
	}
}

func registerRouterCheckout(t testing.TB, registry *repo.Registry, relPath string, checkout *repo.Checkout) {
	if checkout.Repository != nil {
		checkout.Repository = registry.RegisterRepository(*checkout.Repository)
	}
	checkout.RelPath = relPath
	if err := registry.RegisterCheckout(checkout); err != nil {
		t.Fatal(err)
	}
}

type testRouter struct {
	*Router

	taskMgr               *taskmgr.Manager
	checkouts             *repo.Registry
	repoStatus            *ci.RepoStatusStore
	prefs                 *preferences.Store
	forgeMgr              *forgemgr.Manager
	oauthRefreshTokenPath string
}

type testRuntimeSystem struct {
	testRuntimeBackend
	runtimetest.FakeInfo
}

type testRuntimeBackend interface {
	runtime.Lifecycle
	runtime.Repository
}

func (*testRuntimeSystem) Name() runtime.Name { return "test-runtime" }

// newTestRouter creates a Router for tests. Tests commonly mutate auth/host
// fields (authStore, sessionSecret, hostState) on the returned Router and then
// call buildHandler; buildHandler re-syncs hostState into the MCP concern, and
// the authHandlers copies must be synced by the test if exercised.
func newTestRuntime(t testing.TB, backend testRuntimeBackend) *runtime.Router {
	router, err := runtime.NewRouter(testLogger(), []runtime.System{&testRuntimeSystem{testRuntimeBackend: backend}})
	if err != nil {
		t.Fatalf("runtime.NewRouter: %v", err)
	}
	return router
}

func newTestTaskManager(t testing.TB, cfg taskmgr.Config) *taskmgr.Manager { //nolint:gocritic // Config mirrors New's value bag in tests.
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.LogStore == nil {
		cfg.LogStore = taskslog.NewStore(testLogger(), t.TempDir())
	}
	if cfg.RuntimeStartTimeout == 0 {
		cfg.RuntimeStartTimeout = time.Hour
	}
	m, err := taskmgr.New(cfg)
	if err != nil {
		t.Fatalf("taskmgr.New: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newTestRouter(t testing.TB, backends map[harness.Name]agent.Backend) *testRouter {
	checker, err := ipgeo.NewChecker(t.Context(), testLogger(), "0.0.0.0/0,::/0", "", "")
	if err != nil {
		t.Fatalf("ipgeo.NewChecker: %v", err)
	}
	backend := &runtimetest.FakeBackend{}
	runtimeRouter := newTestRuntime(t, backend)
	checkoutRegistry := repo.NewRegistry()
	taskMgr := newTestTaskManager(t, taskmgr.Config{ServerCtx: t.Context(), Runtimes: runtimeRouter, Backends: backends, Checkouts: checkoutRegistry})
	repoStatus := ci.NewRepoStatusStore()
	prefs := newTestPrefs(t)
	forgeManager := forgemgr.New(testLogger(), "", "", nil, forgemgr.NoOAuthTokenSource())
	s, err := New(t.Context(), testLogger(), Dependencies{
		Checkouts:    checkoutRegistry,
		RepoStatus:   repoStatus,
		Runtimes:     runtimeRouter,
		TaskMgr:      taskMgr,
		Preferences:  prefs,
		IPGeoChecker: checker,
		ForgeMgr:     forgeManager,
		Warnings:     NewWarningStore(taskMgr),
		CacheSizes:   NewCacheSizeStore(testLogger()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testRouter{Router: s, taskMgr: taskMgr, checkouts: checkoutRegistry, repoStatus: repoStatus, prefs: prefs, forgeMgr: forgeManager}
}

// newTestRouterWithAuthHost creates a Router with an auth store, suitable for
// OAuth tests that need s.oauthServer to be non-nil at construction time.
// If refreshTokenPath is non-empty, it is used as the OAuth refresh token store.
func newTestRouterWithAuthHost(t testing.TB, authStore *auth.Store, refreshTokenPath string, hostState *auth.HostState) *testRouter {
	checker, err := ipgeo.NewChecker(t.Context(), testLogger(), "0.0.0.0/0,::/0", "", "")
	if err != nil {
		t.Fatalf("ipgeo.NewChecker: %v", err)
	}
	backend := &runtimetest.FakeBackend{}
	runtimeRouter := newTestRuntime(t, backend)
	checkoutRegistry := repo.NewRegistry()
	taskMgr := newTestTaskManager(t, taskmgr.Config{ServerCtx: t.Context(), Runtimes: runtimeRouter, Checkouts: checkoutRegistry})
	repoStatus := ci.NewRepoStatusStore()
	prefs := newTestPrefs(t)
	forgeManager := forgemgr.New(testLogger(), "", "", nil, forgemgr.NoOAuthTokenSource())
	s, err := New(t.Context(), testLogger(), Dependencies{
		Checkouts:                  checkoutRegistry,
		RepoStatus:                 repoStatus,
		Runtimes:                   runtimeRouter,
		TaskMgr:                    taskMgr,
		Preferences:                prefs,
		IPGeoChecker:               checker,
		ForgeMgr:                   forgeManager,
		Warnings:                   NewWarningStore(taskMgr),
		CacheSizes:                 NewCacheSizeStore(testLogger()),
		AuthStore:                  authStore,
		OAuthPrivateKeyPEM:         testMCPOAuthSigningKeyPEM(t),
		OAuthIssuer:                "https://caic.example.com",
		OAuthRefreshTokenStorePath: refreshTokenPath,
		HostState:                  hostState,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testRouter{Router: s, taskMgr: taskMgr, checkouts: checkoutRegistry, repoStatus: repoStatus, prefs: prefs, forgeMgr: forgeManager, oauthRefreshTokenPath: refreshTokenPath}
}

// newTestOAuthRouter creates a Router configured with OAuth host state and a
// session secret, suitable for tests that hit OAuth endpoints or issue tokens.
func newTestOAuthRouter(t testing.TB, authStore *auth.Store) *testRouter {
	s := newTestRouterWithAuthHost(t, authStore, "", auth.NewHostState("https://caic.example.com", nil))
	s.sessionSecret = []byte("0123456789abcdef0123456789abcdef")
	return s
}

func testTaskHandlers(s *testRouter) *taskHandlers {
	return s.taskHandlers
}

func TestNew(t *testing.T) {
	t.Parallel()
	validDependencies := func(t *testing.T) Dependencies {
		runtimeRouter := newTestRuntime(t, &runtimetest.FakeBackend{})
		checkoutRegistry := repo.NewRegistry()
		taskMgr := newTestTaskManager(t, taskmgr.Config{ServerCtx: t.Context(), Runtimes: runtimeRouter, Checkouts: checkoutRegistry})
		return Dependencies{
			Checkouts:   checkoutRegistry,
			RepoStatus:  ci.NewRepoStatusStore(),
			Runtimes:    runtimeRouter,
			TaskMgr:     taskMgr,
			Preferences: newTestPrefs(t),
			ForgeMgr:    forgemgr.New(testLogger(), "", "", nil, forgemgr.NoOAuthTokenSource()),
			Warnings:    NewWarningStore(taskMgr),
			CacheSizes:  NewCacheSizeStore(testLogger()),
		}
	}

	t.Run("missing runtime", func(t *testing.T) {
		t.Parallel()
		if _, err := New(t.Context(), testLogger(), Dependencies{}); err == nil {
			t.Fatal("New() error = nil, want runtime required")
		}
	})

	t.Run("missing repos", func(t *testing.T) {
		t.Parallel()
		runtimeRouter := newTestRuntime(t, &runtimetest.FakeBackend{})
		checkoutRegistry := repo.NewRegistry()
		_, err := New(t.Context(), testLogger(), Dependencies{
			Runtimes:    runtimeRouter,
			TaskMgr:     newTestTaskManager(t, taskmgr.Config{ServerCtx: t.Context(), Runtimes: runtimeRouter, Checkouts: checkoutRegistry}),
			Preferences: newTestPrefs(t),
			RepoStatus:  ci.NewRepoStatusStore(),
		})
		if err == nil || err.Error() != "checkout registry is required" {
			t.Fatalf("New() error = %v, want checkout registry required", err)
		}
	})

	t.Run("missing repo status", func(t *testing.T) {
		t.Parallel()
		runtimeRouter := newTestRuntime(t, &runtimetest.FakeBackend{})
		checkoutRegistry := repo.NewRegistry()
		_, err := New(t.Context(), testLogger(), Dependencies{
			Checkouts:   checkoutRegistry,
			Runtimes:    runtimeRouter,
			TaskMgr:     newTestTaskManager(t, taskmgr.Config{ServerCtx: t.Context(), Runtimes: runtimeRouter, Checkouts: checkoutRegistry}),
			Preferences: newTestPrefs(t),
		})
		if err == nil || err.Error() != "repo status store is required" {
			t.Fatalf("New() error = %v, want repo status store required", err)
		}
	})

	t.Run("missing forge manager", func(t *testing.T) {
		t.Parallel()
		runtimeRouter := newTestRuntime(t, &runtimetest.FakeBackend{})
		checkoutRegistry := repo.NewRegistry()
		_, err := New(t.Context(), testLogger(), Dependencies{
			Checkouts:   checkoutRegistry,
			RepoStatus:  ci.NewRepoStatusStore(),
			Runtimes:    runtimeRouter,
			TaskMgr:     newTestTaskManager(t, taskmgr.Config{ServerCtx: t.Context(), Runtimes: runtimeRouter, Checkouts: checkoutRegistry}),
			Preferences: newTestPrefs(t),
		})
		if err == nil || err.Error() != "forge manager is required" {
			t.Fatalf("New() error = %v, want forge manager required", err)
		}
	})

	t.Run("missing warning store", func(t *testing.T) {
		t.Parallel()
		d := validDependencies(t)
		d.Warnings = nil
		if _, err := New(t.Context(), testLogger(), d); err == nil || err.Error() != "warning store is required" {
			t.Fatalf("New() error = %v, want warning store required", err)
		}
	})

	t.Run("missing cache size store", func(t *testing.T) {
		t.Parallel()
		d := validDependencies(t)
		d.CacheSizes = nil
		if _, err := New(t.Context(), testLogger(), d); err == nil || err.Error() != "cache size store is required" {
			t.Fatalf("New() error = %v, want cache size store required", err)
		}
	})

	t.Run("automatic external URL keeps session MCP without remote OAuth", func(t *testing.T) {
		t.Parallel()
		d := validDependencies(t)
		store, err := auth.Open(filepath.Join(t.TempDir(), "users.json"))
		if err != nil {
			t.Fatalf("auth.Open: %v", err)
		}
		d.AuthStore = store
		d.SessionSecret = []byte("0123456789abcdef0123456789abcdef")
		d.HostState = auth.NewHostState("", nil)
		d.GitHubOAuth = oauthclient.NewGitHubConfig("client-id", "client-secret", func(*http.Request) string {
			return "https://caic.example.com/auth/github/callback"
		})
		s, err := New(t.Context(), testLogger(), d)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if s.oauthServer != nil {
			t.Error("oauthServer is non-nil without an immutable issuer")
		}
		if s.mcpDisabled {
			t.Error("MCP is disabled without an OAuth issuer")
		}
		if s.serverHandlers.mcpOAuthAvailable {
			t.Error("mcpOAuthAvailable is true without an immutable issuer")
		}

		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		loginReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://caic.example.com/auth/github/start", http.NoBody)
		loginW := httptest.NewRecorder()
		h.ServeHTTP(loginW, loginReq)
		if loginW.Code != http.StatusFound {
			t.Errorf("OAuth login status = %d, want %d", loginW.Code, http.StatusFound)
		}

		mcpReq := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://caic.example.com/api/caic/v1/mcp", strings.NewReader(`{}`))
		mcpW := httptest.NewRecorder()
		h.ServeHTTP(mcpW, mcpReq)
		if mcpW.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated MCP status = %d, want %d", mcpW.Code, http.StatusUnauthorized)
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

// TestLoginCallbackPublic verifies OAuth login callbacks bypass RequireUser.
// The provider redirects the browser to the callback before the user holds a
// session, so the callback must live outside the session-gated /api/ subtree.
// Regression: the OAuth redirect URI once pointed at /api/caic/v1/auth/*, which
// RequireUser rejected with 401, silently breaking login.
func TestLoginCallbackPublic(t *testing.T) {
	t.Parallel()
	store, err := auth.Open(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	s := newTestOAuthRouter(t, store)
	redirect := func(*http.Request) string { return "https://caic.example.com/auth/github/callback" }
	s.authHandlers.githubOAuth = oauthclient.NewGitHubConfig("client-id", "client-secret", redirect)
	s.authHandlers.gitlabOAuth = oauthclient.NewGitLabConfig("client-id", "client-secret", "https://gitlab.com", redirect)
	s.authHandlers.googleOAuth = oauthclient.NewGoogleConfig("client-id", "client-secret", redirect)
	h, err := s.buildHandler()
	if err != nil {
		t.Fatalf("buildHandler() error = %v", err)
	}

	// Sanity: the /api/ subtree is session-gated. This makes the callback checks
	// below meaningful — they pass only because the callback is reachable without
	// a session, not because auth is disabled.
	t.Run("api subtree gated", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks", nil)
		req.Host = "caic.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	for _, provider := range []string{"github", "gitlab", "google"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			// No state cookie: the handler runs and rejects with 400, proving it is
			// reachable pre-login. A 401 would mean RequireUser blocked the callback.
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/"+provider+"/callback?code=x&state=y", nil)
			req.Host = "caic.example.com"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code == http.StatusUnauthorized {
				t.Fatalf("callback returned 401; login callback must bypass RequireUser")
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (missing state cookie)", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func mustNewTask(t testing.TB, id ksid.ID, prompt agent.Prompt, h harness.Name) *task.Task {
	tk, err := task.NewTask(id, prompt, h, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// insertTestTask registers a task in the test server's manager and returns the
// entry. It registers the entry under the supplied path id as well as the
// task's own ID string, so handlers that re-resolve via the Manager by
// entry.Task().ID.String() find the same entry (in production the two always
// coincide because Insert keys on t.ID.String()).
func insertTestTask(s *testRouter, id string, tk *task.Task) {
	e := s.taskMgr.NewEntry(tk, nil)
	s.taskMgr.Insert(id, e)
	if taskID := tk.ID.String(); taskID != id {
		s.taskMgr.Insert(taskID, e)
	}
}

// testEntries returns a snapshot of every registered task entry (test-only).
func testEntries(s *testRouter) []*taskmgr.Entry {
	var out []*taskmgr.Entry
	s.taskMgr.Range(func(_ string, e *taskmgr.Entry) bool {
		out = append(out, e)
		return true
	})
	return out
}

// settleLogs compresses the terminal plain logs in logDir, moving them to the
// settled (compressed) form that the per-repo cap applies to.
func settleLogs(t *testing.T, logDir string) {
	if err := taskslog.NewStore(testLogger(), logDir).SettleTerminal(nil); err != nil {
		t.Fatal(err)
	}
}

// loadPurgedTasksForTest reads logs from disk and registers purged tasks via
// the manager. Replaces the deleted Server.loadPurgedTasks helper.
func loadPurgedTasksForTest(s *testRouter, logDir string) error {
	store := taskslog.NewStore(testLogger(), logDir)
	plain, err := store.LoadUnsettled()
	if err != nil {
		return err
	}
	settled, err := store.LoadSettled()
	if err != nil {
		return err
	}
	return s.taskMgr.LoadPurgedTasks(append(plain, settled...))
}

type checkoutConstructionTestFixture struct {
	server   *testRouter
	logDir   string
	cacheDir string
	runtimes *runtime.Router
}

func newCheckoutConstructionTestServer(t *testing.T, root string) checkoutConstructionTestFixture {
	harnessEnv := map[string][]string{string(harness.Codex): {"CODEX_HOME=/tmp/codex"}}
	backend := &mdruntime.Backend{HarnessEnv: harnessEnv}
	runtimeRouter := newTestRuntime(t, backend)
	backends := map[harness.Name]agent.Backend{harness.Codex: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}}
	cacheDir := t.TempDir()
	logDir := filepath.Join(cacheDir, "tasks")
	checkoutRegistry := repo.NewRegistry()
	taskMgr := newTestTaskManager(t, taskmgr.Config{
		ServerCtx:  t.Context(),
		LogStore:   taskslog.NewStore(testLogger(), logDir),
		Runtimes:   runtimeRouter,
		Backends:   backends,
		HarnessEnv: harnessEnv,
		Checkouts:  checkoutRegistry,
	})
	repoStatus := ci.NewRepoStatusStore()
	prefs := newTestPrefs(t)
	s, err := New(t.Context(), testLogger(), Dependencies{
		Checkouts:    checkoutRegistry,
		CheckoutRoot: root,
		RepoStatus:   repoStatus,
		Runtimes:     runtimeRouter,
		TaskMgr:      taskMgr,
		Preferences:  prefs,
		ForgeMgr:     forgemgr.New(testLogger(), "", "", nil, forgemgr.NoOAuthTokenSource()),
		Warnings:     NewWarningStore(taskMgr),
		CacheSizes:   NewCacheSizeStore(testLogger()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return checkoutConstructionTestFixture{
		server:   &testRouter{Router: s, taskMgr: taskMgr, checkouts: checkoutRegistry, repoStatus: repoStatus, prefs: prefs},
		logDir:   logDir,
		cacheDir: cacheDir,
		runtimes: runtimeRouter,
	}
}

func initCloneSourceRepo(t *testing.T) string {
	repoPath := filepath.Join(t.TempDir(), "source")
	runServerGit(t, "", "init", repoPath)
	runServerGit(t, repoPath, "config", "user.email", "test@example.com")
	runServerGit(t, repoPath, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runServerGit(t, repoPath, "add", "README.md")
	runServerGit(t, repoPath, "commit", "-m", "init")
	runServerGit(t, repoPath, "branch", "-M", "main")
	return repoPath
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

	t.Run("valid checkout construction", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		source := initCloneSourceRepo(t)
		fixture := newCheckoutConstructionTestServer(t, root)
		s := fixture.server

		clonedRepo, err := s.serverHandlers.cloneRepo(t.Context(), &v1.CloneRepoReq{URL: source, Path: "./cloned"})
		if err != nil {
			t.Fatalf("cloneRepo: %v", err)
		}
		if clonedRepo.Path != "cloned" {
			t.Fatalf("repo path = %q, want cloned", clonedRepo.Path)
		}
		checkout, ok := s.taskMgr.Checkouts.Checkout("cloned")
		if !ok {
			t.Fatal("cloned checkout not registered")
		}
		if checkout.RelPath != "cloned" {
			t.Fatalf("RelPath = %q, want cloned", checkout.RelPath)
		}
		if checkout.Dir != filepath.Join(root, "cloned") {
			t.Fatalf("Dir = %q, want cloned path", checkout.Dir)
		}
		if len(s.taskMgr.Backends) == 0 {
			t.Fatal("manager backends were not initialized")
		}

		if got := s.checkouts.Repositories(); len(got) != 1 {
			t.Fatalf("repo registry after watcher sync = %+v, want one cloned repo", got)
		}
		after, ok := s.taskMgr.Checkouts.Checkout("cloned")
		if !ok {
			t.Fatal("checkout disappeared after watcher sync")
		}
		if after != checkout {
			t.Fatal("watcher replaced an already registered clone checkout")
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
		s := newCheckoutConstructionTestServer(t, root).server

		if _, err := s.serverHandlers.cloneRepo(t.Context(), &v1.CloneRepoReq{URL: parent, Path: "broken"}); err == nil {
			t.Fatal("cloneRepo succeeded, want submodule clone failure")
		}
		if _, err := os.Stat(filepath.Join(root, "broken")); !os.IsNotExist(err) {
			t.Fatalf("partial clone path still exists: %v", err)
		}
		if got := s.checkouts.Repositories(); len(got) != 0 {
			t.Fatalf("repo registry = %+v, want empty after failed clone", got)
		}
		if _, ok := s.taskMgr.Checkouts.Checkout("broken"); ok {
			t.Fatal("failed clone registered an checkout")
		}
	})
}

func TestHandleTaskEvents(t *testing.T) {
	t.Parallel()
	t.Run("NotFound", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/api/caic/v1/tasks/99/raw_events", http.NoBody)
		req.SetPathValue("id", "99")
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(w, req)
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
		s := newTestRouter(t, nil)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/api/caic/v1/tasks/abc/raw_events", http.NoBody)
		req.SetPathValue("id", "abc")
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(w, req)
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
			s := newTestRouter(t, nil)
			insertTestTask(s, "t1", mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, ""))

			body := strings.NewReader(tt.bodyJSON)
			req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks/t1/input", body)
			req.SetPathValue("id", "t1")
			w := httptest.NewRecorder()
			handleWithTask(testTaskHandlers(s), testTaskHandlers(s).taskSvc.sendInput)(w, req)
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
func testRestart(t *testing.T, state taskslog.State, bodyJSON string, wantStatus int, wantCode api.ErrorCode) {
	s := newTestRouter(t, nil)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
	tk.SetState(state)
	insertTestTask(s, "t1", tk)

	body := strings.NewReader(bodyJSON)
	req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks/t1/restart", body)
	req.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	handleWithTask(testTaskHandlers(s), testTaskHandlers(s).taskSvc.restartTask)(w, req)
	if w.Code != wantStatus {
		t.Errorf("status = %d, want %d", w.Code, wantStatus)
	}
	e := decodeError(t, w)
	if e.Code != wantCode {
		t.Errorf("code = %q, want %q", e.Code, wantCode)
	}
}

func TestTaskHistoryReaders(t *testing.T) {
	t.Parallel()

	t.Run("valid_read_from_log_without_hydrating_task", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		taskID := ksid.NewID()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "inspect history", Harness: harness.Claude,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		toolUse := mustJSON(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{"content": []any{map[string]any{
				"type": "tool_use", "id": "tool-1", "name": "Bash", "input": map[string]any{"command": "make check"},
			}}},
		})
		lastMessage := mustJSON(t, map[string]any{
			"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "history result"}}},
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, logDir, taskID.String()+".jsonl", meta, toolUse, lastMessage, trailer)

		s := newTestRouter(t, map[harness.Name]agent.Backend{
			harness.Claude: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire},
		})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}
		entry, ok := s.taskMgr.GetEntry(taskID.String())
		if !ok {
			t.Fatal("loaded task entry not found")
		}

		resp, err := testTaskHandlers(s).taskSvc.taskToolInput(t.Context(), entry, "tool-1")
		if err != nil {
			t.Fatal(err)
		}
		if string(resp.Input) != `{"command":"make check"}` {
			t.Fatalf("tool input = %s", resp.Input)
		}

		registry := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := registry.handleAgentLastMessage(t.Context(), mcpTaskNumberArgs{TaskNumber: 1})
		if result.IsError {
			t.Fatalf("agent_last_message error = %#v", result.Structured)
		}
		output, ok := result.Structured.(mcp.TextOutput)
		if !ok || output.Result != "Last message from task #1: history result" {
			t.Fatalf("agent_last_message = %#v", result.Structured)
		}

		history, _, unsubscribe := entry.Task().Subscribe(t.Context())
		unsubscribe()
		if len(history) != 0 {
			t.Fatalf("restored task retained %d history messages", len(history))
		}
	})

	t.Run("live_read_without_log", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "no log"}, harness.Claude)
		tk.SeedTimeline([]agent.Message{
			&agent.ToolUseMessage{ToolUseID: "tool-1", Input: json.RawMessage(`{"command":"make check"}`)},
			&agent.TextMessage{Text: "live result"},
		})
		entry := s.taskMgr.NewEntry(tk, nil)
		s.taskMgr.Insert(tk.ID.String(), entry)

		resp, err := testTaskHandlers(s).taskSvc.taskToolInput(t.Context(), entry, "tool-1")
		if err != nil {
			t.Fatal(err)
		}
		if string(resp.Input) != `{"command":"make check"}` {
			t.Fatalf("tool input = %s", resp.Input)
		}

		registry := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := registry.handleAgentLastMessage(t.Context(), mcpTaskNumberArgs{TaskNumber: 1})
		output, ok := result.Structured.(mcp.TextOutput)
		if result.IsError || !ok || output.Result != "Last message from task #1: live result" {
			t.Fatalf("agent_last_message = %#v", result.Structured)
		}
	})

	t.Run("restored_error_no_log", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "no log"}, harness.Claude)
		loaded := &taskslog.LoadedTask{State: taskslog.StatePurged, LastTrailer: &taskslog.Result{State: taskslog.StatePurged}}
		entry := s.taskMgr.NewEntry(tk, loaded)
		s.taskMgr.Insert(tk.ID.String(), entry)

		_, err := testTaskHandlers(s).taskSvc.taskToolInput(t.Context(), entry, "tool-1")
		apiErr, ok := errors.AsType[*api.Error](err)
		if !ok || apiErr.Code() != api.CodeInternalError || !strings.Contains(apiErr.Error(), "history unavailable") {
			t.Fatalf("taskToolInput error = %v, want explicit history-unavailable error", err)
		}

		registry := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := registry.handleAgentLastMessage(t.Context(), mcpTaskNumberArgs{TaskNumber: 1})
		if !result.IsError || !strings.Contains(fmt.Sprint(result.Structured), "history is unavailable") {
			t.Fatalf("agent_last_message = %#v, want explicit history-unavailable error", result.Structured)
		}
	})
}

func TestHandleRestart(t *testing.T) {
	t.Parallel()
	t.Run("NotWaiting", func(t *testing.T) {
		t.Parallel()
		testRestart(t, taskslog.StateRunning, `{"prompt":{"text":"new plan"}}`, http.StatusConflict, api.CodeConflict)
	})

	t.Run("EmptyPrompt", func(t *testing.T) {
		t.Parallel()
		testRestart(t, taskslog.StateWaiting, `{"prompt":{"text":""}}`, http.StatusBadRequest, api.CodeBadRequest)
	})
}

func TestHandlePurge(t *testing.T) {
	t.Parallel()
	t.Run("NotWaiting", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
		// StatePending is the zero value, but set explicitly for clarity.
		insertTestTask(s, "t1", tk)

		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks/t1/purge", http.NoBody)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(testTaskHandlers(s), testTaskHandlers(s).taskSvc.purgeTask)(w, req)
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
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
		tk.Repos = []taskslog.RepoMount{{Name: "r"}}
		tk.SetState(taskslog.StateWaiting)
		s := newTestRouter(t, nil)
		registerRouterCheckout(t, s.taskMgr.Checkouts, "r", newRouterTestCheckout(t.TempDir()))
		insertTestTask(s, "t1", tk)

		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks/t1/purge", http.NoBody)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(testTaskHandlers(s), testTaskHandlers(s).taskSvc.purgeTask)(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		// Verify the response reports that delayed purge was scheduled.
		var resp v1.StatusResp
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Status != "scheduled" {
			t.Errorf("status = %q, want %q", resp.Status, "scheduled")
		}
	})

	t.Run("CancelledContext", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
		tk.Repos = []taskslog.RepoMount{{Name: "r"}}
		tk.SetState(taskslog.StateRunning)
		s := newTestRouter(t, nil)
		registerRouterCheckout(t, s.taskMgr.Checkouts, "r", newRouterTestCheckout(t.TempDir()))
		insertTestTask(s, "t1", tk)

		// Use an already-cancelled context to simulate shutdown scenario
		// where BaseContext is cancelled before the handler completes.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks/t1/purge", http.NoBody)
		req = req.WithContext(ctx)
		req.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		handleWithTask(testTaskHandlers(s), testTaskHandlers(s).taskSvc.purgeTask)(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})
}

func TestHandleCreateTask(t *testing.T) {
	t.Parallel()
	t.Run("ReturnsID", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		registerRouterCheckout(t, s.taskMgr.Checkouts, "myrepo", newRouterTestCheckout(t.TempDir()))
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test task"},"repos":[{"name":"myrepo"}],"harness":"claude","model":"m1"}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
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
		if resp.Model != "m1" {
			t.Errorf("model = %q, want m1", resp.Model)
		}
	})

	t.Run("MissingRepo", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test task"}}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t, nil)
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"nonexistent"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t, nil)
		registerRouterCheckout(t, s.taskMgr.Checkouts, "myrepo", newRouterTestCheckout(t.TempDir()))
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"nonexistent"}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Codex: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		registerRouterCheckout(t, s.taskMgr.Checkouts, "myrepo", newRouterTestCheckout(t.TempDir()))
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"codex","model":"nonexistent"}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
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

	t.Run("WithImage", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		registerRouterCheckout(t, s.taskMgr.Checkouts, "myrepo", newRouterTestCheckout(t.TempDir()))
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		// Set docker image in user preferences.
		if err := s.prefs.Update("default", func(p *preferences.Preferences) {
			p.Settings.BaseImage = "ghcr.io/my/image:v1"
			p.Settings.ContainerPlatform = "linux/amd64"
		}); err != nil {
			t.Fatal(err)
		}

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"claude"}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		registerRouterCheckout(t, s.taskMgr.Checkouts, "myrepo", newRouterTestCheckout(t.TempDir()))
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
		handle(testTaskHandlers(s).taskSvc.createTask)(w, req)

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
				gotCustom = cm.HostPath == "/host/custom" && cm.ContainerPath == "/home/user/.custom"
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
			if m.HostPath == "/host/work" && m.ContainerPath == "/workspace/external" && m.ReadOnly {
				gotCustomMount = true
			}
			if m.HostPath == "/host/disabled-work" || m.ContainerPath == "/workspace/disabled" {
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
			if cm.HostPath == "/host/disabled-cache" || cm.ContainerPath == "/home/user/.disabled-cache" {
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

	t.Run("TaskInfo", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		registerRouterCheckout(t, s.taskMgr.Checkouts, "myrepo", newRouterTestCheckout(t.TempDir()))
		if err := s.prefs.Update("default", func(p *preferences.Preferences) {
			p.Settings.BaseImage = "ghcr.io/my/image:v1"
			p.Settings.ContainerPlatform = "linux/amd64"
			p.Settings.MaxCPUs = 4
			p.Settings.CacheMappings = []preferences.CacheMapping{{HostPath: "/host/cache", ContainerPath: "/home/user/.cache", Enabled: true}}
			p.Settings.CustomMounts = []preferences.MountMapping{{HostPath: "/host/work", ContainerPath: "/workspace/work", Enabled: true, ReadOnly: true}}
		}); err != nil {
			t.Fatal(err)
		}
		createReq := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", strings.NewReader(`{"initialPrompt":{"text":"test"},"repos":[{"name":"myrepo"}],"harness":"claude","gitHubToken":true}`))
		createW := httptest.NewRecorder()
		handle(testTaskHandlers(s).taskSvc.createTask)(createW, createReq)
		if createW.Code != http.StatusOK {
			t.Fatalf("create status = %d, want %d: %s", createW.Code, http.StatusOK, createW.Body.String())
		}
		var taskResp v1.Task
		if err := json.NewDecoder(createW.Body).Decode(&taskResp); err != nil {
			t.Fatal(err)
		}

		infoReq := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/api/caic/v1/tasks/"+taskResp.ID.String()+"/info", http.NoBody)
		infoReq.SetPathValue("id", taskResp.ID.String())
		infoW := httptest.NewRecorder()
		handleWithTask(testTaskHandlers(s), testTaskHandlers(s).taskSvc.getTaskInfo)(infoW, infoReq)
		if infoW.Code != http.StatusOK {
			t.Fatalf("info status = %d, want %d: %s", infoW.Code, http.StatusOK, infoW.Body.String())
		}
		var info v1.TaskInfo
		if err := json.NewDecoder(infoW.Body).Decode(&info); err != nil {
			t.Fatal(err)
		}
		if info.Recorded.BaseImage != "ghcr.io/my/image:v1" || info.Recorded.ContainerOS != "linux" || info.Recorded.ContainerCPUArchitecture != "amd64" || info.Recorded.MaxCPUs != 4 {
			t.Fatalf("recorded config = image %q os %q cpu architecture %q cpus %d", info.Recorded.BaseImage, info.Recorded.ContainerOS, info.Recorded.ContainerCPUArchitecture, info.Recorded.MaxCPUs)
		}
		if !info.Recorded.Capabilities.GitHubToken {
			t.Error("GitHubToken = false, want true")
		}
		if len(info.Recorded.Repos) != 1 || info.Recorded.Repos[0].Name != "myrepo" || info.Recorded.Repos[0].GitRoot == "" {
			t.Errorf("Repos = %+v", info.Recorded.Repos)
		}
		if len(info.Recorded.Caches) != 1 || info.Recorded.Caches[0].HostPath != "/host/cache" {
			t.Errorf("Caches = %+v", info.Recorded.Caches)
		}
		if len(info.Recorded.Mounts) != 1 || info.Recorded.Mounts[0].HostPath != "/host/work" || !info.Recorded.Mounts[0].ReadOnly {
			t.Errorf("Mounts = %+v", info.Recorded.Mounts)
		}
	})

	t.Run("NoRepoTask", func(t *testing.T) {
		t.Parallel()
		// Regression: creating a task with no repos panicked with
		// "makeslice: cap out of range" because len(req.Repos)-1 == -1.
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"no repo task"},"harness":"claude","model":"m1","effort":"high"}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
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

	t.Run("StaleSavedRuntimeUsesDefault", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}})
		if err := s.prefs.Update("default", func(p *preferences.Preferences) {
			p.Settings.RuntimeName = "ghost"
		}); err != nil {
			t.Fatal(err)
		}
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"no repo task"},"harness":"claude","model":"m1"}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp v1.Task
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Runtime.RuntimeName != "test-runtime" {
			t.Fatalf("runtime = %q, want test-runtime", resp.Runtime.RuntimeName)
		}
	})

	t.Run("NoRepoCheckoutNoBackend", func(t *testing.T) {
		t.Parallel()
		// Creating a no-repo task with no registered harness backends returns
		// a clear 400 instead of panicking.
		s := newTestRouter(t, map[harness.Name]agent.Backend{})
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"no repo task"},"harness":"claude"}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("UnknownField", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		handler := handle(testTaskHandlers(s).taskSvc.createTask)

		body := strings.NewReader(`{"initialPrompt":{"text":"test"},"repo":"r","harness":"claude","bogus":true}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks", body)
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
		s := newTestRouter(t, nil)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
		tk.Repos = []taskslog.RepoMount{{Name: "r"}}
		tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{SSHHost: "ctr"}, "", "", 0)
		insertTestTask(s, "t1", tk)
		backend := &runtimetest.FakeBackend{}
		processes := &runtimeProcessHandlers{
			log:         testLogger(),
			taskMgr:     s.taskMgr,
			runtimes:    newTestRuntime(t, backend),
			authEnabled: false,
		}

		body := strings.NewReader(`{"signal":"SIGTERM","extra":true}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/processes/t1/123/signal", body)
		req.SetPathValue("id", "t1")
		req.SetPathValue("pid", "123")
		w := httptest.NewRecorder()
		processes.HandleSignalProcess(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if _, ok := backend.LastSignal("ctr"); ok {
			t.Error("no signal should have been delivered")
		}
	})

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
		tk.Repos = []taskslog.RepoMount{{Name: "r"}}
		tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{SSHHost: "ctr"}, "", "", 0)
		insertTestTask(s, "t1", tk)
		backend := &runtimetest.FakeBackend{}
		processes := &runtimeProcessHandlers{
			log:         testLogger(),
			taskMgr:     s.taskMgr,
			runtimes:    newTestRuntime(t, backend),
			authEnabled: false,
		}

		body := strings.NewReader(`{"signal":"SIGKILL"}`)
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/processes/t1/123/signal", body)
		req.SetPathValue("id", "t1")
		req.SetPathValue("pid", "123")
		w := httptest.NewRecorder()
		processes.HandleSignalProcess(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		sig, ok := backend.LastSignal("ctr")
		if !ok {
			t.Fatal("no signal delivered to ctr, want one")
		}
		if sig.PID != 123 {
			t.Errorf("pid = %d, want 123", sig.PID)
		}
		if sig.Signal != "SIGKILL" {
			t.Errorf("signal = %q, want SIGKILL", sig.Signal)
		}
	})
}

func TestHandleListRepos(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	repositories := repo.NewRegistry()
	registerRouterCheckout(t, repositories, "org/repoA", &repo.Checkout{Dir: "/src/org/repoA", BaseBranch: "main"})
	registerRouterCheckout(t, repositories, "repoB", &repo.Checkout{Dir: "/src/repoB", BaseBranch: "develop"})
	s.checkouts = repositories
	s.serverHandlers.checkouts = repositories

	req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/api/caic/v1/server/repos", http.NoBody)
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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 3 {
			t.Fatalf("len(entries) = %d, want 3", len(entries))
		}

		// Collect prompts sorted by ksid (time-sortable) to verify all loaded.
		prompts := make([]string, 0, len(entries))
		var anyEntry *taskmgr.Entry
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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
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

		settleLogs(t, logDir)

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		// Count tasks per repo.
		counts := map[string]int{}
		for _, e := range entries {
			repoName := ""
			if p := e.Task().Primary(); p != nil {
				repoName = p.Name
			}
			counts[repoName]++
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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		for _, e := range entries {
			h := testTaskHandlers(s)
			j, err := taskDTO(t.Context(), e, h.taskSvc.taskMgr, h.taskSvc.checkouts, h.taskSvc.authStore)
			if err != nil {
				t.Fatal(err)
			}
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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		for _, e := range entries {
			h := testTaskHandlers(s)
			j, err := taskDTO(t.Context(), e, h.taskSvc.taskMgr, h.taskSvc.checkouts, h.taskSvc.authStore)
			if err != nil {
				t.Fatal(err)
			}
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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
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
		s.taskMgr.Range(func(id string, e *taskmgr.Entry) bool {
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
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
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
		// caic_pr is early in the file with >64 KiB of messages after it.
		// Full parser traversal derives its metadata, while retained task messages
		// remain bounded to the tail window.
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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		for _, e := range entries {
			snap := e.Task().Snapshot()
			if snap.ForgePR != 77 {
				t.Errorf("ForgePR = %d, want 77", snap.ForgePR)
			}
			if snap.ForgeOwner != "acme" {
				t.Errorf("ForgeOwner = %q, want acme", snap.ForgeOwner)
			}
			if snap.ForgeRepo != "widget" {
				t.Errorf("ForgeRepo = %q, want widget", snap.ForgeRepo)
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
		}
		// Settle the logs (compress terminal plain logs) so the per-repo cap,
		// which applies to the settled scan, is exercised. Preserve each log's
		// mtime on the compressed file: the cap orders by mtime.
		for i := range 12 {
			name := fmt.Sprintf("%02d.jsonl", i)
			if err := os.Chtimes(filepath.Join(logDir, name), now.Add(-time.Duration(11-i)*time.Hour), now.Add(-time.Duration(11-i)*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		settleLogs(t, logDir)
		for i := range 12 {
			name := fmt.Sprintf("%02d.jsonl.zst", i)
			if err := os.Chtimes(filepath.Join(logDir, name), now.Add(-time.Duration(11-i)*time.Hour), now.Add(-time.Duration(11-i)*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		var taskID string
		s.taskMgr.Range(func(id string, _ *taskmgr.Entry) bool {
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
		testTaskHandlers(s).handleTaskEvents(w, req)
		if err := ctx.Err(); err != nil {
			t.Fatalf("handleTaskEvents blocked until context deadline: %v", err)
		}

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		// With lazy loading, purged task messages are loaded on demand
		// when the events endpoint is accessed. The handler replays the
		// full message history, then emits "ready" and closes the stream.
		body := w.Body.String()
		entry, ok := s.taskMgr.GetEntry(taskID)
		if !ok {
			t.Fatal("purged task disappeared")
		}
		id1 := taskEventID{timeline: entry.Task().TimelineID(), source: taskEventSourceDisk, message: 1}.String()
		id2 := taskEventID{timeline: entry.Task().TimelineID(), source: taskEventSourceDisk, message: 2}.String()
		id3 := taskEventID{timeline: entry.Task().TimelineID(), source: taskEventSourceDisk, message: 3}.String()
		if !strings.Contains(body, "event: ready") {
			t.Error("expected 'ready' event for purged task")
		}
		if !strings.Contains(body, "I found the bug") {
			t.Error("expected text message 'I found the bug' to be replayed for purged task")
		}
		if !strings.Contains(body, "id: "+id1+"\n") || !strings.Contains(body, "id: "+id2+"\n") {
			t.Errorf("task SSE body is missing stable event IDs:\n%s", body)
		}

		resumeReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody)
		resumeReq.SetPathValue("id", taskID)
		resumeReq.Header.Set("Last-Event-ID", id2)
		resumeWriter := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(resumeWriter, resumeReq)
		resumedBody := resumeWriter.Body.String()
		if strings.Contains(resumedBody, "I found the bug") || strings.Contains(resumedBody, "id: "+id1) || strings.Contains(resumedBody, "id: "+id2) {
			t.Errorf("resumed task SSE replayed acknowledged events:\n%s", resumedBody)
		}
		if !strings.Contains(resumedBody, "id: "+id3) || !strings.Contains(resumedBody, "event: ready") {
			t.Errorf("resumed task SSE omitted unseen history or ready marker:\n%s", resumedBody)
		}

		staleReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody)
		staleReq.SetPathValue("id", taskID)
		staleReq.Header.Set("Last-Event-ID", taskEventID{timeline: entry.Task().TimelineID(), source: taskEventSourceDisk, message: 99}.String())
		staleWriter := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(staleWriter, staleReq)
		staleBody := staleWriter.Body.String()
		if !strings.Contains(staleBody, "id:\nevent: reset\n") || !strings.Contains(staleBody, "I found the bug") {
			t.Errorf("stale task SSE cursor did not reset and replay full history:\n%s", staleBody)
		}
	})

	t.Run("PurgedV1ParseFailureEmitsErrorWithoutReady", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		trailer := mustJSON(t, agent.MetaResultMessage{MessageType: "caic_result", State: "purged"})
		writeLogFile(t, logDir, "task.jsonl", meta, `not-json`, trailer)

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}
		var taskID string
		s.taskMgr.Range(func(id string, _ *taskmgr.Entry) bool {
			taskID = id
			return false
		})
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID)
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(w, req)
		body := w.Body.String()
		if !strings.Contains(body, "event: error") || !strings.Contains(body, "task history is unavailable") {
			t.Fatalf("history failure body = %q, want error event", body)
		}
		if strings.Contains(body, "event: ready") {
			t.Fatalf("history failure body unexpectedly contains ready: %q", body)
		}
	})

	t.Run("StoppedHistoryFailureClosesUntilRevivedReconnect", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		taskID := ksid.NewID()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug",
			Repos: []agent.MetaRepo{{Name: "r", Branch: "caic-0"}}, Harness: harness.Claude,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		path := filepath.Join(logDir, taskID.String()+".jsonl")
		writeLogFile(t, logDir, taskID.String()+".jsonl", meta, `not-json`)

		tk := mustNewTask(t, taskID, agent.Prompt{Text: "fix the bug"}, harness.Claude)
		tk.SetState(taskslog.StateStopped)
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}})
		entry := s.taskMgr.NewEntry(tk, nil)
		entry.LogPath.Set(path)
		s.taskMgr.Insert(taskID.String(), entry)

		stoppedReq := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		stoppedReq.SetPathValue("id", taskID.String())
		stoppedWriter := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(stoppedWriter, stoppedReq)
		if body := stoppedWriter.Body.String(); strings.Contains(body, "event: error") || strings.Contains(body, "event: ready") {
			t.Fatalf("stopped scan should close for retry, got %q", body)
		}

		// A new incarnation reconnects through the normal live-history path;
		// it must not be held behind the stopped scan's old parse failure.
		tk.SeedTimeline([]agent.Message{&agent.TextMessage{Text: "revived live history"}})
		tk.SetState(taskslog.StateRunning)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		time.AfterFunc(20*time.Millisecond, cancel)
		revivedReq := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		revivedReq.SetPathValue("id", taskID.String())
		revivedWriter := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(revivedWriter, revivedReq)
		body := revivedWriter.Body.String()
		if !strings.Contains(body, "revived live history") || !strings.Contains(body, "event: ready") {
			t.Fatalf("revived reconnect body = %q, want live history and ready", body)
		}
		if strings.Contains(body, "event: error") {
			t.Fatalf("revived reconnect body unexpectedly has history error: %q", body)
		}
	})

	t.Run("StoppedScanRevivedBeforeReadyClosesForRetry", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		taskID := ksid.NewID()
		meta := mustJSON(t, agent.MetaMessage{
			MessageType: "caic_meta", Version: 1, Prompt: "fix the bug", Harness: harness.Claude,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		message := mustJSON(t, map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"model":   "claude-opus-4-6",
				"content": []map[string]any{{"type": "text", "text": "stopped history"}},
				"usage":   map[string]any{},
			},
		})
		path := filepath.Join(logDir, taskID.String()+".jsonl")
		writeLogFile(t, logDir, taskID.String()+".jsonl", meta, message)

		tk := mustNewTask(t, taskID, agent.Prompt{Text: "fix the bug"}, harness.Claude)
		tk.SetState(taskslog.StateStopped)
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire}})
		entry := s.taskMgr.NewEntry(tk, nil)
		entry.LogPath.Set(path)
		s.taskMgr.Insert(taskID.String(), entry)

		recorder := httptest.NewRecorder()
		w := &reviveDuringStoppedScanWriter{ResponseRecorder: recorder, revive: func() {
			tk.SetState(taskslog.StateRunning)
			tk.SeedTimeline([]agent.Message{&agent.TextMessage{Text: "revived live event"}})
		}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID.String())
		testTaskHandlers(s).handleTaskEvents(w, req)

		body := recorder.Body.String()
		if !strings.Contains(body, "stopped history") {
			t.Fatalf("history body = %q, want stopped history", body)
		}
		if strings.Contains(body, "event: ready") || strings.Contains(body, "revived live event") {
			t.Fatalf("revived stopped scan published new lifecycle events: %q", body)
		}
	})

	t.Run("TerminalTaskWithoutLogPathEmitsHistoryError", func(t *testing.T) {
		t.Parallel()
		taskID := ksid.NewID()
		tk := mustNewTask(t, taskID, agent.Prompt{Text: "fix the bug"}, harness.Claude)
		tk.SeedTimeline([]agent.Message{&agent.TextMessage{Text: "retained in-memory history"}})
		tk.SetState(taskslog.StateFailed)

		s := newTestRouter(t, nil)
		insertTestTask(s, taskID.String(), tk)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID.String())
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(w, req)

		body := w.Body.String()
		if !strings.Contains(body, "event: error") || !strings.Contains(body, "task history is unavailable") {
			t.Fatalf("history body = %q, want explicit history error", body)
		}
		if strings.Contains(body, "retained in-memory history") || strings.Contains(body, "event: ready") {
			t.Fatalf("history body = %q, unexpectedly used in-memory history", body)
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
		logs, err := taskslog.NewStore(testLogger(), logDir).LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) != 1 {
			t.Fatalf("logs len = %d, want 1", len(logs))
		}
		logs[0].SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		})

		tk := mustNewTask(t, taskID, agent.Prompt{Text: "fix the bug"}, harness.Claude)
		tk.SeedTimeline([]agent.Message{
			&agent.TextMessage{Text: "fast in-memory history"},
			&agent.ResultMessage{MessageType: "result", Subtype: "success"},
			&agent.ExitMessage{ExitCode: 2, Error: "suppressed clean-turn exit"},
			&agent.TextMessage{Text: "after sequence gap"},
		})
		tk.SetState(taskslog.StateRunning)

		s := newTestRouter(t, nil)
		s.taskMgr.Insert(taskID.String(), s.taskMgr.NewEntry(tk, logs[0]))

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID.String())
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(w, req)

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
		id1 := taskEventID{timeline: tk.TimelineID(), source: taskEventSourceMemory, message: 1}.String()
		id2 := taskEventID{timeline: tk.TimelineID(), source: taskEventSourceMemory, message: 2}.String()
		id4 := taskEventID{timeline: tk.TimelineID(), source: taskEventSourceMemory, message: 4}.String()
		if !strings.Contains(body, "id: "+id1) || !strings.Contains(body, "id: "+id2) || !strings.Contains(body, "id: "+id4) {
			t.Fatalf("in-memory SSE IDs did not preserve the suppressed sequence gap:\n%s", body)
		}

		resumeCtx, resumeCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer resumeCancel()
		resumeReq := httptest.NewRequestWithContext(resumeCtx, http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		resumeReq.SetPathValue("id", taskID.String())
		resumeReq.Header.Set("Last-Event-ID", id1)
		resumeWriter := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(resumeWriter, resumeReq)
		resumedBody := resumeWriter.Body.String()
		if strings.Contains(resumedBody, "id: "+id1) || !strings.Contains(resumedBody, "id: "+id2) || !strings.Contains(resumedBody, "id: "+id4) {
			t.Fatalf("in-memory SSE resume did not emit only the unseen suffix:\n%s", resumedBody)
		}
		if !strings.Contains(resumedBody, `"result":"fast in-memory history"`) {
			t.Fatalf("in-memory SSE resume lost converter state:\n%s", resumedBody)
		}

		resetCtx, resetCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer resetCancel()
		resetReq := httptest.NewRequestWithContext(resetCtx, http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		resetReq.SetPathValue("id", taskID.String())
		resetReq.Header.Set("Last-Event-ID", taskEventID{timeline: tk.TimelineID(), source: taskEventSourceDisk, message: 1}.String())
		resetWriter := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(resetWriter, resetReq)
		resetBody := resetWriter.Body.String()
		if !strings.Contains(resetBody, "id:\nevent: reset\n") || !strings.Contains(resetBody, "id: "+id1) {
			t.Fatalf("wrong in-memory SSE authority did not reset and replay:\n%s", resetBody)
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
		logs, err := taskslog.NewStore(testLogger(), logDir).LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) != 1 {
			t.Fatalf("logs len = %d, want 1", len(logs))
		}
		logs[0].SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
			return claudecode.New().NewWire().ParseMessage, nil
		})

		tk := mustNewTask(t, taskID, agent.Prompt{Text: "fix the bug"}, harness.Claude)
		tk.Repos = []taskslog.RepoMount{{Name: "r", Branch: "caic-0"}}
		tk.SeedTimeline([]agent.Message{&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"}})
		tk.SetState(taskslog.StateStopped)
		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}}}, WireFactory: claudecode.New().NewWire}})
		s.taskMgr.Insert(taskID.String(), s.taskMgr.NewEntry(tk, logs[0]))

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/caic/v1/tasks/"+taskID.String()+"/raw_events", http.NoBody)
		req.SetPathValue("id", taskID.String())
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(w, req)

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

		s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "m1"}, {ID: "m2"}}}, WireFactory: claudecode.New().NewWire}})
		if err := loadPurgedTasksForTest(s, logDir); err != nil {
			t.Fatal(err)
		}

		entries := testEntries(s)
		if len(entries) != 1 {
			t.Fatalf("len(entries) = %d, want 1", len(entries))
		}
		var taskID string
		s.taskMgr.Range(func(id string, _ *taskmgr.Entry) bool {
			taskID = id
			return false
		})

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody).WithContext(ctx)
		req.SetPathValue("id", taskID)
		w := httptest.NewRecorder()
		testTaskHandlers(s).handleTaskEvents(w, req)

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
		if !strings.Contains(body, `"kind":"textDelta"`) {
			t.Error("expected raw disk history to retain text delta events")
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
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	embedded := newGoModeSettings(v1.VoiceGatewayMetadata{Mode: v1.VoiceGatewayModeEmbedded}, false)
	if embedded.WebShell.VoiceGateway.URL != "/" || embedded.WebShell.VoiceGateway.AuthRequired {
		t.Fatalf("Embedded VoiceGateway = %+v", embedded.WebShell.VoiceGateway)
	}
	if err := embedded.Validate(); err != nil {
		t.Fatalf("Embedded Validate() error = %v", err)
	}
}

func TestBuildHandler(t *testing.T) {
	t.Parallel()
	t.Run("unknown API path returns 404 not SPA", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
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

	t.Run("preferences rejects unknown runtime", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/server/preferences", strings.NewReader(`{"settings":{"runtimeName":"ghost"}}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		prefs := s.prefs.Get("default")
		if prefs.Settings.RuntimeName != "" {
			t.Fatalf("persisted runtime = %q, want empty", prefs.Settings.RuntimeName)
		}
	})

	t.Run("blocked origin error includes IP and category", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		checker, err := ipgeo.NewChecker(t.Context(), testLogger(), "tailscale", "", "")
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
		user, err := store.UpsertUser(&auth.User{Provider: auth.ProviderGitHub, ProviderID: "1", Username: "alice", AccessToken: "forge-token"})
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
		s := newTestRouter(t, nil)
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		for _, spec := range registry.specs() {
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
		s := newTestRouter(t, nil)
		s.hostState = auth.NewHostState("https://caic.example.com", nil)
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
		s := newTestRouter(t, nil)
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
		if err := resp.Validate(); err != nil {
			t.Fatalf("settings validation error = %v", err)
		}
	})

	t.Run("auth disabled", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		if _, err := s.buildHandler(); err != nil {
			t.Fatalf("buildHandler() error = %v", err)
		}
	})

	t.Run("auth enabled", func(t *testing.T) {
		t.Parallel()
		// Regression: adding /auth/ (unqualified) alongside GET / (qualified)
		// caused a pattern conflict panic in Go 1.22+ ServeMux.
		s := newTestRouter(t, nil)
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
		s := newTestRouter(t, nil)
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
		s := newTestRouter(t, nil)
		s.hostState = auth.NewHostState("https://caic.example.com", nil)
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
		s := newTestRouter(t, nil)
		s.hostState = auth.NewHostState("https://caic.example.com", nil)
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
		s := newTestRouter(t, nil)
		s.hostState = auth.NewHostState("https://caic.example.com", nil)
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
		s := newTestRouter(t, nil)
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
		s := newTestRouter(t, nil)
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
		s := newTestRouter(t, nil)
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
		s := newTestRouter(t, nil)
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
		s := newTestRouter(t, nil)
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
		s := newTestRouter(t, nil)
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

	host := auth.NewHostState("http://localhost", nil)
	s := newTestRouter(t, nil)
	s.sessionSecret = secret
	s.authStore = store
	s.hostState = host
	githubCfg := oauthclient.NewGitHubConfig("cid", "csec", func(_ *http.Request) string { return "http://localhost/api/caic/v1/auth/github/callback" })
	githubCfg.TokenURL = tokenServer.URL
	githubCfg.UserInfoURL = userServer.URL
	githubCfg.Scopes = []string{"repo"}
	s.authHandlers.store = s.authStore
	s.authHandlers.sessionSecret = s.sessionSecret
	s.authHandlers.hostState = s.hostState
	s.authHandlers.githubOAuth = githubCfg

	t.Run("valid state round-trip succeeds", func(t *testing.T) {
		t.Parallel()
		// Simulate the start handler to get a valid state cookie.
		startW := httptest.NewRecorder()
		startReq := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/auth/github/start", http.NoBody)
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
		cbReq := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet,
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
		startReq := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/auth/github/start?next="+url.QueryEscape("https://evil.example/callback"), http.NoBody)
		s.authHandlers.handleStart("github")(startW, startReq)
		if startW.Code != http.StatusBadRequest {
			t.Fatalf("start status = %d, want %d", startW.Code, http.StatusBadRequest)
		}
	})

	t.Run("rejects backslash web next", func(t *testing.T) {
		t.Parallel()
		for _, next := range []string{`/\evil.example/callback`, `/oauth/authorize\evil`} {
			startW := httptest.NewRecorder()
			startReq := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/auth/github/start?next="+url.QueryEscape(next), http.NoBody)
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
		startReq := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/auth/github/start?next="+url.QueryEscape(next), http.NoBody)
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

		cbReq := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet,
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
		s := newTestRouter(t, nil)

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
		s := newTestRouter(t, nil)
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
