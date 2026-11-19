// Shared task test doubles and fixtures: fake agent backends, a fake runtime
// lifecycle, and git test-repo helpers.

package taskrun

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/gittest"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// mustNewTask calls NewTask and fails t if it returns an error. baseImage,
// containerPlatform, and title are hardcoded empty: no test in this package
// needs a non-default value. An empty harness selects the fixture's configured Claude backend.
func mustNewTask(t testing.TB, id ksid.ID, prompt agent.Prompt, h harness.Name, model, effort string) *Task {
	if h == "" {
		h = harness.Claude
	}
	tk, err := NewTask(id, prompt, h, model, effort, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// setState puts tk in published state s as of now, as taskruntest.SetState
// does.
func setState(tk *Task, s task.State) {
	setStateAt(tk, s, time.Time{})
}

// setStateAt puts tk in published state s as of at (zero means now) and keeps
// a task already in s. A lifecycle phase replaces the current one. Waiting
// ends a running turn, or starts a session that reports no turn. Running,
// asking, and has_plan start a session and fold a turn, a question, or a plan
// into it without adding them to the timeline.
func setStateAt(tk *Task, s task.State, at time.Time) {
	if tk.GetState() == s {
		return
	}
	var turn []agent.Message
	switch s {
	case task.StatePending, task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateStopping, task.StateStopped, task.StatePurging, task.StatePurged, task.StateCrashed, task.StateFailed:
		tk.Restore(s, at)
		return
	case task.StateWaiting:
		if tk.SessionLost(at) {
			return
		}
	case task.StateRunning:
		turn = []agent.Message{&agent.TurnStartedMessage{}}
	case task.StateAsking:
		turn = []agent.Message{&agent.TurnStartedMessage{}, &agent.AskMessage{ToolUseID: "question"}}
	case task.StateHasPlan:
		turn = []agent.Message{
			&agent.TurnStartedMessage{},
			&agent.ToolUseMessage{ToolUseID: "write", Name: "Write", Input: json.RawMessage(`{"file_path":"/root/.claude/plans/plan.md","content":"plan"}`)},
			&agent.ToolUseMessage{ToolUseID: "exit", Name: "ExitPlanMode"},
			&agent.ResultMessage{},
		}
	default:
		panic("unsupported state " + string(s))
	}
	tk.Restore(task.StateStarting, at)
	tk.mu.Lock()
	for _, m := range turn {
		tk.foldMessageLocked(m, at, at, false)
	}
	tk.mu.Unlock()
	tk.GoLive()
}

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
	session := agent.NewSession(ctx, cmd, agent.NewConn(ctx, opts.Logger, stdin, opts.Records, nil), stdout, opts.Logger)
	if opts.InitialPrompt.Text != "" || len(opts.InitialPrompt.Images) > 0 {
		if err := session.SendPrompt(opts.InitialPrompt); err != nil {
			session.Cancel()
			return nil, err
		}
	}
	return session, nil
}

// NewWire matches the Claude parser used by testBackend's live connection.
func (*testBackend) NewWire() agent.WireFormat { return claudecode.New().NewWire() }

// NewMetadataParser reconstructs the same fake connection without a transcript.
func (*testBackend) NewMetadataParser() agent.MessageParser {
	return claudecode.New().NewMetadataParser()
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
	return agent.NewSession(ctx, cmd, agent.NewConn(ctx, opts.Logger, stdin, opts.Records, nil), stdout, opts.Logger), nil
}

// testContainer returns a fake runtime whose Diff and compact status probe
// report a fixed one-file numstat, matching the diff the agent-runtime/session
// tests parse.
func testContainer() *runtimetest.FakeBackend {
	return &runtimetest.FakeBackend{
		DiffOutput:        "5\t1\tmain.go\n",
		TurnSnapshotValue: []runtime.TurnRepository{{RepoIndex: 0, Status: runtime.RepositoryStatus{DiffStat: []runtime.GitFileStat{{Path: "main.go", LinesAdded: 5, LinesDeleted: 1}}}}},
		RepositoryStatusValue: runtime.RepositoryStatus{
			DiffStat: []runtime.GitFileStat{{Path: "main.go", LinesAdded: 5, LinesDeleted: 1}},
		},
	}
}

// fetchRecorder is a fake runtime that additionally records whether Fetch was
// called, for the few tests that must distinguish a diff computed after a
// fetch from one computed without.
type fetchRecorder struct {
	*runtimetest.FakeBackend

	fetched atomic.Bool
}

func (b *fetchRecorder) Fetch(ctx context.Context, id runtime.ID, opts runtime.FetchOpts) ([]runtime.FetchedBranch, error) {
	b.fetched.Store(true)
	return b.FakeBackend.Fetch(ctx, id, opts)
}

func (b *fetchRecorder) TurnSnapshot(ctx context.Context, id runtime.ID, previous []runtime.FetchedBranch) ([]runtime.TurnRepository, error) {
	b.fetched.Store(true)
	return b.FakeBackend.TurnSnapshot(ctx, id, previous)
}

// messageCursor reads, in order, the messages appended to a task timeline
// after the cursor was created.
type messageCursor struct {
	tk   *Task
	wake <-chan struct{}
	next int
}

func newMessageCursor(t *testing.T, tk *Task) *messageCursor {
	wake, unsub := tk.SubscribeTimelineWakeups(t.Context())
	t.Cleanup(unsub)
	return &messageCursor{tk: tk, wake: wake, next: tk.MessageCount()}
}

// recv returns the next message and fails the test after one second.
func (c *messageCursor) recv(t *testing.T) agent.TimedMessage {
	m, ok := c.recvWithin(time.Second)
	if !ok {
		t.Fatal("timed out waiting for message")
	}
	return m
}

// recvWithin returns the next message, or false when none is appended within
// wait. A zero wait only reports a message that is already appended.
func (c *messageCursor) recvWithin(wait time.Duration) (agent.TimedMessage, bool) {
	timeout := time.After(wait)
	for {
		c.tk.mu.Lock()
		if c.next < c.tk.timelineLenLocked() {
			m := c.tk.timelineViewLocked().At(c.next)
			c.next++
			c.tk.mu.Unlock()
			return m, true
		}
		c.tk.mu.Unlock()
		select {
		case <-c.wake:
		case <-timeout:
			return agent.TimedMessage{}, false
		}
	}
}

// initTestRepo creates a bare "remote" and a local clone with one commit on
// main. Returns the clone directory. origin points to the bare repo so git
// fetch/push work locally.
func initTestRepo(t testing.TB) string {
	dir := t.TempDir()
	clone := filepath.Join(dir, "clone")
	if err := gittest.InitClonedRepo(t.Context(), filepath.Join(dir, "remote.git"), clone, "main"); err != nil {
		t.Fatal(err)
	}
	return clone
}

func runGit(t testing.TB, dir string, args ...string) {
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test helper with controlled args
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
