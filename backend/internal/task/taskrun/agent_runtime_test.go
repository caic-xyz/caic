// Tests and benchmarks ordered recording, task adoption, timeline restoration, session lifetimes, and Git-summary publication.

package taskrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentbackends "github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/klauspost/compress/zstd"
	"github.com/maruel/genai"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/mcp/mcptest"
	"github.com/maruel/ksid"

	"bufio"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/metrics"

	// statusFailingRuntime fails the compact status probe, simulating the
	// transient container probe failures the task card must survive.
	"log/slog"
)

type statusFailingRuntime struct {
	*runtimetest.FakeBackend
}

func (*statusFailingRuntime) CompactRepositoryStatus(context.Context, runtime.ID, int) (runtime.RepositoryStatus, error) {
	return runtime.RepositoryStatus{}, errors.New("probe failed")
}

// titleProviderSpy records title requests and returns a replacement title.
type titleProviderSpy struct {
	genai.Provider

	calls chan struct{}
}

func (p *titleProviderSpy) GenSync(context.Context, genai.Messages, ...genai.GenOption) (genai.Result, error) {
	p.calls <- struct{}{}
	return genai.Result{Message: genai.NewTextMessage("replacement title")}, nil
}

// instantExitBackend embeds testBackend but spawns a process that exits
// immediately (no stdin read), so tests exercising EnsureSession's
// already-done branch don't have to wait out its 10-second liveness timer.
// Start waits for the exit so scheduling cannot race the readiness timeout.
type instantExitBackend struct {
	testBackend
}

func (b *instantExitBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	b.capturedCtx = ctx
	b.capturedOpts = *opts
	cmd := exec.CommandContext(ctx, "true")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := agent.NewSession(ctx, cmd, agent.NewConn(ctx, opts.Logger, stdin, opts.Records, nil), stdout, opts.Logger)
	<-s.Done()
	return s, nil
}

type reviveCaptureBackend struct {
	testBackend

	starts []agent.Options
}

func (b *reviveCaptureBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	b.starts = append(b.starts, *opts)
	return b.testBackend.Start(ctx, opts)
}

// reviveEnsureFailureBackend lets the resumed session exit, then rejects the
// idle replacement started by EnsureSession.
type reviveEnsureFailureBackend struct {
	instantExitBackend

	starts int
}

func (b *reviveEnsureFailureBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	b.starts++
	if b.starts > 1 {
		return nil, errors.New("idle session start failed")
	}
	return b.instantExitBackend.Start(ctx, opts)
}

type reviveFailureRuntime struct {
	*runtimetest.FakeBackend
}

func (*reviveFailureRuntime) Revive(context.Context, runtime.ID) error {
	return errors.New("runtime revive failed")
}

type startupFailingBackend struct {
	*agenttest.FakeBackend

	err error
}

func (b *startupFailingBackend) Start(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, b.err
}

// synchronousPromptBackend finishes a turn before Start returns, reproducing
// harnesses that can respond while their initial prompt is still being sent.
type synchronousPromptBackend struct {
	testBackend

	completed <-chan struct{}
}

func (b *synchronousPromptBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	cmd := exec.CommandContext(ctx, "sleep", "60")
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	stdoutReader, nativeOutput := io.Pipe()
	w := taskInputWriter(func(p []byte) (int, error) {
		_, err := io.WriteString(nativeOutput, `{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}}`+"\n"+`{"t":"agent","ts":1.001,"msg":{"type":"result","result":"done","num_turns":1}}`+"\n")
		if err != nil {
			return 0, err
		}
		return len(p), nil
	})
	session := agent.NewSession(ctx, cmd, agent.NewConn(ctx, opts.Logger, w, opts.Records, nil), stdoutReader, opts.Logger)
	context.AfterFunc(ctx, func() { _ = nativeOutput.Close() })

	if err := session.SendPrompt(opts.InitialPrompt); err != nil {
		return nil, err
	}
	select {
	case <-b.completed:
		return session, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type testRuntimeSystem struct {
	testRuntimeBackend
	runtimetest.FakeInfo
}

func (*testRuntimeSystem) Name() runtime.Name { return "test-runtime" }

type testRuntimeBackend interface {
	runtime.Files
	runtime.Lifecycle
	runtime.Repository
}

var testCheckoutRuntimes sync.Map

type metadataRuntime struct {
	*runtimetest.FakeBackend

	metadata runtime.Metadata
}

func (r *metadataRuntime) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.ID, error) {
	r.metadata = maps.Clone(opts.Metadata)
	return r.FakeBackend.Launch(ctx, repos, opts)
}

type branchCheckingRuntime struct {
	*runtimetest.FakeBackend

	dir     string
	checked bool
}

func (r *branchCheckingRuntime) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.ID, error) {
	if len(repos) != 1 {
		return "", fmt.Errorf("launch received %d repos, want 1", len(repos))
	}
	branch := repos[0].Branch
	cmd := exec.CommandContext(ctx, "git", "-C", r.dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) //nolint:gosec // controlled test arguments
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mapped branch %q did not exist at launch: %w: %s", branch, err, out)
	}
	r.checked = true
	return r.FakeBackend.Launch(ctx, repos, opts)
}

type setupLogFailureRuntime struct {
	*runtimetest.FakeBackend
}

func (r *setupLogFailureRuntime) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.ID, error) {
	if _, err := opts.LogWriter.Write([]byte("md setup output")); err != nil {
		return "", err
	}
	return "", errors.New("runtime launch failed")
}

type connectFailureRuntime struct {
	*runtimetest.FakeBackend

	err error
}

func (r *connectFailureRuntime) Connect(context.Context, runtime.ID, *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	return runtime.ConnectionInfo{}, r.err
}

type forkLogRuntime struct {
	*runtimetest.FakeBackend

	forkErr       error
	metadata      runtime.Metadata
	capturedRepos []runtime.ForkRepo // opts.Repos seen by Fork.
}

func (r *forkLogRuntime) Fork(ctx context.Context, id runtime.ID, opts *runtime.ForkOptions) (runtime.ID, runtime.ConnectionInfo, error) {
	r.capturedRepos = opts.Repos
	r.metadata = maps.Clone(opts.Metadata)
	if _, err := opts.LogWriter.Write([]byte("fork setup complete\nfinal setup line")); err != nil {
		return "", runtime.ConnectionInfo{}, err
	}
	if r.forkErr != nil {
		return "", runtime.ConnectionInfo{}, r.forkErr
	}
	// The real runtime creates DestPrimary verbatim; no repo echo is needed.
	return r.FakeBackend.Fork(ctx, id, opts)
}

// destPrimary returns the captured destination primary branch for hostPath.
func (r *forkLogRuntime) destPrimary(hostPath string) (string, bool) {
	for _, rp := range r.capturedRepos {
		if rp.GitRoot == hostPath {
			return rp.DestPrimary, true
		}
	}
	return "", false
}

func newTestCheckout(t testing.TB, baseBranch, dir string, backend testRuntimeBackend) *repo.Checkout {
	checkout := &repo.Checkout{
		BaseBranch: baseBranch,
		Dir:        dir,
		RelPath:    filepath.Base(dir),
		GitTimeout: time.Minute,
	}
	testCheckoutRuntimes.Store(checkout, newTestRuntimeRouter(t, backend))
	return checkout
}

func newTestRuntimeRouter(t testing.TB, backend testRuntimeBackend) *runtime.Router {
	if backend == nil {
		return nil
	}
	rt, err := runtime.NewRouter([]runtime.System{&testRuntimeSystem{testRuntimeBackend: backend}}, metrics.Nop{})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func newTestAgentRuntime(t testing.TB, checkout *repo.Checkout, logDir string, backends map[harness.Name]agent.Backend) *AgentRuntime {
	if backends == nil {
		backends = agentbackends.Default("", nil)
		backends["test"] = &testBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: "test"}}
	}
	if logDir == "" {
		logDir = t.TempDir()
	}
	var runtimes *runtime.Router
	if value, ok := testCheckoutRuntimes.Load(checkout); ok {
		runtimes, _ = value.(*runtime.Router)
	}
	return &AgentRuntime{
		Backends:             backends,
		LogStore:             taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), agent.Backends(backends)),
		LogPath:              &taskslog.Path{},
		Runtimes:             runtimes,
		Log:                  logtest.Logger(t),
		NotifyTaskChange:     func() {},
		Checkout:             checkout,
		RuntimeStartTimeout:  time.Hour,
		EnsureSessionTimeout: 30 * time.Millisecond,
	}
}

func newTestAgentRuntimeWithRuntime(t testing.TB, backend testRuntimeBackend, backends map[harness.Name]agent.Backend, logDir string) *AgentRuntime {
	r := newTestAgentRuntime(t, nil, logDir, backends)
	if backend == nil {
		return r
	}
	r.Runtimes = newTestRuntimeRouter(t, backend)
	return r
}

func openTaskLog(s *taskhistory.Store, tk *Task) (agent.LogSink, error) {
	log, _, err := s.Open(tk.LogFilename(), tk.LogHeader())
	return log, err
}

func reopenTaskLog(s *taskhistory.Store, tk *Task, path string) (agent.LogSink, error) {
	name := tk.LogFilename()
	if path != "" {
		name = filepath.Base(path)
	}
	log, _, err := s.Reopen(name, tk.LogHeader())
	return log, err
}

func logLines(t *testing.T, path string) []string {
	file, err := os.Open(path) //nolint:gosec // path is test-controlled.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	var reader io.Reader = file
	var decoder *zstd.Decoder
	if strings.HasSuffix(path, ".zst") {
		decoder, err = zstd.NewReader(file)
		if err != nil {
			t.Fatal(err)
		}
		defer decoder.Close()
		reader = decoder
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func caic0BranchExists(t *testing.T, dir string) bool {
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--verify", "--quiet", "refs/heads/caic-0")
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return true
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git rev-parse caic-0: %v", err)
	return false
}

type resultFetchBackend struct {
	*runtimetest.FakeBackend

	onDiff func()
}

func (b *resultFetchBackend) TurnSnapshot(ctx context.Context, id runtime.ID, previous []runtime.FetchedBranch) ([]runtime.TurnRepository, error) {
	if b.onDiff != nil {
		b.onDiff()
	}
	return b.FakeBackend.TurnSnapshot(ctx, id, previous)
}

// shutdownFaultSink exercises actual owner failure without depending on a failing filesystem.
type shutdownFaultSink struct {
	agenttest.LogSink

	appendErr error
	closeErr  error
}

func (s *shutdownFaultSink) AppendRecord(record agent.PhysicalRecord) error {
	if s.appendErr != nil {
		return s.appendErr
	}
	return s.LogSink.AppendRecord(record)
}

func (s *shutdownFaultSink) Close() error { return s.closeErr }

func TestAgentRuntime(t *testing.T) { //nolint:tparallel // Relay subprocess cases set the process-wide environment.
	t.Run("RecordHost", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			r := newTestAgentRuntime(t, nil, t.TempDir(), nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			if err := r.RecordHost(t.Context(), tk, &agent.LogMessage{Line: "hello"}); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("record = %v", err)
			}
		})
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			r, _ := mustTaskRun(t, tk)
			if err := r.RecordHost(t.Context(), tk, &agent.LogMessage{MessageType: "caic_log", Line: "hello"}); err != nil {
				t.Fatal(err)
			}
			rows, err := r.LogStore.LoadAllReadOnly()
			if err != nil || len(rows) != 1 {
				t.Fatalf("load = %v, %v", rows, err)
			}
			if err := rows[0].LoadMessages(); err != nil {
				t.Fatal(err)
			}
			var lines []string
			for _, m := range rows[0].Timeline {
				if log, ok := m.Message.(*agent.LogMessage); ok {
					lines = append(lines, log.Line)
				}
			}
			if !slices.Equal(lines, []string{"hello"}) {
				t.Fatalf("lines = %v", lines)
			}
		})
		t.Run("context_boundary", func(t *testing.T) {
			t.Parallel()
			t.Run("ResetsPlanState", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
				r, _ := mustTaskRun(t, tk)
				tk.SetState(task.StateRunning)
				// Simulate an agent entering plan mode and writing a plan file.
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "EnterPlanMode",
				})
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu2", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
				})
				snap := tk.Snapshot()
				if !snap.InPlanMode {
					t.Fatal("InPlanMode = false before context boundary, want true")
				}
				if snap.PlanContent != "the plan" {
					t.Fatalf("PlanContent = %q before context boundary, want %q", snap.PlanContent, "the plan")
				}

				if err := r.RecordHost(t.Context(), tk, agent.ContextCleared()); err != nil {
					t.Fatal(err)
				}

				snap = tk.Snapshot()
				if snap.InPlanMode {
					t.Error("InPlanMode = true after context boundary, want false")
				}
				if snap.PlanContent != "" {
					t.Errorf("PlanContent = %q after context boundary, want empty", snap.PlanContent)
				}
				if tk.GetPlanFile() != "" {
					t.Errorf("PlanFile = %q after context boundary, want empty", tk.GetPlanFile())
				}
			})
			t.Run("SuppressesPlanRewrite", func(t *testing.T) {
				t.Parallel()
				// After context boundary (restart), the agent may re-enter plan mode
				// and write to .claude/plans/. The plan must not resurface.
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
				r, _ := mustTaskRun(t, tk)
				tk.SetState(task.StateRunning)
				// Original plan.
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
				})
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu2", Name: "ExitPlanMode",
				})
				addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})

				// User clicks "Clear and execute plan".
				if err := r.RecordHost(t.Context(), tk, agent.ContextCleared()); err != nil {
					t.Fatal(err)
				}
				tk.SetState(task.StateRunning)

				// Agent re-enters plan mode during execution.
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu3", Name: "EnterPlanMode",
				})
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu4", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"rewritten plan"}`),
				})
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu5", Name: "ExitPlanMode",
				})
				addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})

				snap := tk.Snapshot()
				if snap.PlanContent != "" {
					t.Errorf("PlanContent = %q, want empty (plan written after context boundary should be suppressed)", snap.PlanContent)
				}
				if tk.GetPlanFile() != "" {
					t.Errorf("PlanFile = %q, want empty", tk.GetPlanFile())
				}
			})
			t.Run("ClearsExitPlanModePlanContent", func(t *testing.T) {
				t.Parallel()
				// After context boundary the plan projection must be dropped so new
				// subscribers don't see stale plans; the message itself stays
				// immutable.
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
				r, _ := mustTaskRun(t, tk)
				tk.SetState(task.StateRunning)
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
				})
				addTaskMessage(tk, &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"})
				addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})
				if snap := tk.Snapshot(); snap.PlanExitID != "tu2" || snap.PlanContent != "the plan" {
					t.Errorf("current plan = (%q, %q), want (tu2, the plan)", snap.PlanExitID, snap.PlanContent)
				}

				if err := r.RecordHost(t.Context(), tk, agent.ContextCleared()); err != nil {
					t.Fatal(err)
				}

				if got := tk.Snapshot().PlanExitID; got == "tu2" {
					t.Errorf("superseded plan owner = %q", got)
				}
			})
			t.Run("SuppressionLiftsAfterTurn", func(t *testing.T) {
				t.Parallel()
				// After the restart turn completes, a subsequent user-initiated turn
				// must be able to produce a plan again.
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
				r, _ := mustTaskRun(t, tk)
				tk.SetState(task.StateRunning)
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan"}`),
				})
				addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})

				// Restart.
				if err := r.RecordHost(t.Context(), tk, agent.ContextCleared()); err != nil {
					t.Fatal(err)
				}
				tk.SetState(task.StateRunning)
				// Turn completes without plan.
				addTaskMessage(tk, &agent.TextMessage{Text: "done"})
				addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})

				// Suppression lifted — next turn can set plan.
				tk.SetState(task.StateRunning)
				addTaskMessage(tk, &agent.ToolUseMessage{
					ToolUseID: "tu2", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"fresh plan"}`),
				})
				addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})

				snap := tk.Snapshot()
				if snap.PlanContent != "fresh plan" {
					t.Errorf("PlanContent = %q, want %q (suppression should have lifted)", snap.PlanContent, "fresh plan")
				}
			})
		})
	})
	t.Run("RecordRepositorySummary", func(t *testing.T) {
		t.Parallel()
		t.Run("replaced_instance", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.SetRuntimeConnectionInfo("test-runtime:new", runtime.ConnectionTarget{}, "", "", 0)
			r, _ := mustTaskRun(t, tk)
			current := v3.DiffStat{{Path: "current.go", LinesAdded: 1}}
			if _, err := r.RecordRepositorySummary(t.Context(), tk, &repo.GitSnapshot{Read: repo.NewGitRead(tk.RuntimeInstanceID()), DiffStat: current}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []runtime.ID{"test-runtime:old", ""} {
				changed, err := r.RecordRepositorySummary(t.Context(), tk, &repo.GitSnapshot{Read: repo.NewGitRead(id), RepoStates: []v3.RepoState{{Branch: "stale"}}})
				if err != nil {
					t.Fatal(err)
				}
				if changed {
					t.Fatal("accepted replaced instance")
				}
			}
			if got := tk.LiveDiffStat(); !reflect.DeepEqual(got, current) {
				t.Fatalf("summary = %+v", got)
			}
		})
		t.Run("partial_failure_and_replay", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
			r, _ := mustTaskRun(t, tk)
			target := repo.GitTarget{InstanceID: tk.RuntimeInstanceID(), Repos: []runtime.Repo{{ContainerPath: "/one", Branch: "branch-one"}, {ContainerPath: "/two", Branch: "branch-two"}}}
			for _, snapshot := range []repo.GitSnapshot{
				{Read: repo.NewGitRead(target.InstanceID), Target: target, DiffStat: v3.DiffStat{{Path: "one/old.go", LinesAdded: 3}, {Path: "two/old.go", LinesAdded: 7}}, RepoStates: []v3.RepoState{{RepoIndex: 0, ChangedFiles: 1, LinesAdded: 3}, {RepoIndex: 1, ChangedFiles: 1, LinesAdded: 7}}},
				{Read: repo.NewGitRead(target.InstanceID), Target: target, FailedRepos: []int{1}, DiffStat: v3.DiffStat{{Path: "one/fresh.go", LinesAdded: 5}}, RepoStates: []v3.RepoState{{RepoIndex: 0, ChangedFiles: 1, LinesAdded: 5}}},
			} {
				if _, err := r.RecordRepositorySummary(t.Context(), tk, &snapshot); err != nil {
					t.Fatal(err)
				}
			}
			if got := tk.LiveDiffStat(); len(got) != 2 || got[0].Path != "one/fresh.go" || got[1].Path != "two/old.go" {
				t.Fatalf("stats = %+v", got)
			}
			states := tk.Snapshot().RepoStates
			if len(states) != 2 || states[0].Stale || !states[1].Stale || states[1].LinesAdded != 7 {
				t.Fatalf("states = %+v", states)
			}
			failed := repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, FailedRepos: []int{0, 1}}
			if _, err := r.RecordRepositorySummary(t.Context(), tk, &failed); err != nil {
				t.Fatal(err)
			}
			rows, err := r.LogStore.LoadAllReadOnly()
			if err != nil || len(rows) != 1 {
				t.Fatalf("load = %v, %v", rows, err)
			}
			if err := rows[0].LoadMessages(); err != nil {
				t.Fatal(err)
			}
			restored := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "restored"}, "", "", "")
			restored.SeedTimelineParts(nil, rows[0].Timeline)
			if !reflect.DeepEqual(tk.LiveDiffStat(), restored.LiveDiffStat()) || !reflect.DeepEqual(tk.Snapshot().RepoStates, restored.Snapshot().RepoStates) {
				t.Fatal("replay lost partial summary")
			}
		})
		t.Run("recovery_rejects_older_failure", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
			r, _ := mustTaskRun(t, tk)
			target := repo.GitTarget{InstanceID: tk.RuntimeInstanceID(), Repos: []runtime.Repo{{ContainerPath: "/repo", Branch: "main"}}}
			failed := repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, FailedRepos: []int{0}}
			if _, err := r.RecordRepositorySummary(t.Context(), tk, &failed); err != nil {
				t.Fatal(err)
			}
			if states := tk.Snapshot().RepoStates; len(states) != 1 || !states[0].Stale {
				t.Fatalf("states = %+v", states)
			}
			clean := repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, RepoStates: []v3.RepoState{{RepoIndex: 0, Branch: "main"}}}
			changed, err := r.RecordRepositorySummary(t.Context(), tk, &clean)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("recovery not published")
			}
			if states := tk.Snapshot().RepoStates; len(states) != 1 || states[0].Stale {
				t.Fatalf("states = %+v", states)
			}
			changed, err = r.RecordRepositorySummary(t.Context(), tk, &failed)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Fatal("older failure replaced recovery")
			}
		})
		t.Run("completed_turn_round_trip", func(t *testing.T) {
			t.Parallel()
			for _, kind := range []string{"compact failure", "result failure", "result clean recovery"} {
				t.Run(kind, func(t *testing.T) {
					t.Parallel()
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
					tk.Repos = []repo.Mount{{Name: "repo", Branch: "main", ContainerPath: "/repo"}}
					tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
					r, run := mustTaskRun(t, tk)
					target := tk.GitTarget()
					if _, err := r.RecordRepositorySummary(t.Context(), tk, &repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, DiffStat: v3.DiffStat{{Path: "old.go", LinesAdded: 7}}, RepoStates: []v3.RepoState{{RepoIndex: 0, Branch: "main", ChangedFiles: 1, LinesAdded: 7, Stale: kind == "result clean recovery"}}}); err != nil {
						t.Fatal(err)
					}
					if kind != "compact failure" {
						if err := taskNative(t.Context(), run.records, `{"type":"result","subtype":"success","is_error":false,"result":"done"}`); err != nil {
							t.Fatal(err)
						}
					}
					snapshot := repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, FailedRepos: []int{0}}
					if kind == "result clean recovery" {
						snapshot.FailedRepos = nil
						snapshot.RepoStates = []v3.RepoState{{RepoIndex: 0, Branch: "main"}}
					}
					changed, err := r.RecordRepositorySummary(t.Context(), tk, &snapshot)
					if err != nil {
						t.Fatal(err)
					}
					if !changed {
						t.Fatal("changed summary not published")
					}
					rows, err := r.LogStore.LoadAllReadOnly()
					if err != nil || len(rows) != 1 {
						t.Fatalf("load = %v, %v", rows, err)
					}
					if err := rows[0].LoadMessages(); err != nil {
						t.Fatal(err)
					}
					restored := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "restored"}, harness.Claude, "", "")
					restored.SeedTimelineParts(nil, rows[0].Timeline)
					live, replay := tk.Snapshot(), restored.Snapshot()
					if !reflect.DeepEqual(live.DiffStat, replay.DiffStat) || !reflect.DeepEqual(live.RepoStates, replay.RepoStates) {
						t.Fatalf("live %+v %+v; replay %+v %+v", live.DiffStat, live.RepoStates, replay.DiffStat, replay.RepoStates)
					}
					if kind == "result clean recovery" {
						if len(replay.DiffStat) != 0 || replay.RepoStates[0].Stale {
							t.Fatal("clean recovery retained old changes")
						}
					} else if len(replay.DiffStat) != 1 || !replay.RepoStates[0].Stale {
						t.Fatal("failure lost retained state")
					}
				})
			}
		})
	})
	t.Run("Shutdown", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, boundary := range []string{"append", "close"} {
				t.Run(boundary, func(t *testing.T) {
					t.Parallel()
					r := newTestAgentRuntime(t, nil, t.TempDir(), nil)
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
					failure := &os.PathError{Op: boundary, Path: "task.jsonl", Err: io.ErrUnexpectedEOF}
					fault := fmt.Errorf("journal operation: %w", errors.Join(context.Canceled, failure))
					sink := &shutdownFaultSink{}
					if boundary == "append" {
						sink.appendErr = fault
					} else {
						sink.closeErr = fault
					}
					ctx, cancel := context.WithCancelCause(t.Context())
					t.Cleanup(func() { cancel(context.Canceled) })
					run := &Run{ctx: ctx, parent: t.Context(), cancel: cancel, runtime: r, task: tk, startupDone: make(chan struct{}), pendingMutating: map[agent.ToolKey]struct{}{}}
					var err error
					run.records, err = agent.NewRecords(context.WithoutCancel(ctx), claudecode.New().NewWire(), sink, run)
					if err != nil {
						t.Fatal(err)
					}
					close(run.startupDone)
					tk.run.Store(run)
					t.Cleanup(func() {
						err := r.Shutdown(context.WithoutCancel(t.Context()), tk)
						pathErr, ok := errors.AsType[*os.PathError](err)
						if !errors.Is(err, io.ErrUnexpectedEOF) || !ok || pathErr != failure {
							t.Errorf("Shutdown lost independent journal failure: %v", err)
						}
						if errors.Is(err, context.Canceled) {
							t.Errorf("Shutdown retained expected cancellation: %v", err)
						}
					})
					cmd := exec.CommandContext(ctx, "python3", "-c", "input()")
					stdin, err := cmd.StdinPipe()
					if err != nil {
						t.Fatal(err)
					}
					stdout, err := cmd.StdoutPipe()
					if err != nil {
						t.Fatal(errors.Join(err, stdin.Close()))
					}
					if err := cmd.Start(); err != nil {
						t.Fatal(errors.Join(err, stdin.Close(), stdout.Close()))
					}
					session := agent.NewSession(ctx, cmd, agent.NewConn(ctx, testLogger(), stdin, run.records, nil), stdout, testLogger())
					tk.AttachSession(&SessionHandle{Session: session, Run: run})
					if boundary == "append" {
						if err := taskNative(t.Context(), run.records, `{"type":"assistant","message":{"content":[{"type":"text","text":"retained evidence"}]}}`); !errors.Is(err, io.ErrUnexpectedEOF) {
							t.Fatalf("failed ingestion = %v", err)
						}
					}
					r.Cancel(tk)
					<-session.Done()
				})
			}
		})
	})

	t.Run("Cancel", func(t *testing.T) {
		t.Run("error_rejects_new_host_admission", func(t *testing.T) {
			t.Parallel()
			for _, mode := range []string{"active", "settled"} {
				t.Run(mode, func(t *testing.T) {
					t.Parallel()
					r := newTestAgentRuntime(t, nil, t.TempDir(), nil)
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
					tk.SetState(task.StateWaiting)
					run := beginTestExecution(t, r, tk)
					if mode == "settled" {
						if err := r.closeRun(tk, run); err != nil {
							t.Fatal(err)
						}
					}
					before, err := os.ReadFile(r.LogPath.Get())
					if err != nil {
						t.Fatal(err)
					}
					count := tk.MessageCount()
					r.Cancel(tk)
					message := &agent.LogMessage{Line: "rejected after cancellation"}
					if err := r.RecordHost(t.Context(), tk, message); !errors.Is(err, context.Canceled) {
						t.Fatalf("RecordHost = %v", err)
					}
					snapshot := &repo.GitSnapshot{}
					if _, err := r.RecordRepositorySummary(t.Context(), tk, snapshot); !errors.Is(err, context.Canceled) {
						t.Fatalf("RecordRepositorySummary = %v", err)
					}
					if _, accepted, err := r.RecordLifecycleIntent(t.Context(), tk, task.StateStopping, task.StateStopping, task.StateWaiting); accepted || !errors.Is(err, context.Canceled) {
						t.Fatalf("RecordLifecycleIntent = %v, %v", accepted, err)
					}
					after, err := os.ReadFile(r.LogPath.Get())
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(before, after) || tk.MessageCount() != count || tk.GetState() != task.StateWaiting {
						t.Fatal("canceled admission changed journal or published task state")
					}
				})
			}
		})
	})
	t.Run("Reconnect", func(t *testing.T) {
		dir := t.TempDir()
		// Remote snapshot commands are supported to reproduce the output-only warm
		// source as well as attach. No client command is present in the relay file.
		script := `#!/usr/bin/python3
import os, subprocess, sys
args = sys.argv[1:]
while args[0] == "-o":
    args = args[2:]
args = args[1:]
path = os.environ["CAIC_TEST_RELAY_OUTPUT"]
if args[:3] == ["python3", "/tmp/caic-relay/relay.py", "attach"]:
    offset = int(args[args.index("--offset") + 1])
    with open(path, "rb") as stream:
        stream.seek(offset)
        sys.stdout.buffer.write(stream.read())
else:
    command = " ".join(args).replace("/tmp/caic-relay/output.jsonl", path)
    sys.exit(subprocess.run(command, shell=True).returncode)
`
		// Use the production relay locations without duplicating their spellings in
		// the subprocess fixture.
		script = strings.ReplaceAll(script, "/tmp/caic-relay/relay.py", agent.RelayScriptPath)
		script = strings.ReplaceAll(script, "/tmp/caic-relay/output.jsonl", agent.RelayOutputPath)
		if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil { //nolint:gosec // Executable test SSH boundary.
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		for _, name := range []string{"zero_offset", "current_generation", "repeated_reconnect"} {
			t.Run(name, func(t *testing.T) {
				r := newTestAgentRuntime(t, nil, filepath.Join(t.TempDir(), "logs"), agent.Backends{harness.OpenCode: opencode.New("", nil)})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.OpenCode, "", "")
				tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "session"})
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "retained"), runtime.ConnectionTarget{SSHHost: "retained"}, "", "", 0)
				log, err := r.openLog(tk)
				if err != nil {
					t.Fatal(err)
				}
				input := `{"t":"input","ts":2.000,"msg":{"jsonrpc":"2.0","id":17,"method":"session/prompt","params":{"sessionId":"session","prompt":[{"type":"text","text":"next"}]}}}` + "\n"
				chunk := func(text string) string {
					return `{"t":"agent","ts":3.000,"msg":{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + text + `"}}}}}` + "\n"
				}
				var prefix, local string
				if name != "zero_offset" {
					local = `{"t":"relay_generation","generation":"previous"}` + "\n" + chunk("old turn")
					prefix = `{"t":"relay_generation","generation":"current"}` + "\n"
					local += prefix + input + chunk("retained ")
					prefix += chunk("retained ")
				} else {
					local = input
				}
				if err := log.AppendRecord(agent.PhysicalRecord{Encoded: []byte(local)}); err != nil {
					t.Fatal(err)
				}
				if err := log.AppendMessage(&agent.LogMessage{MessageType: "log", Line: "host observation"}); err != nil {
					t.Fatal(err)
				}
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(r.LogPath.Get())
				if err != nil {
					t.Fatal(err)
				}
				live := chunk("live") + `{"t":"agent","ts":4.000,"msg":{"jsonrpc":"2.0","id":17,"error":{"code":-32603,"message":"interrupted"}}}` + "\n"
				firstLive := live
				if name == "repeated_reconnect" {
					firstLive = chunk("live")
				}
				relayPath := filepath.Join(t.TempDir(), "output.jsonl")
				if err := os.WriteFile(relayPath, []byte(prefix+firstLive), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CAIC_TEST_RELAY_OUTPUT", relayPath)
				tk.SetRelayOffset(int64(len(prefix)))
				tk.SetState(task.StateRunning)
				h, err := r.Reconnect(t.Context(), tk)
				if err != nil {
					t.Fatal(err)
				}
				if err := h.Drain(); err != nil {
					t.Fatal(err)
				}
				if err := r.closeRun(tk, h.Run); err != nil {
					t.Fatal(err)
				}
				if name == "repeated_reconnect" {
					// Model transport loss before the terminal response. The live
					// prefix is now in local history beyond the import-time offset.
					tk.DetachSession()
					if err := os.WriteFile(relayPath, []byte(prefix+live), 0o600); err != nil {
						t.Fatal(err)
					}
					h, err = r.Reconnect(t.Context(), tk)
					if err != nil {
						t.Fatal(err)
					}
					if err := h.Drain(); err != nil {
						t.Fatal(err)
					}
					if err := r.closeRun(tk, h.Run); err != nil {
						t.Fatal(err)
					}
				}
				var texts []string
				var results []*agent.ResultMessage
				for _, msg := range tk.Messages() {
					switch msg := msg.(type) {
					case *agent.TextMessage:
						texts = append(texts, msg.Text)
					case *agent.ResultMessage:
						results = append(results, msg)
					case *agent.UserInputMessage, *agent.LogMessage:
						t.Fatalf("warm history was delivered again: %#v", msg)
					}
				}
				wantText := "live"
				if name != "zero_offset" {
					wantText = "retained live"
				}
				if !slices.Equal(texts, []string{wantText}) || len(results) != 1 || !results[0].IsError || results[0].Result != "interrupted" {
					t.Fatalf("completion texts=%q results=%+v, want %q and one correlated error", texts, results, wantText)
				}
				if tk.GetState() == task.StateRunning {
					t.Fatal("in-flight prompt error left task running")
				}
				if got := tk.RelayOffsetValue(); got != int64(len(prefix+live)) {
					t.Fatalf("committed relay offset = %d, want %d", got, len(prefix+live))
				}
				after, err := os.ReadFile(r.LogPath.Get())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(after, append(before, live...)) {
					t.Fatalf("reconnect changed retained bytes or duplicated output:\n%s", after)
				}
			})
		}
	})
	t.Run("Sessions", func(t *testing.T) {
		t.Run("Reconnect", func(t *testing.T) {
			t.Parallel()
			t.Run("error_stateful_missing_session_id", func(t *testing.T) {
				t.Parallel()
				for _, harness := range []harness.Name{harness.Codex, harness.OpenCode} {
					t.Run(string(harness), func(t *testing.T) {
						t.Parallel()
						r := newTestAgentRuntime(t, nil, "", nil)
						tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness, "", "")
						tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
						tk.SetState(task.StateRunning)
						_, err := r.Reconnect(t.Context(), tk)
						if err == nil {
							t.Fatal("Reconnect returned nil error")
						}
						if !strings.Contains(err.Error(), "session ID missing") {
							t.Fatalf("err = %v, want missing session ID", err)
						}
					})
				}
			})
			t.Run("preserves_idle_state", func(t *testing.T) {
				t.Parallel()
				for _, state := range []task.State{task.StateAsking, task.StateHasPlan, task.StateWaiting} {
					t.Run(string(state), func(t *testing.T) {
						t.Parallel()
						backend := &attachCaptureBackend{FakeBackend: &agenttest.FakeBackend{}}
						r := newTestAgentRuntime(t, nil, filepath.Join(t.TempDir(), "logs"), agent.Backends{"test": backend})
						tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
						log, err := r.openLog(tk)
						if err != nil {
							t.Fatal(err)
						}
						if err := log.Close(); err != nil {
							t.Fatal(err)
						}
						tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
						tk.SetState(state)
						h, err := r.Reconnect(t.Context(), tk)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							// The test context cancels the subprocess before cleanup.
							h.Session.Cancel()
							_ = h.Drain()
							if err := r.closeRun(tk, h.Run); err != nil {
								t.Error(err)
							}
						})
						if got := tk.GetState(); got != state {
							t.Fatalf("idle state became %s, want %s", got, state)
						}
					})
				}
			})
			t.Run("passes_history_to_attach_backend", func(t *testing.T) {
				t.Parallel()
				backend := &attachCaptureBackend{FakeBackend: &agenttest.FakeBackend{}}
				r := newTestAgentRuntime(t, nil, filepath.Join(t.TempDir(), "logs"), map[harness.Name]agent.Backend{"test": backend})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
				logW, err := r.openLog(tk)
				if err != nil {
					t.Fatal(err)
				}
				if err := logW.Close(); err != nil {
					t.Fatal(err)
				}
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(task.StateRunning)
				tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
					&agent.AskMessage{
						ToolUseID: "toolu-1",
						Questions: []v3.AskQuestion{{Question: "Which?"}},
					},
					&agent.PendingUserActionMessage{
						MessageType: agent.PendingUserActionMessageType,
						Action: v3.PendingUserAction{
							Kind:      agent.PendingUserActionAskUserQuestion,
							RequestID: "req-1",
							ToolUseID: "toolu-1",
							Ask: v3.PendingAskAction{
								Questions: []v3.AskQuestion{{Question: "Which?"}},
							},
						},
					},
				}))

				h, err := r.Reconnect(t.Context(), tk)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = h.GracefulStop(t.Context(), time.Second)
					_ = h.Drain()
				})

				pending := backend.capturedAttachOpts.PendingUserActions
				if len(pending) != 1 {
					t.Fatalf("PendingUserActions len = %d, want 1", len(pending))
				}
				if pending[0].Kind != agent.PendingUserActionAskUserQuestion {
					t.Errorf("Kind = %q, want %q", pending[0].Kind, agent.PendingUserActionAskUserQuestion)
				}
				if pending[0].RequestID != "req-1" {
					t.Errorf("RequestID = %q, want req-1", pending[0].RequestID)
				}
				if len(pending[0].Ask.Questions) != 1 {
					t.Fatalf("Questions len = %d, want 1", len(pending[0].Ask.Questions))
				}
				if pending[0].Ask.Questions[0].Question != "Which?" {
					t.Errorf("question = %q, want Which?", pending[0].Ask.Questions[0].Question)
				}
			})
			t.Run("task_mcp", func(t *testing.T) {
				t.Parallel()
				t.Run("valid", func(t *testing.T) {
					t.Parallel()
					for _, tc := range []struct {
						name     string
						enabled  bool
						registry mcp.Registry
					}{
						{name: "enabled", enabled: true, registry: &mcptest.FakeRegistry{}},
						{name: "disabled", registry: &mcptest.FakeRegistry{}},
					} {
						t.Run(tc.name, func(t *testing.T) {
							t.Parallel()
							backend := &attachCaptureBackend{FakeBackend: &agenttest.FakeBackend{}}
							r := newTestAgentRuntime(t, nil, filepath.Join(t.TempDir(), "logs"), map[harness.Name]agent.Backend{harness.Antigravity: backend})
							r.MCPRegistry = tc.registry
							tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Antigravity, "", "")
							tk.CaicMCP = tc.enabled
							log, err := r.openLog(tk)
							if err != nil {
								t.Fatal(err)
							}
							if err := log.Close(); err != nil {
								t.Fatal(err)
							}
							tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
							tk.SetState(task.StateWaiting)
							ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
							t.Cleanup(cancel)
							h, err := r.Reconnect(ctx, tk)
							if h != nil {
								t.Cleanup(func() {
									ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
									t.Cleanup(cancel)
									if err := h.GracefulStop(ctx, time.Second); err != nil {
										t.Errorf("stop reconnected session: %v", err)
									}
									if err := h.Drain(); err != nil {
										t.Errorf("drain reconnected session: %v", err)
									}
									if err := r.closeRun(tk, h.Run); err != nil {
										t.Errorf("close reconnected log: %v", err)
									}
								})
							}
							if err != nil {
								t.Fatal(err)
							}
							var want mcp.Registry
							if tc.enabled {
								want = tc.registry
							}
							if backend.capturedAttachOpts.MCP != want {
								t.Fatal("reconnect passed the wrong task registry")
							}
						})
					}
				})
				t.Run("error", func(t *testing.T) {
					t.Parallel()
					t.Run("unavailable", func(t *testing.T) {
						t.Parallel()
						backend := &attachCaptureBackend{FakeBackend: &agenttest.FakeBackend{}}
						r := newTestAgentRuntime(t, nil, filepath.Join(t.TempDir(), "logs"), map[harness.Name]agent.Backend{harness.Antigravity: backend})
						tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Antigravity, "", "")
						tk.CaicMCP = true
						log, err := r.openLog(tk)
						if err != nil {
							t.Fatal(err)
						}
						if err := log.Close(); err != nil {
							t.Fatal(err)
						}
						tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
						tk.SetState(task.StateWaiting)
						ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
						t.Cleanup(cancel)
						h, err := r.Reconnect(ctx, tk)
						if h != nil {
							t.Cleanup(func() {
								ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
								t.Cleanup(cancel)
								if err := h.GracefulStop(ctx, time.Second); err != nil {
									t.Errorf("stop reconnected session: %v", err)
								}
								if err := h.Drain(); err != nil {
									t.Errorf("drain reconnected session: %v", err)
								}
								if err := r.closeRun(tk, h.Run); err != nil {
									t.Errorf("close reconnected log: %v", err)
								}
							})
						}
						if err == nil || !strings.Contains(err.Error(), "task-scoped MCP is unavailable") {
							t.Fatalf("Reconnect error = %v, want unavailable registry", err)
						}
						if h != nil || tk.HasSession() || backend.capturedAttachOpts.Records != nil {
							t.Fatal("reconnect attached without required task registry")
						}
						if tk.GetState() != task.StateWaiting {
							t.Fatal("registry failure changed task state")
						}
					})
				})
			})
			t.Run("missing_log_fails_closed", func(t *testing.T) {
				t.Parallel()
				backend := &attachCaptureBackend{FakeBackend: &agenttest.FakeBackend{}}
				r := newTestAgentRuntime(t, nil, filepath.Join(t.TempDir(), "logs"), map[harness.Name]agent.Backend{"test": backend})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(task.StateRunning)

				if _, err := r.Reconnect(t.Context(), tk); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Reconnect error = %v, want os.ErrNotExist", err)
				}
				if backend.capturedAttachOpts.Records != nil {
					t.Fatal("AttachRelay called without authoritative local log")
				}
				if r.LogPath.Get() != "" {
					t.Fatalf("LogPath = %q, want empty", r.LogPath.Get())
				}
			})
		})

		t.Run("LogStore", func(t *testing.T) {
			t.Parallel()
			t.Run("relay_cursor", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntime(t, nil, t.TempDir(), nil)
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.OpenCode, "", "")
				run := beginTestExecution(t, r, tk)
				tk.SetRelayOffset(1000)
				if err := run.records.WriteInput(t.Context(), io.Discard, []byte(`{"id":17,"method":"session/prompt","params":{"prompt":[]}}`+"\n")); err != nil {
					t.Fatal(err)
				}
				if err := run.records.AppendMessage(t.Context(), &agent.LogMessage{Line: "host observation"}); err != nil {
					t.Fatal(err)
				}
				if got := tk.RelayOffsetValue(); got != 1000 {
					t.Fatalf("local appends moved cursor to%d", got)
				}
				record := `{"t":"agent","ts":1.000,"msg":{"jsonrpc":"2.0","id":1,"result":{}}}` + "\n"
				read := func(data string) error {
					reader := agent.NewRelayRecordReader(t.Context(), strings.NewReader(data), run.records)
					for {
						_, _, err := reader.ReadRecord()
						if errors.Is(err, io.EOF) {
							return nil
						}
						if err != nil {
							return err
						}
					}
				}
				if err := read(record); err != nil {
					t.Fatal(err)
				}
				if got := tk.RelayOffsetValue(); got != 1000+int64(len(record)) {
					t.Fatalf("successful append cursor=%d", got)
				}
				generation := `{"t":"relay_generation","generation":"replacement"}` + "\n"
				if err := read(generation + record); err != nil {
					t.Fatal(err)
				}
				want := int64(len(generation + record))
				if got := tk.RelayOffsetValue(); got != want {
					t.Fatalf("replacement cursor=%d,want%d", got, want)
				}
				if err := r.closeRun(tk, run); err != nil {
					t.Fatal(err)
				}
				if err := read(generation + record); err == nil {
					t.Fatal("closed recording owner accepted append")
				}
				if got := tk.RelayOffsetValue(); got != want {
					t.Fatalf("rejected append moved cursor=%d,want%d", got, want)
				}
			})
			t.Run("CreatesFile", func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				logDir := filepath.Join(dir, "logs")
				store := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), agentbackends.Default("", nil))
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "model-1", "high")
				tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0"}}
				w, err := openTaskLog(store, tk)
				if err != nil {
					t.Fatal(err)
				}
				// Write something and close.
				_ = w.AppendRecord(agent.PhysicalRecord{Encoded: []byte("test\n")})
				_ = w.Close()

				entries, err := os.ReadDir(logDir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 {
					t.Fatalf("expected 1 file, got %d", len(entries))
				}
				name := entries[0].Name()
				want := tk.ID.String() + "-org-repo-caic-0.jsonl"
				if name != want {
					t.Errorf("filename = %q, want %q", name, want)
				}
				data, err := os.ReadFile(filepath.Join(logDir, name)) //nolint:gosec // path is test-controlled
				if err != nil {
					t.Fatal(err)
				}
				var meta map[string]any
				if err := json.Unmarshal(bytes.SplitN(data, []byte("\n"), 2)[0], &meta); err != nil {
					t.Fatal(err)
				}
				if meta["model"] != "model-1" {
					t.Errorf("meta model = %v, want model-1", meta["model"])
				}
				if meta["effort"] != "high" {
					t.Errorf("meta effort = %v, want high", meta["effort"])
				}
			})
			t.Run("ReopenNoDuplicateHeader", func(t *testing.T) {
				t.Parallel()
				// Reconnect appends via Reopen, which must not write a second
				// caic_meta header. Otherwise every server restart that re-adopts
				// a running instance duplicates the header.
				logDir := filepath.Join(t.TempDir(), "logs")
				store := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), agentbackends.Default("", nil))
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0"}}

				tk.Harness = harness.Claude

				// Initial Start writes the header.
				w, err := openTaskLog(store, tk)
				if err != nil {
					t.Fatal(err)
				}
				_ = w.Close()

				// Several reconnects (simulating repeated server restarts) append
				// without writing a new header.
				for range 3 {
					w, err := reopenTaskLog(store, tk, "")
					if err != nil {
						t.Fatal(err)
					}
					_ = w.Close()
				}

				name := tk.ID.String() + "-org-repo-caic-0.jsonl"
				data, err := os.ReadFile(filepath.Join(logDir, name)) //nolint:gosec // path is test-controlled
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.Count(string(data), `"t":"caic_meta"`); got != 1 {
					t.Errorf("caic_meta count = %d, want 1", got)
				}
			})
			t.Run("ReopenMissingFile", func(t *testing.T) {
				t.Parallel()
				// Reopen must report os.ErrNotExist without creating a replacement
				// header because a reconnect cannot infer the running relay format.
				store := taskhistory.NewStore(testLogger(), taskslog.NewStore(filepath.Join(t.TempDir(), "logs")), agentbackends.Default("", nil))
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
				tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0"}}
				if _, err := reopenTaskLog(store, tk, ""); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Reopen err = %v, want os.ErrNotExist", err)
				}
			})
		})

		t.Run("RuntimeDir", func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				dir   string
				repos []repo.Mount
				want  string
			}{
				{repos: []repo.Mount{{ContainerPath: "~/src/caic-xyz/md"}}, want: "/home/user/src/caic-xyz/md"},
				{dir: "/home/maruel/src/caic", want: "/home/user/src/caic"},
				{dir: "/home/alice/projects/myrepo", want: "/home/user/src/myrepo"},
				{dir: "/opt/repos/foo", want: "/home/user/src/foo"},
			}
			for _, tc := range tests {
				r := newTestAgentRuntime(t, newTestCheckout(t, "", tc.dir, nil), "", nil)
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.Repos = tc.repos
				got := r.runtimeDir(tk)
				if got != tc.want {
					t.Errorf("runtimeDir(%q) = %q, want %q", tc.dir, got, tc.want)
				}
			}
		})

		t.Run("Recording", func(t *testing.T) {
			t.Parallel()
			t.Run("ProducerTimeCarrier", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", nil), "", nil)
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				_, sub, unsub := tk.Subscribe(t.Context())
				t.Cleanup(unsub)
				run := beginTestExecution(t, r, tk)
				line := `{"t":"agent","ts":1.234,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"timed"}]}}}` + "\n"
				if _, _, err := agent.NewRelayRecordReader(t.Context(), strings.NewReader(line), run.records).ReadRecord(); err != nil {
					t.Fatal(err)
				}
				got := <-sub
				if text, ok := got.Message.(*agent.TextMessage); !ok || text.Text != "timed" {
					t.Fatalf("subscriber message=%#v", got.Message)
				}
				if want := time.Unix(1, 234_000_000); !got.ObservedAt.Equal(want) {
					t.Fatalf("subscriber observed time = %v, want %v", got.ObservedAt, want)
				}
				if err := r.closeRun(tk, run); err != nil {
					t.Fatal(err)
				}
				addTaskMessage(tk, &agent.TextMessage{Text: "synthetic"})
			})
			t.Run("GitSnapshotsRejectDeferredPublication", func(t *testing.T) {
				t.Parallel()
				backend := testContainer()
				checkout := newTestCheckout(t, "", "/repo", backend)
				runtimes := newTestRuntimeRouter(t, backend)
				r := newTestAgentRuntime(t, checkout, t.TempDir(), nil)
				r.Runtimes = runtimes
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.Repos = []repo.Mount{{Branch: "caic-0", ContainerPath: "/repo"}}
				tk.SetRuntimeConnectionInfo("test-runtime:ctr-1", runtime.ConnectionTarget{}, "", "", 0)
				tk.SetState(task.StateRunning)
				full, err := checkout.RepositoryStatuses(t.Context(), runtimes, tk.GitTarget())
				if err != nil {
					t.Fatal(err)
				}
				compact, err := checkout.DiffStatAndRepoStates(t.Context(), logtest.Logger(t), runtimes, tk.GitTarget())
				if err != nil {
					t.Fatal(err)
				}
				result, err := checkout.DiffStat(t.Context(), logtest.Logger(t), runtimes, tk.GitTarget())
				if err != nil {
					t.Fatal(err)
				}
				backend.RepositoryStatusValue = runtime.RepositoryStatus{
					Branch: "caic-0", Behind: 2, DiffStat: []runtime.GitFileStat{{Path: "fresh.go", LinesAdded: 9}},
				}
				latest, err := checkout.RepositoryStatuses(t.Context(), runtimes, tk.GitTarget())
				if err != nil {
					t.Fatal(err)
				}
				run := beginTestExecution(t, r, tk)
				if changed, err := r.RecordRepositorySummary(t.Context(), tk, &latest); err != nil || !changed {
					t.Fatalf("fresh summary changed=%v err=%v", changed, err)
				}
				before := len(tk.Messages())
				for _, older := range []*repo.GitSnapshot{&full, &compact, &result} {
					if changed, err := r.RecordRepositorySummary(t.Context(), tk, older); err != nil || changed {
						t.Fatalf("older summary changed=%v err=%v", changed, err)
					}
				}
				if len(tk.Messages()) != before {
					t.Fatal("stale host read appended a redundant summary")
				}
				if err := run.records.AppendMessage(t.Context(), &agent.DiffStatMessage{DiffStat: v3.DiffStat{{Path: "restored.go", LinesAdded: 1}}}); err != nil {
					t.Fatal(err)
				}
				if changed, err := r.RecordRepositorySummary(t.Context(), tk, &latest); err != nil || changed || tk.LiveDiffStat()[0].Path != "restored.go" {
					t.Fatalf("completed read replaced later native refresh changed=%v err=%v", changed, err)
				}
			})
			t.Run("ResultDiffUsesCompletedProbeOrder", func(t *testing.T) {
				t.Parallel()
				for _, replaced := range []bool{false, true} {
					t.Run(fmt.Sprintf("instance replaced %v", replaced), func(t *testing.T) {
						t.Parallel()
						backend := &resultFetchBackend{FakeBackend: testContainer()}
						r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", backend), "", nil)
						tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
						tk.Repos = []repo.Mount{{Branch: "caic-0", ContainerPath: "/repo"}}
						tk.SetRuntimeConnectionInfo("test-runtime:ctr-1", runtime.ConnectionTarget{}, "", "", 0)
						tk.SetState(task.StateRunning)
						prior := v3.DiffStat{{Path: "prior.go", LinesAdded: 4}}
						backend.onDiff = func() {
							// A summary change before this RPC finishes must not invalidate the
							// completed result read. A changed instance still invalidates it.
							tk.addParsedMessage(agent.TimedMessage{Message: &agent.DiffStatMessage{DiffStat: prior}})
							if replaced {
								tk.SetRuntimeConnectionInfo("test-runtime:replacement", runtime.ConnectionTarget{}, "", "", 0)
							}
						}
						run := beginTestExecution(t, r, tk)
						if err := taskNative(t.Context(), run.records, `{"type":"result","result":"done","num_turns":1}`); err != nil {
							t.Fatal(err)
						}

						var result *agent.ResultMessage
						for _, m := range tk.Messages() {
							if got, ok := m.(*agent.ResultMessage); ok {
								result = got
							}
						}
						if result == nil {
							t.Fatal("recorded result missing")
						}
						want := v3.DiffStat{{Path: "main.go", LinesAdded: 5, LinesDeleted: 1}}
						if replaced {
							want = prior
						}
						if got := tk.LiveDiffStat(); !reflect.DeepEqual(got, want) {
							t.Errorf("live stats = %+v, want %+v", got, want)
						}
						if replaced {
							if len(result.DiffStat) != 0 {
								t.Errorf("old-instance result enriched with %+v", result.DiffStat)
							}
						} else if len(result.DiffStat) != 1 || result.DiffStat[0].Path != "main.go" {
							t.Errorf("result stats=%+v", result.DiffStat)
						}
					})
				}
			})
			t.Run("ResultMessageRecordsDiffStatAndCommitSnapshot", func(t *testing.T) {
				t.Parallel()
				stub := &fetchRecorder{FakeBackend: testContainer()}
				stub.TurnSnapshotValue[0].TurnDiff = []runtime.GitFileStat{{Path: "change.go", LinesAdded: 6, LinesDeleted: 2}, {Path: "image.png", Binary: true}}
				stub.FetchedBranches = []runtime.FetchedBranch{{
					RepositoryPath: "/home/user/src/repo",
					BranchName:     "caic-0",
					CommitHash:     "2222222222222222222222222222222222222222",
				}}
				r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", stub), "", nil)
				changed := make(chan struct{}, 1)
				r.NotifyTaskChange = func() { changed <- struct{}{} }

				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.Repos = []repo.Mount{{Branch: "caic-0", ContainerPath: "/home/user/src/repo"}}
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(task.StateRunning)
				// Exercise delivery before AttachSession.
				addTaskMessage(tk, agent.NewTurnCommitSnapshotMessage([]v3.RepositoryCommit{{
					RepositoryPath: "/home/user/src/repo",
					BranchName:     "caic-0",
					CommitHash:     "1111111111111111111111111111111111111111",
				}}, true, nil))
				_, ch, unsub := tk.Subscribe(t.Context())
				t.Cleanup(unsub)

				run := beginTestExecution(t, r, tk)

				if err := taskNative(t.Context(), run.records, `{"type":"result","result":"done","num_turns":1}`); err != nil {
					t.Fatal(err)
				}

				// Wait for the dispatched message.
				timeout := time.After(time.Second)
				select {
				case got := <-ch:
					rr, ok := got.Message.(*agent.ResultMessage)
					if !ok {
						t.Fatalf("expected *agent.ResultMessage, got %T", got.Message)
					}
					if len(rr.DiffStat) != 1 || rr.DiffStat[0].Path != "main.go" {
						t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", rr.DiffStat)
					}
				case <-timeout:
					t.Fatal("timed out waiting for message")
				}
				if got := tk.LiveDiffStat(); len(got) != 1 || got[0].Path != "main.go" {
					t.Errorf("result did not update live stats without a newer read: %+v", got)
				}

				summaryMessage := recvMsg(t, ch)
				summary, ok := summaryMessage.(*agent.DiffStatMessage)
				if !ok || len(summary.DiffStat) != 1 {
					t.Fatalf("result summary = %#v", summaryMessage)
				}

				wantCommits := []v3.RepositoryCommit{{
					RepositoryPath: "/home/user/src/repo",
					BranchName:     "caic-0",
					CommitHash:     "2222222222222222222222222222222222222222",
				}}
				select {
				case got := <-ch:
					snapshot, ok := got.Message.(*agent.TurnCommitSnapshotMessage)
					if !ok {
						t.Fatalf("expected *agent.TurnCommitSnapshotMessage, got %T", got.Message)
					}
					if !reflect.DeepEqual(snapshot.RepositoryCommits, wantCommits) {
						t.Errorf("commit snapshot = %+v, want commits %+v", snapshot, wantCommits)
					}
					wantChange := &v3.ChangeStat{Files: 2, LinesAdded: 6, LinesDeleted: 2, BinaryFiles: 1}
					if !reflect.DeepEqual(snapshot.ChangeStat, wantChange) {
						t.Errorf("commit snapshot change = %+v, want %+v", snapshot.ChangeStat, wantChange)
					}
				case <-timeout:
					t.Fatal("timed out waiting for commit snapshot")
				}
				// The finished turn hands what it committed to the host, and leaves
				// what it did not commit pending in the container.
				if got := stub.Fetches(); len(got) != 1 || got[0].Commit {
					t.Errorf("Fetch calls = %+v, want one fetch without a commit", got)
				}
				run.work.Wait()
				if got := strings.Join(logLines(t, r.LogPath.Get()), "\n"); !strings.Contains(got, `"t":"git_summary"`) || !strings.Contains(got, `"repos"`) || !strings.Contains(got, `"t":"turn_commit_snapshot"`) || !strings.Contains(got, `"commit_hash":"2222222222222222222222222222222222222222"`) {
					t.Errorf("persisted task log = %q, want turn commit snapshot", got)
				}
				select {
				case <-changed:
					if state := tk.GetState(); state != task.StateWaiting {
						t.Errorf("state = %s, want %s", state, task.StateWaiting)
					}
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for task state change notification")
				}
			})

			t.Run("MutatingToolEmitsDiffStatWithoutFetch", func(t *testing.T) {
				t.Parallel()
				for _, tool := range []string{"Edit", "Bash", "Write", "NotebookEdit"} {
					t.Run(tool, func(t *testing.T) {
						t.Parallel()
						stub := &fetchRecorder{FakeBackend: testContainer()}
						r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", stub), "", nil)

						tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
						tk.Repos = []repo.Mount{{Branch: "caic-0"}}
						tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
						tk.SetState(task.StateRunning)
						_, ch, unsub := tk.Subscribe(t.Context())
						t.Cleanup(unsub)

						run := beginTestExecution(t, r, tk)

						// Send a ToolUseMessage with a mutating tool.
						toolID := "tool_edit_1"
						if err := taskNative(t.Context(), run.records, fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}}`, toolID, tool)); err != nil {
							t.Fatal(err)
						}
						// Drain the tool_use message from the subscriber.
						recvMsg(t, ch)

						// Send the tool result.
						if err := taskNative(t.Context(), run.records, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, toolID)); err != nil {
							t.Fatal(err)
						}

						msg := recvMsg(t, ch)
						if _, ok := msg.(*agent.ToolResultMessage); !ok {
							t.Fatalf("expected *agent.ToolResultMessage, got %T", msg)
						}
						msg = recvMsg(t, ch)
						ds, ok := msg.(*agent.DiffStatMessage)
						if !ok {
							t.Fatalf("expected *agent.DiffStatMessage, got %T", msg)
						}
						if len(ds.DiffStat) != 1 || ds.DiffStat[0].Path != "main.go" {
							t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", ds.DiffStat)
						}
						// The compact per-repo state is what the task summary card
						// renders; without it the card loses its Git summary.
						if len(ds.Repos) != 1 || ds.Repos[0].RepoIndex != 0 || ds.Repos[0].ChangedFiles != 1 || ds.Repos[0].LinesAdded != 5 || ds.Repos[0].LinesDeleted != 1 {
							t.Errorf("Repos = %+v, want one repo with 1 changed file, +5 -1", ds.Repos)
						}
						if stub.fetched.Load() {
							t.Error("Fetch was called for mutating tool result")
						}
					})
				}
			})

			t.Run("NonMutatingToolNoDiffStat", func(t *testing.T) {
				t.Parallel()
				stub := &fetchRecorder{FakeBackend: testContainer()}
				r := newTestAgentRuntime(t, newTestCheckout(t, "", t.TempDir(), stub), "", nil)

				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.Repos = []repo.Mount{{Branch: "caic-0"}}
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(task.StateRunning)
				_, ch, unsub := tk.Subscribe(t.Context())
				t.Cleanup(unsub)

				run := beginTestExecution(t, r, tk)

				toolID := "tool_read_1"
				if err := taskNative(t.Context(), run.records, fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}}`, toolID, "Read")); err != nil {
					t.Fatal(err)
				}
				recvMsg(t, ch) // drain tool_use

				if err := taskNative(t.Context(), run.records, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, toolID)); err != nil {
					t.Fatal(err)
				}
				// Only expect the ToolResultMessage, no DiffStatMessage.
				msg := recvMsg(t, ch)
				if _, ok := msg.(*agent.DiffStatMessage); ok {
					t.Error("unexpected DiffStatMessage emitted for non-mutating tool")
				}
				if stub.fetched.Load() {
					t.Error("Fetch was called for non-mutating tool")
				}
			})

			t.Run("EmitDiffStatBranch", func(t *testing.T) {
				probe := []v3.RepoState{{RepoIndex: 0, Branch: "caic-1", ChangedFiles: 3, LinesAdded: 10, LinesDeleted: 4}}

				newRuntime := func(t *testing.T, backend testRuntimeBackend) *AgentRuntime {
					r := newTestAgentRuntime(t, nil, "", nil)
					r.Runtimes = newTestRuntimeRouter(t, backend)
					r.Checkout = &repo.Checkout{Dir: "/repo", RelPath: "repo", GitTimeout: time.Minute}
					return r
				}
				newTask := func(t *testing.T) *Task {
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
					tk.Repos = []repo.Mount{{Branch: "caic-0"}}
					tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
					return tk
				}

				t.Run("FailedProbeKeepsPreviousState", func(t *testing.T) {
					t.Parallel()
					r := newRuntime(t, &statusFailingRuntime{FakeBackend: testContainer()})
					tk := newTask(t)
					addTaskMessage(tk, &agent.DiffStatMessage{MessageType: "caic_diff_stat", Repos: probe})
					before := len(tk.timeline)

					run := beginTestExecution(t, r, tk)
					if err := taskNative(t.Context(), run.records, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"probe","name":"Edit","input":{}}]}}`); err != nil {
						t.Fatal(err)
					}
					if err := taskNative(t.Context(), run.records, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"probe","content":"ok"}]}}`); err != nil {
						t.Fatal(err)
					}

					// A transient probe failure must not push an empty update
					// that would blank the stats the card already shows.
					if len(tk.timeline) != before+3 {
						t.Errorf("failed refresh did not publish stale state")
					}
					want := slices.Clone(probe)
					want[0].Stale = true
					if got := tk.Snapshot().RepoStates; !reflect.DeepEqual(got, want) {
						t.Errorf("RepoStates = %+v, want retained stale state %+v", got, want)
					}
				})

				t.Run("SuccessfulProbeEmitsRepos", func(t *testing.T) {
					t.Parallel()
					r := newRuntime(t, testContainer())
					tk := newTask(t)
					notifications := 0
					r.NotifyTaskChange = func() { notifications++ }

					run := beginTestExecution(t, r, tk)
					if err := taskNative(t.Context(), run.records, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"probe","name":"Edit","input":{}}]}}`); err != nil {
						t.Fatal(err)
					}
					if err := taskNative(t.Context(), run.records, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"probe","content":"ok"}]}}`); err != nil {
						t.Fatal(err)
					}

					if notifications != 1 {
						t.Fatalf("task-list notifications=%d,want1", notifications)
					}

					if len(tk.timeline) != 3 {
						t.Fatalf("timeline=%d,want tool use/result/summary", len(tk.timeline))
					}
					ds, ok := tk.timeline[2].Message.(*agent.DiffStatMessage)
					if !ok {
						t.Fatalf("message = %T, want *agent.DiffStatMessage", tk.timeline[0].Message)
					}
					if len(ds.Repos) != 1 || ds.Repos[0].RepoIndex != 0 || ds.Repos[0].ChangedFiles != 1 {
						t.Errorf("Repos = %+v, want one repo with 1 changed file", ds.Repos)
					}
					if got := tk.Snapshot().RepoStates; len(got) != 1 || got[0].ChangedFiles != 1 {
						t.Errorf("Snapshot().RepoStates = %+v, want the probe state", got)
					}
				})
			})

			t.Run("CloseJoinsTitleRefresh", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntime(t, nil, "", nil)
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				p := &titleBlockingProvider{started: make(chan struct{}), release: make(chan struct{})}
				tk.Provider = p
				run := beginTestExecution(t, r, tk)
				if err := taskNative(t.Context(), run.records, `{"type":"result","result":"done","num_turns":1}`); err != nil {
					t.Fatal(err)
				}

				<-p.started
				done := make(chan error, 1)
				go func() { done <- r.closeRun(tk, run) }()
				select {
				case err := <-done:
					t.Fatalf("owner closed before title worker: %v", err)
				default:
				}
				close(p.release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if got := tk.Title(); got != "Stale title" {
					t.Fatalf("Title = %q, want worker result", got)
				}
			})
			t.Run("TurnMeasurementsRefreshTitleAtMilestones", func(t *testing.T) {
				t.Parallel()
				stub := &fetchRecorder{FakeBackend: testContainer()}
				r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", stub), "", nil)

				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.Repos = []repo.Mount{{Branch: "caic-0"}}
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(task.StateRunning)
				tk.SetTitle("Stable title")
				tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.ResultMessage{}, &agent.ResultMessage{}}))
				provider := &titleProviderSpy{calls: make(chan struct{}, 4)}
				tk.Provider = provider
				_, ch, unsub := tk.Subscribe(t.Context())
				t.Cleanup(unsub)

				run := beginTestExecution(t, r, tk)

				// Send a mutating tool use + result and a ResultMessage.
				toolID := "tool_edit_1"
				if err := taskNative(t.Context(), run.records, fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}}`, toolID, "Edit")); err != nil {
					t.Fatal(err)
				}
				recvMsg(t, ch)
				if err := taskNative(t.Context(), run.records, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, toolID)); err != nil {
					t.Fatal(err)
				}
				recvMsg(t, ch)
				if err := taskNative(t.Context(), run.records, `{"type":"result","result":"done","num_turns":1}`); err != nil {
					t.Fatal(err)
				}
				recvMsg(t, ch)
				if err := taskNative(t.Context(), run.records, `{"type":"result","result":"done","num_turns":1}`); err != nil {
					t.Fatal(err)
				}
				recvMsg(t, ch)
				if err := r.closeRun(tk, run); err != nil {
					t.Fatal(err)
				}

				if !stub.fetched.Load() {
					t.Error("turn measurements skipped Fetch")
				}
				select {
				case <-provider.calls:
					t.Error("turn 3 or 4 requested a replacement title")
				case <-time.After(100 * time.Millisecond):
				}
				if got := tk.Title(); got != "Stable title" {
					t.Errorf("Title = %q, want Stable title", got)
				}
				run = beginTestExecution(t, r, tk)
				if err := taskNative(t.Context(), run.records, `{"type":"result","result":"done","num_turns":1}`); err != nil {
					t.Fatal(err)
				}
				if err := r.closeRun(tk, run); err != nil {
					t.Fatal(err)
				}
				select {
				case <-provider.calls:
				case <-time.After(time.Second):
					t.Fatal("turn 5 did not request a title refresh")
				}
			})

			t.Run("CloseRetainsCommittedMessages", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntime(t, nil, "", nil)

				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(task.StateRunning)

				run := beginTestExecution(t, r, tk)

				// Buffer several messages, then close without draining.
				msgs := []*agent.TextMessage{
					{Text: "first"},
					{Text: "second"},
					{Text: "third"},
				}
				for _, m := range msgs {
					if err := taskNative(t.Context(), run.records, fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`, m.Text)); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.closeRun(tk, run); err != nil {
					t.Fatal(err)
				}

				// All messages must be in t.msgs after done fires.
				got := tk.Messages()
				if len(got) != len(msgs) {
					t.Fatalf("Messages() has %d items, want %d", len(got), len(msgs))
				}
				for i, m := range got {
					tm, ok := m.(*agent.TextMessage)
					if !ok {
						t.Fatalf("Messages()[%d] = %T, want *agent.TextMessage", i, m)
					}
					if tm.Text != msgs[i].Text {
						t.Errorf("Messages()[%d].Text = %q, want %q", i, tm.Text, msgs[i].Text)
					}
				}
			})
		})

		t.Run("SessionFailurePublishesClearedPlan", func(t *testing.T) {
			t.Parallel()
			for _, restart := range []bool{false, true} {
				t.Run(strconv.FormatBool(restart), func(t *testing.T) {
					t.Parallel()
					backend := &agenttest.FakeBackend{HarnessName: "test"}
					r := newTestAgentRuntime(t, newTestCheckout(t, "", t.TempDir(), testContainer()), t.TempDir(), map[harness.Name]agent.Backend{"test": backend})
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
					tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "fake-instance"), runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)
					tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
						&agent.ToolUseMessage{ToolUseID: "write", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"first plan"}`)},
						&agent.ToolUseMessage{ToolUseID: "exit", Name: "ExitPlanMode"},
					}))
					tk.SetState(task.StateHasPlan)
					changes := make(chan Snapshot, 4)
					r.NotifyTaskChange = func() { changes <- tk.Snapshot() }
					log, err := r.openLog(tk)
					if err != nil {
						t.Fatal(err)
					}
					if err := log.Close(); err != nil {
						t.Fatal(err)
					}
					if restart {
						_, err = r.RestartSession(t.Context(), tk, agent.Prompt{Text: "execute"})
					} else {
						_, err = r.ClearContextSession(t.Context(), tk)
					}
					if err == nil {
						t.Fatal("unconfigured backend unexpectedly started")
					}
					select {
					case snapshot := <-changes:
						if snapshot.PlanExitID != "" || snapshot.PlanContent != "" || snapshot.InPlanMode {
							t.Fatalf("notification retained cleared plan: %+v", snapshot)
						}
					default:
						t.Fatal("no cleared-plan notification before session failure")
					}
				})
			}
		})

		t.Run("RestartSession", func(t *testing.T) {
			t.Parallel()
			for _, startState := range []task.State{task.StateWaiting, task.StateAsking, task.StateHasPlan} {
				t.Run(startState.String(), func(t *testing.T) {
					t.Parallel()
					logDir := t.TempDir()
					backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}

					r := newTestAgentRuntime(t, newTestCheckout(t, "", t.TempDir(), testContainer()), logDir, map[harness.Name]agent.Backend{"test": backend})

					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "old prompt"}, "test", "", "")
					tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0"}}
					tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "fake-instance"), runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)
					tk.SetState(startState)

					log, err := r.openLog(tk)
					if err != nil {
						t.Fatal(err)
					}
					if err := log.Close(); err != nil {
						t.Fatal(err)
					}
					h, err := r.RestartSession(t.Context(), tk, agent.Prompt{Text: "new plan"})
					if err != nil {
						t.Fatal(err)
					}
					if h == nil {
						t.Fatal("RestartSession returned nil handle")
					}
					if tk.GetState() != task.StateRunning {
						t.Errorf("state = %v, want %v", tk.GetState(), task.StateRunning)
					}
					if tk.InitialPrompt.Text != "old prompt" {
						t.Errorf("Prompt.Text = %q, want %q (must not be mutated by RestartSession)", tk.InitialPrompt.Text, "old prompt")
					}

					// The context passed to AgentBackend.Start must still be valid after
					// RestartSession returns (it must not be a request-scoped context).
					select {
					case <-backend.capturedCtx.Done():
						t.Error("context passed to AgentBackend was canceled; must use a long-lived context")
					default:
					}

					// Verify the session is functional: wait briefly and check the context
					// is still alive (not canceled by a short-lived HTTP request context).
					time.Sleep(50 * time.Millisecond)
					select {
					case <-backend.capturedCtx.Done():
						t.Error("context was canceled shortly after RestartSession returned")
					default:
					}

					// Clean up: close the session.
					if err := r.Shutdown(context.WithoutCancel(t.Context()), tk); err != nil {
						t.Error(err)
					}
				})
			}
		})

		t.Run("RestartSession/LogContainsContextCleared", func(t *testing.T) {
			t.Parallel()
			logDir := t.TempDir()
			backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}

			r := newTestAgentRuntime(t, newTestCheckout(t, "", t.TempDir(), testContainer()), logDir, map[harness.Name]agent.Backend{"test": backend})

			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0"}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "fake-instance"), runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)

			h1, err := r.StartSession(t.Context(), tk, agent.Prompt{})
			if err != nil {
				t.Fatal(err)
			}
			tk.SetState(task.StateRunning)

			// Simulate the agent writing a plan file.
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			if snap := tk.Snapshot(); snap.PlanContent != "the plan" {
				t.Fatalf("PlanContent = %q before restart, want %q", snap.PlanContent, "the plan")
			}

			// Gracefully end the first session so we can restart.
			_ = h1.GracefulStop(t.Context(), 5*time.Second)
			tk.SetState(task.StateWaiting)

			// Restart: should write context_cleared to the log before closing it.
			h2, err := r.RestartSession(t.Context(), tk, agent.Prompt{Text: "execute plan"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := r.Shutdown(context.WithoutCancel(t.Context()), tk); err != nil {
					t.Error(err)
				}
			})
			_ = h2

			// Read the raw log file and verify context_cleared is present.
			entries, err := os.ReadDir(logDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 log file, got %d", len(entries))
			}
			data, err := os.ReadFile(filepath.Join(logDir, entries[0].Name())) //nolint:gosec // test code, path is from t.TempDir()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"context_cleared"`) {
				t.Error("log file does not contain context_cleared marker")
			}
		})
	})
	t.Run("PromptPrecedesSynchronousStartupResult", func(t *testing.T) {
		t.Parallel()
		for _, operation := range []string{"start", "start_session", "restart"} {
			t.Run(operation, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				completed := make(chan struct{})
				backend := &synchronousPromptBackend{completed: completed}
				r := newTestAgentRuntimeWithRuntime(t, testContainer(), agent.Backends{"test": backend}, t.TempDir())
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
				tk.SetState(task.StateWaiting)
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "fake-instance"), runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)
				var completion sync.Once
				r.NotifyTaskChange = func() {
					if tk.GetState() == task.StateWaiting && tk.LastAgentResult() != "" {
						completion.Do(func() { close(completed) })
					}
				}
				var h *SessionHandle
				var err error
				switch operation {
				case "start":
					h, err = r.Start(ctx, tk, "")
				case "start_session":
					h, err = r.StartSession(ctx, tk, agent.Prompt{Text: "test"})
				case "restart":
					log, e := r.openLog(tk)
					if e != nil {
						t.Fatal(e)
					}
					if e := log.Close(); e != nil {
						t.Fatal(e)
					}
					h, err = r.RestartSession(ctx, tk, agent.Prompt{Text: "test"})
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cancel()
					_ = h.Drain()
					_ = r.closeRun(tk, h.Run)
				})
				if got := tk.GetState(); got != task.StateWaiting {
					t.Fatalf("state = %s, want completed turn to remain waiting", got)
				}
				var order []string
				for _, m := range tk.Messages() {
					switch m.(type) {
					case *agent.UserInputMessage, *agent.TextMessage, *agent.ResultMessage:
						order = append(order, m.Type())
					}
				}
				if !slices.Equal(order, []string{"user_input", "text", "result"}) {
					t.Fatalf("turn order = %v", order)
				}
				if got := tk.titleInput(); got != "Current title: test\nRecent activity:\nPrompt: test\nResult: done\n" {
					t.Fatalf("title input = %q", got)
				}
			})
		}
	})

	t.Run("MakeMetadata", func(t *testing.T) {
		t.Parallel()
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			metadata := MakeMetadata(tk)
			if len(metadata) != 3 {
				t.Fatalf("len = %d, want 3", len(metadata))
			}

			if metadata[runtime.MetadataTaskID] != tk.ID.String() {
				t.Errorf("metadata[%s] = %q", runtime.MetadataTaskID, metadata[runtime.MetadataTaskID])
			}
			if metadata[runtime.MetadataLegacyTaskID] != tk.ID.String() {
				t.Errorf("metadata[%s] = %q", runtime.MetadataLegacyTaskID, metadata[runtime.MetadataLegacyTaskID])
			}
			if metadata[runtime.MetadataHarness] != string(tk.Harness) {
				t.Errorf("metadata[%s] = %q", runtime.MetadataHarness, metadata[runtime.MetadataHarness])
			}
		})
		t.Run("WithGitHubToken", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.GitHubToken = true
			metadata := MakeMetadata(tk)
			if len(metadata) != 4 {
				t.Fatalf("len = %d, want 4", len(metadata))
			}
			if metadata[runtime.MetadataGitHubToken] != "true" {
				t.Errorf("metadata[%s] = %q, want true", runtime.MetadataGitHubToken, metadata[runtime.MetadataGitHubToken])
			}
		})
	})
	t.Run("StartPreservesTaskMetadata", func(t *testing.T) {
		t.Parallel()
		runtimeBackend := &metadataRuntime{FakeBackend: &runtimetest.FakeBackend{}}
		r := newTestAgentRuntimeWithRuntime(t, runtimeBackend, map[harness.Name]agent.Backend{"test": &instantExitBackend{}}, t.TempDir())
		r.RuntimeMetadata = runtime.Metadata{runtime.MetadataSmokeRun: "run-token", runtime.MetadataTaskID: "wrong"}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
		tk.StartedAt = time.Now().UTC()
		h, err := r.Start(t.Context(), tk, "")
		if err != nil {
			t.Fatal(err)
		}
		if h == nil {
			t.Fatal("Start returned nil handle")
		}
		if got := runtimeBackend.metadata[runtime.MetadataSmokeRun]; got != "run-token" {
			t.Errorf("metadata[%s] = %q, want run-token", runtime.MetadataSmokeRun, got)
		}
		if got := runtimeBackend.metadata[runtime.MetadataTaskID]; got != tk.ID.String() {
			t.Errorf("metadata[%s] = %q, want task ID", runtime.MetadataTaskID, got)
		}
		h.Session.Cancel()
	})

	t.Run("ProvisioningWriter", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		_, ch, unsub := tk.Subscribe(t.Context())
		t.Cleanup(unsub)

		r := newTestAgentRuntime(t, nil, t.TempDir(), nil)
		run := beginTestExecution(t, r, tk)
		w := &provisioningWriter{ctx: t.Context(), records: run.records}

		n, err := w.Write([]byte("hel"))
		if err != nil {
			t.Fatal(err)
		}
		_ = n
		select {
		case <-ch:
			t.Fatal("unexpected message for partial line")
		default:
		}

		_, err = w.Write([]byte("lo\n"))
		if err != nil {
			t.Fatal(err)
		}
		msg := recvMsg(t, ch)
		lm, ok := msg.(*agent.LogMessage)
		if !ok {
			t.Fatalf("expected *agent.LogMessage, got %T", msg)
		}
		if lm.Line != "hello" {
			t.Errorf("LogMessage.Line = %q, want %q", lm.Line, "hello")
		}

		_, err = w.Write([]byte("\n\n"))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
			t.Fatal("unexpected message for empty lines")
		default:
		}

		_, err = w.Write([]byte("line1\nline2\n"))
		if err != nil {
			t.Fatal(err)
		}
		msg1 := recvMsg(t, ch)
		if lm, ok := msg1.(*agent.LogMessage); !ok || lm.Line != "line1" {
			t.Errorf("msg1 = %+v", msg1)
		}
		msg2 := recvMsg(t, ch)
		if lm, ok := msg2.(*agent.LogMessage); !ok || lm.Line != "line2" {
			t.Errorf("msg2 = %+v", msg2)
		}
		if got := strings.Join(logLines(t, r.LogPath.Get()), "\n"); !strings.Contains(got, `{"line":"hello","t":"log"}`) || !strings.Contains(got, `{"line":"line2","t":"log"}`) {
			t.Errorf("persisted logs = %q", got)
		}

		if _, err := w.Write([]byte("final line")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
			t.Fatal("unexpected message before flush")
		default:
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		flushed := recvMsg(t, ch)
		if lm, ok := flushed.(*agent.LogMessage); !ok || lm.Line != "final line" {
			t.Errorf("flushed message = %#v, want final line", flushed)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
			t.Fatal("duplicate message after second flush")
		default:
		}
		if got := strings.Join(logLines(t, r.LogPath.Get()), "\n"); !strings.Contains(got, `{"line":"final line","t":"log"}`) {
			t.Errorf("persisted logs = %q", got)
		}
	})

	t.Run("Start", func(t *testing.T) {
		t.Parallel()
		t.Run("CleansUpInstanceWhenConnectFails", func(t *testing.T) {
			t.Parallel()
			connectErr := errors.New("connect failed")
			runtimeBackend := &connectFailureRuntime{FakeBackend: &runtimetest.FakeBackend{}, err: connectErr}
			r := newTestAgentRuntimeWithRuntime(t, runtimeBackend, nil, t.TempDir())
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			tk.StartedAt = time.Now().UTC()

			if _, err := r.Start(t.Context(), tk, ""); !errors.Is(err, connectErr) {
				t.Fatalf("Start error = %v, want %v", err, connectErr)
			}
			instanceID := runtime.NewID(runtimeBackend.Name(), "fake-container")
			if got := runtimeBackend.Status(instanceID); got != runtimetest.StatusPurged {
				t.Errorf("runtime status = %s, want purged", got)
			}
			if got := tk.RuntimeInstanceID(); got != "" {
				t.Fatalf("purged instance remains associated: %s", got)
			}
		})
		t.Run("CleansUpInstanceWhenSessionStartupFails", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name string
				err  error
			}{
				{name: "relay", err: errors.New("relay startup failed")},
				{name: "harness", err: errors.New("harness startup failed")},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					runtimeBackend := &runtimetest.FakeBackend{}
					r := newTestAgentRuntimeWithRuntime(t, runtimeBackend, map[harness.Name]agent.Backend{
						"test": &startupFailingBackend{FakeBackend: &agenttest.FakeBackend{}, err: tc.err},
					}, t.TempDir())
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
					tk.StartedAt = time.Now().UTC()

					if _, err := r.Start(t.Context(), tk, ""); !errors.Is(err, tc.err) {
						t.Fatalf("Start error = %v, want %v", err, tc.err)
					}
					instanceID := runtime.NewID(runtimeBackend.Name(), "fake-container")
					if got := runtimeBackend.Status(instanceID); got != runtimetest.StatusPurged {
						t.Errorf("runtime status = %s, want purged", got)
					}
					if got := tk.RuntimeInstanceID(); got != "" {
						t.Fatalf("purged instance remains associated: %s", got)
					}
				})
			}
		})
		t.Run("PassesModelEffortAndResumeSession", func(t *testing.T) {
			t.Parallel()
			backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}
			r := newTestAgentRuntimeWithRuntime(t, testContainer(), map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "model-1", "high")
			tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "session:not-a-guid"})
			tk.StartedAt = time.Now().UTC()
			h, err := r.Start(t.Context(), tk, "")
			if err != nil {
				t.Fatal(err)
			}
			stopCtx, stopCancel := context.WithTimeout(t.Context(), time.Second)
			_ = h.Session.Stop(stopCtx)
			stopCancel()
			_ = r.closeRun(tk, h.Run)

			if backend.capturedOpts.Model != "model-1" {
				t.Errorf("Model = %q, want model-1", backend.capturedOpts.Model)
			}
			if backend.capturedOpts.Effort != "high" {
				t.Errorf("Effort = %q, want high", backend.capturedOpts.Effort)
			}
			if got := backend.capturedOpts.ResumeSessionID; got != "session:not-a-guid" {
				t.Errorf("ResumeSessionID = %q, want session:not-a-guid", got)
			}
			if got := backend.capturedOpts.InitialPrompt.Text; got != "test" {
				t.Errorf("InitialPrompt = %q, want test", got)
			}
		})
		t.Run("PersistsSetupLogsOnFailure", func(t *testing.T) {
			t.Parallel()
			logDir := t.TempDir()
			r := newTestAgentRuntimeWithRuntime(t, &setupLogFailureRuntime{FakeBackend: &runtimetest.FakeBackend{}}, nil, logDir)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.StartedAt = time.Now().UTC()

			if _, err := r.Start(t.Context(), tk, ""); err == nil {
				t.Fatal("want launch error")
			}
			logs, err := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), agentbackends.Default("", nil)).LoadSettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != 1 {
				t.Fatalf("loaded %d logs, want 1", len(logs))
			}

			if err := logs[0].LoadMessages(); err != nil {
				t.Fatal(err)
			}
			if len(logs[0].Timeline) != 2 {
				t.Fatalf("loaded %d messages, want setup log and failure", len(logs[0].Timeline))
			}
			for i, want := range []string{"md setup output", "Task startup failed: claude startup failed during runtime setup: runtime launch failed"} {
				log, ok := logs[0].Timeline[i].Message.(*agent.LogMessage)
				if !ok || log.Line != want {
					t.Fatalf("message[%d] = %#v, want log %q", i, logs[0].Timeline[i], want)
				}
			}
			if logs[0].LastTrailer == nil {
				t.Fatal("result trailer is missing")
			}
			if got := logs[0].LastTrailer.StartupFailure; got == nil || got.Harness != "claude" || got.Phase != "runtime setup" || got.Cause != "runtime launch failed" {
				t.Fatalf("startup failure = %#v, want claude runtime setup diagnostic", got)
			}
		})

		t.Run("SurfacesSetupFailure", func(t *testing.T) {
			t.Parallel()
			launchErr := errors.New("invalid context name cache-custom-mount:~/.cache/caic: invalid reference format")
			r := newTestAgentRuntimeWithRuntime(t, &runtimetest.FakeBackend{LaunchErr: launchErr}, nil, t.TempDir())
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			tk.StartedAt = time.Now().UTC()
			_, ch, unsub := tk.Subscribe(t.Context())
			t.Cleanup(unsub)

			if _, err := r.Start(t.Context(), tk, ""); err == nil {
				t.Fatal("want launch error")
			}
			if got := tk.GetState(); got != task.StateFailed {
				t.Errorf("state = %v, want %v", got, task.StateFailed)
			}
			msg := recvMsg(t, ch)
			lm, ok := msg.(*agent.LogMessage)
			if !ok {
				t.Fatalf("message = %T, want *agent.LogMessage", msg)
			}
			if !strings.Contains(lm.Line, launchErr.Error()) {
				t.Errorf("log line = %q, want launch error", lm.Line)
			}
		})
	})

	t.Run("Setup", func(t *testing.T) {
		t.Parallel()
		t.Run("CreatesBranchBeforeLaunch", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runtimeBackend := &branchCheckingRuntime{FakeBackend: testContainer(), dir: clone}
			checkout := newTestCheckout(t, "main", clone, runtimeBackend)
			r := newTestAgentRuntime(t, checkout, t.TempDir(), nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", GitRoot: clone}}

			tk.SetRepoBranch(0, checkout.ReserveBranchName())
			if _, err := r.setup(t.Context(), tk, nil, "", nil); err != nil {
				t.Fatal(err)
			}
			if !runtimeBackend.checked {
				t.Fatal("runtime launch did not validate the mapped branch")
			}
		})
		t.Run("BranchFailurePreventsLaunch", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			runtimeBackend := testContainer()
			checkout := newTestCheckout(t, "main", dir, runtimeBackend)
			r := newTestAgentRuntime(t, checkout, t.TempDir(), nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", GitRoot: dir}}

			tk.SetRepoBranch(0, checkout.ReserveBranchName())
			if _, err := r.setup(t.Context(), tk, nil, "", nil); err == nil {
				t.Fatal("setup succeeded for a directory that is not a git repository")
			}
			instanceID := runtime.NewID(runtimeBackend.Name(), "fake-container")
			if got := runtimeBackend.Status(instanceID); got != runtimetest.StatusAbsent {
				t.Fatalf("runtime status = %s, want absent", got)
			}
		})
		t.Run("CustomBaseBranch", func(t *testing.T) {
			t.Parallel()
			// Verify that setup creates the task branch from t.BaseBranch
			// when it differs from the checkout's default BaseBranch.
			clone := initTestRepo(t, "main")
			// Create a feature branch on origin with a distinct commit.
			runGit(t, clone, "checkout", "-b", "feature")
			if err := os.WriteFile(filepath.Join(clone, "feature.txt"), []byte("feat\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "feature commit")
			runGit(t, clone, "push", "origin", "feature")
			runGit(t, clone, "checkout", "main")

			logDir := t.TempDir()
			stub := testContainer()
			checkout := newTestCheckout(t, "main", clone, stub)
			r := newTestAgentRuntime(t, checkout, logDir, nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", BaseBranch: "feature"}}

			tk.SetRepoBranch(0, checkout.ReserveBranchName())
			if _, err := r.setup(t.Context(), tk, nil, "", nil); err != nil {
				t.Fatal(err)
			}

			// The task branch must contain the feature commit (feature.txt).
			branch := tk.Primary().Branch
			out, execErr := exec.CommandContext(t.Context(), "git", "-C", clone, "show", branch+":feature.txt").Output() //nolint:gosec // controlled test args
			if execErr != nil {
				t.Fatalf("feature.txt not in task branch %s: %v", branch, execErr)
			}
			if string(out) != "feat\n" {
				t.Errorf("feature.txt content = %q, want %q", string(out), "feat\n")
			}
		})
		t.Run("LocalOnlyBaseBranch", func(t *testing.T) {
			t.Parallel()
			// Verify that setup works when BaseBranch exists only locally
			// (not pushed to origin).
			clone := initTestRepo(t, "main")
			runGit(t, clone, "checkout", "-b", "local-only")
			if err := os.WriteFile(filepath.Join(clone, "local.txt"), []byte("local\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "local commit")
			// Do NOT push to origin — branch exists only locally.
			runGit(t, clone, "checkout", "main")

			logDir := t.TempDir()
			stub := testContainer()
			checkout := newTestCheckout(t, "main", clone, stub)
			r := newTestAgentRuntime(t, checkout, logDir, nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", BaseBranch: "local-only"}}

			tk.SetRepoBranch(0, checkout.ReserveBranchName())
			if _, err := r.setup(t.Context(), tk, nil, "", nil); err != nil {
				t.Fatal(err)
			}

			// The task branch must contain the local commit (local.txt).
			branch := tk.Primary().Branch
			out, execErr := exec.CommandContext(t.Context(), "git", "-C", clone, "show", branch+":local.txt").Output() //nolint:gosec // controlled test args
			if execErr != nil {
				t.Fatalf("local.txt not in task branch %s: %v", branch, execErr)
			}
			if string(out) != "local\n" {
				t.Errorf("local.txt content = %q, want %q", string(out), "local\n")
			}
		})
	})

	t.Run("Cleanup", func(t *testing.T) {
		t.Parallel()
		t.Run("NoSessionUsesLiveStats", func(t *testing.T) {
			t.Parallel()
			// Simulate an adopted task after server restart: no active session, but
			// live stats were restored from log messages. Cleanup should fall back to
			// LiveStats for the result cost.
			clone := initTestRepo(t, "main")
			checkout := newTestCheckout(t, "main", clone, nil)
			r := newTestAgentRuntime(t, checkout, "", nil)

			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "main"}}
			tk.SetState(task.StateRunning)

			// Restore messages with cost info (simulates SeedTimelineParts from logs).
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.ResultMessage{
					MessageType:  "result",
					TotalCostUSD: 0.42,
					NumTurns:     5,
					DurationMs:   12345,
				},
			}))

			result := r.Cleanup(t.Context(), tk, task.StatePurged)
			if result.State != task.StatePurged {
				t.Errorf("state = %v, want %v", result.State, task.StatePurged)
			}
			if result.CostUSD != 0.42 {
				t.Errorf("CostUSD = %f, want 0.42", result.CostUSD)
			}
			if result.NumTurns != 5 {
				t.Errorf("NumTurns = %d, want 5", result.NumTurns)
			}
			if result.Duration != 12345*time.Millisecond {
				t.Errorf("Duration = %v, want %v", result.Duration, 12345*time.Millisecond)
			}
		})

		t.Run("StoppedTaskWritesTrailer", func(t *testing.T) {
			t.Parallel()
			// When a stopped task (no session handle) is purged, Cleanup must
			// reopen the existing log and write a caic_result trailer so the
			// task loads as "purged" (not "failed") on the next server restart.
			logDir := t.TempDir()
			clone := initTestRepo(t, "main")
			checkout := newTestCheckout(t, "main", clone, nil)
			r := newTestAgentRuntime(t, checkout, logDir, nil)

			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0"}}
			tk.SetState(task.StateStopped)

			// Create a pre-existing log file (written by StopTask scenario).
			logW, err := r.openLog(tk)
			if err != nil {
				t.Fatal(err)
			}
			_ = logW.Close()

			// Purge the stopped task — should reopen the log and write the trailer.
			r.Cleanup(t.Context(), tk, task.StatePurged)

			plainPath := filepath.Join(logDir, tk.ID.String()+"-org-repo-caic-0.jsonl")
			if _, err := os.Stat(plainPath); !os.IsNotExist(err) {
				t.Fatalf("plain log stat err = %v, want os.ErrNotExist", err)
			}
			if _, err := os.Stat(plainPath + ".zst"); err != nil {
				t.Fatal(err)
			}

			// Load the log and verify the trailer was written.
			lt, err := taskhistory.NewStore(testLogger(), taskslog.NewStore(logDir), agentbackends.Default("", nil)).LoadSettled()
			if err != nil {
				t.Fatal(err)
			}
			if len(lt) != 1 {
				t.Fatalf("Store.LoadSettled returned %d tasks, want 1", len(lt))
			}
			if lt[0].State != task.StatePurged {
				t.Errorf("state = %v, want StatePurged", lt[0].State)
			}
			if lt[0].LastTrailer == nil {
				t.Fatal("Result is nil, want caic_result trailer")
			}
			if lt[0].LastTrailer.State != task.StatePurged {
				t.Errorf("Result.State = %v, want StatePurged", lt[0].LastTrailer.State)
			}
		})

		t.Run("UsesLiveDiffStat", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			checkout := newTestCheckout(t, "main", clone, nil)
			r := newTestAgentRuntime(t, checkout, "", nil)

			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "main"}}
			tk.SetState(task.StateRunning)

			// Restore messages including a DiffStatMessage (simulates relay output).
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat: v3.DiffStat{
						{Path: "a.go", LinesAdded: 10, LinesDeleted: 3},
						{Path: "b.go", LinesAdded: 5, LinesDeleted: 0},
					},
				},
			}))

			result := r.Cleanup(t.Context(), tk, task.StatePurged)
			if len(result.DiffStat) != 2 {
				t.Fatalf("DiffStat has %d entries, want 2", len(result.DiffStat))
			}
			if result.DiffStat[0].Path != "a.go" || result.DiffStat[0].LinesAdded != 10 {
				t.Errorf("DiffStat[0] = %+v, want {a.go 10 3}", result.DiffStat[0])
			}
		})

		t.Run("DeletesUnmodifiedEmptyBranch", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runGit(t, clone, "branch", "caic-0", "origin/main")
			checkout := newTestCheckout(t, "main", clone, &runtimetest.FakeBackend{})
			r := newTestAgentRuntime(t, checkout, "", nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0", GitRoot: clone}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)

			r.Cleanup(t.Context(), tk, task.StatePurged)

			if caic0BranchExists(t, clone) {
				t.Fatal("caic-0 still exists, want deleted")
			}
		})

		t.Run("KeepsBranchWhenRuntimeReportsDiffAtPurge", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runGit(t, clone, "branch", "caic-0", "origin/main")
			checkout := newTestCheckout(t, "main", clone, testContainer())
			r := newTestAgentRuntime(t, checkout, "", nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0", GitRoot: clone}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)

			r.Cleanup(t.Context(), tk, task.StatePurged)

			if !caic0BranchExists(t, clone) {
				t.Fatal("caic-0 was deleted, want preserved")
			}
		})

		t.Run("KeepsBranchWhenDiffWasEverCreated", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runGit(t, clone, "branch", "caic-0", "origin/main")
			checkout := newTestCheckout(t, "main", clone, &runtimetest.FakeBackend{})
			r := newTestAgentRuntime(t, checkout, "", nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0", GitRoot: clone}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: v3.DiffStat{{Path: "main.go", LinesAdded: 1}}},
				&agent.DiffStatMessage{MessageType: "caic_diff_stat"},
			}))

			r.Cleanup(t.Context(), tk, task.StatePurged)

			if !caic0BranchExists(t, clone) {
				t.Fatal("caic-0 was deleted, want preserved")
			}
		})

		t.Run("KeepsBranchModifiedOnHost", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runGit(t, clone, "checkout", "-b", "caic-0", "origin/main")
			if err := os.WriteFile(filepath.Join(clone, "host.txt"), []byte("host\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, clone, "add", ".")
			runGit(t, clone, "commit", "-m", "host change")
			runGit(t, clone, "checkout", "main")
			checkout := newTestCheckout(t, "main", clone, &runtimetest.FakeBackend{})
			r := newTestAgentRuntime(t, checkout, "", nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0", GitRoot: clone}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)

			r.Cleanup(t.Context(), tk, task.StatePurged)

			if !caic0BranchExists(t, clone) {
				t.Fatal("caic-0 was deleted, want preserved")
			}
		})
	})

	t.Run("StopTask", func(t *testing.T) {
		t.Parallel()
		for _, state := range []task.State{task.StatePurging, task.StatePurged, task.StateCrashed, task.StateFailed} {
			t.Run(state.String(), func(t *testing.T) {
				t.Parallel()
				stub := testContainer()
				r := newTestAgentRuntimeWithRuntime(t, stub, nil, "")
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(state)

				r.StopTask(t.Context(), tk)

				if got := tk.GetState(); got != state {
					t.Errorf("state = %v, want %v", got, state)
				}
				if stub.Status("ctr-1") == runtimetest.StatusStopped {
					t.Error("StopTask called backend Stop for terminal/cleanup state")
				}
			})
		}
		t.Run("announces_the_stopped_transition", func(t *testing.T) {
			t.Parallel()
			stub := testContainer()
			r := newTestAgentRuntimeWithRuntime(t, stub, nil, "")
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			var announced []task.State
			r.NotifyTaskChange = func() { announced = append(announced, tk.GetState()) }

			r.StopTask(t.Context(), tk)

			// A scheduled purge waits for this announcement, so the stop cannot
			// leave the settled state unannounced until its own tail finishes.
			if !slices.Contains(announced, task.StateStopped) {
				t.Fatalf("announced states = %v, want the stopped transition", announced)
			}
		})
		t.Run("recordsStoppedDiskUsage", func(t *testing.T) {
			t.Parallel()
			stub := testContainer()
			runtimes, err := runtime.NewRouter([]runtime.System{&testRuntimeSystem{
				testRuntimeBackend: stub,
				DiskSizes: map[runtime.ID]int64{
					runtime.NewID("test-runtime", "ctr-1"): 700,
				},
			}}, metrics.Nop{})
			if err != nil {
				t.Fatal(err)
			}
			r := newTestAgentRuntimeWithRuntime(t, stub, nil, "")
			r.Runtimes = runtimes
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)

			r.recordStoppedDiskUsage(t.Context(), tk, tk.RuntimeInstanceID(), logtest.Logger(t))

			if diskUsed, ok := tk.DiskUsage(); !ok || diskUsed != 700 {
				t.Fatalf("DiskUsage() = (%d, %t), want (700, true)", diskUsed, ok)
			}
		})
	})

	t.Run("ReviveTask", func(t *testing.T) {
		t.Parallel()

		t.Run("refreshes_repository_state_after_live_tool", func(t *testing.T) {
			t.Parallel()
			backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}
			stub := &fetchRecorder{FakeBackend: testContainer()}
			r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", stub), t.TempDir(), map[harness.Name]agent.Backend{"test": backend})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "accepted"}, "test", "", "")
			tk.Repos = []repo.Mount{{Branch: "caic-0", ContainerPath: "/repo"}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "session-1"})
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.UserInputMessage{Text: "accepted"},
				&agent.ResultMessage{},
				&agent.ResultMessage{},
			}))
			tk.SetState(task.StateStopped)
			tk.SetTitle("existing title")
			provider := &titleProviderSpy{calls: make(chan struct{}, 1)}
			tk.Provider = provider

			previousLog, err := r.openLog(tk)
			if err != nil {
				t.Fatal(err)
			}
			if err := previousLog.Close(); err != nil {
				t.Fatal(err)
			}
			h, err := r.ReviveTask(t.Context(), tk)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = r.Shutdown(context.WithoutCancel(t.Context()), tk)
			})
			// Replace the one-shot revival probe with stale state. The next
			// live Bash result must publish an authoritative probe again.
			tk.addParsedMessage(agent.TimedMessage{Message: &agent.DiffStatMessage{Repos: []v3.RepoState{{RepoIndex: 0, Branch: "caic-0", Behind: 2}}}})
			if err := taskNative(t.Context(), h.Run.records, fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]}}`, "rebase", "Bash")); err != nil {
				t.Fatal(err)
			}
			if err := taskNative(t.Context(), h.Run.records, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}}`, "rebase")); err != nil {
				t.Fatal(err)
			}
			if err := taskNative(t.Context(), h.Run.records, `{"type":"result","result":"rebased","num_turns":1}`); err != nil {
				t.Fatal(err)
			}
			h.Run.work.Wait()

			states := tk.Snapshot().RepoStates
			if len(states) != 1 || states[0].Behind != 0 || states[0].ChangedFiles != 1 {
				t.Fatalf("RepoStates = %+v, want fresh probe with one changed file and no behind commits", states)
			}
			if !stub.fetched.Load() {
				t.Error("live turn did not fetch its commit snapshot")
			}
			select {
			case <-provider.calls:
				t.Error("revived session requested a replacement title")
			case <-time.After(100 * time.Millisecond):
			}
			if got := tk.Title(); got != "existing title" {
				t.Errorf("Title = %q, want existing title", got)
			}
		})
		t.Run("rejects_task_that_never_accepted_initial_prompt", func(t *testing.T) {
			t.Parallel()
			backend := &reviveCaptureBackend{FakeBackend: &agenttest.FakeBackend{}}
			r := newTestAgentRuntimeWithRuntime(t, testContainer(), map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			prompt := agent.Prompt{Text: "recover this task", Images: []v3.ImageData{{MediaType: "image/png", Data: "aW1hZ2U="}}}
			tk := mustNewTask(t, ksid.NewID(), prompt, "test", "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)

			if _, err := r.ReviveTask(t.Context(), tk); err == nil || !strings.Contains(err.Error(), "did not accept its initial prompt") {
				t.Fatalf("ReviveTask error = %v, want incomplete-startup error", err)
			}
			if len(backend.starts) != 0 {
				t.Fatalf("backend starts = %d, want 0", len(backend.starts))
			}
		})
		t.Run("does_not_duplicate_an_accepted_initial_prompt", func(t *testing.T) {
			t.Parallel()
			backend := &reviveCaptureBackend{FakeBackend: &agenttest.FakeBackend{}}
			r := newTestAgentRuntimeWithRuntime(t, testContainer(), map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "already accepted"}, "test", "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "session-1"})
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.UserInputMessage{Text: "already accepted"}}))
			tk.SetState(task.StateStopped)

			previousLog, err := r.openLog(tk)
			if err != nil {
				t.Fatal(err)
			}
			if err := previousLog.AppendMessage(taskhistory.ResultMessage(tk.Title(), &task.Result{State: task.StateStopped})); err != nil {
				t.Fatal(err)
			}
			if err := previousLog.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = r.ReviveTask(t.Context(), tk)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = r.Shutdown(context.WithoutCancel(t.Context()), tk)
			})

			intent, err := r.LogStore.LoadForTaskIDs([]string{tk.ID.String()})
			if err != nil || len(intent) != 1 || intent[0].State != task.StateProvisioning {
				t.Fatalf("revive intent=%v err=%v", intent, err)
			}
			if len(backend.starts) == 0 {
				t.Fatal("backend received no starts")
			}
			if got := backend.starts[0].InitialPrompt; got.Text != "" || len(got.Images) != 0 {
				t.Errorf("InitialPrompt = %#v, want empty prompt", got)
			}
			if got := backend.starts[0].ResumeSessionID; got != "session-1" {
				t.Errorf("ResumeSessionID = %q, want session-1", got)
			}
			if got := len(tk.Messages()); got != 1 {
				t.Errorf("len(Messages()) = %d, want 1", got)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("no_runtime", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, nil, nil, "")
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				if _, err := r.ReviveTask(t.Context(), tk); err == nil {
					t.Fatal("want error")
				}
			})
			t.Run("no_instance", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, testContainer(), nil, "")
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				if _, err := r.ReviveTask(t.Context(), tk); err == nil {
					t.Fatal("want error")
				}
			})
			t.Run("wrong_state", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, testContainer(), nil, "")
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(task.StateRunning)
				if _, err := r.ReviveTask(t.Context(), tk); err == nil {
					t.Fatal("want error")
				}
			})
		})
		t.Run("failure_stops_runtime_and_preserves_revivable_log", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name     string
				runtime  testRuntimeBackend
				backends map[harness.Name]agent.Backend
			}{
				{
					name:     "runtime_revive",
					runtime:  &reviveFailureRuntime{FakeBackend: &runtimetest.FakeBackend{}},
					backends: map[harness.Name]agent.Backend{"test": &agenttest.FakeBackend{}},
				},
				{
					name:    "session_start",
					runtime: testContainer(),
					backends: map[harness.Name]agent.Backend{
						"test": &agenttest.FakeBackend{},
					},
				},
				{
					name:    "ensure_session",
					runtime: testContainer(),
					backends: map[harness.Name]agent.Backend{
						"test": &reviveEnsureFailureBackend{},
					},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					logDir := t.TempDir()
					r := newTestAgentRuntimeWithRuntime(t, tc.runtime, tc.backends, logDir)
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
					tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
					tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.UserInputMessage{Text: "test"}}))
					tk.SetState(task.StateStopped)
					log, err := r.openLog(tk)
					if err != nil {
						t.Fatal(err)
					}
					if err := log.AppendMessage(taskhistory.ResultMessage(tk.Title(), &task.Result{State: task.StateStopped})); err != nil {
						t.Fatal(err)
					}
					if err := log.Close(); err != nil {
						t.Fatal(err)
					}
					if _, err := r.ReviveTask(t.Context(), tk); err == nil {
						t.Fatal("ReviveTask succeeded, want failure")
					}
					if got := tk.GetState(); got != task.StateCrashed {
						t.Fatalf("state = %s, want crashed", got)
					}
					status, ok := tc.runtime.(interface {
						Status(id runtime.ID) runtimetest.InstanceStatus
					})
					if !ok {
						t.Fatal("runtime fake has no status inspection")
					}
					if got := status.Status(tk.RuntimeInstanceID()); got != runtimetest.StatusStopped {
						t.Fatalf("runtime status = %s, want stopped", got)
					}
					loaded, err := r.LogStore.LoadUnsettled()
					if err != nil {
						t.Fatal(err)
					}
					if len(loaded) != 1 || loaded[0].LastTrailer == nil || loaded[0].LastTrailer.State != task.StateCrashed {
						t.Fatalf("loaded result = %+v, want crashed trailer", loaded)
					}
					if strings.HasSuffix(loaded[0].LogPath(), ".zst") {
						t.Fatalf("revivable log = %q, want uncompressed", loaded[0].LogPath())
					}
				})
			}
		})
		t.Run("resume_exit_preserves_context", func(t *testing.T) {
			t.Parallel()
			backend := &reviveEnsureFailureBackend{}
			r := newTestAgentRuntimeWithRuntime(t, testContainer(), map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "accepted"}, "test", "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.UserInputMessage{Text: "accepted"}}))
			tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "saved-session"})
			tk.SetState(task.StateStopped)
			log, err := r.openLog(tk)
			if err != nil {
				t.Fatal(err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReviveTask(t.Context(), tk); err == nil || !strings.Contains(err.Error(), "agent session exited") {
				t.Fatalf("resume error=%v", err)
			}
			if backend.starts != 1 || tk.GetSessionID() != "saved-session" || tk.GetState() != task.StateCrashed {
				t.Fatalf("starts=%d session=%s state=%s", backend.starts, tk.GetSessionID(), tk.GetState())
			}
		})
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			backend := &instantExitBackend{}
			r := newTestAgentRuntimeWithRuntime(t, testContainer(), map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.UserInputMessage{Text: "test"}}))
			tk.SetState(task.StateStopped)

			previousLog, err := r.openLog(tk)
			if err != nil {
				t.Fatal(err)
			}
			if err := previousLog.Close(); err != nil {
				t.Fatal(err)
			}
			h, err := r.ReviveTask(t.Context(), tk)
			if err != nil {
				t.Fatal(err)
			}
			if h == nil {
				t.Fatal("ReviveTask returned nil handle")
			}
			// The instantly-exiting session makes EnsureSession replace it with a
			// fresh idle relay; the task is left waiting for new input.
			if got := tk.GetState(); got != task.StateWaiting {
				t.Errorf("state = %v, want %v", got, task.StateWaiting)
			}
			if err := r.Shutdown(context.WithoutCancel(t.Context()), tk); err != nil {
				t.Error(err)
			}
		})
	})

	t.Run("ForkTask", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("no_runtime", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, nil, nil, "")
				source := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				fork := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				if _, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, ""); err == nil {
					t.Fatal("want error")
				}
			})
			t.Run("no_source_instance", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, testContainer(), nil, "")
				source := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				fork := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				if _, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, ""); err == nil {
					t.Fatal("want error")
				}
			})
		})
		t.Run("persists_setup_logs", func(t *testing.T) {
			t.Parallel()
			runtimeBackend := &forkLogRuntime{FakeBackend: testContainer()}
			backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}
			r := newTestAgentRuntimeWithRuntime(t, runtimeBackend, map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			source := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			source.Repos = []repo.Mount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "fork prompt"}, "test", "", "")
			fork.Repos = []repo.Mount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}}
			fork.StartedAt = time.Now().UTC()

			_, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = r.Shutdown(context.WithoutCancel(t.Context()), fork)
			})

			// Readable "<id>-<repo>-<branch>" name, computed at Open from the
			// pinned branch and stable afterward (matches a later Reopen).
			if got, want := filepath.Base(r.LogPath.Get()), fork.LogFilename(); got != want {
				t.Errorf("log filename = %q, want %q", got, want)
			}
			// The fork's primary branch is pinned before Fork and handed to the
			// runtime keyed by GitRoot, so the runtime creates exactly it.
			if _, ok := runtimeBackend.destPrimary("/src/caic"); !ok {
				t.Errorf("Fork received repos = %v, want an entry for /src/caic", runtimeBackend.capturedRepos)
			}
			logs := strings.Join(logLines(t, r.LogPath.Get()), "\n")
			first := strings.Index(logs, `{"line":"fork setup complete","t":"log"}`)
			last := strings.Index(logs, `{"line":"final setup line","t":"log"}`)
			if first < 0 || last <= first {
				t.Errorf("persisted setup logs missing or out of order: %q", logs)
			}
			if got := strings.Count(logs, `"t":"caic_meta"`); got != 1 {
				t.Errorf("metadata header count = %d, want 1", got)
			}
		})
		t.Run("pins_each_repo_to_its_own_branch", func(t *testing.T) {
			t.Parallel()
			runtimeBackend := &forkLogRuntime{FakeBackend: testContainer()}
			backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}
			r := newTestAgentRuntimeWithRuntime(t, runtimeBackend, map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			source := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			source.Repos = []repo.Mount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic-3"}}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			// Primary repo plus an extra repo whose branch the caller already
			// allocated from its own checkout (a different number than primary).
			fork := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			fork.Repos = []repo.Mount{
				{Name: "caic", GitRoot: "/src/caic", Branch: "caic-3"},
				{Name: "other", GitRoot: "/src/other", Branch: "caic-9"},
			}
			fork.StartedAt = time.Now().UTC()

			_, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = r.Shutdown(context.WithoutCancel(t.Context()), fork)
			})

			// Each repo is pinned to its own branch, not both to the primary's.
			if got, _ := runtimeBackend.destPrimary("/src/caic"); got != "caic-3" {
				t.Errorf("primary pin = %q, want caic-3", got)
			}
			if got, _ := runtimeBackend.destPrimary("/src/other"); got != "caic-9" {
				t.Errorf("extra repo pin = %q, want caic-9 (its own branch, not the primary's)", got)
			}
		})
		t.Run("persists_setup_logs_on_failure", func(t *testing.T) {
			t.Parallel()
			runtimeBackend := &forkLogRuntime{
				FakeBackend: testContainer(),
				forkErr:     errors.New("fork failed"),
			}
			r := newTestAgentRuntimeWithRuntime(t, runtimeBackend, agent.Backends{"test": &testBackend{FakeBackend: &agenttest.FakeBackend{}}}, t.TempDir())
			source := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			source.Repos = []repo.Mount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "fork prompt"}, "test", "", "")
			fork.Repos = []repo.Mount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}}
			fork.StartedAt = time.Now().UTC()

			if _, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, ""); err == nil {
				t.Fatal("want fork error")
			}
			if got := fork.GetState(); got != task.StateFailed {
				t.Errorf("state = %s, want failed", got)
			}
			logs := strings.Join(logLines(t, r.LogPath.Get()), "\n")
			for _, want := range []string{"fork setup complete", "final setup line", "Task startup failed: fork instance: fork failed"} {
				if !strings.Contains(logs, want) {
					t.Errorf("persisted log does not contain %q: %s", want, logs)
				}
			}
			if !strings.Contains(logs, `"t":"result"`) {
				t.Errorf("persisted log has no result trailer: %s", logs)
			}
		})
		t.Run("persists_setup_logs_on_session_failure", func(t *testing.T) {
			t.Parallel()
			runtimeBackend := &forkLogRuntime{FakeBackend: testContainer()}
			backend := &agenttest.FakeBackend{}
			r := newTestAgentRuntimeWithRuntime(t, runtimeBackend, map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			source := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			source.Repos = []repo.Mount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "fork prompt"}, "test", "", "")
			fork.Repos = []repo.Mount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}}
			fork.StartedAt = time.Now().UTC()

			if _, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, ""); err == nil {
				t.Fatal("want session start error")
			}
			if got := fork.GetState(); got != task.StateFailed {
				t.Errorf("state = %s, want failed", got)
			}
			logs := strings.Join(logLines(t, r.LogPath.Get()), "\n")
			for _, want := range []string{"fork setup complete", "final setup line", "Task startup failed: start session on fork"} {
				if !strings.Contains(logs, want) {
					t.Errorf("persisted log does not contain %q: %s", want, logs)
				}
			}
		})
		t.Run("preserves configured metadata on fork", func(t *testing.T) {
			t.Parallel()
			runtimeBackend := &forkLogRuntime{FakeBackend: &runtimetest.FakeBackend{}}
			r := newTestAgentRuntimeWithRuntime(t, runtimeBackend, map[harness.Name]agent.Backend{"test": &instantExitBackend{}}, t.TempDir())
			r.RuntimeMetadata = runtime.Metadata{runtime.MetadataSmokeRun: "run-token", runtime.MetadataTaskID: "wrong"}
			source := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "fork"}, "test", "", "")
			fork.StartedAt = time.Now().UTC()
			h, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if h == nil {
				t.Fatal("ForkTask returned nil handle")
			}
			if got := runtimeBackend.metadata[runtime.MetadataSmokeRun]; got != "run-token" {
				t.Errorf("metadata[%s] = %q, want run-token", runtime.MetadataSmokeRun, got)
			}
			if got := runtimeBackend.metadata[runtime.MetadataTaskID]; got != fork.ID.String() {
				t.Errorf("metadata[%s] = %q, want fork ID", runtime.MetadataTaskID, got)
			}
			h.Session.Cancel()
		})
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}
			r := newTestAgentRuntimeWithRuntime(t, testContainer(), map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			source := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "test", "", "")
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "fork prompt"}, "test", "", "")

			h, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if h == nil {
				t.Fatal("ForkTask returned nil handle")
			}
			if fork.RuntimeInstanceID() != runtime.NewID("test-runtime", "fake-fork") {
				t.Errorf("fork instance = %q, want test-runtime:fake-fork", fork.RuntimeInstanceID())
			}
			if got := fork.GetState(); got != task.StateRunning {
				t.Errorf("state = %v, want %v", got, task.StateRunning)
			}
			if err := r.Shutdown(context.WithoutCancel(t.Context()), fork); err != nil {
				t.Error(err)
			}
		})
	})
}

// BenchmarkAgentRuntimeRelayAppend isolates physical append plus its successful
// cursor callback, retaining the same operation measured before ordered ingestion.
// It does not include decoding, queue admission, or semantic publication.
func BenchmarkAgentRuntimeRelayAppend(b *testing.B) {
	r := newTestAgentRuntime(b, nil, b.TempDir(), nil)
	tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark"}, harness.OpenCode, "", "")
	sink, err := r.openLog(tk)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := sink.Close(); err != nil {
			b.Error(err)
		}
	})
	callback := &Run{task: tk}
	record := agent.PhysicalRecord{Encoded: []byte(`{"t":"agent","ts":1.000,"msg":{"text":"` + strings.Repeat("x", 1000) + `"}}` + "\n"), Provenance: agent.RecordProvenance{Source: agent.RecordSourceRelay}}
	b.SetBytes(int64(len(record.Encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := sink.AppendRecord(record); err != nil {
			b.Fatal(err)
		}
		callback.RecordAppended(record)
	}
}

// BenchmarkAgentRuntimeRelayIngestion includes actual ordered ingestion of a
// native no-output record through one persistent reader, with bounded input.
// It reports the new complete operation, not a delta from physical append alone.
func BenchmarkAgentRuntimeRelayIngestion(b *testing.B) {
	r := newTestAgentRuntime(b, nil, b.TempDir(), nil)
	tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark"}, harness.OpenCode, "", "")
	run := beginTestExecution(b, r, tk)
	frame := []byte(`{"t":"agent","ts":1.000,"msg":{"text":"` + strings.Repeat("x", 1000) + `"}}` + "\n")
	source := &repeatedRelayReader{frame: frame, remaining: int64(b.N) * int64(len(frame))}
	reader := agent.NewRelayRecordReader(b.Context(), source, run.records)
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, err := reader.ReadRecord(); err != nil {
			b.Fatal(err)
		}
	}
}

type repeatedRelayReader struct {
	frame     []byte
	offset    int
	remaining int64
}

func (r *repeatedRelayReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:int(r.remaining)]
	}
	n := copy(p, r.frame[r.offset:])
	r.offset = (r.offset + n) % len(r.frame)
	r.remaining -= int64(n)
	return n, nil
}

func BenchmarkAgentRuntimeSetup(b *testing.B) {
	clone := initTestRepo(b, "main")
	runtimeBackend := testContainer()
	checkout := newTestCheckout(b, "main", clone, runtimeBackend)
	r := newTestAgentRuntime(b, checkout, b.TempDir(), nil)
	r.Log = testLogger()

	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark"}, harness.Claude, "", "")
		tk.Repos = []repo.Mount{{Name: "org/repo", GitRoot: clone}}
		branch := checkout.ReserveBranchName()
		tk.SetRepoBranch(0, branch)
		b.StartTimer()

		if _, err := r.setup(b.Context(), tk, nil, "", nil); err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		runGit(b, clone, "branch", "-D", branch)
		b.StartTimer()
	}
}

func beginTestExecution(t testing.TB, r *AgentRuntime, tk *Task) *Run {
	run, err := r.beginRun(t.Context(), tk, false, false)
	if err != nil {
		t.Fatal(err)
	}
	close(run.startupDone)
	t.Cleanup(func() {
		tk.recordingMu.Lock()
		owned := tk.run.Load() == run
		tk.recordingMu.Unlock()
		if owned {
			if err := r.closeRun(tk, run); err != nil {
				t.Error(err)
			}
		}
	})
	return run
}

func mustNewAdoptionTask(t testing.TB, id ksid.ID, prompt agent.Prompt) *Task {
	tk, err := NewTask(id, prompt, harness.Claude, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func BenchmarkTaskAdoption(b *testing.B) {
	b.StopTimer()
	dir := b.TempDir()
	id := ksid.NewID()
	name := id.String() + "-org-repo-caic-0.jsonl"
	path := filepath.Join(dir, name)
	writeProductionAdoptionFixture(b, path)
	store := taskhistory.NewStore(slog.New(slog.DiscardHandler), taskslog.NewStore(dir), agentbackends.Default("", nil))

	b.ResetTimer()
	b.StartTimer()
	for range b.N {
		logs, err := store.LoadForTaskIDs([]string{id.String()})
		if err != nil {
			b.Fatal(err)
		}
		if len(logs) != 1 {
			b.Fatalf("Store.LoadForTaskIDs returned %d tasks, want 1", len(logs))
		}
		lt := logs[0]

		if err := lt.LoadMessages(); err != nil {
			b.Fatal(err)
		}
		tk := mustNewAdoptionTask(b, id, agent.Prompt{Text: "benchmark adoption"})
		tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-0"}}
		w, _, err := store.Reopen(name, tk.LogHeader())
		if err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSeedTimeline measures the replay pass that turns an adopted log
// into task state. The message mix mirrors a long session: mostly text and
// tool traffic punctuated by turn results, diff stats and context boundaries.
//
// The seeded entries contain no streaming deltas, so the in-place compaction
// performed by SeedTimelineParts leaves the shared fixture unchanged.
func BenchmarkSeedTimeline(b *testing.B) {
	const turns = 500
	entries := make([]agent.TimedMessage, 0, turns*8)
	producerTime := time.Unix(1_700_000_000, 0)
	appendMessage := func(message agent.Message) {
		entries = append(entries, agent.TimedMessage{Message: message, ProducerTime: producerTime})
		producerTime = producerTime.Add(time.Millisecond)
	}
	for turn := range turns {
		appendMessage(&agent.TextMessage{Text: "Working on the next step."})
		appendMessage(&agent.ToolUseMessage{ToolUseID: fmt.Sprintf("tool-%d", turn), Name: "Read", Input: json.RawMessage(`{"file_path":"/src/main.go"}`)})
		appendMessage(&agent.ToolResultMessage{ToolUseID: fmt.Sprintf("tool-%d", turn)})
		appendMessage(&agent.DiffStatMessage{MessageType: "diff_stat", DiffStat: v3.DiffStat{{Path: "main.go", LinesAdded: 3, LinesDeleted: 1}}})
		appendMessage(&agent.UsageMessage{Usage: agent.Usage{InputTokens: 900, OutputTokens: 120, CacheTTLSeconds: 300}, ContextWindow: 200000})
		appendMessage(&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done", TotalCostUSD: 0.02, NumTurns: 1, DurationMs: 1200,
			Usage: agent.Usage{InputTokens: 900, OutputTokens: 120, CacheReadInputTokens: 4000}})
		if turn%50 == 0 {
			appendMessage(&agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary"})
		}
	}
	id := ksid.NewID()
	b.ReportAllocs()

	for b.Loop() {
		mustNewAdoptionTask(b, id, agent.Prompt{Text: "benchmark seed"}).SeedTimelineParts(nil, entries)
	}
}

// BenchmarkBackwardMessagesLatestMatch measures the live request path when a
// caller searches a long timeline newest first for its latest matching message.
func BenchmarkBackwardMessagesLatestMatch(b *testing.B) {
	const messageCount = 4000
	msgs := make([]agent.Message, 0, messageCount)
	for i := range messageCount - 1 {
		msgs = append(msgs, &agent.TextMessage{Text: fmt.Sprintf("message %d", i)})
	}
	want := &agent.ToolUseMessage{ToolUseID: "target"}
	msgs = append(msgs, want)
	tk := mustNewAdoptionTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark lookup"})
	tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs))
	b.ReportAllocs()

	for b.Loop() {
		var got agent.Message
		for message := range tk.BackwardMessages() {
			if toolUse, ok := message.(*agent.ToolUseMessage); ok && toolUse.ToolUseID == "target" {
				got = toolUse
				break
			}
		}
		if got != want {
			b.Fatalf("latest matching message = %#v, want target", got)
		}
	}
}

func writeProductionAdoptionFixture(b *testing.B, path string) {
	file, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	meta, err := json.Marshal(v3.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV3),
		Prompt:      "benchmark adoption",
		Repos:       []v3.MetaRepo{{Name: "org/repo", Branch: "caic-0"}},
		Harness:     string(harness.Claude),
		StartedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		b.Fatal(err)
	}
	session, err := json.Marshal(v3.MetaSessionMessage{MessageType: "session", SessionID: "benchmark-session", AgentVersion: "2.1.0"})
	if err != nil {
		b.Fatal(err)
	}
	for _, line := range [][]byte{meta, session} {
		if _, err := writer.Write(append(line, '\n')); err != nil {
			b.Fatal(err)
		}
	}
	const native = `{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"Completed one adoption step."}]}}}` + "\n"
	for written := len(meta) + len(session) + 2; written < 128<<20; written += len(native) {
		if _, err := writer.WriteString(native); err != nil {
			b.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		b.Fatal(fmt.Errorf("sync production adoption fixture: %w", err))
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		b.Fatal(err)
	} else if info.Size() < 128<<20 {
		b.Fatalf("fixture size = %d, want at least %d", info.Size(), 128<<20)
	}
}
