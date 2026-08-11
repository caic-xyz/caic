// Runtime smoke test for the caic server with a real md container.

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
// exercise the task lifecycle.
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
			if h.Name == string(harness.Codex) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q harness in list", harness.Codex)
		}
	})

	// --- Task lifecycle ---

	t.Run("TaskLifecycle", func(t *testing.T) {
		// Get available repos and harnesses.
		var repos []v1.Repo
		getJSON(t, baseURL, "/api/caic/v1/server/repos", &repos)
		var harnesses []v1.HarnessInfo
		getJSON(t, baseURL, "/api/caic/v1/server/harnesses", &harnesses)

		var prefs v1.PreferencesResp
		getJSON(t, baseURL, "/api/caic/v1/server/preferences", &prefs)
		prefs.Settings.WellKnownCaches = nil
		prefs.Settings.CacheMappings = nil
		postJSON(t, baseURL, "/api/caic/v1/server/preferences", v1.UpdatePreferencesReq{Settings: prefs.Settings}, &prefs)

		// Create a task.
		createReq := v1.CreateTaskReq{
			InitialPrompt: v1.Prompt{Text: "smoke test " + fmt.Sprint(time.Now().UnixNano())},
			Repos:         []v1.RepoSpec{{Name: repos[0].Path}},
			Harness:       v1.Harness(harnesses[0].Name),
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
		waitForTaskState := func(id, want string) v1.Task {
			deadline := time.After(10 * time.Minute)
			var task v1.Task
			for {
				var tasks []v1.Task
				getJSON(t, baseURL, "/api/caic/v1/tasks", &tasks)
				for _, tk := range tasks {
					if tk.ID.String() == id {
						task = tk
						break
					}
				}
				if string(task.State) == want {
					return task
				}
				select {
				case <-deadline:
					t.Fatalf("task %s: timed out waiting for state %q, current: %q", id, want, task.State)
				case <-time.After(500 * time.Millisecond):
				}
			}
		}

		task := waitForTaskState(taskID, "waiting")
		if task.NumTurns != 1 {
			t.Fatalf("task %s: NumTurns = %d, want 1; error=%q", taskID, task.NumTurns, task.Error)
		}
		t.Logf("task %s reached 'waiting'", taskID)

		t.Run("ServerRestart", func(t *testing.T) {
			runtimeID := task.Runtime.ID
			baseURL = smoke.restart()

			restored := waitForTaskState(taskID, "waiting")
			if restored.Runtime.ID != runtimeID {
				t.Errorf("task %s: runtime ID = %q after restart, want %q", taskID, restored.Runtime.ID, runtimeID)
			}

			const resumePrompt = "resume after server restart"
			postJSON(t, baseURL, "/api/caic/v1/tasks/"+taskID+"/input", v1.InputReq{
				Prompt: v1.Prompt{Text: resumePrompt},
			}, nil)
			task = waitForTaskState(taskID, "waiting")
			if task.NumTurns != 2 {
				t.Errorf("task %s: NumTurns = %d after restart, want 2; error=%q", taskID, task.NumTurns, task.Error)
			}
		})

		disableSudo := false
		forkReq := v1.ForkTaskReq{
			Prompt: v1.Prompt{Text: "fork smoke test " + fmt.Sprint(time.Now().UnixNano())},
			Sudo:   &disableSudo,
		}
		var forkResp v1.Task
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+taskID+"/fork", forkReq, &forkResp)
		forkID := forkResp.ID.String()
		if forkID == "" {
			t.Fatal("fork response has empty task ID")
		}
		forkTask := waitForTaskState(forkID, "waiting")
		if forkTask.NumTurns != 1 {
			t.Fatalf("fork task %s: NumTurns = %d, want 1; error=%q", forkID, forkTask.NumTurns, forkTask.Error)
		}
		t.Logf("fork task %s reached 'waiting'", forkID)

		forkRuntimeID := runtime.ID(forkTask.Runtime.ID)
		if forkRuntimeID == "" {
			t.Fatalf("fork task %s has no runtime ID before purge", forkID)
		}
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+forkID+"/stop", nil, nil)
		waitForTaskState(forkID, "stopped")
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+forkID+"/purge", nil, nil)
		waitForTaskState(forkID, "purged")
		forkPurgeCtx, forkPurgeCancel := context.WithTimeout(t.Context(), 2*time.Minute)
		if err := smoketest.WaitForRuntimeGone(forkPurgeCtx, smoketest.SmokeRuntime(), forkRuntimeID); err != nil {
			forkPurgeCancel()
			t.Fatal(err)
		}
		forkPurgeCancel()
		t.Logf("fork task %s reached 'purged'", forkID)

		// Stop the task.
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+taskID+"/stop", nil, nil)
		task = waitForTaskState(taskID, "stopped")
		t.Logf("task %s reached 'stopped'", taskID)

		// Purge the task.
		runtimeID := runtime.ID(task.Runtime.ID)
		if runtimeID == "" {
			t.Fatalf("task %s has no runtime ID before purge", taskID)
		}
		postJSON(t, baseURL, "/api/caic/v1/tasks/"+taskID+"/purge", nil, nil)
		waitForTaskState(taskID, "purged")
		purgeCtx, purgeCancel := context.WithTimeout(t.Context(), 2*time.Minute)
		if err := smoketest.WaitForRuntimeGone(purgeCtx, smoketest.SmokeRuntime(), runtimeID); err != nil {
			purgeCancel()
			t.Fatal(err)
		}
		purgeCancel()
		t.Logf("task %s reached 'purged'", taskID)
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
	t       *testing.T
	rootDir string
	cfg     *server.Config

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

	srv, err := app.New(ctx, s.rootDir, s.cfg)
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

// startSmokeServer creates an isolated real-runtime smoke fixture and starts
// its initial server instance.
func startSmokeServer(t *testing.T) *smokeServer {
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

	// Pre-populate harness model cache so startup does not launch unrelated
	// model-refresh containers. The task below still launches a real md
	// container and agent relay.
	if err := smoketest.InitSmokeHarnessCache(cacheDir); err != nil {
		t.Fatalf("init harness cache: %v", err)
	}

	// Use a deterministic no-LLM agent, but run it inside the real md
	// container through the normal relay over SSH.
	sb := smoketest.NewSmokeBackend()
	s := &smokeServer{
		t:       t,
		rootDir: filepath.Dir(clone),
		cfg: &server.Config{
			Dirs: server.DirsConfig{
				ConfigDir: configDir,
				CacheDir:  cacheDir,
			},
			Runtime: server.RuntimeConfig{
				SkipWarmup: true,
			},
			Agent: server.AgentConfig{
				Backends: map[harness.Name]agent.Backend{sb.Harness(): sb},
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
	s.start()
	return s
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
