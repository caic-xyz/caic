package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/gitutil"
)

// ContainerBackend abstracts md container lifecycle operations for testability.
type ContainerBackend interface {
	Start(ctx context.Context, dir, branch string, labels []string) (name string, err error)
	Diff(ctx context.Context, dir, branch string, args ...string) (string, error)
	Pull(ctx context.Context, dir, branch string) error
	Push(ctx context.Context, dir, branch string) error
	Kill(ctx context.Context, dir, branch string) error
}

// Result holds the outcome of a completed task.
type Result struct {
	Task        string
	Repo        string
	Branch      string
	Container   string
	State       State
	DiffStat    string
	CostUSD     float64
	DurationMs  int64
	NumTurns    int
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
	// AgentStartFn launches an agent session. Defaults to agent.StartWithRelay.
	AgentStartFn func(ctx context.Context, opts agent.Options, msgCh chan<- agent.Message, logW io.Writer) (*agent.Session, error)

	initOnce sync.Once
	branchMu sync.Mutex // Serializes operations that need a specific branch checked out (md commands).
	nextID   int        // Next branch sequence number (protected by branchMu).
}

func (r *Runner) initDefaults() {
	r.initOnce.Do(func() {
		if r.AgentStartFn == nil {
			r.AgentStartFn = agent.StartWithRelay
		}
		if r.GitTimeout == 0 {
			r.GitTimeout = time.Minute
		}
		if r.ContainerStartTimeout == 0 {
			r.ContainerStartTimeout = time.Hour
		}
	})
}

// Init sets nextID past any existing caic/w* branches so that restarts don't
// waste attempts on branches that already exist.
func (r *Runner) Init(ctx context.Context) error {
	r.initDefaults()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	highest, err := gitutil.MaxBranchSeqNum(ctx, r.Dir)
	if err != nil {
		return err
	}
	if highest >= r.nextID {
		r.nextID = highest + 1
	}
	return nil
}

// Reconnect reattaches to a running relay, or starts a new Claude session
// resuming the previous conversation if no relay is available. The caller must
// use SendInput to provide the next prompt after reconnecting.
func (r *Runner) Reconnect(ctx context.Context, t *Task) error {
	r.initDefaults()
	t.mu.Lock()
	if t.session != nil {
		t.mu.Unlock()
		return errors.New("session already active")
	}
	if t.Container == "" {
		t.mu.Unlock()
		return errors.New("no container to reconnect to")
	}
	// Remember the state inferred from restored messages so we don't
	// blindly override it to StateRunning for an idle relay.
	prevState := t.State
	t.mu.Unlock()

	msgCh := make(chan agent.Message, 256)
	go func() {
		for m := range msgCh {
			t.addMessage(m)
		}
	}()

	logW, err := r.openLog(t)
	if err != nil {
		close(msgCh)
		return err
	}

	// Prefer attaching to a live relay (claude process still running).
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
			t.mu.Lock()
			t.setState(StateRunning)
			t.mu.Unlock()
		}
		session, err = agent.AttachRelay(ctx, t.Container, t.RelayOffset, msgCh, logW)
		if err != nil {
			slog.Warn("attach relay failed, falling back to --resume", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "err", err)
			relayAlive = false
		}
	}
	if !relayAlive {
		// Starting a new session via --resume always re-engages the agent.
		t.mu.Lock()
		t.setState(StateRunning)
		t.mu.Unlock()
		maxTurns := t.MaxTurns
		if maxTurns == 0 {
			maxTurns = r.MaxTurns
		}
		session, err = r.AgentStartFn(ctx, agent.Options{
			Container:       t.Container,
			MaxTurns:        maxTurns,
			Model:           t.Model,
			ResumeSessionID: t.SessionID,
		}, msgCh, logW)
	}
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		t.mu.Lock()
		t.setState(StateWaiting)
		t.mu.Unlock()
		return fmt.Errorf("reconnect: %w", err)
	}

	t.mu.Lock()
	t.session = session
	t.msgCh = msgCh
	t.logW = logW
	t.mu.Unlock()
	return nil
}

// Start performs branch/container setup, starts the agent session, and sends
// the initial prompt.
//
// The session is left open for follow-up messages via SendInput.
//
// Call Kill to close the session.
func (r *Runner) Start(ctx context.Context, t *Task) error {
	r.initDefaults()
	if r.Container == nil {
		return errors.New("runner has no container backend configured")
	}
	t.StartedAt = time.Now().UTC()
	t.setState(StateBranching)
	t.InitDoneCh()

	// 1. Create branch + start container (serialized).
	slog.Info("setting up task", "repo", t.Repo)
	r.branchMu.Lock()
	name, err := r.setup(ctx, t, []string{"caic=" + t.ID.String()})
	r.branchMu.Unlock()
	if err != nil {
		t.setState(StateFailed)
		return err
	}
	t.Container = name
	slog.Info("container ready", "repo", t.Repo, "branch", t.Branch, "container", name)

	// 2. Start the agent session.
	t.setState(StateStarting)
	msgCh := make(chan agent.Message, 256)
	go func() {
		for m := range msgCh {
			t.addMessage(m)
		}
	}()
	maxTurns := t.MaxTurns
	if maxTurns == 0 {
		maxTurns = r.MaxTurns
	}
	logW, err := r.openLog(t)
	if err != nil {
		close(msgCh)
		t.setState(StateFailed)
		return err
	}

	slog.Info("starting agent session", "repo", t.Repo, "branch", t.Branch, "container", name, "maxTurns", maxTurns)
	session, err := r.AgentStartFn(ctx, agent.Options{
		Container: name,
		MaxTurns:  maxTurns,
		Model:     t.Model,
	}, msgCh, logW)
	if err != nil {
		_ = logW.Close()
		close(msgCh)
		t.setState(StateFailed)
		slog.Warn("agent session failed to start", "repo", t.Repo, "branch", t.Branch, "container", name, "err", err)
		return err
	}

	// Store session so SendInput can reach it.
	t.mu.Lock()
	t.session = session
	t.msgCh = msgCh
	t.logW = logW
	t.mu.Unlock()

	t.addMessage(syntheticUserInput(t.Prompt))
	if err := session.Send(t.Prompt); err != nil {
		_ = logW.Close()
		close(msgCh)
		t.setState(StateFailed)
		return fmt.Errorf("write prompt: %w", err)
	}
	t.setState(StateRunning)
	slog.Info("agent running", "repo", t.Repo, "branch", t.Branch, "container", name)
	return nil
}

// Kill terminates the agent session and kills the container. It blocks until
// t.Done() is signaled, then proceeds. Pull/push must be done separately.
func (r *Runner) Kill(ctx context.Context, t *Task) Result {
	// Wait for user to signal terminate.
	select {
	case <-t.Done():
	case <-ctx.Done():
		t.setState(StateFailed)
		return Result{Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: t.Container, State: StateFailed, Err: ctx.Err()}
	}

	t.mu.Lock()
	session := t.session
	t.session = nil
	msgCh := t.msgCh
	logW := t.logW
	t.logW = nil
	t.mu.Unlock()

	name := t.Container

	// Graceful shutdown: close stdin so the agent can emit a final
	// ResultMessage with accurate cost/turns stats, then force-kill.
	var result *agent.ResultMessage
	if session != nil {
		session.Close()
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-session.Done():
			timer.Stop()
			result, _ = session.Wait()
		case <-timer.C:
			slog.Warn("agent session did not exit after stdin close, killing container", "repo", t.Repo, "branch", t.Branch)
		}
	}

	t.setState(StateTerminated)
	slog.Info("killing container", "repo", t.Repo, "branch", t.Branch, "container", name)
	if name != "" {
		if err := r.KillContainer(ctx, t.Branch); err != nil {
			slog.Warn("failed to kill container", "repo", t.Repo, "branch", t.Branch, "container", name, "err", err)
		}
	}

	// If the graceful wait timed out, wait for the session to drain now
	// that the container is dead and the SSH connection is severed.
	if session != nil && result == nil {
		result, _ = session.Wait()
	}
	if msgCh != nil {
		close(msgCh)
	}

	res := Result{
		Task:      t.Prompt,
		Repo:      t.Repo,
		Branch:    t.Branch,
		Container: name,
		State:     StateTerminated,
	}
	if result != nil {
		res.CostUSD = result.TotalCostUSD
		res.DurationMs = result.DurationMs
		res.NumTurns = result.NumTurns
		res.AgentResult = result.Result
	}
	// Use accumulated live stats when they exceed the session result
	// (e.g. adopted container after restart where the session only
	// reflects the reconnected portion, not the full run).
	if liveCost, liveTurns, liveDur := t.LiveStats(); liveCost > res.CostUSD {
		res.CostUSD = liveCost
		res.NumTurns = liveTurns
		res.DurationMs = liveDur
	}
	writeLogTrailer(logW, &res)
	if logW != nil {
		_ = logW.Close()
	}
	return res
}

// setup creates the branch and starts the container. Must be called under
// branchMu.
func (r *Runner) setup(ctx context.Context, t *Task, labels []string) (string, error) {
	detached := context.WithoutCancel(ctx)

	gitCtx, gitCancel := context.WithTimeout(detached, r.GitTimeout)
	defer gitCancel()
	// Fetch so that origin/<BaseBranch> is up to date.
	if err := gitutil.Fetch(gitCtx, r.Dir); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	// Assign a sequential branch name, skipping existing ones.
	var err error
	for range 100 {
		if gitCtx.Err() != nil {
			return "", gitCtx.Err()
		}
		t.Branch = fmt.Sprintf("caic/w%d", r.nextID)
		r.nextID++
		slog.Info("creating branch", "repo", t.Repo, "branch", t.Branch)
		err = gitutil.CreateBranch(gitCtx, r.Dir, t.Branch, "origin/"+r.BaseBranch)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}

	t.setState(StateProvisioning)
	slog.Info("starting container", "repo", t.Repo, "branch", t.Branch)
	startCtx, startCancel := context.WithTimeout(detached, r.ContainerStartTimeout)
	defer startCancel()
	name, err := r.Container.Start(startCtx, r.Dir, t.Branch, labels)
	if err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	slog.Info("container started", "repo", t.Repo, "branch", t.Branch)

	// Switch back to the base branch so the next task can create its branch.
	// Fresh timeout since the previous gitCtx likely expired during container start.
	gitCtx, gitCancel = context.WithTimeout(detached, r.GitTimeout)
	defer gitCancel()
	if err := gitutil.CheckoutBranch(gitCtx, r.Dir, r.BaseBranch); err != nil {
		return "", fmt.Errorf("checkout base: %w", err)
	}
	return name, nil
}

// PullChanges runs md diff + md pull for the given branch. Returns the diff
// stat and the first error encountered.
func (r *Runner) PullChanges(ctx context.Context, branch string) (string, error) {
	r.initDefaults()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	r.branchMu.Lock()
	defer r.branchMu.Unlock()
	diffStat, _ := r.Container.Diff(ctx, r.Dir, branch, "--stat")
	slog.Info("pulling changes", "repo", filepath.Base(r.Dir), "branch", branch)
	if err := r.Container.Pull(ctx, r.Dir, branch); err != nil {
		return diffStat, err
	}
	return diffStat, nil
}

// PushChanges pushes local changes into the container.
func (r *Runner) PushChanges(ctx context.Context, branch string) error {
	r.initDefaults()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	slog.Info("pushing changes to container", "repo", filepath.Base(r.Dir), "branch", branch)
	return r.Container.Push(ctx, r.Dir, branch)
}

// KillContainer kills the md container for the given branch.
func (r *Runner) KillContainer(ctx context.Context, branch string) error {
	r.initDefaults()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.GitTimeout)
	defer cancel()
	return r.Container.Kill(ctx, r.Dir, branch)
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
		Prompt:      t.Prompt,
		Repo:        t.Repo,
		Branch:      t.Branch,
		Model:       t.Model,
		StartedAt:   t.StartedAt,
	}
	if data, err := json.Marshal(meta); err == nil {
		_, _ = f.Write(append(data, '\n'))
	}
	return f, nil
}

// writeLogTrailer appends a MetaResultMessage to the log file.
func writeLogTrailer(w io.Writer, res *Result) {
	if w == nil {
		return
	}
	mr := agent.MetaResultMessage{
		MessageType: "caic_result",
		State:       res.State.String(),
		CostUSD:     res.CostUSD,
		DurationMs:  res.DurationMs,
		NumTurns:    res.NumTurns,
		DiffStat:    res.DiffStat,
		AgentResult: res.AgentResult,
	}
	if res.Err != nil {
		mr.Error = res.Err.Error()
	}
	if data, err := json.Marshal(mr); err == nil {
		_, _ = w.Write(append(data, '\n'))
	}
}
