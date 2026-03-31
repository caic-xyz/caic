//go:build e2e

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claude"
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
	clone, err := initFakeRepo(tmpDir)
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
	cfg.CacheDir = filepath.Join(os.TempDir(), "caic-e2e-logs")
	srv, err := server.New(ctx, rootDir, cfg)
	if err != nil {
		return fmt.Errorf("new server: %w", err)
	}
	fb := &fakeBackend{}
	srv.SetRunnerOps(&fakeContainer{}, map[agent.Harness]agent.Backend{fb.Harness(): fb})

	err = srv.ListenAndServe(ctx, addr)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// initFakeRepo creates two fake repos (clone and clone2) in tmpDir so that the
// add-repo button is visible after the first repo is auto-selected on load.
// Returns the path to the primary clone.
func initFakeRepo(tmpDir string) (string, error) {
	if err := initOneRepo(tmpDir, "remote.git", "clone"); err != nil {
		return "", err
	}
	if err := initOneRepo(tmpDir, "remote2.git", "clone2"); err != nil {
		return "", err
	}
	return filepath.Join(tmpDir, "clone"), nil
}

// initOneRepo initialises a bare remote and a clone under tmpDir.
func initOneRepo(tmpDir, bareName, cloneName string) error {
	bare := filepath.Join(tmpDir, bareName)
	clone := filepath.Join(tmpDir, cloneName)
	for _, args := range [][]string{
		{"init", "--bare", bare},
		{"init", clone},
		{"-C", clone, "config", "user.name", "Test"},
		{"-C", clone, "config", "user.email", "test@test.com"},
		{"-C", clone, "checkout", "-b", "main"},
	} {
		if err := runGit(args...); err != nil {
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
		if err := runGit(args...); err != nil {
			return err
		}
	}
	return nil
}

func runGit(args ...string) error {
	out, err := exec.Command("git", args...).CombinedOutput() //nolint:gosec // args are hardcoded git subcommands
	if err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return nil
}

// fakeContainer implements task.ContainerBackend with no-op operations.
type fakeContainer struct{}

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

// fakeBackend implements agent.Backend with a shell process that emits
// streaming text deltas followed by complete messages, simulating
// --include-partial-messages output. It supports multiple turns: each
// line read from stdin triggers the next joke response, cycling through
// a fixed list.
type fakeBackend struct{}

var _ agent.Backend = (*fakeBackend)(nil)

func (*fakeBackend) Harness() agent.Harness { return "fake" }

func (*fakeBackend) Start(_ context.Context, opts *agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error) {
	cmd := exec.Command("python3", "-u", "-c", string(fake.Script)) //nolint:gosec // fake.Script is an embedded constant
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := agent.NewSession(cmd, stdin, stdout, msgCh, logW, claude.Wire, nil)
	if opts.InitialPrompt.Text != "" {
		if err := s.Send(opts.InitialPrompt); err != nil {
			s.Close()
			return nil, fmt.Errorf("write prompt: %w", err)
		}
	}
	return s, nil
}

func (*fakeBackend) AttachRelay(context.Context, *agent.Options, chan<- agent.Message, io.Writer) (*agent.Session, error) {
	return nil, errors.New("fake backend does not support relay")
}

func (*fakeBackend) ReadRelayOutput(context.Context, string) ([]agent.Message, int64, error) {
	return nil, 0, errors.New("fake backend does not support relay")
}

func (*fakeBackend) ParseMessage(line []byte) ([]agent.Message, error) {
	return claude.ParseMessage(line)
}

func (*fakeBackend) Models() []string { return []string{"fake-model"} }

func (*fakeBackend) SupportsImages() bool { return true }

func (*fakeBackend) ContextWindowLimit(string) int { return 180_000 }
