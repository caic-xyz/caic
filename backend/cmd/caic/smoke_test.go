// End-to-end smoke test for the caic server with fake backends.

// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

//go:build smoke

package main

import (
	"context"
	"encoding/json"
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
	v1 "github.com/caic-xyz/caic/backend/internal/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/smoketest"
)

// TestSmoke verifies end-to-end: start the server with fake backends, query
// API endpoints, create a task, and exercise the full task lifecycle.
func TestSmoke(t *testing.T) {
	baseURL, cancel := startSmokeServer(t)
	defer cancel()

	// --- API endpoints ---

	t.Run("Config", func(t *testing.T) {
		var cfg v1.Config
		getJSON(t, baseURL, "/api/v1/server/config", &cfg)
		// Version is empty in dev builds (no ldflags), so only check that the
		// endpoint returns successfully.
		if cfg.DisplayName == "" {
			t.Error("config.DisplayName is empty")
		}
	})

	t.Run("Repos", func(t *testing.T) {
		var repos []v1.Repo
		getJSON(t, baseURL, "/api/v1/server/repos", &repos)
		if len(repos) == 0 {
			t.Fatal("expected at least one repo")
		}
		// Fake mode creates two repos (clone and clone2).
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
		getJSON(t, baseURL, "/api/v1/server/harnesses", &harnesses)
		if len(harnesses) == 0 {
			t.Fatal("expected at least one harness")
		}
		// Fake mode registers the "fake" harness.
		found := false
		for _, h := range harnesses {
			if h.Name == "fake" {
				found = true
			}
		}
		if !found {
			t.Error("expected 'fake' harness in list")
		}
	})

	// --- Task lifecycle ---

	t.Run("TaskLifecycle", func(t *testing.T) {
		// Get available repos and harnesses.
		var repos []v1.Repo
		getJSON(t, baseURL, "/api/v1/server/repos", &repos)
		var harnesses []v1.HarnessInfo
		getJSON(t, baseURL, "/api/v1/server/harnesses", &harnesses)

		// Create a task.
		createReq := v1.CreateTaskReq{
			InitialPrompt: v1.Prompt{Text: "smoke test " + fmt.Sprint(time.Now().UnixNano())},
			Repos:         []v1.RepoSpec{{Name: repos[0].Path}},
			Harness:       v1.Harness(harnesses[0].Name),
		}
		var createResp v1.CreateTaskResp
		postJSON(t, baseURL, "/api/v1/tasks", createReq, &createResp)
		taskID := createResp.ID.String()
		if taskID == "" {
			t.Fatal("create response has empty task ID")
		}
		t.Logf("created task %s", taskID)

		// Poll until the task reaches "waiting" (fake agent exits after
		// consuming stdin).
		var task v1.Task
		waitForState := func(want string) {
			deadline := time.After(30 * time.Second)
			for {
				var tasks []v1.Task
				getJSON(t, baseURL, "/api/v1/tasks", &tasks)
				for _, tk := range tasks {
					if tk.ID.String() == taskID {
						task = tk
						break
					}
				}
				if string(task.State) == want {
					return
				}
				select {
				case <-deadline:
					t.Fatalf("task %s: timed out waiting for state %q, current: %q", taskID, want, task.State)
				case <-time.After(500 * time.Millisecond):
				}
			}
		}

		// The fake agent should transition to "waiting" once it finishes.
		waitForState("waiting")
		t.Logf("task %s reached 'waiting'", taskID)

		// Stop the task.
		postJSON(t, baseURL, "/api/v1/tasks/"+taskID+"/stop", nil, nil)
		waitForState("stopped")
		t.Logf("task %s reached 'stopped'", taskID)

		// Purge the task.
		postJSON(t, baseURL, "/api/v1/tasks/"+taskID+"/purge", nil, nil)
		waitForState("purged")
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
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /: status %d, want %d", resp.StatusCode, http.StatusOK)
		}
		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Errorf("GET /: Content-Type %q, want text/html", ct)
		}
	})
}

// startSmokeServer starts the caic HTTP server with fake backends and returns
// the base URL. The caller must defer cancel() to shut down the server.
func startSmokeServer(t *testing.T) (string, context.CancelFunc) {
	ctx, cancel := context.WithCancel(t.Context())

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

	// Initialize fake repos.
	clone, err := smoketest.InitRepo(ctx, tmpDir)
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	rootDir := filepath.Dir(clone)

	// Pre-populate harness model cache.
	if err := smoketest.InitHarnessCache(cacheDir); err != nil {
		t.Fatalf("InitHarnessCache: %v", err)
	}

	cfg := &server.Config{
		ConfigDir:      configDir,
		CacheDir:       cacheDir,
		SkipWarmup:     true,
		DisableLLM:     true,
		Runtime:        "docker",
		IPGeoAllowlist: "0.0.0.0/0,::/0",
	}

	// Listen on a random port.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	srv, err := server.New(ctx, rootDir, cfg)
	if err != nil {
		ln.Close()
		t.Fatalf("server.New: %v", err)
	}

	// Inject fake backends.
	fc := smoketest.NewRuntimeBackend(0)
	fb := smoketest.NewFakeBackend()
	srv.SetRunnerBackends(fc, map[agent.Harness]agent.Backend{fb.Harness(): fb})
	srv.SetUsageFetchers(smoketest.UsageFetchers())
	srv.SetFakeCI(smoketest.SimulateCI)

	// Start serving in background.
	go func() {
		if err := srv.Serve(ctx, ln); err != nil && ctx.Err() == nil {
			t.Errorf("server.Serve: %v", err)
		}
	}()

	// Wait for the server to be ready.
	baseURL := "http://" + addr
	if err := waitForReady(ctx, baseURL); err != nil {
		cancel()
		t.Fatalf("server not ready: %v", err)
	}
	return baseURL, cancel
}

// waitForReady polls GET /api/v1/server/config until it returns 200.
func waitForReady(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 50; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/server/config", http.NoBody)
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode %s: %v", path, err)
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, respBody)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
}
