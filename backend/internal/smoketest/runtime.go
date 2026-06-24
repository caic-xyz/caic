// Package smoketest provides fake runtime and repository fixtures for smoke and e2e tests.
package smoketest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// InitRepo creates two fake repos (clone and clone2) in tmpDir so that the
// add-repo button is visible after the first repo is auto-selected on load.
// Returns the path to the primary clone.
func InitRepo(ctx context.Context, tmpDir string) (string, error) {
	if err := initOneRepo(ctx, tmpDir, filepath.Join("remotes", "remote.git"), filepath.Join("repos", "clone")); err != nil {
		return "", err
	}
	if err := initOneRepo(ctx, tmpDir, filepath.Join("remotes", "remote2.git"), filepath.Join("repos", "clone2")); err != nil {
		return "", err
	}
	return filepath.Join(tmpDir, "repos", "clone"), nil
}

// InitHarnessCache pre-populates the harness model cache with fresh dummy
// entries so refreshHarnessModels skips launching temp containers for real
// harness model discovery during smoke and e2e tests.
func InitHarnessCache(cacheDir string) error {
	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
	for _, h := range []harness.Name{harness.Codex, harness.Pi, harness.OpenCode} {
		cache.SetModels(h, []string{"fake-model"}, "")
	}
	return nil
}

// RuntimeBackend implements runtime.Backend with no-op operations and canned
// process data.
type RuntimeBackend struct {
	vncPort int // non-zero when a fake VNC server is running.
}

var _ runtime.Backend = (*RuntimeBackend)(nil)
var _ runtime.Inventory = (*RuntimeBackend)(nil)
var _ runtime.Monitor = (*RuntimeBackend)(nil)
var _ runtime.PrivilegeInfo = (*RuntimeBackend)(nil)

// NewRuntimeBackend creates a fake runtime backend for smoke and e2e tests.
func NewRuntimeBackend(vncPort int) *RuntimeBackend {
	return &RuntimeBackend{vncPort: vncPort}
}

// Launch implements runtime.Backend.
func (*RuntimeBackend) Launch(_ context.Context, repos []runtime.Repo, _ *runtime.StartOptions) (runtime.InstanceID, error) {
	if len(repos) == 0 {
		return "md-test-no-repo", nil
	}
	return runtime.InstanceID("md-test-" + strings.ReplaceAll(repos[0].Branch, "/", "-")), nil
}

// Connect implements runtime.Backend.
func (*RuntimeBackend) Connect(_ context.Context, id runtime.InstanceID, _ *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	return runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: string(id)}}, nil
}

// Diff implements runtime.Backend.
func (*RuntimeBackend) Diff(_ context.Context, _ runtime.InstanceID, _ int, _ ...string) (string, error) {
	return "", nil
}

// Fetch implements runtime.Backend.
func (*RuntimeBackend) Fetch(_ context.Context, _ runtime.InstanceID) error { return nil }

// Stop implements runtime.Backend.
func (*RuntimeBackend) Stop(_ context.Context, _ runtime.InstanceID) error { return nil }

// Purge implements runtime.Backend.
func (*RuntimeBackend) Purge(_ context.Context, _ runtime.InstanceID) error { return nil }

// Revive implements runtime.Backend.
func (*RuntimeBackend) Revive(_ context.Context, _ runtime.InstanceID) error { return nil }

// Fork implements runtime.Backend.
func (*RuntimeBackend) Fork(_ context.Context, _ runtime.InstanceID, _ []runtime.Repo, _ *runtime.ForkOptions) (runtime.InstanceID, runtime.ConnectionInfo, []runtime.Repo, error) {
	return "fake-fork", runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: "fake-fork"}}, nil, errors.New("fork not supported in fake runtime")
}

// VNCPort implements runtime.Backend.
func (b *RuntimeBackend) VNCPort(_ context.Context, _ runtime.InstanceID) int { return b.vncPort }

// Processes implements runtime.Backend.
func (*RuntimeBackend) Processes(_ context.Context, _ runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	return fakeProcesses(), nil
}

// Signal implements runtime.Backend.
func (*RuntimeBackend) Signal(_ context.Context, _ runtime.InstanceID, _ int, _ string) error {
	return nil
}

// List implements runtime.Inventory.
func (*RuntimeBackend) List(context.Context) ([]runtime.Instance, error) {
	return nil, nil
}

// Metadata implements runtime.Inventory.
func (*RuntimeBackend) Metadata(context.Context, runtime.InstanceID, runtime.MetadataKey) (string, error) {
	return "", nil
}

// Inspect implements runtime.Inventory.
func (*RuntimeBackend) Inspect(_ context.Context, id runtime.InstanceID) (*runtime.InstanceInspect, error) {
	return &runtime.InstanceInspect{Runtime: "fake", ID: id, State: "running", OS: "linux", CPUArchitecture: stdruntime.GOARCH}, nil
}

// WatchStats implements runtime.Monitor.
func (*RuntimeBackend) WatchStats(ctx context.Context, _ []runtime.InstanceID) (iter.Seq2[runtime.StatsSample, error], error) {
	return func(func(runtime.StatsSample, error) bool) {
		<-ctx.Done()
	}, nil
}

// WatchEvents implements runtime.Monitor.
func (*RuntimeBackend) WatchEvents(ctx context.Context, _ runtime.EventFilter) (<-chan runtime.Event, error) {
	ch := make(chan runtime.Event)
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

// SudoPassword implements runtime.PrivilegeInfo.
func (*RuntimeBackend) SudoPassword(context.Context, runtime.InstanceID) (string, error) {
	return "", nil
}

// initOneRepo initialises a bare remote and a clone under tmpDir.
func initOneRepo(ctx context.Context, tmpDir, bareName, cloneName string) error {
	bare := filepath.Join(tmpDir, bareName)
	clone := filepath.Join(tmpDir, cloneName)
	if err := os.MkdirAll(filepath.Dir(bare), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		return err
	}
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

func runGit(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput() //nolint:gosec // args are hardcoded git subcommands
	if err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return nil
}

// fakeProcesses returns a canned process tree for e2e screenshots:
//
//	init(1)
//	  sshd(42)
//	    sshd-session(99)
//	      bash(100)
//	        node(200) - agent harness
//	        make(201)
//	          gcc(300)
//	          gcc(301)
//	        ps(202)
func fakeProcesses() []runtime.ProcessInfo {
	return []runtime.ProcessInfo{
		{PID: 1, PPID: 0, User: "root", State: "S", CPU: 0.0, Mem: 0.1, Time: "0:00", Command: "/sbin/init"},
		{PID: 42, PPID: 1, User: "root", State: "S", CPU: 0.0, Mem: 0.2, Time: "0:01", Command: "sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups"},
		{PID: 99, PPID: 42, User: "root", State: "S", CPU: 0.0, Mem: 0.3, Time: "0:00", Command: "sshd: user [priv]"},
		{PID: 100, PPID: 99, User: "user", State: "S", CPU: 0.1, Mem: 0.5, Time: "0:02", Command: "-bash"},
		{PID: 200, PPID: 100, User: "user", State: "R", CPU: 45.2, Mem: 12.3, Time: "1:23", Command: "node /home/user/.npm/_npx/abc123/node_modules/.bin/claude --dangerously-skip-permissions"},
		{PID: 201, PPID: 100, User: "user", State: "S", CPU: 0.0, Mem: 0.1, Time: "0:00", Command: "make -j$(nproc)"},
		{PID: 300, PPID: 201, User: "user", State: "R", CPU: 98.7, Mem: 5.6, Time: "0:45", Command: "/usr/lib/gcc/x86_64-linux-gnu/14/cc1 -quiet -Iinclude -D_FORTIFY_SOURCE=2 src/main.c -o /tmp/ccXyz.s"},
		{PID: 301, PPID: 201, User: "user", State: "R", CPU: 97.1, Mem: 4.8, Time: "0:42", Command: "/usr/lib/gcc/x86_64-linux-gnu/14/cc1 -quiet -Iinclude -D_FORTIFY_SOURCE=2 src/parser.c -o /tmp/ccAbc.s"},
		{PID: 202, PPID: 100, User: "user", State: "R", CPU: 0.3, Mem: 0.1, Time: "0:00", Command: "ps -eo pid,ppid,user,stat,%cpu,%mem,time,args"},
	}
}
