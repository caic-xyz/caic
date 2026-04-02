package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claude"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/gitutil"
	"golang.org/x/sync/errgroup"
)

// StartOptions holds optional flags for container startup.
type StartOptions struct {
	DockerImage string
	Harness     agent.Harness
	Tailscale   bool
	USB         bool
	Display     bool
	// GitHubToken is the resolved GitHub token to inject into the container's
	// environment. Empty means no token is injected.
	GitHubToken string
	// LogWriter receives provisioning log lines from the container backend.
	// Must not be nil.
	LogWriter io.Writer
}

// ContainerBackend abstracts md container lifecycle operations for testability.
type ContainerBackend interface {
	// Launch starts the container (image check/build + docker run) and
	// writes SSH config. Does NOT wait for SSH. Repos must have branches set.
	// Returns the container name assigned during launch.
	Launch(ctx context.Context, repos []md.Repo, labels []string, opts *StartOptions) (name string, err error)
	// Connect waits for SSH and pushes repos into the container identified
	// by name (returned by Launch). Returns the optional Tailscale FQDN.
	Connect(ctx context.Context, name string, repos []md.Repo, opts *StartOptions) (tailscaleFQDN string, err error)
	Diff(ctx context.Context, repo md.Repo, args ...string) (string, error)
	Fetch(ctx context.Context, repos []md.Repo) error
	// Stop gracefully stops the container without removing it. The container
	// can be restarted later with Revive.
	Stop(ctx context.Context, name string) error
	// Purge stops and removes the container identified by name, cleaning up
	// SSH config and git remotes for the given repos.
	Purge(ctx context.Context, name string, repos []md.Repo) error
	// Revive restarts a stopped (exited) container, re-establishes SSH, and
	// waits for connectivity. The container's filesystem is preserved.
	Revive(ctx context.Context, name string, repos []md.Repo) error
}

// Result holds the outcome of a completed task.
type Result struct {
	State       State
	DiffStat    agent.DiffStat
	CostUSD     float64
	Duration    time.Duration
	NumTurns    int
	Usage       agent.Usage
	AgentResult string
	Err         error
}

// Runner manages the serialization of setup and push operations.
type Runner struct {
	BaseBranch            string
	Dir                   string        // Absolute path to the git repository.
	GitTimeout            time.Duration // Timeout for git/container ops; defaults to 1 minute.
	ContainerStartTimeout time.Duration // Timeout for container start (image pull); defaults to 1 hour.
	LogDir                string        // Directory for raw JSONL session logs (required).

	// Container provides md container lifecycle operations. Must be set before
	// calling Start.
	Container ContainerBackend
	// Backends maps harness names to their Backend implementations. The runner
	// selects the backend matching Task.Harness.
	Backends map[agent.Harness]agent.Backend

	log      *slog.Logger
	initOnce sync.Once
	branchMu sync.Mutex // Serializes branch creation (nextID + git branch) to avoid duplicate names.
	nextID   int        // Next branch sequence number (protected by branchMu).
}

// provisioningWriter is an io.Writer that converts line-by-line output from the
// container backend into LogMessage events stored on the task for SSE streaming.
type provisioningWriter struct {
	ctx context.Context
	t   *Task
	buf []byte
}

func (w *provisioningWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			w.t.addMessage(w.ctx, &agent.LogMessage{Line: line}, false)
		}
	}
	return len(p), nil
}

func (r *Runner) initDefaults() {
	r.initOnce.Do(func() {
		if r.Backends == nil {
			r.Backends = map[agent.Harness]agent.Backend{
				agent.Claude:   claude.New(),
				agent.Codex:    codex.New(),
				agent.OpenCode: opencode.New(),
			}
		}
		if r.GitTimeout == 0 {
			r.GitTimeout = time.Minute
		}
		if r.ContainerStartTimeout == 0 {
			r.ContainerStartTimeout = time.Hour
		}
		repoName := filepath.Base(r.Dir)
		if r.Dir == "" {
			repoName = "(none)"
		}
		r.log = slog.With("repo", repoName)
	})
}

// backend returns the Backend for the given agent name.
func (r *Runner) backend(name agent.Harness) agent.Backend {
	return r.Backends[name]
}

// containerDir returns the working directory path inside an md container.
// md always mounts repos at /home/user/src/<basename>. Returns /home/user for no-repo runners.
func (r *Runner) containerDir() string {
	if r.Dir == "" {
		return "/home/user"
	}
	return "/home/user/src/" + filepath.Base(r.Dir)
}

// Init sets nextID past any existing caic-* branches so that restarts don't
// waste attempts on branches that already exist. No-op for no-repo runners.
func (r *Runner) Init(ctx context.Context) error {
	r.initDefaults()
	if r.Dir == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	highest, err := maxBranchSeqNum(ctx, r.Dir)
	if err != nil {
		return err
	}
	if highest >= r.nextID {
		r.nextID = highest + 1
	}
	return nil
}

// Reconnect reattaches to a running relay, or starts a new agent session
// resuming the previous conversation if no relay is available. Returns the
// SessionHandle so the caller can start a session watcher.
//
// Strategy:
//  1. Check if the relay daemon is alive (Unix socket exists in container).
//  2. If alive, attach to the relay. This is the preferred path because it
//     reconnects to the still-running agent process with zero message loss.
//  3. If attaching fails (relay died between check and attach), fall back to
//     starting a new agent session with --resume to continue the conversation.
//  4. If both fail, revert to StateWaiting so the user can retry or purge.
//
// State transitions:
//   - Relay attach: keeps StateWaiting/StateAsking if agent already finished its
//     turn; transitions to StateRunning only if the agent was mid-output.
//   - --resume fallback: always transitions to StateRunning since a new agent
//     process is started.
//   - All-fail: reverts to StateWaiting.
func (r *Runner) Reconnect(ctx context.Context, t *Task, skipSideEffects bool) (*SessionHandle, error) {
	r.initDefaults()
	if t.HasSession() {
		return nil, errors.New("session already active")
	}
	if t.Container == "" {
		return nil, errors.New("no container to reconnect to")
	}
	// Remember the state inferred from restored messages so we don't
	// blindly override it to StateRunning for an idle relay.
	prevState := t.GetState()

	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, skipSideEffects)

	logW, err := r.openLog(t)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		return nil, err
	}

	// Prefer attaching to a live relay (agent process still running).
	relayAlive, relayErr := agent.IsRelayRunning(ctx, t.Container)
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	if relayErr != nil {
		r.log.Warn("relay check failed, using --resume", "br", primaryBranch, "ctr", t.Container, "err", relayErr)
	}

	var session *agent.Session
	if relayAlive {
		// Only transition to StateRunning if the restored messages indicate
		// the agent was still producing output (no trailing ResultMessage).
		// If the agent had already completed its turn, keep the inferred
		// StateWaiting/StateAsking so the UI shows the correct status.
		if prevState != StateWaiting && prevState != StateAsking {
			t.SetState(StateRunning)
		}
		session, err = r.backend(t.Harness).AttachRelay(ctx, &agent.Options{
			Container:       t.Container,
			RelayOffset:     t.RelayOffset,
			ResumeSessionID: t.GetSessionID(),
		}, msgCh, logW)
		if err != nil {
			// Relay died between the IsRelayRunning check and the attach
			// attempt. This is a known race; fall back to --resume.
			r.log.Warn("attach relay failed, using --resume", "br", primaryBranch, "ctr", t.Container, "err", err)
			relayAlive = false
		}
	}
	if !relayAlive {
		// Starting a new session via --resume always re-engages the agent.
		t.SetState(StateRunning)
		session, err = r.backend(t.Harness).Start(ctx, &agent.Options{
			Container:       t.Container,
			Dir:             r.containerDir(),
			Model:           t.Model,
			ResumeSessionID: t.GetSessionID(),
		}, msgCh, logW)
	}
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		<-dispatchDone
		// Both attach and --resume failed. Revert to StateWaiting so the
		// user can try again (restart) or purge.
		t.SetState(StateWaiting)
		return nil, fmt.Errorf("reconnect: %w", err)
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, LogW: logW}
	t.AttachSession(h)
	return h, nil
}

// Start performs branch/container setup, starts the agent session, and sends
// the initial prompt. Returns the SessionHandle so the caller can start a
// session watcher.
//
// Sequence:
//  1. Create a new git branch from origin/<BaseBranch> (or the local branch if not on origin).
//  2. Start an md container on that branch.
//  3. Deploy the relay script and launch the agent (claude/gemini) via the
//     relay daemon. The relay owns the agent's stdin/stdout and persists
//     across SSH disconnects.
//  4. Send the initial prompt to the agent.
//
// The session is left open for follow-up messages via SendInput.
func (r *Runner) Start(ctx context.Context, t *Task) (*SessionHandle, error) {
	r.initDefaults()
	if r.Container == nil {
		return nil, errors.New("runner has no container backend configured")
	}
	if r.Dir != "" {
		t.SetState(StateBranching)
	}

	tStart := time.Now()
	// 1. Create branch (serialized) + start container (concurrent).
	r.log.Info("setup task")
	sr, err := r.setup(ctx, t, []string{"caic=" + t.ID.String(), "harness=" + string(t.Harness)})
	if err != nil {
		t.SetState(StateFailed)
		return nil, err
	}
	t.Container = sr.Container
	t.TailscaleFQDN = sr.TailscaleFQDN
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.log.Info("container ready", "br", primaryBranch, "ctr", t.Container, "dur", time.Since(tStart))

	// 2. Start the agent session.
	t.SetState(StateStarting)
	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false)
	logW, err := r.openLog(t)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		t.SetState(StateFailed)
		return nil, err
	}

	tSession := time.Now()
	tlog := r.log.With("br", primaryBranch, "ctr", t.Container)
	tlog.Info("starting session", "hns", t.Harness)
	session, err := r.backend(t.Harness).Start(ctx, &agent.Options{
		Container:     t.Container,
		Dir:           r.containerDir(),
		Model:         t.Model,
		InitialPrompt: t.InitialPrompt,
	}, msgCh, logW)
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		<-dispatchDone
		t.SetState(StateFailed)
		tlog.Error("session start failed", "err", err)
		return nil, err
	}

	// Store handle so SendInput can reach it.
	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, LogW: logW}
	t.AttachSession(h)

	t.addMessage(ctx, syntheticUserInput(t.InitialPrompt), false)
	t.SetState(StateRunning)
	tlog.Info("agent running", "session_dur", time.Since(tSession), "total_startup_dur", time.Since(tStart))
	return h, nil
}

// Cleanup is the single shutdown path for a task (Flow 1 in the relay
// shutdown protocol — see package agent). It sends the null-byte sentinel
// to trigger graceful agent exit, then kills the container.
//
// This is only called for intentional purge (user action or container
// death), never during backend restart. On restart, the relay daemon stays
// alive and the server reconnects via adoptOne → Reconnect.
//
// Steps:
//  1. Detach the session handle from the task.
//  2. If a session exists: Session.Close sends \x00 + closes stdin, wait up to 10s.
//  3. Set task state to reason (StatePurged or StateFailed).
//  4. Kill the container.
//  5. If graceful wait timed out, drain session now (container dead, SSH severed).
//  6. Close msgCh and logW, write log trailer.
//  7. Build and return Result.
func (r *Runner) Cleanup(ctx context.Context, t *Task, reason State) Result {
	r.initDefaults()
	h := t.DetachSession()

	name := t.Container

	// Graceful shutdown: close stdin so the agent can emit a final
	// ResultMessage with accurate cost/turns stats, then force-kill.
	var result *agent.ResultMessage
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.log.With("br", primaryBranch, "ctr", name)
	if h != nil {
		h.Session.Close()
		timer := time.NewTimer(20 * time.Second)
		select {
		case <-h.Session.Done():
			timer.Stop()
			result, _ = h.Session.Wait()
		case <-timer.C:
			tlog.Warn("session timeout, killing")
		}
	}

	t.SetState(reason)

	tlog.Info("purge container")
	if name != "" && r.Container != nil {
		if err := r.PurgeContainer(ctx, name, primaryBranch, t.ExtraMDRepos()); err != nil {
			tlog.Warn("purge failed", "err", err)
		}
	}

	// If the graceful wait timed out, wait for the session to drain now
	// that the container is dead and the SSH connection is severed.
	if h != nil && result == nil {
		result, _ = h.Session.Wait()
	}
	if h != nil {
		h.CloseMsgCh()
		<-h.DispatchDone
	}

	res := Result{
		State: reason,
	}
	if result != nil {
		res.CostUSD = result.TotalCostUSD
		res.Duration = time.Duration(result.DurationMs) * time.Millisecond
		res.NumTurns = result.NumTurns
		res.Usage = result.Usage
		res.AgentResult = result.Result
	}
	// Use accumulated live stats when they exceed the session result
	// (e.g. adopted container after restart where the session only
	// reflects the reconnected portion, not the full run).
	if liveCost, liveTurns, liveDur, liveUsage, _ := t.LiveStats(); liveCost > res.CostUSD {
		res.CostUSD = liveCost
		res.NumTurns = liveTurns
		res.Duration = liveDur
		res.Usage = liveUsage
	}
	// Use the relay's live diff stat. The ResultMessage.DiffStat is set
	// by startMessageDispatch during normal flow, but Cleanup may run
	// without a ResultMessage (e.g. user-initiated purge).
	if ds := t.LiveDiffStat(); len(ds) > 0 {
		res.DiffStat = ds
	}
	var logW io.WriteCloser
	if h != nil {
		logW = h.LogW
	} else {
		// Task was stopped before purge: the session handle (and its LogW) was
		// released by StopTask. Reopen the log for appending so we can write
		// the caic_result trailer; without it the task would load as "failed"
		// on the next server restart instead of "purged".
		var reopenErr error
		logW, reopenErr = r.reopenLog(t)
		if reopenErr != nil {
			tlog.Warn("reopen log for trailer failed", "err", reopenErr)
		}
	}
	writeLogTrailer(logW, t.Title(), &res)
	if logW != nil {
		_ = logW.Close()
	}
	return res
}

// StopTask gracefully shuts down the agent session and stops the container
// without removing it. The container can be revived later. Unlike Cleanup,
// this preserves git remotes and SSH config.
func (r *Runner) StopTask(ctx context.Context, t *Task) {
	r.initDefaults()
	h := t.DetachSession()

	name := t.Container
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.log.With("br", primaryBranch, "ctr", name)

	// Graceful shutdown: close stdin so the agent can emit a final result.
	if h != nil {
		h.Session.Close()
		timer := time.NewTimer(20 * time.Second)
		select {
		case <-h.Session.Done():
			timer.Stop()
		case <-timer.C:
			tlog.Warn("session timeout during stop")
		}
	}

	t.SetState(StateStopping)

	tlog.Info("stop container")
	if name != "" && r.Container != nil {
		if err := r.Container.Stop(ctx, name); err != nil {
			tlog.Warn("stop failed", "err", err)
		}
	}

	// Drain session after container is stopped, then wait for the dispatch
	// goroutine to finish processing all buffered messages so that t.msgs
	// is complete before the state transitions to StateStopped.
	if h != nil {
		_, _ = h.Session.Wait()
		h.CloseMsgCh()
		<-h.DispatchDone
	}

	t.SetState(StateStopped)

	var logW io.WriteCloser
	if h != nil {
		logW = h.LogW
	}
	if logW != nil {
		_ = logW.Close()
	}
}

// ReviveTask restarts a stopped container and reconnects to the agent.
// The container's filesystem is preserved from the previous run. After
// docker-start + SSH, Reconnect attaches to the relay (if alive) or
// resumes the session via --resume, landing in StateWaiting.
func (r *Runner) ReviveTask(ctx context.Context, t *Task) (*SessionHandle, error) {
	r.initDefaults()
	if r.Container == nil {
		return nil, errors.New("runner has no container backend configured")
	}
	if t.Container == "" {
		return nil, errors.New("no container to revive")
	}
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.log.With("br", primaryBranch, "ctr", t.Container)

	// 1. Revive the container (docker start + SSH).
	t.SetState(StateProvisioning)
	repos := t.MDRepos()
	tlog.Info("reviving container")
	if err := r.Container.Revive(ctx, t.Container, repos); err != nil {
		t.SetState(StateFailed)
		return nil, fmt.Errorf("revive container: %w", err)
	}

	// 2. Reconnect to the agent (attach relay or --resume).
	// skipSideEffects=true: the relay replays all historical messages on
	// attach and each would trigger fetch+diff+title if side effects were
	// enabled. Instead we do a single BranchDiffStat at the end.
	t.SetState(StateStarting)
	tlog.Info("reconnecting after revive", "sess", t.GetSessionID())
	h, err := r.Reconnect(ctx, t, true)
	if err != nil {
		t.SetState(StateFailed)
		return nil, fmt.Errorf("reconnect after revive: %w", err)
	}

	// 3. If --resume caused the session to exit immediately (e.g. previous
	// session was already complete), start a fresh idle session.
	h, err = r.EnsureSession(ctx, t, h, tlog)
	if err != nil {
		t.SetState(StateFailed)
		return nil, err
	}

	// 4. Compute host-side diff stat once, covering all replayed messages.
	if ds := r.BranchDiffStat(ctx, primaryBranch, t.ExtraMDRepos()); len(ds) > 0 {
		t.SetLiveDiffStat(ds)
	}
	tlog.Info("agent ready after revive", "state", t.GetState())
	return h, nil
}

// EnsureSession waits briefly for h to confirm it's alive. If the session
// exits within 10 seconds (e.g. --resume found a completed session), it
// starts a fresh idle relay so the task can accept new prompts.
func (r *Runner) EnsureSession(ctx context.Context, t *Task, h *SessionHandle, tlog *slog.Logger) (*SessionHandle, error) {
	select {
	case <-h.Session.Done():
		// Session exited immediately. Detach and start fresh.
		t.DetachSession()
		result, _ := h.Session.Wait()
		h.CloseMsgCh()
		<-h.DispatchDone
		_ = h.LogW.Close()
		sub := ""
		if result != nil {
			sub = result.Subtype
		}
		tlog.Info("resumed session exited, starting fresh relay", "result", sub)
		t.SetStateIf(StateRunning, StateWaiting)
		return r.StartSession(ctx, t, agent.Prompt{})
	case <-time.After(10 * time.Second):
		// Session is alive — all good.
		return h, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// StartSession starts a fresh relay+agent session on an existing container.
// If prompt is non-empty, it is sent as the initial input and the task
// transitions to StateRunning. If prompt is empty, the agent starts idle
// and the task stays in its current state (typically StateWaiting).
func (r *Runner) StartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	r.initDefaults()
	if t.Container == "" {
		return nil, errors.New("no container")
	}
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	tlog := r.log.With("br", primaryBranch, "ctr", t.Container)

	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false)
	logW, err := r.openLog(t)
	if err != nil {
		close(msgCh)
		<-dispatchDone
		return nil, err
	}

	tlog.Info("starting session", "hns", t.Harness)
	session, err := r.backend(t.Harness).Start(ctx, &agent.Options{
		Container:     t.Container,
		Dir:           r.containerDir(),
		Model:         t.Model,
		InitialPrompt: prompt,
	}, msgCh, logW)
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		<-dispatchDone
		tlog.Error("session start failed", "err", err)
		return nil, err
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, LogW: logW}
	t.AttachSession(h)
	if prompt.Text != "" || len(prompt.Images) > 0 {
		t.addMessage(ctx, syntheticUserInput(prompt), false)
		t.SetState(StateRunning)
	}
	return h, nil
}

// setupResult holds the outputs of setup: the container name and optional Tailscale FQDN.
// The primary branch is written directly into t.Repos[0].Branch during setup.
type setupResult struct {
	Container     string
	TailscaleFQDN string
}

// allocateBranchLocked fetches origin, resolves the start point, and creates
// the task branch. Must be called under branchMu. Used by AllocateBranch for
// extra repos; primary repo branch allocation uses reserveBranchID + fetchAndCreateBranch.
func (r *Runner) allocateBranchLocked(ctx context.Context, t *Task) (string, error) {
	detached := context.WithoutCancel(ctx)
	gitCtx, gitCancel := context.WithTimeout(detached, r.GitTimeout)
	defer gitCancel()
	// Fetch so that origin/<base> is up to date.
	if err := gitutil.Fetch(gitCtx, r.Dir); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	// Resolve effective base branch: use task override if provided.
	effectiveBase := r.BaseBranch
	if p := t.Primary(); p != nil && p.BaseBranch != "" {
		effectiveBase = p.BaseBranch
	}
	// Prefer the remote tracking ref, but fall back to the local branch when
	// the base branch only exists locally (not yet pushed to origin).
	startPoint := "origin/" + effectiveBase
	if _, err := gitutil.RevParse(gitCtx, r.Dir, startPoint); err != nil {
		startPoint = effectiveBase
	}
	// Assign a sequential branch name, skipping existing ones.
	var branch string
	var err error
	for range 100 {
		if gitCtx.Err() != nil {
			return "", gitCtx.Err()
		}
		branch = fmt.Sprintf("caic-%d", r.nextID)
		r.nextID++
		r.log.Info("creating branch", "br", branch, "base", effectiveBase)
		err = gitutil.CreateBranch(gitCtx, r.Dir, branch, startPoint)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}
	return branch, nil
}

// AllocateBranch allocates a caic-N branch for this runner's repo using the
// runner's base branch. Used by the server to allocate branches for extra repos
// before starting a container.
func (r *Runner) AllocateBranch(ctx context.Context) (string, error) {
	r.initDefaults()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	return r.allocateBranchLocked(ctx, &Task{})
}

// fetchAndCreateBranch fetches origin and creates the given branch from the
// resolved base. Acquires branchMu to serialize git operations across concurrent
// task setups on the same repo (git fetch/branch are not safe to run in parallel
// on the same working tree). Container.Launch can still run concurrently since it
// does not touch the repo.
func (r *Runner) fetchAndCreateBranch(ctx context.Context, t *Task, branch string) error {
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	gitCtx, gitCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer gitCancel()
	if err := gitutil.Fetch(gitCtx, r.Dir); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	effectiveBase := r.BaseBranch
	if p := t.Primary(); p != nil && p.BaseBranch != "" {
		effectiveBase = p.BaseBranch
	}
	startPoint := "origin/" + effectiveBase
	if _, err := gitutil.RevParse(gitCtx, r.Dir, startPoint); err != nil {
		startPoint = effectiveBase
	}
	r.log.Info("creating branch", "br", branch, "base", effectiveBase)
	if err := gitutil.CreateBranch(gitCtx, r.Dir, branch, startPoint); err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	return nil
}

// setup reserves a branch name, starts the container (Phase A) and creates the
// git branch concurrently, then completes container startup (Phase B).
// Phase A (docker run) and git fetch+branch-create overlap, cutting the
// branch-allocation time off the critical path.
func (r *Runner) setup(ctx context.Context, t *Task, labels []string) (setupResult, error) {
	// Reserve the branch ID instantly (under lock, ~µs). The branch itself is
	// created concurrently with docker run in Phase A.
	if r.Dir != "" {
		r.branchMu.Lock()
		t.Repos[0].Branch = fmt.Sprintf("caic-%d", r.nextID)
		r.nextID++
		r.branchMu.Unlock()
	}

	t.SetState(StateProvisioning)
	detached := context.WithoutCancel(ctx)
	var primaryBranch string
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	r.log.Info("starting container", "br", primaryBranch, "img", t.DockerImage, "hns", t.Harness, "ts", t.Tailscale, "usb", t.USB, "dpy", t.Display)
	tContainer := time.Now()
	startCtx, startCancel := context.WithTimeout(detached, r.ContainerStartTimeout)
	defer startCancel()

	opts := &StartOptions{
		DockerImage: t.DockerImage, Harness: t.Harness, Tailscale: t.Tailscale, USB: t.USB, Display: t.Display,
		GitHubToken: t.GitHubToken,
		LogWriter:   &provisioningWriter{ctx: ctx, t: t},
	}

	// Phase A: docker run + SSH config. Branch creation runs concurrently so
	// git fetch overlaps with the container SSH boot time (~500 ms–3 s).
	var repos []md.Repo
	if r.Dir != "" {
		repos = t.MDRepos()
	}
	var containerName string
	eg, egCtx := errgroup.WithContext(startCtx)
	eg.Go(func() error {
		name, err := r.Container.Launch(egCtx, repos, labels, opts)
		if err != nil {
			return err
		}
		containerName = name
		return nil
	})
	if r.Dir != "" {
		eg.Go(func() error {
			return r.fetchAndCreateBranch(egCtx, t, primaryBranch)
		})
	}
	if err := eg.Wait(); err != nil {
		return setupResult{}, err
	}

	// Phase B: wait for SSH + push (branch now exists locally).
	tailscaleFQDN, err := r.Container.Connect(startCtx, containerName, repos, opts)
	if err != nil {
		return setupResult{}, fmt.Errorf("start container: %w", err)
	}
	r.log.Info("container started", "br", primaryBranch, "dur", time.Since(tContainer))
	return setupResult{Container: containerName, TailscaleFQDN: tailscaleFQDN}, nil
}

// SyncToOrigin fetches changes from the container, runs safety checks, and
// pushes the container's remote-tracking ref to origin. If safety issues are
// found and force is false, it returns the issues without pushing.
func (r *Runner) SyncToOrigin(ctx context.Context, branch, container string, force bool, extraRepos []md.Repo) (agent.DiffStat, []SafetyIssue, error) {
	r.initDefaults()
	if r.Dir == "" {
		return nil, nil, errors.New("sync is not supported for no-repo tasks")
	}
	fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer fetchCancel()
	r.branchMu.Lock()
	r.log.Info("fetch", "br", branch)
	if err := r.Container.Fetch(fetchCtx, append([]md.Repo{{GitRoot: r.Dir, Branch: branch}}, extraRepos...)); err != nil {
		r.branchMu.Unlock()
		return nil, nil, err
	}
	ds := r.diffStat(fetchCtx, branch)
	r.branchMu.Unlock()

	ref := "refs/remotes/" + container + "/" + branch
	safetyCtx, safetyCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer safetyCancel()
	issues, err := CheckSafety(safetyCtx, r.Dir, ref, r.BaseBranch, ds)
	if err != nil {
		return ds, issues, fmt.Errorf("safety check: %w", err)
	}
	if len(issues) > 0 && !force {
		return ds, issues, nil
	}

	pushCtx, pushCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer pushCancel()
	if err := gitutil.PushRef(pushCtx, r.Dir, ref, branch, true); err != nil {
		return ds, issues, fmt.Errorf("push to origin: %w", err)
	}
	return ds, issues, nil
}

// SyncToDefault fetches changes from the container, runs safety checks, and
// squash-pushes onto the repo's default branch. Safety issues always block
// (no force override). The commit message is built from the task title.
func (r *Runner) SyncToDefault(ctx context.Context, branch, container, message string, extraRepos []md.Repo) (agent.DiffStat, []SafetyIssue, error) {
	r.initDefaults()
	if r.Dir == "" {
		return nil, nil, errors.New("sync is not supported for no-repo tasks")
	}
	fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer fetchCancel()
	r.branchMu.Lock()
	r.log.Info("fetch for default sync", "br", branch)
	if err := r.Container.Fetch(fetchCtx, append([]md.Repo{{GitRoot: r.Dir, Branch: branch}}, extraRepos...)); err != nil {
		r.branchMu.Unlock()
		return nil, nil, err
	}
	ds := r.diffStat(fetchCtx, branch)
	r.branchMu.Unlock()

	ref := "refs/remotes/" + container + "/" + branch
	safetyCtx, safetyCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer safetyCancel()
	issues, err := CheckSafety(safetyCtx, r.Dir, ref, r.BaseBranch, ds)
	if err != nil {
		return ds, issues, fmt.Errorf("safety check: %w", err)
	}
	if len(issues) > 0 {
		return ds, issues, nil
	}
	squashCtx, squashCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer squashCancel()
	if err := gitutil.SquashOnto(squashCtx, r.Dir, ref, r.BaseBranch, message); err != nil {
		return ds, issues, fmt.Errorf("squash onto %s: %w", r.BaseBranch, err)
	}
	return ds, issues, nil
}

// RestartSession closes the current agent session and starts a fresh one in
// the same container with a new prompt. Returns the new SessionHandle so the
// caller can start a session watcher.
func (r *Runner) RestartSession(ctx context.Context, t *Task, prompt agent.Prompt) (*SessionHandle, error) {
	r.initDefaults()

	state := t.GetState()
	if state != StateWaiting && state != StateAsking && state != StateHasPlan {
		return nil, fmt.Errorf("cannot restart in state %s", state)
	}

	// 1. Close current session gracefully and persist a context_cleared
	// marker to the log so that RestoreMessages can reset plan state on
	// server restart. The marker must be written before closing the log.
	oldH := t.CloseAndDetachSession()
	if oldH != nil {
		oldH.CloseMsgCh()
		<-oldH.DispatchDone
		if oldH.LogW != nil {
			writeContextCleared(oldH.LogW)
			_ = oldH.LogW.Close()
		}
	}

	// 2. Clear in-memory messages (sends context_cleared to subscribers).
	t.ClearMessages(ctx)

	// 3. Open new log segment.
	logW, err := r.openLog(t)
	if err != nil {
		t.SetState(StateFailed)
		return nil, fmt.Errorf("open log: %w", err)
	}

	// 4. Start new session.
	t.SetState(StateStarting)

	msgCh, dispatchDone := r.startMessageDispatch(ctx, t, false)

	var restartBranch string
	if p := t.Primary(); p != nil {
		restartBranch = p.Branch
	}
	tlog := r.log.With("br", restartBranch, "ctr", t.Container)
	tlog.Info("restarting session", "hns", t.Harness)
	session, err := r.backend(t.Harness).Start(ctx, &agent.Options{
		Container:     t.Container,
		Dir:           r.containerDir(),
		Model:         t.Model,
		InitialPrompt: prompt,
	}, msgCh, logW)
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		<-dispatchDone
		t.SetState(StateFailed)
		return nil, fmt.Errorf("start session: %w", err)
	}

	// 5. Store new handle.
	h := &SessionHandle{Session: session, MsgCh: msgCh, DispatchDone: dispatchDone, LogW: logW}
	t.AttachSession(h)

	t.addMessage(ctx, syntheticUserInput(prompt), false)

	t.SetState(StateRunning)
	tlog.Info("session restarted")
	return h, nil
}

// ReadRelayOutput reads the relay output.jsonl from the container using the
// backend matching agentName to parse messages.
func (r *Runner) ReadRelayOutput(ctx context.Context, container string, agentName agent.Harness) ([]agent.Message, int64, error) {
	r.initDefaults()
	b := r.backend(agentName)
	if b == nil {
		return nil, 0, fmt.Errorf("unknown harness %q", agentName)
	}
	return b.ReadRelayOutput(ctx, container)
}

// DiffContent returns the unified diff for the given branch, optionally
// filtered to a single file path. Holds branchMu during the fetch+diff.
func (r *Runner) DiffContent(ctx context.Context, branch, path string) (string, error) {
	r.initDefaults()
	if r.Dir == "" {
		return "", errors.New("diff is not supported for no-repo tasks")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	args := []string{}
	if path != "" {
		args = append(args, "--", path)
	}
	return r.Container.Diff(ctx, md.Repo{GitRoot: r.Dir, Branch: branch}, args...)
}

// PurgeContainer stops and removes the md container identified by containerName,
// cleaning up any git remotes for repos associated with this runner.
func (r *Runner) PurgeContainer(ctx context.Context, containerName, branch string, extraRepos []md.Repo) error {
	r.initDefaults()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	var repos []md.Repo
	if r.Dir != "" {
		repos = append([]md.Repo{{GitRoot: r.Dir, Branch: branch}}, extraRepos...)
	}
	return r.Container.Purge(ctx, containerName, repos)
}

// mutatingTools lists tool names whose execution may change files in the
// container, warranting a diff stat refresh after their result arrives.
var mutatingTools = map[string]struct{}{
	"Bash":         {},
	"Edit":         {},
	"Write":        {},
	"NotebookEdit": {},
}

// startMessageDispatch starts a goroutine that reads from msgCh and dispatches
// to t.addMessage. For ResultMessages, it fetches from the container first and
// attaches the diff stat. For tool results following a mutating tool (Edit,
// Bash, Write, NotebookEdit), it also fetches and emits a DiffStatMessage.
// When skipSideEffects is true, fetch+diff and title generation are suppressed
// (used during adoption where these are handled once at the end).
// Returns the message channel and a done channel that closes when the
// goroutine exits (after msgCh is fully drained).
func (r *Runner) startMessageDispatch(ctx context.Context, t *Task, skipSideEffects bool) (msgCh chan agent.Message, dispatchDone <-chan struct{}) {
	// Capture branch and extra repos outside the goroutine to avoid races.
	primaryBranch := ""
	if p := t.Primary(); p != nil {
		primaryBranch = p.Branch
	}
	extraRepos := t.ExtraMDRepos()
	msgCh = make(chan agent.Message, 256)
	done := make(chan struct{})
	dispatchDone = done
	go func() {
		defer close(done)
		// Track tool_use IDs from ToolUseMessage that may mutate files.
		pendingMutating := make(map[string]struct{})
		for m := range msgCh {
			switch msg := m.(type) {
			case *agent.ToolUseMessage:
				if _, ok := mutatingTools[msg.Name]; ok {
					pendingMutating[msg.ToolUseID] = struct{}{}
				}
			case *agent.ToolResultMessage:
				if !skipSideEffects && r.Container != nil && r.Dir != "" {
					if _, ok := pendingMutating[msg.ToolUseID]; ok {
						delete(pendingMutating, msg.ToolUseID)
						r.fetchDiffStatBranch(ctx, t, primaryBranch, extraRepos)
					}
				}
			case *agent.ResultMessage:
				if !skipSideEffects && r.Container != nil && r.Dir != "" {
					fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
					r.branchMu.Lock()
					if err := r.Container.Fetch(fetchCtx, append([]md.Repo{{GitRoot: r.Dir, Branch: primaryBranch}}, extraRepos...)); err != nil {
						r.log.Warn("fetch on result failed", "br", primaryBranch, "err", err)
					}
					msg.DiffStat = r.diffStat(fetchCtx, primaryBranch)
					r.branchMu.Unlock()
					fetchCancel()
				}
			}
			t.addMessage(ctx, m, skipSideEffects)
		}
	}()
	return
}

// fetchDiffStatBranch fetches from the container and emits a DiffStatMessage
// into the task's message stream. Used after mutating tool results to keep the
// live diff stat up to date. Branch and extraRepos are passed explicitly so
// this can be called safely from a goroutine started before branch allocation.
func (r *Runner) fetchDiffStatBranch(ctx context.Context, t *Task, branch string, extraRepos []md.Repo) {
	fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer fetchCancel()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	if err := r.Container.Fetch(fetchCtx, append([]md.Repo{{GitRoot: r.Dir, Branch: branch}}, extraRepos...)); err != nil {
		r.log.Warn("fetch on tool result failed", "br", branch, "err", err)
		return
	}
	ds := r.diffStat(fetchCtx, branch)
	if len(ds) == 0 {
		return
	}
	t.addMessage(ctx, &agent.DiffStatMessage{
		MessageType: "caic_diff_stat",
		DiffStat:    ds,
	}, false)
}

// BranchDiffStat fetches from the container and returns the host-side branch
// diff stat (md diff --numstat). Unlike the relay's diff_watcher which only
// tracks uncommitted changes, this captures the full branch diff relative to
// the base. Used by adoptOne to restore the diff stat after server restart.
func (r *Runner) BranchDiffStat(ctx context.Context, branch string, extraRepos []md.Repo) agent.DiffStat {
	r.initDefaults()
	if r.Container == nil || r.Dir == "" {
		return nil
	}
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	if err := r.Container.Fetch(fetchCtx, append([]md.Repo{{GitRoot: r.Dir, Branch: branch}}, extraRepos...)); err != nil {
		r.log.Warn("fetch for branch diff stat failed", "br", branch, "err", err)
		return nil
	}
	return r.diffStat(fetchCtx, branch)
}

// diffStat runs Diff("--numstat") and parses the output. Returns nil for no-repo runners.
func (r *Runner) diffStat(ctx context.Context, branch string) agent.DiffStat {
	if r.Dir == "" {
		return nil
	}
	numstat, err := r.Container.Diff(ctx, md.Repo{GitRoot: r.Dir, Branch: branch}, "--numstat")
	if err != nil {
		r.log.Warn("diff numstat failed", "br", branch, "err", err)
		return nil
	}
	return ParseDiffNumstat(numstat)
}

// openLog creates a JSONL log file in LogDir and writes a metadata header as
// the first line.
func (r *Runner) openLog(t *Task) (io.WriteCloser, error) {
	if err := os.MkdirAll(r.LogDir, 0o750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	safeRepo := ""
	safeBranch := ""
	if p := t.Primary(); p != nil {
		safeRepo = strings.ReplaceAll(p.Name, "/", "-")
		safeBranch = strings.ReplaceAll(p.Branch, "/", "-")
	}
	name := t.ID.String() + "-" + safeRepo + "-" + safeBranch + ".jsonl"
	f, err := os.OpenFile(filepath.Join(r.LogDir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // name is derived from ksid, not arbitrary user input.
	if err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}
	// Write metadata header as the first line.
	metaRepos := make([]agent.MetaRepo, len(t.Repos))
	for i, r := range t.Repos {
		metaRepos[i] = agent.MetaRepo{Name: r.Name, BaseBranch: r.BaseBranch, Branch: r.Branch}
	}
	meta := agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     1,
		Prompt:      t.InitialPrompt.Text,
		Title:       t.Title(),
		Repos:       metaRepos,
		Harness:     t.Harness,
		Model:       t.Model,
		StartedAt:   t.StartedAt,
		ForgeIssue:  t.ForgeIssue,
		Tailscale:   t.Tailscale,
		USB:         t.USB,
		Display:     t.Display,
	}
	if data, err := json.Marshal(meta); err == nil {
		_, _ = f.Write(append(data, '\n'))
	}
	return f, nil
}

// reopenLog opens an existing log file for appending without writing a new
// metadata header. Used by Cleanup to write the caic_result trailer for
// stopped tasks whose session handle has already been released.
func (r *Runner) reopenLog(t *Task) (io.WriteCloser, error) {
	if r.LogDir == "" {
		return nil, errors.New("no log dir")
	}
	safeRepo := ""
	safeBranch := ""
	if p := t.Primary(); p != nil {
		safeRepo = strings.ReplaceAll(p.Name, "/", "-")
		safeBranch = strings.ReplaceAll(p.Branch, "/", "-")
	}
	name := t.ID.String() + "-" + safeRepo + "-" + safeBranch + ".jsonl"
	return os.OpenFile(filepath.Join(r.LogDir, name), os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // name is derived from ksid, not arbitrary user input.
}

// writeLogTrailer appends a MetaResultMessage to the log file.
func writeLogTrailer(w io.Writer, title string, res *Result) {
	if w == nil {
		return
	}
	mr := agent.MetaResultMessage{
		MessageType:              "caic_result",
		State:                    res.State.String(),
		Title:                    title,
		CostUSD:                  res.CostUSD,
		Duration:                 res.Duration.Seconds(),
		NumTurns:                 res.NumTurns,
		InputTokens:              res.Usage.InputTokens,
		OutputTokens:             res.Usage.OutputTokens,
		CacheCreationInputTokens: res.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     res.Usage.CacheReadInputTokens,
		DiffStat:                 res.DiffStat,
		AgentResult:              res.AgentResult,
	}
	if res.Err != nil {
		mr.Error = res.Err.Error()
	}
	if data, err := json.Marshal(mr); err == nil {
		_, _ = w.Write(append(data, '\n'))
	}
}

// writeContextCleared appends a context_cleared system message to the log.
// Called before closing the old log writer in RestartSession so that
// RestoreMessages can reset plan state on server restart.
func writeContextCleared(w io.Writer) {
	msg := syntheticContextCleared()
	if data, err := json.Marshal(msg); err == nil {
		_, _ = w.Write(append(data, '\n'))
	}
}

// maxBranchSeqNum finds the highest sequence number N among all branches
// (local and remote) matching "caic-N". Returns -1 if no matching branches
// exist. Checking both local and remote is necessary because stopped tasks
// leave local branches that may never be pushed.
func maxBranchSeqNum(ctx context.Context, dir string) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "branch", "-a", "--format=%(refname:short)")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return -1, fmt.Errorf("git branch -a: %w", err)
	}
	highest := -1
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		// Match "caic-N" (local) or "<remote>/caic-N" (remote).
		// Use strings.Cut on "/caic-" for remote refs and
		// strings.HasPrefix for local refs to avoid matching unrelated
		// branch names that happen to contain "caic-".
		var numStr string
		if strings.HasPrefix(line, "caic-") {
			numStr = line[len("caic-"):]
		} else if _, after, ok := strings.Cut(line, "/caic-"); ok {
			numStr = after
		} else {
			continue
		}
		n, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	return highest, nil
}
