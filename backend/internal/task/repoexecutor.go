// RepoExecutor is a thin, temporary facade over Runner, RepoWorkspace, and
// SessionRunner. It holds construction-time config (log/cache directories,
// harness env, backends) and delegates all lifecycle and session logic to
// those services; it owns no business logic of its own.

package task

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// RepoExecutor holds construction-time config and delegates lifecycle work to a
// Runner, session work to a SessionRunner, and repo/git/diff work to the
// embedded RepoWorkspace. It owns no business logic of its own.
type RepoExecutor struct {
	RepoWorkspace

	RuntimeStartTimeout time.Duration       // Timeout for instance start (image pull); defaults to 1 hour.
	LogDir              string              // Directory for raw JSONL session logs (required).
	CacheDir            string              // Cache directory (e.g. ~/.cache/caic) for harness model lists.
	HarnessEnv          map[string][]string // Per-harness KEY=VALUE env vars for runtime instances.
	EventReplayFactory  func(logPath string, h harness.Name) EventReplayWriter

	// Backends maps harness names to their Backend implementations. The executor
	// selects the backend matching Task.Harness.
	Backends map[harness.Name]agent.Backend

	initOnce sync.Once
}

// Start delegates task startup to a Runner. See Runner.Start.
func (r *RepoExecutor) Start(ctx context.Context, t *Task, resolvedGitHubToken string) (*SessionHandle, error) {
	r.initDefaults()
	return r.runner().Start(ctx, t, resolvedGitHubToken)
}

// Cleanup delegates task shutdown/purge to a Runner. See Runner.Cleanup.
func (r *RepoExecutor) Cleanup(ctx context.Context, t *Task, reason State) Result {
	r.initDefaults()
	return r.runner().Cleanup(ctx, t, reason)
}

// StopTask delegates graceful stop to a Runner. See Runner.StopTask.
func (r *RepoExecutor) StopTask(ctx context.Context, t *Task) {
	r.initDefaults()
	r.runner().StopTask(ctx, t)
}

// ReviveTask delegates instance revival to a Runner. See Runner.ReviveTask.
func (r *RepoExecutor) ReviveTask(ctx context.Context, t *Task) (*SessionHandle, error) {
	r.initDefaults()
	return r.runner().ReviveTask(ctx, t)
}

// ForkTask delegates task forking to a Runner. See Runner.ForkTask.
func (r *RepoExecutor) ForkTask(ctx context.Context, source, fork *Task, forkOpts *runtime.ForkOptions, resolvedGitHubToken string) (*SessionHandle, error) {
	r.initDefaults()
	return r.runner().ForkTask(ctx, source, fork, forkOpts, resolvedGitHubToken)
}

// AllocateBranch allocates a caic-N branch through the repo workspace.
func (r *RepoExecutor) AllocateBranch(ctx context.Context) (string, error) {
	r.initDefaults()
	return r.RepoWorkspace.AllocateBranch(ctx)
}

// SyncToOrigin delegates repo sync to the workspace.
func (r *RepoExecutor) SyncToOrigin(ctx context.Context, t *Task, force bool) (agent.DiffStat, []SafetyIssue, error) {
	r.initDefaults()
	return r.RepoWorkspace.SyncToOrigin(ctx, t, force)
}

// SyncToDefault delegates default-branch sync to the workspace.
func (r *RepoExecutor) SyncToDefault(ctx context.Context, t *Task, message string) (agent.DiffStat, []SafetyIssue, error) {
	r.initDefaults()
	return r.RepoWorkspace.SyncToDefault(ctx, t, message)
}

// Reconnect delegates agent relay reconnection to SessionRunner.
func (r *RepoExecutor) Reconnect(ctx context.Context, t *Task, skipSideEffects bool) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().Reconnect(ctx, t, skipSideEffects)
}

// EnsureSession delegates live-session confirmation to SessionRunner.
func (r *RepoExecutor) EnsureSession(ctx context.Context, t *Task, h *SessionHandle, tlog *slog.Logger) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().EnsureSession(ctx, t, h, tlog)
}

// StartSession delegates existing-instance session start to SessionRunner.
func (r *RepoExecutor) StartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().StartSession(ctx, t, prompt)
}

// RestartSession delegates context replacement to SessionRunner.
func (r *RepoExecutor) RestartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().RestartSession(ctx, t, prompt)
}

// ClearContextSession delegates context clearing to SessionRunner.
func (r *RepoExecutor) ClearContextSession(ctx context.Context, t *Task) (*SessionHandle, error) {
	r.initDefaults()
	return r.sessionRunner().ClearContextSession(ctx, t)
}

// DiffContent delegates repo diff rendering to the workspace.
func (r *RepoExecutor) DiffContent(ctx context.Context, t *Task, path string) (string, error) {
	r.initDefaults()
	return r.RepoWorkspace.DiffContent(ctx, t, path)
}

// BranchDiffStat delegates host-side diff stat restoration to the workspace.
func (r *RepoExecutor) BranchDiffStat(ctx context.Context, t *Task) agent.DiffStat {
	r.initDefaults()
	return r.RepoWorkspace.BranchDiffStat(ctx, t)
}

func (r *RepoExecutor) taskRuntime(t *Task) (runtime.InstanceID, []runtime.Repo, error) {
	r.initDefaults()
	return r.RepoWorkspace.taskRuntime(t)
}

// initDefaults populates timeout values and harness backend maps.
// Safe to call multiple times (sync.Once).
func (r *RepoExecutor) initDefaults() {
	r.RepoWorkspace.initDefaults()
	r.initOnce.Do(func() {
		if r.Backends == nil {
			r.Backends = map[harness.Name]agent.Backend{}
		}
		if r.RuntimeStartTimeout == 0 {
			r.RuntimeStartTimeout = time.Hour
		}
	})
}

// runner constructs a Runner bound to this executor's workspace, session
// runner, and start timeout. Callers must call initDefaults first.
func (r *RepoExecutor) runner() *Runner {
	return &Runner{
		Workspace:           &r.RepoWorkspace,
		Sessions:            r.sessionRunner(),
		RuntimeStartTimeout: r.RuntimeStartTimeout,
	}
}

func (r *RepoExecutor) sessionRunner() *SessionRunner {
	return &SessionRunner{
		Backends:  r.Backends,
		Workspace: &r.RepoWorkspace,
		Logs:      r.logStore(),
	}
}

func (r *RepoExecutor) logStore() *LogStore {
	return &LogStore{LogDir: r.LogDir, EventReplayFactory: r.EventReplayFactory}
}
