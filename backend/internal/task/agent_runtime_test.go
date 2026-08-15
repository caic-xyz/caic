// Tests for AgentRuntime: task runtime and agent-session operations through
// Start, Cleanup, StopTask, ReviveTask, and ForkTask.

package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
)

// instantExitBackend embeds testBackend but spawns a process that exits
// immediately (no stdin read), so tests exercising EnsureSession's
// already-done branch don't have to wait out its 10-second liveness timer.
type instantExitBackend struct {
	testBackend
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

type testRuntimeSystem struct {
	runtime.Lifecycle
	runtimetest.FakeInfo
}

var testCheckoutRuntimes sync.Map

func (*testRuntimeSystem) Name() runtime.Name { return "test-runtime" }

type metadataRuntime struct {
	*runtimetest.FakeBackend

	metadata runtime.Metadata
}

func (r *metadataRuntime) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.ID, error) {
	r.metadata = maps.Clone(opts.Metadata)
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

type forkLogRuntime struct {
	*runtimetest.FakeBackend

	forkErr       error
	metadata      runtime.Metadata
	capturedRepos []runtime.ForkRepo // opts.Repos seen by Fork.
}

func (r *forkLogRuntime) Fork(ctx context.Context, id runtime.ID, opts *runtime.ForkOptions) (runtime.ID, runtime.ConnectionInfo, []runtime.Repo, error) {
	r.capturedRepos = opts.Repos
	r.metadata = maps.Clone(opts.Metadata)
	if _, err := opts.LogWriter.Write([]byte("fork setup complete\nfinal setup line")); err != nil {
		return "", runtime.ConnectionInfo{}, nil, err
	}
	if r.forkErr != nil {
		return "", runtime.ConnectionInfo{}, nil, r.forkErr
	}
	forkID, conn, _, err := r.FakeBackend.Fork(ctx, id, opts)
	// Honor the pinned destination branch per repo, like the real runtime.
	out := make([]runtime.Repo, len(opts.Repos))
	for i, rp := range opts.Repos {
		branch := rp.DestPrimary
		if branch == "" {
			branch = "caic/fork"
		}
		out[i] = runtime.Repo{Branch: branch}
	}
	return forkID, conn, out, err
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

func newTestCheckout(t *testing.T, baseBranch, dir string, backend runtime.Lifecycle) *repo.Checkout {
	checkout := &repo.Checkout{
		BaseBranch: baseBranch,
		Dir:        dir,
		RelPath:    filepath.Base(dir),
		GitTimeout: time.Minute,
	}
	testCheckoutRuntimes.Store(checkout, newTestRuntimeRouter(t, backend))
	return checkout
}

func newTestRuntimeRouter(t *testing.T, backend runtime.Lifecycle) *runtime.Router {
	if backend == nil {
		return nil
	}
	rt, err := runtime.NewRouter(logtest.Logger(t), []runtime.System{&testRuntimeSystem{Lifecycle: backend}})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func newTestAgentRuntime(t *testing.T, checkout *repo.Checkout, logDir string, backends map[harness.Name]agent.Backend) *AgentRuntime {
	var runtimes *runtime.Router
	if value, ok := testCheckoutRuntimes.Load(checkout); ok {
		runtimes, _ = value.(*runtime.Router)
	}
	return &AgentRuntime{
		Backends:            backends,
		Logs:                LogStore{LogDir: logDir},
		Runtimes:            runtimes,
		Log:                 logtest.Logger(t),
		NotifyTaskChange:    func() {},
		Checkout:            checkout,
		RuntimeStartTimeout: time.Hour,
	}
}

func newTestAgentRuntimeWithRuntime(t *testing.T, backend runtime.Lifecycle, backends map[harness.Name]agent.Backend, logDir string) *AgentRuntime {
	r := newTestAgentRuntime(t, nil, logDir, backends)
	if backend == nil {
		return r
	}
	r.Runtimes = newTestRuntimeRouter(t, backend)
	return r
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
	return agent.NewSession(ctx, cmd, agent.NewConn(ctx, opts.Logger, stdin, opts.Log, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, opts.MsgCh, opts.Logger), nil
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

func TestRunner(t *testing.T) {
	t.Parallel()
	t.Run("Sessions", testRunnerSessions)

	t.Run("MakeMetadata", func(t *testing.T) {
		t.Parallel()
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
			tk := &Task{
				ID:      ksid.NewID(),
				Harness: harness.Claude,
			}
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
			tk := &Task{
				ID:          ksid.NewID(),
				Harness:     harness.Claude,
				GitHubToken: true,
			}
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
		tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: "test", StartedAt: time.Now().UTC()}
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
		_ = h.Session.Close()
	})

	t.Run("ProvisioningWriter", func(t *testing.T) {
		t.Parallel()
		tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
		_, ch, unsub := tk.Subscribe(t.Context())
		t.Cleanup(unsub)

		persisted := &agenttest.LogSink{Version: agent.LogVersionV2}
		w := &provisioningWriter{ctx: t.Context(), t: tk, log: persisted}

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
		if got := persisted.String(); !strings.Contains(got, `{"line":"hello","t":"log"}`) || !strings.Contains(got, `{"line":"line2","t":"log"}`) {
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
		if got := persisted.String(); !strings.Contains(got, `{"line":"final line","t":"log"}`) {
			t.Errorf("persisted logs = %q", got)
		}
	})

	t.Run("Start", func(t *testing.T) {
		t.Parallel()
		t.Run("PassesModelAndEffort", func(t *testing.T) {
			t.Parallel()
			backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}
			r := newTestAgentRuntimeWithRuntime(t, testContainer(), map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       "test",
				Model:         "model-1",
				Effort:        "high",
				StartedAt:     time.Now().UTC(),
			}
			h, err := r.Start(t.Context(), tk, "")
			if err != nil {
				t.Fatal(err)
			}
			stopCtx, stopCancel := context.WithTimeout(t.Context(), time.Second)
			_ = h.Session.Stop(stopCtx)
			stopCancel()
			h.CloseMsgCh()
			<-h.DispatchDone
			_ = h.Log.Close()

			if backend.capturedOpts.Model != "model-1" {
				t.Errorf("Model = %q, want model-1", backend.capturedOpts.Model)
			}
			if backend.capturedOpts.Effort != "high" {
				t.Errorf("Effort = %q, want high", backend.capturedOpts.Effort)
			}
		})
		t.Run("PersistsSetupLogsOnFailure", func(t *testing.T) {
			t.Parallel()
			logDir := t.TempDir()
			r := newTestAgentRuntimeWithRuntime(t, &setupLogFailureRuntime{FakeBackend: &runtimetest.FakeBackend{}}, nil, logDir)
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       harness.Claude,
				StartedAt:     time.Now().UTC(),
			}

			if _, err := r.Start(t.Context(), tk, ""); err == nil {
				t.Fatal("want launch error")
			}
			logs, err := LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != 1 {
				t.Fatalf("loaded %d logs, want 1", len(logs))
			}
			logs[0].SetNativeParserResolver(func(harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return claudecode.New().NewWire().ParseMessage, nil
			})
			if err := logs[0].LoadMessages(); err != nil {
				t.Fatal(err)
			}
			if len(logs[0].Msgs) != 2 {
				t.Fatalf("loaded %d messages, want setup log and failure", len(logs[0].Msgs))
			}
			for i, want := range []string{"md setup output", "Task startup failed: runtime launch failed"} {
				log, ok := logs[0].Msgs[i].(*agent.LogMessage)
				if !ok || log.Line != want {
					t.Fatalf("message[%d] = %#v, want log %q", i, logs[0].Msgs[i], want)
				}
			}
		})

		t.Run("SurfacesSetupFailure", func(t *testing.T) {
			t.Parallel()
			launchErr := errors.New("invalid context name cache-custom-mount:~/.cache/caic: invalid reference format")
			r := newTestAgentRuntimeWithRuntime(t, &runtimetest.FakeBackend{LaunchErr: launchErr}, nil, t.TempDir())
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       "test",
				StartedAt:     time.Now().UTC(),
			}
			_, ch, unsub := tk.Subscribe(t.Context())
			t.Cleanup(unsub)

			if _, err := r.Start(t.Context(), tk, ""); err == nil {
				t.Fatal("want launch error")
			}
			if got := tk.GetState(); got != StateFailed {
				t.Errorf("state = %v, want %v", got, StateFailed)
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
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", BaseBranch: "feature"}},
				Harness:       harness.Claude,
			}

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
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", BaseBranch: "local-only"}},
				Harness:       harness.Claude,
			}

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

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "main"}},
			}
			tk.SetState(StateRunning)

			// Restore messages with cost info (simulates RestoreMessages from logs).
			tk.RestoreMessages([]agent.Message{
				&agent.ResultMessage{
					MessageType:  "result",
					TotalCostUSD: 0.42,
					NumTurns:     5,
					DurationMs:   12345,
				},
			})

			result := r.Cleanup(t.Context(), tk, StatePurged)
			if result.State != StatePurged {
				t.Errorf("state = %v, want %v", result.State, StatePurged)
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

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
				Harness:       harness.Claude,
			}
			tk.SetState(StateStopped)

			// Create a pre-existing log file (written by StopTask scenario).
			logW, err := r.Logs.Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			_ = logW.Close()

			// Purge the stopped task — should reopen the log and write the trailer.
			r.Cleanup(t.Context(), tk, StatePurged)

			plainPath := filepath.Join(logDir, tk.ID.String()+"-org-repo-caic-0.jsonl")
			if _, err := os.Stat(plainPath); !os.IsNotExist(err) {
				t.Fatalf("plain log stat err = %v, want os.ErrNotExist", err)
			}
			if _, err := os.Stat(plainPath + ".zst"); err != nil {
				t.Fatal(err)
			}

			// Load the log and verify the trailer was written.
			lt, err := LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(lt) != 1 {
				t.Fatalf("LoadLogs returned %d tasks, want 1", len(lt))
			}
			if lt[0].State != StatePurged {
				t.Errorf("state = %v, want StatePurged", lt[0].State)
			}
			if lt[0].Result == nil {
				t.Fatal("Result is nil, want caic_result trailer")
			}
			if lt[0].Result.State != StatePurged {
				t.Errorf("Result.State = %v, want StatePurged", lt[0].Result.State)
			}
		})

		t.Run("UsesLiveDiffStat", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			checkout := newTestCheckout(t, "main", clone, nil)
			r := newTestAgentRuntime(t, checkout, "", nil)

			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "main"}},
			}
			tk.SetState(StateRunning)

			// Restore messages including a DiffStatMessage (simulates relay output).
			tk.RestoreMessages([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat: agent.DiffStat{
						{Path: "a.go", Added: 10, Deleted: 3},
						{Path: "b.go", Added: 5, Deleted: 0},
					},
				},
			})

			result := r.Cleanup(t.Context(), tk, StatePurged)
			if len(result.DiffStat) != 2 {
				t.Fatalf("DiffStat has %d entries, want 2", len(result.DiffStat))
			}
			if result.DiffStat[0].Path != "a.go" || result.DiffStat[0].Added != 10 {
				t.Errorf("DiffStat[0] = %+v, want {a.go 10 3}", result.DiffStat[0])
			}
		})

		t.Run("DeletesUnmodifiedEmptyBranch", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			runGit(t, clone, "branch", "caic-0", "origin/main")
			checkout := newTestCheckout(t, "main", clone, &runtimetest.FakeBackend{})
			r := newTestAgentRuntime(t, checkout, "", nil)
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0", GitRoot: clone}},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateStopped)

			r.Cleanup(t.Context(), tk, StatePurged)

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
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0", GitRoot: clone}},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateStopped)

			r.Cleanup(t.Context(), tk, StatePurged)

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
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0", GitRoot: clone}},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateStopped)
			tk.RestoreMessages([]agent.Message{
				&agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: agent.DiffStat{{Path: "main.go", Added: 1}}},
				&agent.DiffStatMessage{MessageType: "caic_diff_stat"},
			})

			r.Cleanup(t.Context(), tk, StatePurged)

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
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0", GitRoot: clone}},
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateStopped)

			r.Cleanup(t.Context(), tk, StatePurged)

			if !caic0BranchExists(t, clone) {
				t.Fatal("caic-0 was deleted, want preserved")
			}
		})
	})

	t.Run("StopTask", func(t *testing.T) {
		t.Parallel()
		for _, state := range []State{StatePurging, StatePurged, StateCrashed, StateFailed} {
			t.Run(state.String(), func(t *testing.T) {
				t.Parallel()
				stub := testContainer()
				r := newTestAgentRuntimeWithRuntime(t, stub, nil, "")
				tk := &Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
				}
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
	})

	t.Run("ReviveTask", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("no_runtime", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, nil, nil, "")
				tk := &Task{ID: ksid.NewID()}
				if _, err := r.ReviveTask(t.Context(), tk); err == nil {
					t.Fatal("want error")
				}
			})
			t.Run("no_instance", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, testContainer(), nil, "")
				tk := &Task{ID: ksid.NewID()}
				if _, err := r.ReviveTask(t.Context(), tk); err == nil {
					t.Fatal("want error")
				}
			})
			t.Run("wrong_state", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, testContainer(), nil, "")
				tk := &Task{ID: ksid.NewID()}
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
				tk.SetState(StateRunning)
				if _, err := r.ReviveTask(t.Context(), tk); err == nil {
					t.Fatal("want error")
				}
			})
		})
		t.Run("failure_persists_terminal_log", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name     string
				runtime  runtime.Lifecycle
				backends map[harness.Name]agent.Backend
			}{
				{
					name:    "runtime_revive",
					runtime: &reviveFailureRuntime{FakeBackend: &runtimetest.FakeBackend{}},
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
					tk := &Task{
						ID:            ksid.NewID(),
						InitialPrompt: agent.Prompt{Text: "test"},
						Harness:       "test",
					}
					tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
					tk.SetState(StateStopped)
					log, err := r.Logs.Open(tk)
					if err != nil {
						t.Fatal(err)
					}
					if err := r.Logs.WriteResultTrailer(log, tk.Title(), &Result{State: StateStopped}); err != nil {
						t.Fatal(err)
					}
					if err := log.Close(); err != nil {
						t.Fatal(err)
					}
					if _, err := r.ReviveTask(t.Context(), tk); err == nil {
						t.Fatal("ReviveTask succeeded, want failure")
					}
					loaded, err := LoadLogs(logDir)
					if err != nil {
						t.Fatal(err)
					}
					if len(loaded) != 1 || loaded[0].Result == nil || loaded[0].Result.State != StateFailed {
						t.Fatalf("loaded terminal result = %+v, want failed trailer", loaded)
					}
					if !isLogCompressed(loaded[0].LogPath()) {
						t.Fatalf("final log = %q, want compressed", loaded[0].LogPath())
					}
				})
			}
		})
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			backend := &instantExitBackend{}
			r := newTestAgentRuntimeWithRuntime(t, testContainer(), map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       "test",
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateStopped)

			h, err := r.ReviveTask(t.Context(), tk)
			if err != nil {
				t.Fatal(err)
			}
			if h == nil {
				t.Fatal("ReviveTask returned nil handle")
			}
			// The instantly-exiting session makes EnsureSession replace it with a
			// fresh idle relay; the task is left waiting for new input.
			if got := tk.GetState(); got != StateWaiting {
				t.Errorf("state = %v, want %v", got, StateWaiting)
			}
			tk.CloseAndDetachSession(t.Context())
		})
	})

	t.Run("ForkTask", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("no_runtime", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, nil, nil, "")
				source := &Task{ID: ksid.NewID()}
				fork := &Task{ID: ksid.NewID()}
				if _, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, ""); err == nil {
					t.Fatal("want error")
				}
			})
			t.Run("no_source_instance", func(t *testing.T) {
				t.Parallel()
				r := newTestAgentRuntimeWithRuntime(t, testContainer(), nil, "")
				source := &Task{ID: ksid.NewID()}
				fork := &Task{ID: ksid.NewID()}
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
			source := &Task{
				ID:      ksid.NewID(),
				Harness: "test",
				Repos:   []RepoMount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}},
			}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "fork prompt"},
				Harness:       "test",
				Repos:         []RepoMount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}},
				StartedAt:     time.Now().UTC(),
			}

			h, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				fork.CloseAndDetachSession(t.Context())
				_ = h.Log.Close()
			})

			// Readable "<id>-<repo>-<branch>" name, computed at Open from the
			// pinned branch and stable afterward (matches a later Reopen).
			if got, want := filepath.Base(fork.LogPath()), taskLogFileName(fork); got != want {
				t.Errorf("log filename = %q, want %q", got, want)
			}
			// The fork's primary branch is pinned before Fork and handed to the
			// runtime keyed by GitRoot, so the runtime creates exactly it.
			if _, ok := runtimeBackend.destPrimary("/src/caic"); !ok {
				t.Errorf("Fork received repos = %v, want an entry for /src/caic", runtimeBackend.capturedRepos)
			}
			logs := strings.Join(logLines(t, fork.LogPath()), "\n")
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
			source := &Task{
				ID:      ksid.NewID(),
				Harness: "test",
				Repos:   []RepoMount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic-3"}},
			}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			// Primary repo plus an extra repo whose branch the caller already
			// allocated from its own checkout (a different number than primary).
			fork := &Task{
				ID:      ksid.NewID(),
				Harness: "test",
				Repos: []RepoMount{
					{Name: "caic", GitRoot: "/src/caic", Branch: "caic-3"},
					{Name: "other", GitRoot: "/src/other", Branch: "caic-9"},
				},
				StartedAt: time.Now().UTC(),
			}

			h, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				fork.CloseAndDetachSession(t.Context())
				_ = h.Log.Close()
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
			r := newTestAgentRuntimeWithRuntime(t, runtimeBackend, nil, t.TempDir())
			source := &Task{
				ID:      ksid.NewID(),
				Harness: "test",
				Repos:   []RepoMount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}},
			}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "fork prompt"},
				Harness:       "test",
				Repos:         []RepoMount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}},
				StartedAt:     time.Now().UTC(),
			}

			if _, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, ""); err == nil {
				t.Fatal("want fork error")
			}
			if got := fork.GetState(); got != StateFailed {
				t.Errorf("state = %s, want failed", got)
			}
			logs := strings.Join(logLines(t, fork.LogPath()), "\n")
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
			source := &Task{
				ID:      ksid.NewID(),
				Harness: "test",
				Repos:   []RepoMount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}},
			}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "fork prompt"},
				Harness:       "test",
				Repos:         []RepoMount{{Name: "caic", GitRoot: "/src/caic", Branch: "caic/source"}},
				StartedAt:     time.Now().UTC(),
			}

			if _, err := r.ForkTask(t.Context(), source, fork, &runtime.ForkOptions{}, ""); err == nil {
				t.Fatal("want session start error")
			}
			if got := fork.GetState(); got != StateFailed {
				t.Errorf("state = %s, want failed", got)
			}
			logs := strings.Join(logLines(t, fork.LogPath()), "\n")
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
			source := &Task{ID: ksid.NewID(), Harness: "test"}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "fork"}, Harness: "test", StartedAt: time.Now().UTC()}
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
			_ = h.Session.Close()
		})
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			backend := &instantExitBackend{}
			r := newTestAgentRuntimeWithRuntime(t, testContainer(), map[harness.Name]agent.Backend{"test": backend}, t.TempDir())
			source := &Task{ID: ksid.NewID(), Harness: "test"}
			source.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-src"), runtime.ConnectionTarget{SSHHost: "ctr-src"}, "", "", 0)
			fork := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "fork prompt"},
				Harness:       "test",
			}

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
			if got := fork.GetState(); got != StateRunning {
				t.Errorf("state = %v, want %v", got, StateRunning)
			}
			fork.CloseAndDetachSession(t.Context())
		})
	})
}
func testRunnerSessions(t *testing.T) {
	t.Run("Reconnect", func(t *testing.T) {
		t.Parallel()
		t.Run("error_stateful_missing_session_id", func(t *testing.T) {
			t.Parallel()
			for _, harness := range []harness.Name{harness.Codex, harness.OpenCode} {
				t.Run(string(harness), func(t *testing.T) {
					t.Parallel()
					r := newTestAgentRuntime(t, nil, "", nil)
					tk := &Task{
						ID:            ksid.NewID(),
						InitialPrompt: agent.Prompt{Text: "test"},
						Harness:       harness,
					}
					tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
					tk.SetState(StateRunning)
					_, err := r.Reconnect(t.Context(), tk, true)
					if err == nil {
						t.Fatal("Reconnect returned nil error")
					}
					if !strings.Contains(err.Error(), "session ID missing") {
						t.Fatalf("err = %v, want missing session ID", err)
					}
				})
			}
		})
		t.Run("passes_history_to_attach_backend", func(t *testing.T) {
			t.Parallel()
			backend := &attachCaptureBackend{testBackend: testBackend{FakeBackend: &agenttest.FakeBackend{}}}
			r := newTestAgentRuntime(t, nil, filepath.Join(t.TempDir(), "logs"), map[harness.Name]agent.Backend{"test": backend})
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Harness:       "test",
			}
			logW, err := r.Logs.Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			if err := logW.Close(); err != nil {
				t.Fatal(err)
			}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateRunning)
			tk.RestoreMessages([]agent.Message{
				&agent.AskMessage{
					ToolUseID: "toolu-1",
					Questions: []agent.AskQuestion{{Question: "Which?"}},
				},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: agent.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: agent.PendingAskAction{
							Questions: []agent.AskQuestion{{Question: "Which?"}},
						},
					},
				},
			})

			h, err := r.Reconnect(t.Context(), tk, true)
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
		t.Run("missing_log_fails_closed", func(t *testing.T) {
			t.Parallel()
			backend := &attachCaptureBackend{testBackend: testBackend{FakeBackend: &agenttest.FakeBackend{}}}
			r := newTestAgentRuntime(t, nil, filepath.Join(t.TempDir(), "logs"), map[harness.Name]agent.Backend{"test": backend})
			tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Harness: "test"}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateRunning)

			if _, err := r.Reconnect(t.Context(), tk, true); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Reconnect error = %v, want os.ErrNotExist", err)
			}
			if backend.capturedAttachOpts.Log != nil {
				t.Fatal("AttachRelay called without authoritative local log")
			}
			if tk.LogPath() != "" {
				t.Fatalf("LogPath = %q, want empty", tk.LogPath())
			}
		})
	})

	t.Run("LogStore", func(t *testing.T) {
		t.Parallel()
		t.Run("CreatesFile", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			logDir := filepath.Join(dir, "logs")
			store := &LogStore{LogDir: logDir}
			tk := &Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
				Model:         "model-1",
				Effort:        "high",
			}
			w, err := store.Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			// Write something and close.
			_ = w.AppendNative([]byte("test\n"))
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
			store := &LogStore{LogDir: logDir}
			tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Name: "org/repo", Branch: "caic-0"}}}

			tk.Harness = harness.Claude

			// Initial Start writes the header.
			w, err := store.Open(tk)
			if err != nil {
				t.Fatal(err)
			}
			_ = w.Close()

			// Several reconnects (simulating repeated server restarts) append
			// without writing a new header.
			for range 3 {
				w, err := store.Reopen(tk)
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
			store := &LogStore{LogDir: filepath.Join(t.TempDir(), "logs")}
			tk := &Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Name: "org/repo", Branch: "caic-0"}}, Harness: harness.Claude}
			if _, err := store.Reopen(tk); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Reopen err = %v, want os.ErrNotExist", err)
			}
		})
	})

	t.Run("RuntimeDir", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			dir  string
			task *Task
			want string
		}{
			{task: &Task{Repos: []RepoMount{{ContainerPath: "~/src/caic-xyz/md"}}}, want: "/home/user/src/caic-xyz/md"},
			{dir: "/home/maruel/src/caic", want: "/home/user/src/caic"},
			{dir: "/home/alice/projects/myrepo", want: "/home/user/src/myrepo"},
			{dir: "/opt/repos/foo", want: "/home/user/src/foo"},
		}
		for _, tc := range tests {
			r := newTestAgentRuntime(t, newTestCheckout(t, "", tc.dir, nil), "", nil)
			tk := tc.task
			if tk == nil {
				tk = &Task{}
			}
			got := r.runtimeDir(tk)
			if got != tc.want {
				t.Errorf("runtimeDir(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		}
	})

	t.Run("StartMessageDispatch", func(t *testing.T) {
		t.Parallel()
		t.Run("ProducerTimeCarrier", func(t *testing.T) {
			t.Parallel()
			r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", nil), "", nil)
			tk := &Task{}
			_, sub, unsub := tk.Subscribe(t.Context())
			defer unsub()
			msgCh, done := r.startMessageDispatch(t.Context(), tk, false)
			log := &agenttest.LogSink{Version: agent.LogVersionV2}
			opts := agent.Options{Logger: logtest.Logger(t), MsgCh: msgCh, Log: log}
			var v2Message agent.Message
			cmd := exec.CommandContext(t.Context(), "python3", "-c", "import sys; print(sys.argv[1])", `{"t":"agent","ts":1.234,"msg":{}}`)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			session := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), logtest.Logger(t), stdin, log, &testWire{parse: func([]byte) ([]agent.Message, error) {
				v2Message = &agent.TextMessage{Text: "v2"}
				return []agent.Message{v2Message}, nil
			}}), stdout, opts.MsgCh, logtest.Logger(t))
			if err := session.Wait(); err != nil {
				t.Fatal(err)
			}
			if got := <-sub; got != v2Message {
				t.Fatalf("subscriber message = %T, want original %T", got, v2Message)
			}
			var v1 agent.ParsedMessage
			err = agent.DefaultReadMessages(t.Context(), logtest.Logger(t), strings.NewReader(`{"event":"legacy"}`+"\n"), func(parsed agent.ParsedMessage) {
				v1 = parsed
				opts.MsgCh <- parsed
			}, agent.DiscardLogSink{Version: agent.LogVersionV1}, agent.LogVersionV1, func([]byte) ([]agent.Message, error) {
				return []agent.Message{&agent.TextMessage{Text: "v1"}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !v1.ProducerTime.IsZero() {
				t.Fatalf("v1 producer time = %v, want zero", v1.ProducerTime)
			}
			<-sub
			close(msgCh)
			<-done
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "synthetic"}, false)
		})
		t.Run("ResultMessage", func(t *testing.T) {
			t.Parallel()
			stub := &fetchRecorder{FakeBackend: testContainer()}
			r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", stub), "", nil)
			changed := make(chan struct{}, 1)
			r.NotifyTaskChange = func() { changed <- struct{}{} }

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh, _ := r.startMessageDispatch(t.Context(), tk, false)

			rm := &agent.ResultMessage{MessageType: "result"}
			msgCh <- agent.ParsedMessage{Message: rm}
			close(msgCh)

			// Wait for the dispatched message.
			timeout := time.After(time.Second)
			select {
			case got := <-ch:
				rr, ok := got.(*agent.ResultMessage)
				if !ok {
					t.Fatalf("expected *agent.ResultMessage, got %T", got)
				}
				if len(rr.DiffStat) != 1 || rr.DiffStat[0].Path != "main.go" {
					t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", rr.DiffStat)
				}
			case <-timeout:
				t.Fatal("timed out waiting for message")
			}
			if !stub.fetched.Load() {
				t.Error("Fetch was not called on result message")
			}
			select {
			case <-changed:
				if state := tk.GetState(); state != StateWaiting {
					t.Errorf("state = %s, want %s", state, StateWaiting)
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

					tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
					tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
					tk.SetState(StateRunning)
					_, ch, unsub := tk.Subscribe(t.Context())
					defer unsub()

					msgCh, _ := r.startMessageDispatch(t.Context(), tk, false)

					// Send a ToolUseMessage with a mutating tool.
					toolID := "tool_edit_1"
					msgCh <- agent.ParsedMessage{Message: &agent.ToolUseMessage{
						ToolUseID: toolID,
						Name:      tool,
						Input:     json.RawMessage(`{}`),
					}}
					// Drain the tool_use message from the subscriber.
					recvMsg(t, ch)

					// Send the tool result.
					msgCh <- agent.ParsedMessage{Message: &agent.ToolResultMessage{
						ToolUseID: toolID,
					}}

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
					if stub.fetched.Load() {
						t.Error("Fetch was called for mutating tool result")
					}
					close(msgCh)
				})
			}
		})

		t.Run("NonMutatingToolNoDiffStat", func(t *testing.T) {
			t.Parallel()
			stub := &fetchRecorder{FakeBackend: testContainer()}
			r := newTestAgentRuntime(t, newTestCheckout(t, "", t.TempDir(), stub), "", nil)

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh, _ := r.startMessageDispatch(t.Context(), tk, false)

			toolID := "tool_read_1"
			msgCh <- agent.ParsedMessage{Message: &agent.ToolUseMessage{
				ToolUseID: toolID,
				Name:      "Read",
				Input:     json.RawMessage(`{}`),
			}}
			recvMsg(t, ch) // drain tool_use

			msgCh <- agent.ParsedMessage{Message: &agent.ToolResultMessage{
				ToolUseID: toolID,
			}}
			// Only expect the ToolResultMessage, no DiffStatMessage.
			msg := recvMsg(t, ch)
			if _, ok := msg.(*agent.DiffStatMessage); ok {
				t.Error("unexpected DiffStatMessage emitted for non-mutating tool")
			}
			if stub.fetched.Load() {
				t.Error("Fetch was called for non-mutating tool")
			}
			close(msgCh)
		})

		t.Run("SkipSideEffects", func(t *testing.T) {
			t.Parallel()
			stub := &fetchRecorder{FakeBackend: testContainer()}
			r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", stub), "", nil)

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}, Repos: []RepoMount{{Branch: "caic-0"}}}
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(StateRunning)
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			msgCh, done := r.startMessageDispatch(t.Context(), tk, true)

			// Send a mutating tool use + result and a ResultMessage.
			toolID := "tool_edit_1"
			msgCh <- agent.ParsedMessage{Message: &agent.ToolUseMessage{ToolUseID: toolID, Name: "Edit", Input: json.RawMessage(`{}`)}}
			recvMsg(t, ch)
			msgCh <- agent.ParsedMessage{Message: &agent.ToolResultMessage{ToolUseID: toolID}}
			recvMsg(t, ch)
			msgCh <- agent.ParsedMessage{Message: &agent.ResultMessage{MessageType: "result"}}
			recvMsg(t, ch)
			close(msgCh)
			<-done

			if stub.fetched.Load() {
				t.Error("Fetch was called despite skipSideEffects=true")
			}
		})

		t.Run("DispatchDrainBeforeClose", func(t *testing.T) {
			t.Parallel()
			r := newTestAgentRuntime(t, nil, "", nil)

			tk := &Task{InitialPrompt: agent.Prompt{Text: "test"}}
			tk.SetState(StateRunning)

			msgCh, done := r.startMessageDispatch(t.Context(), tk, false)

			// Buffer several messages, then close without draining.
			msgs := []*agent.TextMessage{
				{Text: "first"},
				{Text: "second"},
				{Text: "third"},
			}
			for _, m := range msgs {
				msgCh <- agent.ParsedMessage{Message: m}
			}
			close(msgCh)
			<-done

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

	t.Run("RestartSession", func(t *testing.T) {
		t.Parallel()
		for _, startState := range []State{StateWaiting, StateAsking, StateHasPlan} {
			t.Run(startState.String(), func(t *testing.T) {
				t.Parallel()
				logDir := t.TempDir()
				backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}

				r := newTestAgentRuntime(t, newTestCheckout(t, "", t.TempDir(), testContainer()), logDir, map[harness.Name]agent.Backend{"test": backend})

				tk := &Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "old prompt"},
					Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
					Harness:       "test",
				}
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "fake-instance"), runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)
				tk.SetState(startState)

				h, err := r.RestartSession(t.Context(), tk, agent.Prompt{Text: "new plan"})
				if err != nil {
					t.Fatal(err)
				}
				if h == nil {
					t.Fatal("RestartSession returned nil handle")
				}
				if tk.GetState() != StateRunning {
					t.Errorf("state = %v, want %v", tk.GetState(), StateRunning)
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
				tk.CloseAndDetachSession(t.Context())
			})
		}
	})

	t.Run("RestartSession/LogContainsContextCleared", func(t *testing.T) {
		t.Parallel()
		logDir := t.TempDir()
		backend := &testBackend{FakeBackend: &agenttest.FakeBackend{}}

		r := newTestAgentRuntime(t, newTestCheckout(t, "", t.TempDir(), testContainer()), logDir, map[harness.Name]agent.Backend{"test": backend})

		tk := &Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
			Repos:         []RepoMount{{Name: "org/repo", Branch: "caic-0"}},
			Harness:       "test",
		}
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "fake-instance"), runtime.ConnectionTarget{SSHHost: "fake-instance"}, "", "", 0)

		// Create an initial session with a log writer by using the backend
		// directly (Checkout.Start needs a instance backend).
		logW, err := (&LogStore{LogDir: logDir}).Open(tk)
		if err != nil {
			t.Fatal(err)
		}
		msgCh := make(chan agent.ParsedMessage, 16)
		session, err := backend.Start(t.Context(), &agent.Options{Logger: logtest.Logger(t), MsgCh: msgCh, Log: logW})
		if err != nil {
			t.Fatal(err)
		}
		alreadyDone := make(chan struct{})
		close(alreadyDone)
		h1 := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: alreadyDone, Log: logW}
		tk.AttachSession(h1)
		tk.SetState(StateRunning)

		// Simulate the agent writing a plan file.
		tk.addMessage(t.Context(), &agent.ToolUseMessage{
			ToolUseID: "tu1", Name: "Write",
			Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
		}, false)
		if snap := tk.Snapshot(); snap.PlanContent != "the plan" {
			t.Fatalf("PlanContent = %q before restart, want %q", snap.PlanContent, "the plan")
		}

		// Gracefully end the first session so we can restart.
		_ = h1.GracefulStop(t.Context(), 5*time.Second)
		tk.SetState(StateWaiting)

		// Restart: should write context_cleared to the log before closing it.
		h2, err := r.RestartSession(t.Context(), tk, agent.Prompt{Text: "execute plan"})
		if err != nil {
			t.Fatal(err)
		}
		defer tk.CloseAndDetachSession(t.Context())
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
}
