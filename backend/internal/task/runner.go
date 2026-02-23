package task

import (
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

	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/agent/claude"
	"github.com/maruel/caic/backend/internal/agent/codex"
	"github.com/maruel/md/gitutil"
)

// StartOptions holds optional flags for container startup.
type StartOptions struct {
	DockerImage string
	Tailscale   bool
	USB         bool
	Display     bool
}

// ContainerBackend abstracts md container lifecycle operations for testability.
type ContainerBackend interface {
	Start(ctx context.Context, dir, branch string, labels []string, opts StartOptions) (name, tailscaleFQDN string, err error)
	Diff(ctx context.Context, dir, branch string, args ...string) (string, error)
	Fetch(ctx context.Context, dir, branch string) error
	Kill(ctx context.Context, dir, branch string) error
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
	Dir                   string // Absolute path to the git repository.
	MaxTurns              int
	GitTimeout            time.Duration // Timeout for git/container ops; defaults to 1 minute.
	ContainerStartTimeout time.Duration // Timeout for container start (image pull); defaults to 1 hour.
	LogDir                string        // Directory for raw JSONL session logs (required).

	// Container provides md container lifecycle operations. Must be set before
	// calling Start.
	Container ContainerBackend
	// Backends maps harness names to their Backend implementations. The runner
	// selects the backend matching Task.Harness.
	Backends map[agent.Harness]agent.Backend

	initOnce sync.Once
	branchMu sync.Mutex // Serializes operations that need a specific branch checked out (md commands).
	nextID   int        // Next branch sequence number (protected by branchMu).
}

func (r *Runner) initDefaults() {
	r.initOnce.Do(func() {
		if r.Backends == nil {
			r.Backends = map[agent.Harness]agent.Backend{
				agent.Claude: &claude.Backend{},
				agent.Codex:  &codex.Backend{},
			}
		}
		if r.GitTimeout == 0 {
			r.GitTimeout = time.Minute
		}
		if r.ContainerStartTimeout == 0 {
			r.ContainerStartTimeout = time.Hour
		}
	})
}

// backend returns the Backend for the given agent name.
func (r *Runner) backend(name agent.Harness) agent.Backend {
	return r.Backends[name]
}

// containerDir returns the working directory path inside an md container.
// md always mounts repos at /home/user/src/<basename>.
func (r *Runner) containerDir() string {
	return "/home/user/src/" + filepath.Base(r.Dir)
}

// Init sets nextID past any existing caic-* branches so that restarts don't
// waste attempts on branches that already exist.
func (r *Runner) Init(ctx context.Context) error {
	r.initDefaults()
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
//  4. If both fail, revert to StateWaiting so the user can retry or terminate.
//
// State transitions:
//   - Relay attach: keeps StateWaiting/StateAsking if agent already finished its
//     turn; transitions to StateRunning only if the agent was mid-output.
//   - --resume fallback: always transitions to StateRunning since a new agent
//     process is started.
//   - All-fail: reverts to StateWaiting.
func (r *Runner) Reconnect(ctx context.Context, t *Task) (*SessionHandle, error) {
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

	msgCh := r.startMessageDispatch(ctx, t)

	logW, err := r.openLog(t)
	if err != nil {
		close(msgCh)
		return nil, err
	}

	// Prefer attaching to a live relay (agent process still running).
	relayAlive, relayErr := agent.IsRelayRunning(ctx, t.Container)
	if relayErr != nil {
		slog.Warn("relay check failed, falling back to --resume", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "err", relayErr)
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
		session, err = r.backend(t.Harness).AttachRelay(ctx, t.Container, t.RelayOffset, msgCh, logW)
		if err != nil {
			// Relay died between the IsRelayRunning check and the attach
			// attempt. This is a known race; fall back to --resume.
			slog.Warn("attach relay failed, falling back to --resume", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "err", err)
			relayAlive = false
		}
	}
	if !relayAlive {
		// Starting a new session via --resume always re-engages the agent.
		t.SetState(StateRunning)
		maxTurns := t.MaxTurns
		if maxTurns == 0 {
			maxTurns = r.MaxTurns
		}
		session, err = r.backend(t.Harness).Start(ctx, &agent.Options{
			Container:       t.Container,
			Dir:             r.containerDir(),
			MaxTurns:        maxTurns,
			Model:           t.Model,
			ResumeSessionID: t.GetSessionID(),
		}, msgCh, logW)
	}
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		// Both attach and --resume failed. Revert to StateWaiting so the
		// user can try again (restart) or terminate.
		t.SetState(StateWaiting)
		return nil, fmt.Errorf("reconnect: %w", err)
	}

	h := &SessionHandle{Session: session, MsgCh: msgCh, LogW: logW}
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
	t.SetState(StateBranching)

	// 1. Create branch + start container (serialized).
	slog.Info("setting up task", "repo", t.Repo)
	r.branchMu.Lock()
	sr, err := r.setup(ctx, t, []string{"caic=" + t.ID.String(), "harness=" + string(t.Harness)})
	r.branchMu.Unlock()
	if err != nil {
		t.SetState(StateFailed)
		return nil, err
	}
	t.Branch = sr.Branch
	t.Container = sr.Container
	t.TailscaleFQDN = sr.TailscaleFQDN
	slog.Info("container ready", "repo", t.Repo, "branch", t.Branch, "container", t.Container)

	// 2. Start the agent session.
	t.SetState(StateStarting)
	msgCh := r.startMessageDispatch(ctx, t)
	maxTurns := t.MaxTurns
	if maxTurns == 0 {
		maxTurns = r.MaxTurns
	}
	logW, err := r.openLog(t)
	if err != nil {
		close(msgCh)
		t.SetState(StateFailed)
		return nil, err
	}

	slog.Info("starting agent session", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "agent", t.Harness, "maxTurns", maxTurns)
	session, err := r.backend(t.Harness).Start(ctx, &agent.Options{
		Container:     t.Container,
		Dir:           r.containerDir(),
		MaxTurns:      maxTurns,
		Model:         t.Model,
		InitialPrompt: t.InitialPrompt,
	}, msgCh, logW)
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		t.SetState(StateFailed)
		slog.Error("agent session failed to start", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "err", err)
		return nil, err
	}

	// Store handle so SendInput can reach it.
	h := &SessionHandle{Session: session, MsgCh: msgCh, LogW: logW}
	t.AttachSession(h)

	t.addMessage(ctx, syntheticUserInput(t.InitialPrompt))
	t.SetState(StateRunning)
	slog.Info("agent running", "repo", t.Repo, "branch", t.Branch, "container", t.Container)
	return h, nil
}

// Cleanup is the single shutdown path for a task (Flow 1 in the relay
// shutdown protocol — see package agent). It sends the null-byte sentinel
// to trigger graceful agent exit, then kills the container.
//
// This is only called for intentional termination (user action or container
// death), never during backend restart. On restart, the relay daemon stays
// alive and the server reconnects via adoptOne → Reconnect.
//
// Steps:
//  1. Detach the session handle from the task.
//  2. If a session exists: Session.Close sends \x00 + closes stdin, wait up to 10s.
//  3. Set task state to reason (StateTerminated or StateFailed).
//  4. Kill the container.
//  5. If graceful wait timed out, drain session now (container dead, SSH severed).
//  6. Close msgCh and logW, write log trailer.
//  7. Build and return Result.
func (r *Runner) Cleanup(ctx context.Context, t *Task, reason State) Result {
	h := t.DetachSession()

	name := t.Container

	// Graceful shutdown: close stdin so the agent can emit a final
	// ResultMessage with accurate cost/turns stats, then force-kill.
	var result *agent.ResultMessage
	if h != nil {
		h.Session.Close()
		timer := time.NewTimer(20 * time.Second)
		select {
		case <-h.Session.Done():
			timer.Stop()
			result, _ = h.Session.Wait()
		case <-timer.C:
			slog.Warn("agent session did not exit after stdin close, killing container", "repo", t.Repo, "branch", t.Branch)
		}
	}

	t.SetState(reason)

	// Backup container commits before killing, in case they were never pushed.
	r.backupIfNeeded(ctx, t)

	slog.Info("killing container", "repo", t.Repo, "branch", t.Branch, "container", name)
	if name != "" && r.Container != nil {
		if err := r.KillContainer(ctx, t.Branch); err != nil {
			slog.Warn("failed to kill container", "repo", t.Repo, "branch", t.Branch, "container", name, "err", err)
		}
	}

	// If the graceful wait timed out, wait for the session to drain now
	// that the container is dead and the SSH connection is severed.
	if h != nil && result == nil {
		result, _ = h.Session.Wait()
	}
	if h != nil {
		close(h.MsgCh)
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
	// without a ResultMessage (e.g. user-initiated termination).
	if ds := t.LiveDiffStat(); len(ds) > 0 {
		res.DiffStat = ds
	}
	var logW io.WriteCloser
	if h != nil {
		logW = h.LogW
	}
	writeLogTrailer(logW, t.Title(), &res)
	if logW != nil {
		_ = logW.Close()
	}
	return res
}

// setupResult holds the outputs of setup: the branch name, container name,
// and optional Tailscale FQDN.
type setupResult struct {
	Branch        string
	Container     string
	TailscaleFQDN string
}

// setup creates the branch and starts the container. Must be called under
// branchMu.
func (r *Runner) setup(ctx context.Context, t *Task, labels []string) (setupResult, error) {
	detached := context.WithoutCancel(ctx)

	gitCtx, gitCancel := context.WithTimeout(detached, r.GitTimeout)
	defer gitCancel()
	// Fetch so that origin/<base> is up to date.
	if err := gitutil.Fetch(gitCtx, r.Dir); err != nil {
		return setupResult{}, fmt.Errorf("fetch: %w", err)
	}
	// Resolve effective base branch: use task override if provided.
	effectiveBase := r.BaseBranch
	if t.BaseBranch != "" {
		effectiveBase = t.BaseBranch
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
			return setupResult{}, gitCtx.Err()
		}
		branch = fmt.Sprintf("caic-%d", r.nextID)
		r.nextID++
		slog.Info("creating branch", "repo", t.Repo, "branch", branch, "base", effectiveBase)
		err = gitutil.CreateBranch(gitCtx, r.Dir, branch, startPoint)
		if err == nil {
			break
		}
	}
	if err != nil {
		return setupResult{}, fmt.Errorf("create branch: %w", err)
	}

	t.SetState(StateProvisioning)
	slog.Info("starting container", "repo", t.Repo, "branch", branch, "image", t.DockerImage, "harness", t.Harness, "tailscale", t.Tailscale, "usb", t.USB, "display", t.Display)
	startCtx, startCancel := context.WithTimeout(detached, r.ContainerStartTimeout)
	defer startCancel()
	name, tailscaleFQDN, err := r.Container.Start(startCtx, r.Dir, branch, labels, StartOptions{
		DockerImage: t.DockerImage, Tailscale: t.Tailscale, USB: t.USB, Display: t.Display,
	})
	if err != nil {
		return setupResult{}, fmt.Errorf("start container: %w", err)
	}
	slog.Info("container started", "repo", t.Repo, "branch", branch)

	// Switch back to the base branch so the next task can create its branch.
	// Fresh timeout since the previous gitCtx likely expired during container start.
	gitCtx, gitCancel = context.WithTimeout(detached, r.GitTimeout)
	defer gitCancel()
	if err := gitutil.CheckoutBranch(gitCtx, r.Dir, r.BaseBranch); err != nil {
		return setupResult{}, fmt.Errorf("checkout base: %w", err)
	}
	return setupResult{Branch: branch, Container: name, TailscaleFQDN: tailscaleFQDN}, nil
}

// SyncToOrigin fetches changes from the container, runs safety checks, and
// pushes the container's remote-tracking ref to origin. If safety issues are
// found and force is false, it returns the issues without pushing.
func (r *Runner) SyncToOrigin(ctx context.Context, branch, container string, force bool) (agent.DiffStat, []SafetyIssue, error) {
	r.initDefaults()
	fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer fetchCancel()
	r.branchMu.Lock()
	ds := r.diffStat(fetchCtx, branch)
	slog.Info("fetching changes", "repo", filepath.Base(r.Dir), "branch", branch)
	if err := r.Container.Fetch(fetchCtx, r.Dir, branch); err != nil {
		r.branchMu.Unlock()
		return ds, nil, err
	}
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
func (r *Runner) SyncToDefault(ctx context.Context, branch, container, message string) (agent.DiffStat, []SafetyIssue, error) {
	r.initDefaults()
	fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer fetchCancel()
	r.branchMu.Lock()
	ds := r.diffStat(fetchCtx, branch)
	slog.Info("fetching changes for default-branch sync", "repo", filepath.Base(r.Dir), "branch", branch)
	if err := r.Container.Fetch(fetchCtx, r.Dir, branch); err != nil {
		r.branchMu.Unlock()
		return ds, nil, err
	}
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
	if state != StateWaiting && state != StateAsking {
		return nil, fmt.Errorf("cannot restart in state %s", state)
	}

	// 1. Close current session gracefully.
	oldH := t.CloseAndDetachSession()
	if oldH != nil {
		close(oldH.MsgCh)
		if oldH.LogW != nil {
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

	msgCh := r.startMessageDispatch(ctx, t)

	maxTurns := t.MaxTurns
	if maxTurns == 0 {
		maxTurns = r.MaxTurns
	}
	slog.Info("restarting agent session", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "agent", t.Harness, "maxTurns", maxTurns)
	session, err := r.backend(t.Harness).Start(ctx, &agent.Options{
		Container:     t.Container,
		Dir:           r.containerDir(),
		MaxTurns:      maxTurns,
		Model:         t.Model,
		InitialPrompt: prompt,
	}, msgCh, logW)
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		t.SetState(StateFailed)
		return nil, fmt.Errorf("start session: %w", err)
	}

	// 5. Store new handle.
	h := &SessionHandle{Session: session, MsgCh: msgCh, LogW: logW}
	t.AttachSession(h)

	t.addMessage(ctx, syntheticUserInput(prompt))

	t.SetState(StateRunning)
	slog.Info("agent restarted", "repo", t.Repo, "branch", t.Branch, "container", t.Container)
	return h, nil
}

// ReadRelayOutput reads the relay output.jsonl from the container using the
// backend matching agentName to parse messages.
func (r *Runner) ReadRelayOutput(ctx context.Context, container string, agentName agent.Harness) ([]agent.Message, int64, error) {
	r.initDefaults()
	return r.backend(agentName).ReadRelayOutput(ctx, container)
}

// DiffContent returns the unified diff for the given branch, optionally
// filtered to a single file path. Holds branchMu during the fetch+diff.
func (r *Runner) DiffContent(ctx context.Context, branch, path string) (string, error) {
	r.initDefaults()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	args := []string{}
	if path != "" {
		args = append(args, "--", path)
	}
	return r.Container.Diff(ctx, r.Dir, branch, args...)
}

// KillContainer kills the md container for the given branch.
func (r *Runner) KillContainer(ctx context.Context, branch string) error {
	r.initDefaults()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	return r.Container.Kill(ctx, r.Dir, branch)
}

// mutatingTools lists tool names whose execution may change files in the
// container, warranting a diff stat refresh after their result arrives.
var mutatingTools = map[string]struct{}{
	"Bash":         {},
	"Edit":         {},
	"Write":        {},
	"NotebookEdit": {},
}

// backupIfNeeded fetches the latest commits from the container and creates a
// local backup branch if the container's HEAD is not reachable from any
// durable ref (refs/heads/* or refs/remotes/origin/*). Errors are logged as
// warnings and never prevent the subsequent container kill.
func (r *Runner) backupIfNeeded(ctx context.Context, t *Task) {
	r.initDefaults()
	if t.Container == "" || r.Container == nil {
		return
	}

	fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer fetchCancel()

	// Fetch latest commits from the container. Needs branchMu because md
	// fetch may interact with the working tree.
	r.branchMu.Lock()
	err := r.Container.Fetch(fetchCtx, r.Dir, t.Branch)
	r.branchMu.Unlock()
	if err != nil {
		slog.Warn("backup: fetch from container failed", "branch", t.Branch, "err", err)
		return
	}

	// Resolve the container remote-tracking ref to a commit hash.
	ref := "refs/remotes/" + t.Container + "/" + t.Branch
	commit, err := gitutil.RevParse(fetchCtx, r.Dir, ref)
	if err != nil {
		slog.Warn("backup: cannot resolve container ref", "ref", ref, "err", err)
		return
	}

	// Check if the commit is reachable from any local branch or origin ref.
	reachable, err := gitutil.IsReachable(fetchCtx, r.Dir, commit)
	if err != nil {
		slog.Warn("backup: reachability check failed", "commit", commit, "err", err)
		return
	}
	if reachable {
		slog.Info("backup: commit already reachable, skipping", "branch", t.Branch, "commit", commit[:min(12, len(commit))])
		return
	}

	backupBranch := "caic-backup/" + t.Branch
	if err := gitutil.CreateBranchAt(fetchCtx, r.Dir, backupBranch, commit); err != nil {
		slog.Warn("backup: create backup branch failed", "backupBranch", backupBranch, "err", err)
		return
	}
	slog.Info("backup: created backup branch", "backupBranch", backupBranch, "commit", commit[:min(12, len(commit))])
}

// startMessageDispatch starts a goroutine that reads from msgCh and dispatches
// to t.addMessage. For ResultMessages, it fetches from the container first and
// attaches the diff stat. For tool results following a mutating tool (Edit,
// Bash, Write, NotebookEdit), it also fetches and emits a DiffStatMessage.
// Returns the channel for the caller to pass to the agent backend.
func (r *Runner) startMessageDispatch(ctx context.Context, t *Task) chan agent.Message {
	msgCh := make(chan agent.Message, 256)
	go func() {
		// Track tool_use IDs from AssistantMessage blocks that may mutate files.
		pendingMutating := make(map[string]struct{})
		for m := range msgCh {
			switch msg := m.(type) {
			case *agent.AssistantMessage:
				for _, b := range msg.Message.Content {
					if _, ok := mutatingTools[b.Name]; b.Type == "tool_use" && ok {
						pendingMutating[b.ID] = struct{}{}
					}
				}
			case *agent.UserMessage:
				if msg.ParentToolUseID != nil && r.Container != nil {
					id := *msg.ParentToolUseID
					if _, ok := pendingMutating[id]; ok {
						delete(pendingMutating, id)
						r.fetchDiffStat(ctx, t)
					}
				}
			case *agent.ResultMessage:
				if r.Container != nil {
					fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
					r.branchMu.Lock()
					if err := r.Container.Fetch(fetchCtx, r.Dir, t.Branch); err != nil {
						slog.Warn("fetch on result failed", "branch", t.Branch, "err", err)
					}
					msg.DiffStat = r.diffStat(fetchCtx, t.Branch)
					r.branchMu.Unlock()
					fetchCancel()
				}
			}
			t.addMessage(ctx, m)
		}
	}()
	return msgCh
}

// fetchDiffStat fetches from the container and emits a DiffStatMessage into
// the task's message stream. Used after mutating tool results to keep the
// live diff stat up to date.
func (r *Runner) fetchDiffStat(ctx context.Context, t *Task) {
	fetchCtx, fetchCancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer fetchCancel()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	if err := r.Container.Fetch(fetchCtx, r.Dir, t.Branch); err != nil {
		slog.Warn("fetch on tool result failed", "branch", t.Branch, "err", err)
		return
	}
	ds := r.diffStat(fetchCtx, t.Branch)
	if len(ds) == 0 {
		return
	}
	t.addMessage(ctx, &agent.DiffStatMessage{
		MessageType: "caic_diff_stat",
		DiffStat:    ds,
	})
}

// diffStat runs Diff("--numstat") and parses the output.
func (r *Runner) diffStat(ctx context.Context, branch string) agent.DiffStat {
	numstat, err := r.Container.Diff(ctx, r.Dir, branch, "--numstat")
	if err != nil {
		slog.Warn("diff numstat failed", "branch", branch, "err", err)
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
	safeRepo := strings.ReplaceAll(t.Repo, "/", "-")
	safeBranch := strings.ReplaceAll(t.Branch, "/", "-")
	name := t.ID.String() + "-" + safeRepo + "-" + safeBranch + ".jsonl"
	f, err := os.OpenFile(filepath.Join(r.LogDir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // name is derived from ksid, not arbitrary user input.
	if err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}
	// Write metadata header as the first line.
	meta := agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     1,
		Prompt:      t.InitialPrompt.Text,
		Title:       t.Title(),
		Repo:        t.Repo,
		Branch:      t.Branch,
		Harness:     t.Harness,
		Model:       t.Model,
		StartedAt:   t.StartedAt,
	}
	if data, err := json.Marshal(meta); err == nil {
		_, _ = f.Write(append(data, '\n'))
	}
	return f, nil
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

// maxBranchSeqNum finds the highest sequence number N among remote branches
// matching "caic-N" across all remotes. Returns -1 if no matching branches
// exist.
func maxBranchSeqNum(ctx context.Context, dir string) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "branch", "-r", "--format=%(refname:short)")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return -1, fmt.Errorf("git branch -r: %w", err)
	}
	highest := -1
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		// Match "<remote>/caic-N" for any remote name.
		_, after, ok := strings.Cut(line, "/caic-")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(after)
		if err != nil {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	return highest, nil
}
