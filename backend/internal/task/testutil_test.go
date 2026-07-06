// Shared test doubles and fixtures for the task package's Runner and
// SessionRunner tests: fake agent backends, a fake runtime.Backend, and git
// test-repo helpers.

package task

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo/repowork"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// testBackend implements agent.Backend for tests. It launches a process that
// reads one line from stdin then exits. capturedCtx records the context passed
// to Start so tests can assert context lifetime.
type testBackend struct {
	capturedCtx  context.Context
	capturedOpts agent.Options
}

func (b *testBackend) Harness() harness.Name { return "test" }

func (b *testBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	b.capturedCtx = ctx
	b.capturedOpts = *opts
	// Read one line from stdin then exit. Session.Stop writes \x00\n which
	// satisfies the read, making Stop return immediately instead of timing out.
	cmd := exec.CommandContext(ctx, "python3", "-c", "input()")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return agent.NewSession(cmd, agent.NewConn(stdin, opts.LogW, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, opts.MsgCh, nil), nil
}

func (b *testBackend) AttachRelay(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("test backend does not support relay")
}

func (b *testBackend) ReadRelayOutput(context.Context, string) ([]agent.Message, int64, error) {
	return nil, 0, errors.New("test backend does not support relay")
}

func (b *testBackend) Models() []string   { return []string{"test-model"} }
func (b *testBackend) SetModels([]string) {}

// SupportsImages always returns false in the test backend.
func (b *testBackend) SupportsImages() bool { return false }

func (b *testBackend) AgentArgs(agent.HarnessArgs) []string { return nil }

func (b *testBackend) NewWire() agent.WireFormat { return claudecode.New() }

func (b *testBackend) SupportsCompact() bool { return false }

func (b *testBackend) ContextWindowLimit(string) int { return 180_000 }

type attachCaptureBackend struct {
	testBackend

	capturedAttachOpts agent.Options
}

func (b *attachCaptureBackend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	b.capturedAttachOpts = *opts
	cmd := exec.CommandContext(ctx, "python3", "-c", "import sys; sys.stdin.buffer.read()")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return agent.NewSession(cmd, agent.NewConn(stdin, opts.LogW, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, opts.MsgCh, nil), nil
}

// testWire implements agent.WireFormat for testing.
type testWire struct {
	parse func([]byte) ([]agent.Message, error)
}

func (*testWire) WritePrompt(w io.Writer, p agent.Prompt, logW io.Writer) error {
	msg := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{Type: "user"}
	msg.Message.Role = "user"
	msg.Message.Content = p.Text
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	if logW != nil {
		_, _ = logW.Write(data)
	}
	return nil
}

func (w *testWire) ParseMessage(line []byte) ([]agent.Message, error) {
	return w.parse(line)
}

func newTestSessionRunner(workspace *repowork.Workspace, logDir string, backends map[harness.Name]agent.Backend) *SessionRunner {
	return &SessionRunner{Backends: backends, Workspace: workspace, Logs: &LogStore{LogDir: logDir}}
}

// stubContainer implements runtime.Backend for testing. Diff returns a fixed
// numstat line; Fetch records that it was called.
type stubContainer struct {
	fetched   bool
	launchErr error // If set, Launch returns this error.
	fetchErr  error // If set, Fetch returns this error.
	stopped   bool
	diffIDs   []runtime.InstanceID
	fetchIDs  []runtime.InstanceID
	diffIdxs  []int
}

func (s *stubContainer) Launch(_ context.Context, _ []runtime.Repo, _ *runtime.StartOptions) (runtime.InstanceID, error) {
	if s.launchErr != nil {
		return "", s.launchErr
	}
	return "stub", nil
}

func (s *stubContainer) Connect(_ context.Context, id runtime.InstanceID, _ *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	return runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: string(id)}}, nil
}

func (s *stubContainer) Diff(_ context.Context, id runtime.InstanceID, repoIdx int, _ ...string) (string, error) {
	s.diffIDs = append(s.diffIDs, id)
	s.diffIdxs = append(s.diffIdxs, repoIdx)
	return "5\t1\tmain.go\n", nil
}

func (s *stubContainer) Fetch(_ context.Context, id runtime.InstanceID) error {
	s.fetched = true
	s.fetchIDs = append(s.fetchIDs, id)
	if s.fetchErr != nil {
		return s.fetchErr
	}
	return nil
}

func (s *stubContainer) Stop(_ context.Context, _ runtime.InstanceID) error {
	s.stopped = true
	return nil
}
func (s *stubContainer) Purge(_ context.Context, _ runtime.InstanceID) error {
	return nil
}
func (s *stubContainer) Revive(_ context.Context, _ runtime.InstanceID) error {
	return nil
}

func (s *stubContainer) Fork(_ context.Context, _ runtime.InstanceID, _ []runtime.Repo, _ *runtime.ForkOptions) (runtime.InstanceID, runtime.ConnectionInfo, []runtime.Repo, error) {
	return "stub-fork", runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: "stub-fork"}}, nil, nil
}
func (s *stubContainer) VNCPort(_ context.Context, _ runtime.InstanceID) int { return 0 }

func (s *stubContainer) Processes(_ context.Context, _ runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	return nil, nil
}

func (s *stubContainer) Signal(_ context.Context, _ runtime.InstanceID, _ int, _ string) error {
	return nil
}

// recvMsg reads a single message from ch, respecting the test context and a
// 1-second safety timeout.
func recvMsg(t *testing.T, ch <-chan agent.Message) agent.Message {
	select {
	case m := <-ch:
		return m
	case <-t.Context().Done():
		t.Fatal("test context canceled waiting for message")
		return nil
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
		return nil
	}
}

// initTestRepo creates a bare "remote" and a local clone with one commit on
// baseBranch. Returns the clone directory. origin points to the bare repo so
// git fetch/push work locally.
func initTestRepo(t *testing.T, baseBranch string) string { //nolint:unparam // baseBranch is parameterized for clarity.
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	runGit(t, "", "init", "--bare", bare)
	runGit(t, "", "init", clone)
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "checkout", "-b", baseBranch)

	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-m", "init")
	runGit(t, clone, "remote", "add", "origin", bare)
	runGit(t, clone, "push", "-u", "origin", baseBranch)
	return clone
}

func runGit(t *testing.T, dir string, args ...string) {
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test helper with controlled args
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
