// Implements fake container and agent backends for e2e testing without real SSH or containers.

//go:build e2e

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/app"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/smoketest"
)

const isFakeMode = true

// serveFake starts the HTTP server with fake container/agent ops and a temp
// git repo. Used for e2e testing without md CLI or SSH.
func serveFake(ctx context.Context, addr string, cfg *server.Config, traceFile string) (retErr error) {
	addr = localizeAddr(addr)

	// Always create a temp git repo; fake mode doesn't use real repos.
	tmpDir, err := os.MkdirTemp("", "caic-e2e-*")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(tmpDir)) }()
	clone, err := smoketest.InitRepo(ctx, tmpDir)
	if err != nil {
		return fmt.Errorf("init fake repo: %w", err)
	}
	rootDir := filepath.Dir(clone)

	// Use a temp dir for XDG_CONFIG_HOME so md can write its keys without
	// hitting the read-only ~/.config/md mount in the dev container.
	mdConfigDir, err := os.MkdirTemp("", "caic-e2e-md-*")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(mdConfigDir)) }()
	if err := os.Setenv("XDG_CONFIG_HOME", mdConfigDir); err != nil {
		return fmt.Errorf("set XDG_CONFIG_HOME: %w", err)
	}
	// Override config/cache dirs for the fake server.
	fakeConfigDir, err := os.MkdirTemp("", "caic-e2e-cfg-*")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(fakeConfigDir)) }()
	cfg.Dirs.ConfigDir = fakeConfigDir
	fakeLogsDir, err := os.MkdirTemp("", "caic-e2e-logs-*")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(fakeLogsDir)) }()
	cfg.Dirs.CacheDir = fakeLogsDir
	cfg.LLM.Disable = true

	// Start a fake VNC server serving a generated IDE screenshot.
	fvnc, err := smoketest.StartVNC(ctx)
	if err != nil {
		return fmt.Errorf("start fake VNC: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, fvnc.Close()) }()

	fc := smoketest.NewRuntimeBackend(fvnc.Port())
	cfg.Runtime.System = fc
	fb := smoketest.NewFakeBackend()
	cfg.Agent.Backends = map[harness.Name]agent.Backend{fb.Harness(): fb}

	// If a trace file is specified, copy it to the tasks log directory so it
	// gets loaded as a purged task on startup.
	if traceFile != "" {
		absTrace, err := filepath.Abs(traceFile)
		if err != nil {
			return fmt.Errorf("resolve trace path: %w", err)
		}
		if _, err := os.Stat(absTrace); err != nil {
			return fmt.Errorf("trace file not found: %w", err)
		}
		tasksDir := filepath.Join(fakeLogsDir, "tasks")
		if err := os.MkdirAll(tasksDir, 0o750); err != nil {
			return fmt.Errorf("create tasks dir: %w", err)
		}
		dst := filepath.Join(tasksDir, "trace-replay-fake-fake.jsonl")
		if err := os.Symlink(absTrace, dst); err != nil {
			return fmt.Errorf("symlink trace file: %w", err)
		}
		slog.InfoContext(ctx, "preloaded trace file", "src", absTrace, "dst", dst)
	}

	// Pre-populate the harness model cache so refreshHarnessModels skips
	// launching temporary containers for real harness model discovery.
	if err := smoketest.InitHarnessCache(fakeLogsDir); err != nil {
		return fmt.Errorf("init fake harness cache: %w", err)
	}
	cfg.Runtime.SkipWarmup = true
	cfg.FakeCI = smoketest.SimulateCI
	cfg.UsageFetchers = smoketest.UsageFetchers()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer ln.Close()

	srv, err := app.New(ctx, rootDir, cfg)
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}

	return srv.Serve(ctx, ln)
}
