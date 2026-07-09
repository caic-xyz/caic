// Shared test doubles and fixtures for the task package's Runner and
// SessionRunner tests: fake agent backends, a fake runtime.Backend, and git
// test-repo helpers.

package task

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/repo/repowork"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
)

// testBackend implements agent.Backend for tests. It launches a process that
// reads one line from stdin then exits. capturedCtx records the context passed
// to Start so tests can assert context lifetime.
type testBackend struct {
	*agenttest.FakeBackend

	capturedCtx  context.Context
	capturedOpts agent.Options
}

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

func newTestSessionRunner(t *testing.T, workspace *repowork.Workspace, logDir string, backends map[harness.Name]agent.Backend) *SessionRunner {
	if workspace == nil {
		workspace = &repowork.Workspace{GitTimeout: time.Minute, Log: logtest.Logger(t)}
	}
	return &SessionRunner{Backends: backends, Workspace: workspace, Logs: &LogStore{LogDir: logDir}}
}

// testContainer returns a fake runtime whose Diff reports a fixed one-file
// numstat, matching the diff the runner/session tests parse.
func testContainer() *runtimetest.FakeBackend {
	return &runtimetest.FakeBackend{DiffOutput: "5\t1\tmain.go\n"}
}

// fetchRecorder is a fake runtime that additionally records whether Fetch was
// called, for the few tests that must distinguish a diff computed after a
// fetch from one computed without.
type fetchRecorder struct {
	*runtimetest.FakeBackend

	fetched atomic.Bool
}

func (b *fetchRecorder) Fetch(ctx context.Context, id runtime.InstanceID) error {
	b.fetched.Store(true)
	return b.FakeBackend.Fetch(ctx, id)
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
