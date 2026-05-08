//go:build e2e

// Implements fake container and agent backends for e2e testing without real SSH or containers.

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/fake"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md"
)

const isFakeMode = true

// serveFake starts the HTTP server with fake container/agent ops and a temp
// git repo. Used for e2e testing without md CLI or SSH.
func serveFake(ctx context.Context, addr string, cfg *server.Config) (retErr error) {
	addr = localizeAddr(addr)

	// Always create a temp git repo — fake mode doesn't use real repos.
	tmpDir, err := os.MkdirTemp("", "caic-e2e-*")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(tmpDir)) }()
	clone, err := initFakeRepo(ctx, tmpDir)
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
	cfg.ConfigDir = fakeConfigDir
	fakeLogsDir, err := os.MkdirTemp("", "caic-e2e-logs-*")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(fakeLogsDir)) }()
	cfg.CacheDir = fakeLogsDir

	// Pre-populate the harness model cache so refreshHarnessModels skips
	// launching temporary containers for Pi and OpenCode model discovery.
	if err := initFakeHarnessCache(fakeLogsDir); err != nil {
		return fmt.Errorf("init fake harness cache: %w", err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer ln.Close()

	srv, err := server.New(ctx, rootDir, cfg)
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}

	// Start a fake VNC server serving a generated IDE screenshot.
	fvnc, err := startFakeVNC(ctx)
	if err != nil {
		return fmt.Errorf("start fake VNC: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, fvnc.Close()) }()

	fc := &fakeContainer{vncPort: fvnc.Port()}
	fb := fake.New()
	srv.SetRunnerOps(fc, map[agent.Harness]agent.Backend{fb.Harness(): fb})
	srv.SetUsageFetchers(fakeUsageFetchers())

	return srv.Serve(ctx, ln)
}

// initFakeRepo creates two fake repos (clone and clone2) in tmpDir so that the
// add-repo button is visible after the first repo is auto-selected on load.
// Returns the path to the primary clone.
func initFakeRepo(ctx context.Context, tmpDir string) (string, error) {
	if err := initOneRepo(ctx, tmpDir, "remote.git", "clone"); err != nil {
		return "", err
	}
	if err := initOneRepo(ctx, tmpDir, "remote2.git", "clone2"); err != nil {
		return "", err
	}
	return filepath.Join(tmpDir, "clone"), nil
}

// initOneRepo initialises a bare remote and a clone under tmpDir.
func initOneRepo(ctx context.Context, tmpDir, bareName, cloneName string) error {
	bare := filepath.Join(tmpDir, bareName)
	clone := filepath.Join(tmpDir, cloneName)
	for _, args := range [][]string{
		{"init", "--bare", bare},
		{"init", clone},
		{"-C", clone, "config", "user.name", "Test"},
		{"-C", clone, "config", "user.email", "test@test.com"},
		{"-C", clone, "checkout", "-b", "main"},
	} {
		if err := runGit(ctx, args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("hello\n"), 0o600); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"-C", clone, "add", "."},
		{"-C", clone, "commit", "-m", "init"},
		{"-C", clone, "remote", "add", "origin", bare},
		{"-C", clone, "push", "-u", "origin", "main"},
	} {
		if err := runGit(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

// initFakeHarnessCache pre-populates the harness model cache with fresh
// dummy entries so refreshHarnessModels skips launching temp containers
// for Pi and OpenCode model discovery during e2e tests.
func initFakeHarnessCache(cacheDir string) error {
	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
	for _, h := range []agent.Harness{agent.Pi, agent.OpenCode} {
		cache.SetModels(h, []string{"fake-model"})
	}
	return nil
}

func runGit(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput() //nolint:gosec // args are hardcoded git subcommands
	if err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return nil
}

// fakeContainer implements task.ContainerBackend with no-op operations.
type fakeContainer struct {
	vncPort int // non-zero when a fake VNC server is running.
}

var _ task.ContainerBackend = (*fakeContainer)(nil)

func (*fakeContainer) Launch(_ context.Context, repos []md.Repo, _ []string, _ *task.StartOptions) (string, error) {
	if len(repos) == 0 {
		return "md-test-no-repo", nil
	}
	return "md-test-" + strings.ReplaceAll(repos[0].Branch, "/", "-"), nil
}

func (*fakeContainer) Connect(_ context.Context, _ string, _ []md.Repo, _ *task.StartOptions) (string, error) {
	return "", nil
}

func (*fakeContainer) Diff(_ context.Context, _ md.Repo, _ ...string) (string, error) {
	return "", nil
}

func (*fakeContainer) Fetch(_ context.Context, _ []md.Repo) error            { return nil }
func (*fakeContainer) Stop(_ context.Context, _ string) error                { return nil }
func (*fakeContainer) Purge(_ context.Context, _ string, _ []md.Repo) error  { return nil }
func (*fakeContainer) Revive(_ context.Context, _ string, _ []md.Repo) error { return nil }

func (*fakeContainer) Fork(_ context.Context, _ string, _ []md.Repo, _ *task.ForkOptions) (string, []md.Repo, error) {
	return "fake-fork", nil, fmt.Errorf("fork not supported in fake mode")
}

func (fc *fakeContainer) VNCPort(_ context.Context, _ string) int { return fc.vncPort }
