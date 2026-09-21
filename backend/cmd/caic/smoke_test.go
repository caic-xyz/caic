// Runtime smoke test for the caic server's real md lifecycle and terminal raw-log history.

// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

//go:build smoke

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/app"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/smoketest"
)

// TestSmoke verifies the real runtime path: start the server, launch an md
// container, run a deterministic agent through the relay over SSH, and
// exercise the task lifecycle. Unlike the e2e suite it must not use the fake
// server or the smoketest runtime backend: it intentionally exercises the
// md/container runtime path, which makes it host-dependent and isolated
// behind the smoke build tag and its own make target (make test-smoke).
func TestSmoke(t *testing.T) {
	smoke := startSmokeServer(t)
	baseURL := smoke.baseURL

	// --- API endpoints ---

	t.Run("Config", func(t *testing.T) {
		var cfg v1.Config
		getJSON(t, baseURL, "/api/caic/v1/server/config", &cfg)
		// Version is empty in dev builds (no ldflags), so only check that the
		// endpoint returns successfully.
		if cfg.DisplayName == "" {
			t.Error("config.DisplayName is empty")
		}
	})

	t.Run("Repos", func(t *testing.T) {
		var repos []v1.Repo
		getJSON(t, baseURL, "/api/caic/v1/server/repos", &repos)
		if len(repos) == 0 {
			t.Fatal("expected at least one repo")
		}
		// Smoke setup creates two repos (clone and clone2).
		if len(repos) < 2 {
			t.Errorf("expected at least 2 repos, got %d", len(repos))
		}
		for _, r := range repos {
			if r.Path == "" {
				t.Error("repo path is empty")
			}
			if r.BaseBranch.Name == "" {
				t.Errorf("repo %q has empty base branch", r.Path)
			}
		}
	})

	t.Run("Harnesses", func(t *testing.T) {
		var harnesses []v1.HarnessInfo
		getJSON(t, baseURL, "/api/caic/v1/server/harnesses", &harnesses)
		if len(harnesses) == 0 {
			t.Fatal("expected at least one harness")
		}
		found := false
		for _, h := range harnesses {
			if h.Name == v1.HarnessCodex {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q harness in list", harness.Codex)
		}
	})

	// --- Task lifecycle ---

	t.Run("TaskLifecycle", func(t *testing.T) {
		var repos []v1.Repo
		getJSON(t, baseURL, "/api/caic/v1/server/repos", &repos)

		var prefs v1.PreferencesResp
		getJSON(t, baseURL, "/api/caic/v1/server/preferences", &prefs)
		prefs.Settings.WellKnownCaches = nil
		prefs.Settings.CacheMappings = nil
		postJSON(t, baseURL, "/api/caic/v1/server/preferences", v1.UpdatePreferencesReq{Settings: prefs.Settings}, &prefs)

		// Create a task on codex so the fork below switches harness (codex -> pi).
		initialPrompt := "smoke test " + fmt.Sprint(time.Now().UnixNano())
		createReq := v1.CreateTaskReq{
			InitialPrompt: v1.Prompt{Text: initialPrompt},
			Repos:         []v1.RepoSpec{{Name: repos[0].Path}},
			Harness:       v1.HarnessCodex,
			RuntimeName:   smoketest.SmokeRuntime(),
		}
		var createResp v1.Task
		postJSON(t, baseURL, "/api/caic/v1/tasks", createReq, &createResp)
		taskID := createResp.ID.String()
		if taskID == "" {
			t.Fatal("create response has empty task ID")
		}
		t.Logf("created task %s", taskID)

		// Poll until the task reaches "waiting" after the in-container smoke
		// agent responds through the relay.
		task := waitForTaskState(t, smoke, taskID, "waiting")
		if task.NumTurns != 1 {
			t.Fatalf("task %s: NumTurns = %d, want 1; error=%q", taskID, task.NumTurns, task.Error)
		}
		t.Logf("task %s reached 'waiting'", taskID)

		logDir := filepath.Join(smoke.cfg.Dirs.CacheDir, "tasks")
		entries, err := os.ReadDir(logDir)
		if err != nil {
			t.Fatalf("read task logs: %v", err)
		}
		var logPath string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), taskID+"-") && strings.HasSuffix(entry.Name(), ".jsonl") {
				logPath = filepath.Join(logDir, entry.Name())
				break
			}
		}
		if logPath == "" {
			t.Fatalf("task %s has no raw log in %s", taskID, logDir)
		}
		logData, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read task log: %v", err)
		}
		for i, line := range strings.FieldsFunc(string(logData), func(r rune) bool { return r == '\n' }) {
			var record map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("decode task log line %d: %v", i+1, err)
			}
			if string(record["t"]) == "" {
				t.Fatalf("task log line %d has no task-log discriminator: %s", i+1, line)
			}
			if i == 0 && (string(record["t"]) != `"caic_meta"` || string(record["version"]) != "3") {
				t.Fatalf("task log header = %s, want v3 caic_meta", line)
			}
		}

		const resumePrompt = "resume after server restart"
		t.Run("ServerRestart", func(t *testing.T) {
			runtimeID := task.Runtime.ID
			baseURL = smoke.restart()

			restored := waitForTaskState(t, smoke, taskID, "waiting")
			if restored.Runtime.ID != runtimeID {
				t.Errorf("task %s: runtime ID = %q after restart, want %q", taskID, restored.Runtime.ID, runtimeID)
			}

			postJSON(t, baseURL, "/api/caic/v1/tasks/"+taskID+"/input", v1.InputReq{
				Prompt: v1.Prompt{Text: resumePrompt},
			}, nil)
			task = waitForTaskState(t, smoke, taskID, "waiting")
			if task.NumTurns != 2 {
				t.Errorf("task %s: NumTurns = %d after restart, want 2; error=%q", taskID, task.NumTurns, task.Error)
			}
		})

		disableSudo := false
		forkReq := v1.ForkTaskReq{
			// Switch harness on the fork so the test verifies the target
			// harness's env is injected, not the source harness's.
			Prompt:  v1.Prompt{Text: "fork smoke test " + fmt.Sprint(time.Now().UnixNano())},
			Harness: v1.HarnessPi,
			Sudo:    &disableSudo,
		}
		var forkResp v1.Task
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+taskID+"/fork", forkReq, &forkResp)
		forkID := forkResp.ID.String()
		if forkID == "" {
			t.Fatal("fork response has empty task ID")
		}
		forkTask := waitForTaskState(t, smoke, forkID, "waiting")
		if forkTask.NumTurns != 1 {
			t.Fatalf("fork task %s: NumTurns = %d, want 1; error=%q", forkID, forkTask.NumTurns, forkTask.Error)
		}
		t.Logf("fork task %s reached 'waiting'", forkID)

		forkRuntimeID := runtime.ID(forkTask.Runtime.ID)
		if forkRuntimeID == "" {
			t.Fatalf("fork task %s has no runtime ID before purge", forkID)
		}

		// The fork's ~/.env is rewritten from scratch by md, so the target
		// harness's env vars from config must be re-injected into the fork,
		// and the source harness's vars must not survive the rewrite. The two
		// harnesses use distinct marker values, so a fork that kept (or only
		// re-injected) the source harness's env fails one of the checks below.
		for _, c := range []struct {
			name, id, want, absent string
		}{
			{"source", string(runtime.ID(task.Runtime.ID).InstanceID()), smoke.runToken + "-codex", smoke.runToken + "-pi"},
			{"fork", string(forkRuntimeID.InstanceID()), smoke.runToken + "-pi", smoke.runToken + "-codex"},
		} {
			env, err := smoketest.ContainerFile(t.Context(), smoketest.SmokeRuntime(), c.id, "/home/user/.env")
			if err != nil {
				t.Fatalf("read %s container .env: %v", c.name, err)
			}
			if !strings.Contains(env, c.want) {
				t.Fatalf("%s container /home/user/.env missing harness env %s:\n%s", c.name, c.want, env)
			}
			if strings.Contains(env, c.absent) {
				t.Fatalf("%s container /home/user/.env has other harness's env %s:\n%s", c.name, c.absent, env)
			}
		}
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+forkID+"/stop", nil, nil)
		waitForTaskState(t, smoke, forkID, "stopped")
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+forkID+"/purge", nil, nil)
		waitForTaskState(t, smoke, forkID, "purged")
		forkPurgeCtx, forkPurgeCancel := context.WithTimeout(t.Context(), 2*time.Minute)
		if err := smoketest.WaitForRuntimeGone(forkPurgeCtx, smoketest.SmokeRuntime(), forkRuntimeID); err != nil {
			forkPurgeCancel()
			t.Fatal(err)
		}
		forkPurgeCancel()
		t.Logf("fork task %s reached 'purged'", forkID)

		// Stop the task.
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+taskID+"/stop", nil, nil)
		task = waitForTaskState(t, smoke, taskID, "stopped")
		t.Logf("task %s reached 'stopped'", taskID)

		// Purge the task.
		runtimeID := runtime.ID(task.Runtime.ID)
		if runtimeID == "" {
			t.Fatalf("task %s has no runtime ID before purge", taskID)
		}
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+taskID+"/purge", nil, nil)
		waitForTaskState(t, smoke, taskID, "purged")
		purgeCtx, purgeCancel := context.WithTimeout(t.Context(), 2*time.Minute)
		if err := smoketest.WaitForRuntimeGone(purgeCtx, smoketest.SmokeRuntime(), runtimeID); err != nil {
			purgeCancel()
			t.Fatal(err)
		}
		purgeCancel()
		t.Logf("task %s reached 'purged'", taskID)

		t.Run("TerminalHistoryRestart", func(t *testing.T) {
			history := func() string {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/api/caic/v1/tasks/"+taskID+"/raw_events", http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				data, readErr := io.ReadAll(resp.Body)
				closeErr := resp.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("terminal history status = %d, body = %q", resp.StatusCode, data)
				}
				return string(data)
			}
			first := history()
			baseURL = smoke.restart()
			waitForTaskState(t, smoke, taskID, "purged")
			second := history()
			for name, history := range map[string]string{"before restart": first, "after restart": second} {
				for _, want := range []string{"event: ready", initialPrompt, "smoke agent received: " + initialPrompt, resumePrompt, "smoke agent received: " + resumePrompt} {
					if !strings.Contains(history, want) {
						t.Fatalf("terminal raw history %s missing %q:\n%s", name, want, history)
					}
				}
			}
		})

	})

	// --- Frontend serving ---

	t.Run("Frontend", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/", http.NoBody)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			t.Errorf("GET /: status %d, want %d", resp.StatusCode, http.StatusOK)
			return
		}
		ct := resp.Header.Get("Content-Type")
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("close / response: %v", err)
		}
		if !strings.Contains(ct, "text/html") {
			t.Errorf("GET /: Content-Type %q, want text/html", ct)
		}
	})
}

type smokeServer struct {
	t        *testing.T
	rootDir  string
	cfg      *server.Config
	runToken string

	addr    string
	baseURL string
	cancel  context.CancelFunc
	done    chan error
}

func (s *smokeServer) close() {
	s.stop()
}

func (s *smokeServer) restart() string {
	s.stop()
	s.start()
	return s.baseURL
}

func (s *smokeServer) start() {
	ctx, cancel := context.WithCancel(s.t.Context())
	addr := s.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		cancel()
		s.t.Fatalf("listen: %v", err)
	}

	// CAIC_SMOKE_LOG writes the fixture's server logs to a file, which is how a
	// host-dependent smoke failure gets diagnosed.
	var logWriter io.Writer = io.Discard
	var logOptions *slog.HandlerOptions
	if path := os.Getenv("CAIC_SMOKE_LOG"); path != "" {
		logFile, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path comes from the test operator.
		if err != nil {
			s.t.Fatalf("open CAIC_SMOKE_LOG %s: %v", path, err)
		}
		s.t.Cleanup(func() {
			if err := logFile.Close(); err != nil {
				s.t.Error(err)
			}
		})
		logWriter = logFile
		logOptions = &slog.HandlerOptions{Level: slog.LevelDebug}
	}

	srv, err := app.New(ctx, slog.New(slog.NewTextHandler(logWriter, logOptions)), s.rootDir, s.cfg)
	if err != nil {
		cancel()
		if closeErr := ln.Close(); closeErr != nil {
			s.t.Fatalf("close listener after server.New failure: %v", closeErr)
		}
		s.t.Fatalf("server.New: %v", err)
	}

	s.addr = ln.Addr().String()
	s.baseURL = "http://" + s.addr
	s.cancel = cancel
	s.done = make(chan error, 1)
	go func() {
		s.done <- srv.Serve(ctx, ln)
	}()
	if err := waitForReady(ctx, s.baseURL); err != nil {
		s.stop()
		s.t.Fatalf("server not ready: %v", err)
	}
}

func (s *smokeServer) stop() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	err := <-s.done
	s.cancel = nil
	s.done = nil
	if err != nil && !errors.Is(err, context.Canceled) {
		s.t.Errorf("server.Serve: %v", err)
	}
}

// serverFixture configures one isolated real-runtime server fixture.
type serverFixture struct {
	// backends overrides the agent backends. Empty uses the standard caic set.
	backends map[harness.Name]agent.Backend
	// harnessEnv builds the per-harness container environment from the fixture's
	// run token.
	harnessEnv func(runToken string) map[string][]string
}

// startSmokeServer creates the deterministic real-runtime smoke fixture.
func startSmokeServer(t *testing.T) *smokeServer {
	// Use deterministic no-LLM agents, but run them inside real md containers
	// through the normal relay over SSH.
	sb := smoketest.NewSmokeBackend(harness.Codex)
	sbFork := smoketest.NewSmokeBackend(harness.Pi)
	return startServerFixture(t, serverFixture{
		backends: map[harness.Name]agent.Backend{sb.Harness(): sb, sbFork.Harness(): sbFork},
		harnessEnv: func(runToken string) map[string][]string {
			return map[string][]string{
				// Re-injected into each instance's ~/.env; the fork section of
				// the test verifies the fork gets the target harness's marker
				// after md rewrites ~/.env.
				string(sb.Harness()):     {"CAIC_SMOKE_FORK_ENV=" + runToken + "-codex"},
				string(sbFork.Harness()): {"CAIC_SMOKE_FORK_ENV=" + runToken + "-pi"},
			}
		},
	})
}

// startServerFixture creates an isolated real-runtime fixture and starts its
// initial server instance.
func startServerFixture(t *testing.T, fx serverFixture) *smokeServer {
	ctx := t.Context()

	// Create isolated temp dirs for config, cache, and md state.
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "config")
	cacheDir := filepath.Join(tmpDir, "cache")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}

	// Set XDG_CONFIG_HOME so md can write its keys in isolation.
	xdgDir := filepath.Join(tmpDir, "xdg")
	if err := os.MkdirAll(xdgDir, 0o700); err != nil {
		t.Fatalf("mkdir xdg: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdgDir)

	// Initialize local repos used by the runtime smoke task.
	clone, err := smoketest.InitSmokeRepos(ctx, tmpDir)
	if err != nil {
		t.Fatalf("init smoke repos: %v", err)
	}

	runToken, err := smoketest.NewSmokeRunToken()
	if err != nil {
		t.Fatalf("create smoke run token: %v", err)
	}

	harnessEnv := map[string][]string(nil)
	if fx.harnessEnv != nil {
		harnessEnv = fx.harnessEnv(runToken)
	}
	// Pre-populate the harness model cache so startup does not launch unrelated
	// model-refresh containers.
	if err := smoketest.InitSmokeHarnessCache(cacheDir); err != nil {
		t.Fatalf("init harness cache: %v", err)
	}

	s := &smokeServer{
		t:        t,
		rootDir:  filepath.Dir(clone),
		runToken: runToken,
		cfg: &server.Config{
			Dirs: server.DirsConfig{
				ConfigDir: configDir,
				CacheDir:  cacheDir,
			},
			Runtime: server.RuntimeConfig{
				SkipWarmup: true,
				Metadata:   runtime.Metadata{runtime.MetadataSmokeRun: runToken},
			},
			Agent: server.AgentConfig{
				Backends:   fx.backends,
				HarnessEnv: harnessEnv,
			},
			LLM: server.LLMConfig{
				Disable: true,
			},
			IPGeo: server.IPGeoConfig{
				Allowlist: "0.0.0.0/0,::/0",
			},
		},
	}
	t.Cleanup(s.close)
	t.Cleanup(func() {
		s.stop()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
		defer cancel()
		if err := smoketest.CleanupSmokeRunContainers(ctx, smoketest.SmokeRuntime(), runToken); err != nil {
			t.Errorf("cleanup smoke containers: %v", err)
		}
	})
	s.start()
	return s
}

// smokeTaskTimeout bounds one smoke task's wait for a state or a runtime.
const smokeTaskTimeout = 10 * time.Minute

// waitForTaskState polls the task list until the task reaches want.
func waitForTaskState(t *testing.T, s *smokeServer, taskID, want string) v1.Task {
	t.Helper()
	deadline := time.Now().Add(smokeTaskTimeout)
	for {
		task := findTask(t, s, taskID)
		if string(task.State) == want {
			return task
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s: timed out waiting for state %q, current %q error %q", taskID, want, task.State, task.Error)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitForTaskRuntime polls the task list until the task's container is assigned.
func waitForTaskRuntime(t *testing.T, s *smokeServer, taskID string) runtime.ID {
	t.Helper()
	deadline := time.Now().Add(smokeTaskTimeout)
	for {
		task := findTask(t, s, taskID)
		if task.Runtime.ID != "" {
			return runtime.ID(task.Runtime.ID)
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s: timed out waiting for a runtime instance", taskID)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// findTask returns the task list entry for taskID, or the zero task.
func findTask(t *testing.T, s *smokeServer, taskID string) v1.Task {
	t.Helper()
	var tasks []v1.Task
	getJSON(t, s.baseURL, "/api/caic/v1/tasks", &tasks)
	for _, task := range tasks {
		if task.ID.String() == taskID {
			return task
		}
	}
	return v1.Task{}
}

// waitForReady polls GET /api/caic/v1/server/config until it returns 200.
func waitForReady(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 50; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/caic/v1/server/config", http.NoBody)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for server at %s", baseURL)
}

// getJSON performs a GET request and decodes the JSON response into dst.
func getJSON(t *testing.T, baseURL, path string, dst any) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+path, http.NoBody)
	if err != nil {
		t.Fatalf("new request %s: %v", path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode %s: %v", path, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close GET %s: %v", path, err)
	}
}

// postJSON performs a POST request with a JSON body and decodes the response.
func postJSON(t *testing.T, baseURL, path string, reqBody, dst any) {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			t.Fatalf("marshal request for %s: %v", path, err)
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+path, body)
	if err != nil {
		t.Fatalf("new request %s: %v", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, respBody)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			_ = resp.Body.Close()
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close POST %s: %v", path, err)
	}
}
