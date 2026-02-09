// Package task orchestrates a single coding agent task: branch creation,
// container lifecycle, agent execution, and git integration.
package task

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/maruel/wmao/backend/internal/agent"
	"github.com/maruel/wmao/backend/internal/container"
	"github.com/maruel/wmao/backend/internal/gitutil"
)

// State represents the lifecycle state of a task.
type State int

// Task lifecycle states.
const (
	StatePending  State = iota
	StateStarting       // Creating branch + container.
	StateRunning        // Agent is executing.
	StatePulling        // Pulling changes from container.
	StatePushing        // Pushing to origin.
	StateDone           // Successfully completed.
	StateFailed         // Failed at some stage.
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StatePulling:
		return "pulling"
	case StatePushing:
		return "pushing"
	case StateDone:
		return "done"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Result holds the outcome of a completed task.
type Result struct {
	Task        string
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

// Task represents a single unit of work.
type Task struct {
	Prompt    string
	MaxTurns  int
	Branch    string
	Container string
	State     State
	StartedAt time.Time

	mu   sync.Mutex
	msgs []agent.Message
}

// Messages returns a copy of all received agent messages.
func (t *Task) Messages() []agent.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]agent.Message(nil), t.msgs...)
}

func (t *Task) addMessage(m agent.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msgs = append(t.msgs, m)
}

// Runner manages the serialization of setup and push operations.
type Runner struct {
	BaseBranch string
	MaxTurns   int

	setupMu sync.Mutex // Serializes branch creation + md start.
	pushMu  sync.Mutex // Serializes git push to origin.
}

// Run executes the full task lifecycle. It is meant to be called in a
// goroutine. The result is returned; the task is self-contained.
func (r *Runner) Run(ctx context.Context, t *Task) Result {
	t.StartedAt = time.Now()
	t.State = StateStarting

	// 1. Create branch + start container (serialized).
	t.Branch = branchName(t.Prompt)

	r.setupMu.Lock()
	name, err := r.setup(ctx, t)
	r.setupMu.Unlock()
	if err != nil {
		t.State = StateFailed
		return Result{Task: t.Prompt, Branch: t.Branch, State: StateFailed, Err: err}
	}
	t.Container = name

	// 2. Run the agent (parallel, each in its own container).
	t.State = StateRunning
	slog.Info("running agent", "container", name, "task", t.Prompt)
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
	result, err := agent.Run(ctx, name, t.Prompt, maxTurns, msgCh)
	close(msgCh)
	if err != nil {
		t.State = StateFailed
		return Result{Task: t.Prompt, Branch: t.Branch, Container: name, State: StateFailed, Err: err}
	}

	// 3. Get diff summary.
	t.State = StatePulling
	diffStat, _ := container.Diff(ctx, "--stat")

	// 4. Pull changes.
	slog.Info("pulling changes", "container", name)
	if err := container.Pull(ctx); err != nil {
		t.State = StateFailed
		return Result{
			Task: t.Prompt, Branch: t.Branch, Container: name,
			State: StateFailed, DiffStat: diffStat, Err: err,
		}
	}

	// 5. Push to origin (serialized).
	t.State = StatePushing
	slog.Info("pushing to origin", "branch", t.Branch)
	r.pushMu.Lock()
	pushErr := gitutil.Push(ctx, t.Branch)
	r.pushMu.Unlock()
	if pushErr != nil {
		t.State = StateFailed
		return Result{
			Task: t.Prompt, Branch: t.Branch, Container: name,
			State: StateFailed, DiffStat: diffStat, Err: pushErr,
		}
	}

	// 6. Cleanup.
	t.State = StateDone
	slog.Info("task done, killing container", "container", name)
	if err := container.Kill(ctx); err != nil {
		slog.Warn("failed to kill container", "container", name, "err", err)
	}

	return Result{
		Task:        t.Prompt,
		Branch:      t.Branch,
		Container:   name,
		State:       StateDone,
		DiffStat:    diffStat,
		CostUSD:     result.TotalCostUSD,
		DurationMs:  result.DurationMs,
		NumTurns:    result.NumTurns,
		AgentResult: result.Result,
	}
}

// setup creates the branch and starts the container. Must be called under
// setupMu.
func (r *Runner) setup(ctx context.Context, t *Task) (string, error) {
	slog.Info("creating branch", "branch", t.Branch)
	if err := gitutil.CreateBranch(ctx, t.Branch); err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}

	slog.Info("starting container", "branch", t.Branch)
	name, err := container.Start(ctx, t.Branch)
	if err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}

	// Switch back to the base branch so the next task can create its branch.
	if err := gitutil.CheckoutBranch(ctx, r.BaseBranch); err != nil {
		return "", fmt.Errorf("checkout base: %w", err)
	}
	return name, nil
}

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

// branchName generates a short, Docker-safe branch name from a prompt.
// Docker container names only allow [a-zA-Z0-9_.-], so no slashes.
func branchName(prompt string) string {
	return "wmao/" + slugify(prompt)
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlphaNum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 20 {
		s = s[:20]
		s = strings.TrimRight(s, "-")
	}
	return s
}
