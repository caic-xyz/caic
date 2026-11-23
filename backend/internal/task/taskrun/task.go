// Package taskrun orchestrates coding-agent state and ordered conversation and Git history.
//
// It owns branch creation, instance lifecycle, agent execution, resource
// history, and git integration.
package taskrun

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/genai"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
)

// StatsRingSize is how many of its latest resource samples a task retains;
// clients receive it with the samples.
//
// TODO: Save more performance metrics. The ring keeps only the latest 60
// readings, in memory, so the turn view's resource lanes stay empty for a turn
// older than those readings, and a server restart drops them all. Persist the
// samples with the task, or a downsampled series per turn, so every turn keeps
// its CPU and memory readings; the system stats ring in
// server/system_stats.go has the same 60-reading limit.
const StatsRingSize = 60

// stateTransitionHistory bounds each task's journal of recent state
// transitions. It only needs to cover the states a task-list connection can
// miss between two snapshots, so a short window is sufficient.
const stateTransitionHistory = 16

// SessionHandle associates a transport with its owning execution lifetime.
type SessionHandle struct {
	Session *agent.Session
	Run     *Run
}

// Done closes after the local transport process exits.
func (h *SessionHandle) Done() <-chan struct{} { return h.Session.Done() }

// GracefulStop stops the retained remote agent and waits up to timeout.
func (h *SessionHandle) GracefulStop(ctx context.Context, timeout time.Duration) error {
	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return h.Session.Stop(stopCtx)
}

// Drain joins the transport's reader, input, and protocol workers.
func (h *SessionHandle) Drain() error { return h.Session.Wait() }

// Task represents a single unit of work.
type Task struct {
	// recordingMu serializes active lifetime installation/claim and exclusive
	// settled host writes. Never hold mu while submitting to the record owner.
	recordingMu  sync.Mutex
	run          atomic.Pointer[Run]
	shuttingDown atomic.Bool

	// Immutable fields — set at creation, never modified.
	ID                             ksid.ID
	InitialPrompt                  agent.Prompt // Initial prompt text and optional images.
	Harness                        harness.Name // Agent harness ("claude", "codex", etc.).
	RequestedModel                 string       // User-requested model; passed to agent CLI.
	RequestedCompactionBudget      agent.CompactionBudget
	RequestedAutoCompactTokenLimit int                  // Captured launch threshold; zero uses the backend default.
	RequestedEffort                string               // Thinking effort; passed to agent CLI. Empty = default.
	RuntimeName                    runtime.Name         // Runtime backend used for this task.
	BaseImage                      string               // Custom runtime base image; empty means use the default.
	ContainerPlatform              string               // Container CPU architecture; empty means use the host default.
	MaxCPUs                        int                  // Max CPU cores for the instance; 0 means use the default.
	CacheMounts                    []runtime.CacheMount // Build cache mounts baked into the runtime image.
	Mounts                         []runtime.Mount      // Host directories bind-mounted into the runtime instance.
	Tailscale                      bool                 // Enable Tailscale networking in the instance.
	USB                            bool                 // Enable USB passthrough in the instance.
	Display                        bool                 // Enable Xvfb display in the instance.
	Sudo                           bool                 // Enable root access (password-based sudo) in the instance.
	StartedAt                      time.Time            // When the task was created.
	OwnerID                        string               // Human authorization principal; distinct from the delegating task's identity.
	ForgeIssue                     int                  // Originating issue number for bot comment callbacks; 0 = none.
	ForkedFromTaskID               ksid.ID              // Parent task ID when created by fork; zero otherwise.
	ParentTaskID                   ksid.ID              // Delegating task ID for a child task; zero for root tasks and ordinary forks.
	CaicMCP                        bool                 // Enables task-scoped CAIC MCP delegation.
	Pricer                         usage.ModelPricer    // Quota-provider per-model pricing for cost reporting; set by the task manager before the task runs. Nil keeps harness-reported totals only.
	Rollup                         task.RollupSink      // Cross-task usage rollup sink; NewTask defaults to DiscardRollup, the task manager replaces it with the wired store.
	Provider                       genai.Provider

	timelineID string

	// Mutable task metadata. These fields are populated at construction, setup, or
	// import. After a task is published in the Manager registry, access them
	// through Task methods so readers and async lifecycle goroutines synchronize.
	Repos         []repo.Mount // index 0 = primary; empty = no-repo
	TailscaleFQDN string       // Tailscale FQDN assigned to the instance (empty if not available).
	RelayOffset   int64        // Committed relay output bytes in the current generation, for reconnect.
	SudoPassword  string       // Random sudo password; empty if sudo is not enabled.
	VNCPort       int          // VNC WebSocket port inside the instance (0 = no VNC). Set during launch.
	GitHubToken   bool         // Inject GitHub token into the instance's environment.

	inputMu sync.Mutex // Serializes follow-up delivery attempts.

	// mu protects mutable task metadata above and all fields below.
	mu                     sync.Mutex
	runtimeInstanceID      runtime.ID
	runtimeConnection      runtime.ConnectionTarget
	statsRing              [StatsRingSize]runtime.Stats
	statsRingSeq           [StatsRingSize]uint64 // statsSeq at each slot's last write
	statsLen               int
	statsHead              int
	statsSeq               uint64          // counts ring writes, including disk revisions
	statsWakeSubs          []chan struct{} // active coalescing stats wakeup subscribers
	diskUsed               int64
	diskKnown              bool
	lifecycle              task.State        // Phase runtime code drives through the lifecycle transitions.
	activity               task.State        // Turn activity the fold derived for the starting or live session; empty until it reports one.
	stateUpdatedAt         time.Time         // UTC timestamp of the last published state transition.
	stateSeq               uint64            // Monotonic sequence of recorded state transitions.
	stateTransitions       []StateTransition // Bounded journal of recent state transitions.
	sessionMetadata        agent.SessionMetadata
	contextMeter           agent.ContextMeter              // Root context size, as the history projection folds it.
	reportedContextWindow  int                             // Context window size reported by the agent (0 = unknown).
	planFile               string                          // Path to plan file inside instance, captured from Write tool_use.
	planContent            string                          // Content of the plan file, captured from Write tool_use input.
	planExitID             string                          // ToolUseID of the ExitPlanMode carrying planContent; "" after context_cleared.
	planDismissed          bool                            // True after a context boundary; suppresses plan tracking until the next ResultMessage.
	inPlanMode             bool                            // True while the agent is in plan mode (between EnterPlanMode and ExitPlanMode).
	completedRootTurns     int                             // Completed root turns, including turns reconstructed from history.
	titleGeneration        uint64                          // Orders concurrent title requests so stale responses cannot replace newer titles.
	title                  string                          // LLM-generated short title; set via SetTitle.
	timelinePrefix         []agent.TimedMessage            // Imported durable history; ownership transfers at adoption.
	timeline               []agent.TimedMessage            // Live tail and ordinary one-slice histories.
	timelineReaders        int                             // Active zero-copy iterators and snapshots.
	timelineCompactPending []int                           // Final-message indexes waiting for readers to release timeline slots.
	nativeSubagents        agent.NativeSubagentTimeline    // harness-native subagent cards folded from timeline
	backgroundCommands     agent.BackgroundCommandTimeline // harness-native detached shell cards folded from timeline
	backgroundStarts       map[string]backgroundStart      // First running observation of each card's current run, by card ID.
	turnContinues          bool                            // The last root result reported that the root turn continues past it.
	turnEnded              bool                            // The last root turn has a trailing result or host exit; only background work or a queued turn keeps the task running.
	turnAsks               []turnAsk                       // Unanswered root questions of the current turn, oldest first.
	turnExitPlan           bool                            // The current turn presented a plan with ExitPlanMode.
	inputDelivering        bool                            // An input delivery started and has no outcome yet.
	deferredInput          bool                            // A root input arrived while its delivery is unresolved.
	deferredInputAt        time.Time                       // When the deferred input arrived; its acceptance starts the turn then.
	lastProducerAt         time.Time                       // Latest recorded producer time; state transitions of messages without one use it.
	skillReads             agent.SkillReadTracker

	wakeSubs       []chan struct{} // active coalescing timeline wakeup subscribers
	handle         *SessionHandle  // current active session; nil when no session is attached
	turnStartedAt  time.Time       // when the current running turn started; zero when not running
	costTracker    task.CostTracker
	turnMetrics    task.TurnMetrics
	liveUsage      agent.Usage
	lastUsage      agent.Usage  // Most recent ResultMessage usage (active context).
	cacheExpiresAt time.Time    // When the prompt cache from the last API call expires.
	liveDiffStat   v3.DiffStat  // Updated by DiffStatMessage from relay.
	lastGitRead    repo.GitRead // Rejects deferred snapshots older than the applied Git state.
	liveRepoStates []v3.RepoState
	// Compact per-repo git state, updated by the backend's post-tool probe.
	diffCreated  bool // True after any non-empty diff was reported for the task.
	exitStatus   task.ExitStatus
	forgeOwner   string
	forgeRepo    string
	forgePR      int
	forgePRState forge.PRState // "open", "closed", "merged"; empty when no PR.
	ciStatus     forge.CIStatus
	ciChecks     []forge.Check
	rateLimit    RateLimit                    // Active task quota block resolved across provider windows.
	rateLimits   map[quotaWindowKey]RateLimit // Latest quota status for each provider window.
}

// NewTask creates a task with a valid ID, a pending state, and prompt.
func NewTask(id ksid.ID, prompt agent.Prompt, h harness.Name, model, effort, baseImage, containerPlatform, title string) (*Task, error) {
	if id == 0 {
		return nil, errors.New("task: NewTask requires a non-zero ID")
	}
	if prompt.Text == "" && len(prompt.Images) == 0 {
		return nil, errors.New("task: NewTask requires a prompt with text or images")
	}
	t := &Task{
		ID:                id,
		timelineID:        rand.Text(),
		InitialPrompt:     prompt,
		Harness:           h,
		RequestedModel:    model,
		RequestedEffort:   effort,
		costTracker:       task.CostTracker{Harness: h, Model: model},
		BaseImage:         baseImage,
		ContainerPlatform: containerPlatform,
		StartedAt:         time.Now().UTC(),
		Rollup:            task.DiscardRollup{},
	}
	t.Restore(task.StatePending, t.StartedAt)
	if title == "" {
		title = prompt.Text
	}
	t.SetTitle(title)
	return t, nil
}

// TimelineID returns this task object's event-stream incarnation.
func (t *Task) TimelineID() string {
	return t.timelineID
}

// Primary returns a copy of the primary repository mount (Repos[0]), or nil for no-repo tasks.
func (t *Task) Primary() *repo.Mount {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.Repos) == 0 {
		return nil
	}
	p := t.Repos[0]
	return &p
}

// PrimaryBaseBranch returns the primary repo's base-branch override, or "".
func (t *Task) PrimaryBaseBranch() string {
	if p := t.Primary(); p != nil {
		return p.BaseBranch
	}
	return ""
}

// RuntimeRepos returns all repos for use with the runtime backend.
func (t *Task) RuntimeRepos() []runtime.Repo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]runtime.Repo, len(t.Repos))
	for i, r := range t.Repos {
		out[i] = r.ToRuntimeRepo()
	}
	return out
}

// GitTarget captures the instance and repository mapping in one task read.
func (t *Task) GitTarget() repo.GitTarget {
	t.mu.Lock()
	defer t.mu.Unlock()
	repos := make([]runtime.Repo, len(t.Repos))
	for i, r := range t.Repos {
		repos[i] = r.ToRuntimeRepo()
	}
	return repo.GitTarget{InstanceID: t.runtimeInstanceID, Repos: repos}
}

// ExtraRuntimeRepos returns all repos after the primary.
func (t *Task) ExtraRuntimeRepos() []runtime.Repo {
	repos := t.RuntimeRepos()
	if len(repos) <= 1 {
		return nil
	}
	return repos[1:]
}

// ReposSnapshot returns a copy of the task repo mounts.
func (t *Task) ReposSnapshot() []repo.Mount {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]repo.Mount(nil), t.Repos...)
}

// SetRepoBranch updates the branch for a repo by index.
func (t *Task) SetRepoBranch(i int, branch string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Repos[i].Branch = branch
}

// RuntimeInstanceID returns the current runtime instance ID.
func (t *Task) RuntimeInstanceID() runtime.ID {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runtimeInstanceID
}

// RuntimeConnectionTarget returns the current runtime connection target.
func (t *Task) RuntimeConnectionTarget() runtime.ConnectionTarget {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runtimeConnection
}

// SetRuntimeConnectionInfo records runtime instance metadata and agent connection details.
func (t *Task) SetRuntimeConnectionInfo(id runtime.ID, target runtime.ConnectionTarget, tailscaleFQDN string, vncPort int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.runtimeInstanceID != id {
		t.lastGitRead = repo.NewGitRead(id)
	}
	t.runtimeInstanceID = id
	t.runtimeConnection = target
	t.TailscaleFQDN = tailscaleFQDN
	t.VNCPort = vncPort
}

// SetVNCPort records the current VNC host port.
func (t *Task) SetVNCPort(port int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.VNCPort = port
}

// RelayOffsetValue returns the current relay log byte offset.
func (t *Task) RelayOffsetValue() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.RelayOffset
}

// SetRelayOffset anchors reconnect to a verified relay snapshot after import.
// Successful live relay appends subsequently advance this same cursor.
func (t *Task) SetRelayOffset(offset int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.RelayOffset = offset
}

// LogFilename returns the canonical filename for a new task-log segment.
func (t *Task) LogFilename() string {
	safeRepo := ""
	safeBranch := ""
	if p := t.Primary(); p != nil {
		safeRepo = strings.ReplaceAll(p.Name, "/", "-")
		safeBranch = strings.ReplaceAll(p.Branch, "/", "-")
	}
	return t.ID.String() + "-" + safeRepo + "-" + safeBranch + ".jsonl"
}

// LogHeader builds the immutable metadata header for a new task-log segment.
func (t *Task) LogHeader() *agent.MetaMessage {
	repos := t.ReposSnapshot()
	metaRepos := make([]v3.MetaRepo, len(repos))
	for i, r := range repos {
		metaRepos[i] = v3.MetaRepo{Name: r.Name, BaseBranch: r.BaseBranch, Branch: r.Branch, ContainerPath: r.ContainerPath}
	}
	parentTaskID := ""
	if t.ParentTaskID != 0 {
		parentTaskID = t.ParentTaskID.String()
	}
	return &agent.MetaMessage{
		MessageType:                    "caic_meta",
		Prompt:                         t.InitialPrompt.Text,
		Title:                          t.Title(),
		Repos:                          metaRepos,
		Harness:                        string(t.Harness),
		RequestedModel:                 t.RequestedModel,
		RequestedEffort:                t.RequestedEffort,
		RequestedAutoCompactTokenLimit: t.RequestedAutoCompactTokenLimit,
		RequestedCompactionBudget:      v3.CompactionBudget(t.RequestedCompactionBudget),
		StartedAt:                      t.StartedAt,
		ForgeIssue:                     t.ForgeIssue,
		OwnerID:                        t.OwnerID,
		ForkedFromTaskID:               t.ForkedFromTaskID.String(),
		ParentTaskID:                   parentTaskID,
		CaicMCP:                        t.CaicMCP,
		Tailscale:                      t.Tailscale,
		USB:                            t.USB,
		Display:                        t.Display,
		Sudo:                           t.Sudo,
		GitHubToken:                    t.GitHubTokenEnabled(),
		RuntimeName:                    string(t.RuntimeName),
		BaseImage:                      t.BaseImage,
		ContainerPlatform:              t.ContainerPlatform,
		MaxCPUs:                        t.MaxCPUs,
		CacheMounts:                    metaCacheMountsFromRuntime(t.CacheMounts),
		Mounts:                         metaMountsFromRuntime(t.Mounts),
	}
}

// SudoLookupState returns the sudo lookup inputs and cached password.
func (t *Task) SudoLookupState() (enabled bool, id runtime.ID, password string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Sudo, t.runtimeInstanceID, t.SudoPassword
}

// SetSudoPassword caches a sudo password on the task.
func (t *Task) SetSudoPassword(password string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.SudoPassword = password
}

// GitHubTokenEnabled reports whether this task injects a GitHub token.
func (t *Task) GitHubTokenEnabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.GitHubToken
}

// SetGitHubTokenEnabled records whether this task injects a GitHub token.
func (t *Task) SetGitHubTokenEnabled(enabled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.GitHubToken = enabled
}

// live is the lifecycle phase of an attached agent session.
const live task.State = "live"

// lifecycleSources lists the phases runtime code may move a task from into
// each lifecycle phase. The message fold owns the activity of a starting or
// live session; stateLocked derives the published state from both.
// RuntimeStarted and RuntimeDestroyed also act on a stopped task. A pending
// task stops when its admission is cancelled before it starts.
var lifecycleSources = map[task.State][]task.State{
	task.StateBranching:    {task.StatePending},
	task.StateProvisioning: {task.StatePending, task.StateBranching, task.StateStopped, task.StateCrashed},
	task.StateStarting:     {task.StateProvisioning, live},
	live:                   {task.StateStarting},
	task.StateStopping:     {task.StateStarting, live, task.StateCrashed, task.StateFailed},
	task.StateStopped:      {task.StatePending, live, task.StateStopping, task.StateCrashed},
	task.StatePurging:      {task.StateStopped},
	task.StatePurged:       {task.StatePurging},
	task.StateCrashed:      {task.StateProvisioning, task.StateStarting, live},
	task.StateFailed:       {task.StatePending, task.StateBranching, task.StateProvisioning, task.StateStarting, live, task.StateCrashed},
}

// GetState returns the published state under the mutex.
func (t *Task) GetState() task.State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stateLocked()
}

// Restore sets the lifecycle phase of a task that is not published yet, as of
// at.
func (t *Task) Restore(s task.State, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.stateLocked()
	t.lifecycle, t.activity = s, ""
	t.publishLocked(prev, at)
}

// Transition moves the lifecycle to `to` if lifecycleSources allows it.
func (t *Task) Transition(to task.State) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.transitionLocked(to)
}

// GoLive moves a starting session live once its agent runs.
func (t *Task) GoLive() bool { return t.Transition(live) }

// RuntimeStarted moves a stopped task live when its runtime started by itself.
func (t *Task) RuntimeStarted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lifecycle != task.StateStopped {
		return false
	}
	t.moveLocked(live)
	return true
}

// RuntimeDestroyed fails a task, including a stopped one, whose runtime
// instance was destroyed.
func (t *Task) RuntimeDestroyed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lifecycle != task.StateStopped {
		return t.transitionLocked(task.StateFailed)
	}
	t.moveLocked(task.StateFailed)
	return true
}

// RuntimeExited stops a live task whose runtime exited by itself, and reports
// the state it left.
func (t *Task) RuntimeExited() (task.State, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.stateLocked()
	return prev, t.lifecycle == live && t.transitionLocked(task.StateStopped)
}

// SessionLost ends the running turn of a session caic lost as of at; zero
// means now. It reports whether a turn ended.
func (t *Task) SessionLost(at time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stateLocked() != task.StateRunning {
		return false
	}
	t.setActivityLocked(task.StateWaiting, at)
	return true
}

// InvalidationState is bounded metadata needed to classify resource effects.
// Sequence is task-incarnation-local, not an externally stable occurrence ID.
type InvalidationState struct {
	CIStatus  forge.CIStatus
	ForgePR   int
	State     task.State
	Sequence  uint64
	Title     string
	RateLimit RateLimit
}

// InvalidationState captures classification metadata in one critical section
// without copying task bodies, repository details, or retained history.
func (t *Task) InvalidationState() InvalidationState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return InvalidationState{State: t.stateLocked(), Sequence: t.stateSeq, Title: t.title, RateLimit: t.rateLimit, CIStatus: t.ciStatus, ForgePR: t.forgePR}
}

// GetSessionID returns the agent session ID under the mutex.
func (t *Task) GetSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionMetadata.SessionID
}

// SetSessionMetadata records persisted agent session metadata.
func (t *Task) SetSessionMetadata(m agent.SessionMetadata) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionMetadata.Restore(m)
}

// GetPlanFile returns the plan file path under the mutex.
func (t *Task) GetPlanFile() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.planFile
}

// HasSession reports whether a session handle is attached.
func (t *Task) HasSession() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.handle != nil
}

// LastAgentResult returns the result text from the most recent ResultMessage,
// falling back to the visible text of that turn when the agent reported
// none, or "" if no result was received. Used by Cleanup/StopTask for the
// log trailer.
func (t *Task) LastAgentResult() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	view := t.timelineViewLocked()
	// Walk messages in reverse to find the last ResultMessage.
	for i, entry := range view.Backward() {
		if entry.Message.Provenance().IsChild() {
			continue
		}
		if rm, ok := entry.Message.(*agent.ResultMessage); ok {
			if rm.Result != "" {
				return rm.Result
			}
			return fallbackResultText(view.Slice(i + 1))
		}
	}
	return ""
}

// CompletedRootTurns returns the number of root turns that ended with a
// result, including turns restored from history. Pair it with
// SubscribeTimelineWakeups to wait for the next turn to end.
func (t *Task) CompletedRootTurns() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.completedRootTurns
}

// LastExitError returns the most recent non-zero relay exit diagnostic.
func (t *Task) LastExitError() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exitStatus.Error
}

// DiffCreated reports whether any non-empty diff was observed for the task.
func (t *Task) DiffCreated() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.diffCreated
}

// MarkDiffCreated records that a non-empty diff was observed, without a diff
// stat payload. Import uses it to restore the persisted DiffCreated flag from
// the log summary, so the signal survives a restart even when message replay is
// skipped or the relay's last diff was empty. The flag is sticky and never
// cleared here.
func (t *Task) MarkDiffCreated() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.diffCreated = true
}

// RestoreRepositorySummary installs a validated snapshot before task
// registration. Live and settled host observations use RecordRepositorySummary
// through their recording owner. It rejects stale or replaced-instance reads.
func (t *Task) RestoreRepositorySummary(snapshot *repo.GitSnapshot) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.acceptGitReadLocked(snapshot.Read) {
		return false
	}
	stats, states := snapshot.Summary(t.liveDiffStat, t.liveRepoStates)
	if slices.Equal(t.liveDiffStat, stats) && (len(states) == 0 || slices.Equal(t.liveRepoStates, states)) {
		return false
	}
	t.setLiveDiffStatLocked(stats)
	t.setLiveRepoStatesLocked(states)
	return true
}

// SetPR stores the forge owner, repo, and PR/MR number. Does not change task state.
func (t *Task) SetPR(owner, repoName string, pr int) {
	t.mu.Lock()
	t.forgeOwner = owner
	t.forgeRepo = repoName
	t.forgePR = pr
	t.forgePRState = forge.PRStateOpen
	t.mu.Unlock()
}

// SetPRState updates the PR state.
func (t *Task) SetPRState(state forge.PRState) {
	t.mu.Lock()
	t.forgePRState = state
	t.mu.Unlock()
}

// GetPR returns the forge PR number (0 if no PR has been created).
func (t *Task) GetPR() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.forgePR
}

// SetCIStatus updates the ciStatus and ciChecks fields under the mutex.
func (t *Task) SetCIStatus(status forge.CIStatus, checks []forge.Check) {
	t.mu.Lock()
	t.ciStatus = status
	t.ciChecks = checks
	t.mu.Unlock()
}

// Title returns the task title under the mutex.
func (t *Task) Title() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.title
}

// StateTransition records one task state change in the bounded journal used to
// replay short-lived states to snapshot-diffing clients. Seq increases
// monotonically per task and is never reused.
type StateTransition struct {
	Seq   uint64
	State task.State
	At    time.Time
}

// Snapshot holds volatile task fields read under the mutex. Used by the
// server to build API responses without data races on fields that
// ordered ingestion and SeedTimelineParts modify concurrently.
type Snapshot struct {
	agent.SessionMetadata

	State              task.State
	StateUpdatedAt     time.Time
	TurnStartedAt      time.Time // non-zero only while state is Running
	Repos              []repo.Mount
	RuntimeName        runtime.Name
	RuntimeInstanceID  runtime.ID
	Tailscale          bool
	TailscaleFQDN      string
	USB                bool
	Display            bool
	Sudo               bool
	SudoPassword       string
	VNCPort            int // VNC WebSocket port (0 = no VNC).
	GitHubToken        bool
	RelayOffset        int64
	Title              string
	RequestedModel     string
	RequestedEffort    string
	ContextWindowLimit int  // Non-zero when reported by the agent at runtime.
	ContextUsed        *int // Native occupancy; nil means unknown.
	InPlanMode         bool
	PlanFile           string
	PlanContent        string
	PlanExitID         string
	CostUSD            float64
	CostKnown          bool
	CostComplete       bool
	NumTurns           int
	Duration           time.Duration
	Usage              agent.Usage
	LastUsage          agent.Usage
	LastAPIUsage       agent.Usage
	CacheExpiresAt     time.Time
	DiffStat           v3.DiffStat
	RepoStates         []v3.RepoState // Compact per-repo git state from the latest diff probe.
	DiskUsed           int64
	DiskKnown          bool
	ResourceUsage      *runtime.Stats // Latest sample from the bounded resource ring; nil before sampling.
	ForgeOwner         string
	ForgeRepo          string
	ForgePR            int
	ForgePRState       forge.PRState // "open", "closed", "merged"; empty when no PR.
	ForgeIssue         int
	CIStatus           forge.CIStatus
	CIChecks           []forge.Check
	RateLimit          RateLimit
	TurnError          string // Latest root turn failure; empty after a successful or new turn.
	Background         BackgroundCounts
	BackgroundItems    []BackgroundItem
}

// BackgroundCounts counts running background work by kind. Agents and the
// commands whose end wakes the agent keep the task running after its turn ends;
// watches and other commands do not.
type BackgroundCounts struct {
	Commands int // Running detached commands that are not watches, waking the agent or not.
	Watches  int // Running watches, such as Claude Code's Monitor tool.
	Agents   int // Running background native subagents.
}

// BackgroundKind classifies one running background item.
type BackgroundKind string

// Background item kinds, matching the BackgroundCounts fields.
const (
	BackgroundKindAgent   BackgroundKind = "agent"
	BackgroundKindCommand BackgroundKind = "command"
	BackgroundKindWatch   BackgroundKind = "watch"
)

// BackgroundItem is one running background card.
type BackgroundItem struct {
	Kind       BackgroundKind
	ID         string
	Label      string
	AgentKind  string
	AgentScope agent.NativeSubagentScope
	// StartedAt is the first running observation of the card's current run,
	// as in the task history; zero when that observation carried no time.
	StartedAt time.Time
}

// BackgroundActivity is one consistent read of running background work.
type BackgroundActivity struct {
	Counts BackgroundCounts
	Items  []BackgroundItem // Running work that Counts counts, oldest first.
	// WakingCommands counts the running commands in Counts.Commands whose end
	// wakes the agent: with Counts.Agents, the work that keeps the task running.
	WakingCommands int
	// TurnEnded reports that the last root turn has a trailing result, so only
	// background work or a queued turn can keep the task running.
	TurnEnded bool
}

// turnAsk is an unanswered root question and the pending user action the
// harness reported for it, if any.
type turnAsk struct {
	key    agent.ToolKey
	action *v3.PendingUserAction
}

// backgroundStart records the first running observation of a card's run. Run
// is the subagent RunID: only a new run restarts the clock. Pauses,
// retirements, and host exits keep it, matching the task history's StartedAt.
type backgroundStart struct {
	run string
	at  time.Time
}

// RateLimit records the active quota block resolved for a task. It is retained
// with task state so summaries and provider monitors can react immediately
// without waiting for a separate provider-usage refresh.
type RateLimit struct {
	Status          agent.RateLimitStatus
	ResetsAt        time.Time
	RateLimitType   string  // Harness-native window ID; QuotaWindow is the canonical provider window.
	Utilization     float64 // Fraction of the window used in [0, 1]; -1 when unknown.
	IsUsingOverage  bool
	OverageResetsAt time.Time
	QuotaProvider   agent.QuotaProvider // Canonical usage-provider ID matching ProviderQuota.Provider.
	QuotaLabel      string
	QuotaWindow     string
	ObservedAt      time.Time
}

type quotaWindowKey struct {
	provider agent.QuotaProvider
	window   string
}

// Snapshot returns a consistent read of all volatile fields under the mutex.
func (t *Task) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

// SnapshotWithStateHistory returns a consistent snapshot plus the task's state
// transition sequence and the retained transitions newer than after, oldest
// first. Reading both in one critical section lets a snapshot-diffing client
// replay a short-lived state that the snapshot's own state already overwrote.
func (t *Task) SnapshotWithStateHistory(after uint64) (Snapshot, uint64, []StateTransition) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var history []StateTransition
	if after < t.stateSeq {
		for _, tr := range t.stateTransitions {
			if tr.Seq > after {
				history = append(history, tr)
			}
		}
	}
	return t.snapshotLocked(), t.stateSeq, history
}

// BackgroundActivity returns the running background work, its counts, and
// whether the last root turn ended, read under one lock.
func (t *Task) BackgroundActivity() BackgroundActivity {
	t.mu.Lock()
	defer t.mu.Unlock()
	counts := t.backgroundCountsLocked()
	return BackgroundActivity{
		Counts: counts, Items: t.backgroundItemsLocked(counts),
		WakingCommands: t.backgroundCommands.Active(), TurnEnded: t.turnEnded,
	}
}

// Messages returns a copy of all received agent messages.
func (t *Task) Messages() []agent.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	view := t.timelineViewLocked()
	messages := make([]agent.Message, view.Len())
	for i, entry := range view.Forward() {
		messages[i] = entry.Message
	}
	return messages
}

// MessageCount returns the number of messages retained in the task timeline.
func (t *Task) MessageCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timelineLenLocked()
}

// ForwardMessages returns the task's messages oldest first as a snapshot taken
// when ForwardMessages is called. The sequence must be ranged exactly once so
// its snapshot lease is released; a second range is a no-op. Messages appended
// afterward are excluded, and finalized deltas are not compacted during the
// range.
func (t *Task) ForwardMessages() iter.Seq[agent.Message] {
	t.mu.Lock()
	view := t.acquireTimelineReaderLocked()
	t.mu.Unlock()
	var once sync.Once
	return func(yield func(agent.Message) bool) {
		once.Do(func() {
			defer t.releaseTimelineReader()
			for _, entry := range view.Forward() {
				if !yield(entry.Message) {
					return
				}
			}
		})
	}
}

// HasAcceptedInputEvidence reports whether durable conversation history proves
// that a user prompt reached the harness. Marked input attempts require a
// completed delivery outcome or real root output. Unmarked historical inputs
// retain their established meaning.
func (t *Task) HasAcceptedInputEvidence() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	view := t.timelineViewLocked()
	rejected := unacceptedInputs(view, 0)
	for _, entry := range view.Backward() {
		if task.AcceptedInputOutput(entry.Message) {
			return true
		}
		if message, ok := entry.Message.(*agent.UserInputMessage); ok && !message.Source.IsChild() {
			if _, rejected := rejected[message]; !rejected {
				return true
			}
		}
	}
	return false
}

// unacceptedInputs finds marked attempts without successful delivery. Unmarked
// historical inputs retain their established accepted-input meaning.
func unacceptedInputs(entries timelineEntries, start int) map[*agent.UserInputMessage]struct{} {
	var rejected map[*agent.UserInputMessage]struct{}
	var attempt []*agent.UserInputMessage
	var last *agent.UserInputMessage
	pending, proven := false, false
	complete := func() {
		for _, input := range attempt {
			delete(rejected, input)
		}
		attempt = attempt[:0]
		pending, proven = false, true
	}
	for i := start; i < entries.Len(); i++ {
		m := entries.At(i).Message
		if m.Provenance().IsChild() {
			continue
		}
		if task.AcceptedInputOutput(m) {
			complete()
			continue
		}
		switch m := m.(type) {
		case *agent.SystemMessage:
			switch m.Subtype {
			case agent.SystemSubtypeInputDeliveryStarted:
				pending, proven = true, false
				attempt = attempt[:0]
			case agent.SystemSubtypeInputDeliveryCompleted:
				complete()
			case agent.SystemSubtypeInputDeliveryFailed:
				if !proven && last != nil {
					if rejected == nil {
						rejected = make(map[*agent.UserInputMessage]struct{})
					}
					rejected[last] = struct{}{}
				}
				pending = false
				attempt = attempt[:0]
			}
		case *agent.UserInputMessage:
			last, proven = m, false
			if pending {
				if rejected == nil {
					rejected = make(map[*agent.UserInputMessage]struct{})
				}
				rejected[m] = struct{}{}
				attempt = append(attempt, m)
			}
		}
	}
	return rejected
}

// BackwardMessages returns the task's messages newest first as a snapshot taken
// when BackwardMessages is called. The sequence must be ranged exactly once so
// its snapshot lease is released; a second range is a no-op. Messages appended
// afterward are excluded, and finalized deltas are not compacted during the
// range.
func (t *Task) BackwardMessages() iter.Seq[agent.Message] {
	t.mu.Lock()
	view := t.acquireTimelineReaderLocked()
	t.mu.Unlock()
	var once sync.Once
	return func(yield func(agent.Message) bool) {
		once.Do(func() {
			defer t.releaseTimelineReader()
			for _, entry := range view.Backward() {
				if !yield(entry.Message) {
					return
				}
			}
		})
	}
}

// PendingUserActions returns current user-facing actions that still need input.
// Today AskUserQuestion is the only pending action kind: an unanswered question
// of the current turn whose action the harness reported.
func (t *Task) PendingUserActions() []v3.PendingUserAction {
	t.mu.Lock()
	defer t.mu.Unlock()
	var actions []v3.PendingUserAction
	for _, a := range t.turnAsks {
		if a.action != nil {
			actions = append(actions, agent.ClonePendingUserAction(*a.action))
		}
	}
	return actions
}

// CurrentTurnProtocolMessages returns the provider messages without a
// canonical form, the pending user actions of every owner, and the
// input-delivery controls since the last root result, oldest first. A delivery
// start keeps its adjacency to the input it identifies.
func (t *Task) CurrentTurnProtocolMessages() []agent.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	var msgs []agent.Message
	for _, entry := range t.timelineViewLocked().Backward() {
		switch m := entry.Message.(type) {
		case *agent.ResultMessage:
			if !m.Source.IsChild() {
				slices.Reverse(msgs)
				return msgs
			}
		case *agent.RawMessage, *agent.PendingUserActionMessage:
			msgs = append(msgs, m)
		default:
			if agent.IsInputDeliveryControl(m) {
				msgs = append(msgs, m)
			}
		}
	}
	slices.Reverse(msgs)
	return msgs
}

// SeedTimelineParts fills an empty task with the message history from
// previously saved logs and folds every message into task state as live
// ingestion does, starting from the task's current state.
//
// Seeding is for an adopted task that will resume live operation. Restored
// completed tasks intentionally leave the timeline empty and serve rare
// history reads from taskslog instead.
//
// It panics if the task already holds messages. Seeding is one-shot
// initialization, not a merge: replaying a second batch onto an existing
// timeline would double-count cost, turns and token usage. A live timeline
// grows through the session message pump instead; task import runs exactly once
// before registration.
//
// The owned durable prefix and live suffix remain logically contiguous without
// concatenation. Both slices transfer to the task.
//
// settled lists, in order, the number of entries that precede each settlement
// trailer of the log. A trailer ends the agent process there, as on the live
// path, which can die with its container without recording an exit.
func (t *Task) SeedTimelineParts(prefix, suffix []agent.TimedMessage, settled []int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timelineLenLocked() > 0 {
		panic(fmt.Sprintf("task %s: SeedTimeline on a timeline that already holds %d messages", t.ID, t.timelineLenLocked()))
	}
	t.lastGitRead = repo.NewGitRead(t.runtimeInstanceID)
	view := timelineEntries{prefix: prefix, suffix: suffix}
	compactFinalizedDeltas(view)
	t.timelinePrefix = prefix
	t.timeline = suffix
	next := 0
	for i, entry := range view.Forward() {
		for ; next < len(settled) && settled[next] <= i; next++ {
			t.hostExitedLocked()
		}
		t.foldMessageLocked(entry.Message, entry.ProducerTime, entry.ProducerTime, true)
	}
	for ; next < len(settled); next++ {
		t.hostExitedLocked()
	}
}

func compactFinalizedDeltas(entries timelineEntries) {
	for i := range entries.Forward() {
		compactFinalizedDeltaAt(entries, i)
	}
}

func compactFinalizedDeltaAt(entries timelineEntries, i int) {
	switch final := entries.At(i).Message.(type) {
	case *agent.TextMessage:
		compacted := compactedTextDelta
		if final.Source != (v3.MessageSource{}) {
			compacted = &agent.TextDeltaMessage{Source: final.Source}
		}
		for j := i - 1; j >= 0; j-- {
			delta, ok := entries.At(j).Message.(*agent.TextDeltaMessage)
			if !ok {
				break
			}
			if delta.Source == final.Source {
				entries.SetMessage(j, compacted)
			}
		}
	case *agent.ThinkingMessage:
		compacted := compactedThinkingDelta
		if final.Source != (v3.MessageSource{}) {
			compacted = &agent.ThinkingDeltaMessage{Source: final.Source}
		}
		for j := i - 1; j >= 0; j-- {
			delta, ok := entries.At(j).Message.(*agent.ThinkingDeltaMessage)
			if !ok {
				break
			}
			if delta.Source == final.Source {
				entries.SetMessage(j, compacted)
			}
		}
	case *agent.WidgetMessage:
		for j := i - 1; j >= 0; j-- {
			if delta, ok := entries.At(j).Message.(*agent.WidgetDeltaMessage); !ok || agent.ToolKeyForSource(delta.Provenance(), delta.ToolUseID) != agent.ToolKeyForSource(final.Provenance(), final.ToolUseID) {
				break
			}
			entries.SetMessage(j, &agent.WidgetDeltaMessage{ToolUseID: final.ToolUseID, Source: final.Source})
		}
	case *agent.ToolResultMessage:
		for j := i - 1; j >= 0; j-- {
			delta, ok := entries.At(j).Message.(*agent.ToolOutputDeltaMessage)
			if !ok || agent.ToolKeyForSource(delta.Provenance(), delta.ToolUseID) != agent.ToolKeyForSource(final.Provenance(), final.ToolUseID) {
				break
			}
			entries.SetMessage(j, &agent.ToolOutputDeltaMessage{ToolUseID: delta.ToolUseID, Source: delta.Source})
		}
	}
}

func isDeltaFinal(message agent.Message) bool {
	switch message.(type) {
	case *agent.TextMessage, *agent.ThinkingMessage, *agent.WidgetMessage, *agent.ToolResultMessage:
		return true
	default:
		return false
	}
}

// AttachSession stores the SessionHandle under the mutex.
func (t *Task) AttachSession(h *SessionHandle) {
	t.mu.Lock()
	t.handle = h
	t.mu.Unlock()
}

// DetachSession atomically removes and returns the current SessionHandle,
// or nil if no session is attached. The caller must not hold t.mu.
func (t *Task) DetachSession() *SessionHandle {
	t.mu.Lock()
	h := t.handle
	t.handle = nil
	t.mu.Unlock()
	return h
}

// SessionDone returns the Done channel for the current session, or nil if no
// session is attached. The caller must not hold t.mu.
func (t *Task) SessionDone() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == nil {
		return nil
	}
	return t.handle.Session.Done()
}

// CloseAndDetachSession gracefully shuts down the current agent session and
// returns the detached handle. Returns nil if no session was attached. Used by
// RestartSession which needs the graceful drain before starting a new session.
func (t *Task) CloseAndDetachSession(ctx context.Context) (*SessionHandle, error) {
	h := t.DetachSession()
	if h == nil {
		return nil, nil //nolint:nilnil // No attached transport requires no shutdown.
	}
	stopErr := h.GracefulStop(ctx, 10*time.Second)
	return h, errors.Join(stopErr, h.Drain())
}

// GracefulStopSession detaches the currently attached session (if any) and
// sends the relay's shutdown sentinel, waiting up to timeout for the agent to
// exit gracefully. Returns the detached handle (nil if no session was
// attached) and any error from the graceful stop, e.g. a timeout. Used by
// callers (Cleanup, StopTask) that need the handle afterward to reuse its
// Run for terminal recording after transport producers have joined.
func (t *Task) GracefulStopSession(ctx context.Context, timeout time.Duration) (*SessionHandle, error) {
	h := t.DetachSession()
	if h == nil {
		return nil, nil //nolint:nilnil // no session attached is not an error
	}
	err := h.GracefulStop(ctx, timeout)
	return h, err
}

// SubscribeTimelineWakeups returns a channel that receives a signal after
// timeline messages are published. Signals coalesce: one pending signal stands
// for any number of messages, so a burst never drops the subscriber. Consumers
// read the messages from another authoritative source, such as
// RateLimitsSince. The channel is never closed; select on ctx as well.
// unsubFn is idempotent and safe after ctx cancellation, which also
// unsubscribes.
func (t *Task) SubscribeTimelineWakeups(ctx context.Context) (wake <-chan struct{}, unsubFn func()) {
	ch := make(chan struct{}, 1)
	t.mu.Lock()
	t.wakeSubs = append(t.wakeSubs, ch)
	t.mu.Unlock()
	return ch, unsubscribe(t, ctx, &t.wakeSubs, ch, func() {})
}

// RateLimitsSince returns the root quota messages at timeline positions from
// next onward, and the position to pass to the next call. Start at 0 for the
// full history.
func (t *Task) RateLimitsSince(next int) (rateLimits []*agent.RateLimitMessage, end int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	view := t.timelineViewLocked()
	for i := next; i < view.Len(); i++ {
		if rateLimit, ok := view.At(i).Message.(*agent.RateLimitMessage); ok && !rateLimit.IsChildThreadGoal() {
			rateLimits = append(rateLimits, rateLimit)
		}
	}
	return rateLimits, view.Len()
}

// PushStats records a runtime stats snapshot and wakes stats subscribers.
func (t *Task) PushStats(s *runtime.Stats) {
	t.mu.Lock()
	val := *s
	if val.DiskUsed >= 0 {
		t.diskUsed = val.DiskUsed
		t.diskKnown = true
	} else if t.diskKnown {
		val.DiskUsed = t.diskUsed
	}
	idx := (t.statsHead + t.statsLen) % StatsRingSize
	t.statsRing[idx] = val
	t.statsSeq++
	t.statsRingSeq[idx] = t.statsSeq
	if t.statsLen < StatsRingSize {
		t.statsLen++
	} else {
		t.statsHead = (t.statsHead + 1) % StatsRingSize
	}
	wakeLocked(t.statsWakeSubs)
	t.mu.Unlock()
}

// UpdateDiskUsage records the latest writable-layer size and applies it to the
// most recent resource sample. Future samples inherit the value until the next
// disk measurement.
func (t *Task) UpdateDiskUsage(bytes int64) {
	t.mu.Lock()
	t.diskUsed = bytes
	t.diskKnown = true
	if t.statsLen == 0 {
		t.mu.Unlock()
		return
	}
	idx := (t.statsHead + t.statsLen - 1) % StatsRingSize
	t.statsRing[idx].DiskUsed = bytes
	t.statsSeq++
	t.statsRingSeq[idx] = t.statsSeq
	wakeLocked(t.statsWakeSubs)
	t.mu.Unlock()
}

// SubscribeStatsWakeups returns a channel that receives a coalesced signal
// after each stats ring write. Read the ring with StatsSince; subscribe first
// so a write between the two still wakes the reader. The channel is never
// closed. unsubFn is idempotent and safe after ctx cancellation.
func (t *Task) SubscribeStatsWakeups(ctx context.Context) (wake <-chan struct{}, unsubFn func()) {
	ch := make(chan struct{}, 1)
	t.mu.Lock()
	t.statsWakeSubs = append(t.statsWakeSubs, ch)
	t.mu.Unlock()
	return ch, unsubscribe(t, ctx, &t.statsWakeSubs, ch, func() {})
}

// StatsSince returns, oldest first, the retained samples written or revised
// after the write numbered after, and the number to pass to the next call.
// Start at 0 for the whole ring. A reader that falls behind the ring loses
// only evicted samples.
func (t *Task) StatsSince(after uint64) (samples []runtime.Stats, through uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.statsLen {
		idx := (t.statsHead + i) % StatsRingSize
		if t.statsRingSeq[idx] > after {
			samples = append(samples, t.statsRing[idx])
		}
	}
	return samples, t.statsSeq
}

// wakeLocked signals each coalescing wakeup subscriber without blocking.
func wakeLocked(subs []chan struct{}) {
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
			// A signal is already pending.
		}
	}
}

// unsubscribe returns a function that, on its first call or on ctx
// cancellation, removes s from subs and runs release while holding t.mu, the
// lock publishers hold while sending to s.
func unsubscribe[S comparable](t *Task, ctx context.Context, subs *[]S, s S, release func()) func() {
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if i := slices.Index(*subs, s); i >= 0 {
				*subs = slices.Delete(*subs, i, i+1)
			}
			release()
		})
	}
	stop := context.AfterFunc(ctx, unsub)
	return func() {
		stop()
		unsub()
	}
}

// SessionStatus describes why SendInput could not deliver a message.
//
// Session lifecycle:
//   - A session wraps an SSH process bridging the server to the in-instance
//     relay daemon. It is set by Checkout.Start, Checkout.Reconnect, or
//     Checkout.RestartSession.
//   - The session is cleared by CloseSession (during restart), Kill (during
//     purge), or lazily by SendInput when it detects the SSH process
//     already exited (Done channel closed).
//   - "none" means no session was ever attached for this task — either the task
//     hasn't started, or the relay died and reconnect failed.
//   - "exited" means a session existed but the underlying SSH process exited
//     (relay or agent crashed, SSH dropped) before the user sent input.
type SessionStatus string

const (
	// SessionNone indicates no session was set on the task.
	SessionNone SessionStatus = "none"
	// SessionExited indicates the session's SSH process had already exited.
	SessionExited SessionStatus = "exited"
)

// ErrNoActiveSession reports that input cannot be delivered because no live
// session is attached to the task.
var ErrNoActiveSession = errors.New("no active session")

// ErrInitialPromptNotAccepted reports that startup ended before the task's
// first user input became part of the durable conversation.
var ErrInitialPromptNotAccepted = errors.New("task startup did not accept its initial prompt")

// SendInput sends a user message to the running agent.
//
// Returns an error if no session is active. The error includes the task state
// and a SessionStatus so the caller can diagnose why the session is missing
// (e.g. relay died vs. never connected). The session watcher now handles
// dead-session detection proactively, so SendInput no longer does lazy
// cleanup.
func (t *Task) SendInput(ctx context.Context, p agent.Prompt) error {
	if run := t.run.Load(); run != nil && run.ctx.Err() != nil {
		return run.ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
	if run := t.run.Load(); run != nil && run.ctx.Err() != nil {
		return run.ctx.Err()
	}
	t.mu.Lock()
	h := t.handle
	sessionStatus := SessionNone
	if h != nil {
		select {
		case <-h.Session.Done():
			sessionStatus = SessionExited
			h = nil
		default:
		}
	}
	state := t.stateLocked()
	t.mu.Unlock()
	if h == nil {
		return fmt.Errorf("%w (state=%s session=%s)", ErrNoActiveSession, state, sessionStatus)
	}
	return h.Session.SendPrompt(p)
}

// SendCompact sends a compact command to the running agent without changing
// the task state. Returns an error if no session is active or the backend
// does not support compaction.
func (t *Task) SendCompact(ctx context.Context, instructions string) error {
	if run := t.run.Load(); run != nil && run.ctx.Err() != nil {
		return run.ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	h := t.handle
	sessionStatus := SessionNone
	if h != nil {
		select {
		case <-h.Session.Done():
			sessionStatus = SessionExited
			h = nil
		default:
		}
	}
	state := t.stateLocked()
	t.mu.Unlock()
	if h == nil {
		return fmt.Errorf("no active session (state=%s session=%s)", state, sessionStatus)
	}
	return h.Session.SendCompact(instructions)
}

// SetTitle sets the title under the mutex, limiting it to 80 Unicode characters
// including a trailing ellipsis when truncated. Empty strings are ignored to
// preserve the prompt-fallback invariant.
func (t *Task) SetTitle(title string) {
	if title == "" {
		return
	}
	t.mu.Lock()
	t.titleGeneration++
	t.setTitleLocked(title)
	t.mu.Unlock()
}

// GenerateTitle asks the LLM for a short title from the last ten root turns,
// making up to two shortening requests when the title exceeds the
// character limit and logging through the task-scoped log. No-op when the
// provider is unconfigured. notify publishes only an accepted title change.
func (t *Task) GenerateTitle(ctx context.Context, log *slog.Logger, notify func()) {
	// Initial title generation belongs to the task, rather than whichever
	// execution happens to be active during the provider request.
	t.generateTitle(ctx, log, nil, notify)
}

// MarkSessionCrashed records operational health when the session of a
// starting or live task ends after reporting activity, or its runtime fails.
// The execution owner separately reports its error through the lifecycle result.
func (t *Task) MarkSessionCrashed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return (t.lifecycle == live || t.activity != "") && t.transitionLocked(task.StateCrashed)
}

// MarkSessionFailed records operational health when a starting session ends
// before it reports activity. The execution owner separately reports its
// error through the lifecycle result.
func (t *Task) MarkSessionFailed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lifecycle == task.StateStarting && t.activity == "" && t.transitionLocked(task.StateFailed)
}

// DiscardRollup stops forwarding this task's events and tells its previous
// sink to forget unflushed usage and replay state. It is called once a purge
// begins, before cleanup can receive late agent messages.
func (t *Task) DiscardRollup() {
	t.mu.Lock()
	meta := t.rollupMetaLocked()
	sink := t.Rollup
	t.Rollup = task.DiscardRollup{}
	t.mu.Unlock()
	sink.Discard(meta)
}

// recordingAdmissionErrorLocked rejects new host work after local shutdown.
// Accepted finalization records use the owning Run directly and remain drainable.
func (t *Task) recordingAdmissionErrorLocked() error {
	if t.shuttingDown.Load() {
		return context.Canceled
	}
	if run := t.run.Load(); run != nil {
		return context.Cause(run.ctx)
	}
	return nil
}

func (t *Task) generateTitle(ctx context.Context, log *slog.Logger, run *Run, notify func()) {
	if t.Provider == nil {
		return
	}
	t.mu.Lock()
	t.titleGeneration++
	generation := t.titleGeneration
	t.mu.Unlock()
	input := t.titleInput()

	start := time.Now()
	msgs := genai.Messages{genai.NewTextMessage(input)}
	opts := &genai.GenOptionText{SystemPrompt: fmt.Sprintf(titleSystemPrompt, maxTitleChars)}
	res, err := t.Provider.GenSync(ctx,
		msgs,
		opts,
	)
	dur := time.Since(start).Round(time.Millisecond)
	if err != nil {
		log.WarnContext(ctx, "title failed", "err", err, "dur", dur)
		return
	}
	// Strip surrounding quotes if the model adds them despite instructions.
	title := strings.Trim(strings.TrimSpace(res.String()), "\"'`")
	for attempt := 0; attempt < 2 && utf8.RuneCountInString(title) > maxTitleChars; attempt++ {
		msgs = append(msgs, res.Message, genai.NewTextMessage(fmt.Sprintf(
			"The title is too long. Shorten it to at most %d characters, including spaces and punctuation. Reply with ONLY the shorter title, no quotes.", maxTitleChars)))
		res, err = t.Provider.GenSync(ctx, msgs, opts)
		dur = time.Since(start).Round(time.Millisecond)
		if err != nil {
			log.WarnContext(ctx, "title shortening failed", "err", err, "dur", dur)
			return
		}
		title = strings.Trim(strings.TrimSpace(res.String()), "\"'`")
	}
	if title == "" {
		log.WarnContext(ctx, "title empty", "dur", dur)
		return
	}
	if ctx.Err() != nil {
		return
	}
	intent := &titleIntent{task: t, generation: generation, title: title, notify: notify}
	if run == nil {
		// Resolve the current owner only when the task-wide title is ready,
		// and keep its lifetime claim through admission. A completed execution
		// cannot leave a delayed initial title holding stale Records.
		t.recordingMu.Lock()
		run = t.run.Load()
		if run == nil {
			_, _ = intent.PrepareRecords(ctx)
			err = intent.ApplyRecords(ctx)
		} else {
			err = run.records.CommitIntent(ctx, intent)
		}
		t.recordingMu.Unlock()
	} else {
		// Turn-derived titles are bound to their Run and joined by Run.work.
		err = run.records.CommitIntent(ctx, intent)
	}
	if err != nil {
		log.WarnContext(ctx, "applying generated title failed", "err", err)
		return
	}
	if !intent.accepted {
		return
	}
	title = t.Title()
	log.InfoContext(ctx, "title generated", "title", title, "dur", dur)
}

// clearContextLocked resets turn and plan state when a root context boundary
// is applied as of at: the turn, question, or plan of the cleared
// conversation no longer applies, so the task waits. The caller holds t.mu.
func (t *Task) clearContextLocked(at time.Time) {
	t.inPlanMode = false
	t.planFile = ""
	t.planContent = ""
	t.planDismissed = true
	t.planExitID = ""
	t.turnAsks, t.turnExitPlan, t.turnContinues, t.turnEnded = nil, false, false, true
	if s := t.stateLocked(); s == task.StateRunning || s == task.StateAsking || s == task.StateHasPlan {
		t.setActivityLocked(task.StateWaiting, at)
	}
}

// snapshotLocked builds a Snapshot. Callers hold t.mu.
func (t *Task) snapshotLocked() Snapshot {
	var sample *runtime.Stats
	if t.statsLen > 0 {
		sample = new(t.statsRing[(t.statsHead+t.statsLen-1)%StatsRingSize])
	}
	counts := t.backgroundCountsLocked()
	return Snapshot{
		SessionMetadata:    t.sessionMetadata,
		State:              t.stateLocked(),
		StateUpdatedAt:     t.stateUpdatedAt,
		TurnStartedAt:      t.turnStartedAt,
		Repos:              append([]repo.Mount(nil), t.Repos...),
		RuntimeName:        t.RuntimeName,
		RuntimeInstanceID:  t.runtimeInstanceID,
		Tailscale:          t.Tailscale,
		TailscaleFQDN:      t.TailscaleFQDN,
		USB:                t.USB,
		Display:            t.Display,
		Sudo:               t.Sudo,
		SudoPassword:       t.SudoPassword,
		VNCPort:            t.VNCPort,
		GitHubToken:        t.GitHubToken,
		RelayOffset:        t.RelayOffset,
		Title:              t.title,
		RequestedModel:     t.RequestedModel,
		RequestedEffort:    t.RequestedEffort,
		ContextWindowLimit: t.reportedContextWindow,
		ContextUsed:        t.contextMeter.Used,
		InPlanMode:         t.inPlanMode,
		PlanFile:           t.planFile,
		PlanContent:        t.planContent,
		PlanExitID:         t.planExitID,
		CostUSD:            t.costTracker.TotalUSD,
		CostKnown:          t.costTracker.Known,
		CostComplete:       t.costTracker.Complete(),
		NumTurns:           t.turnMetrics.NumTurns,
		Duration:           t.turnMetrics.Duration,
		Usage:              t.liveUsage.Clone(),
		LastUsage:          t.lastUsage.Clone(),
		LastAPIUsage:       t.contextMeter.API.Clone(),
		CacheExpiresAt:     t.cacheExpiresAt,
		DiffStat:           t.liveDiffStat,
		RepoStates:         t.liveRepoStates,
		DiskUsed:           t.diskUsed,
		DiskKnown:          t.diskKnown,
		ResourceUsage:      sample,
		ForgeOwner:         t.forgeOwner,
		ForgeRepo:          t.forgeRepo,
		ForgePR:            t.forgePR,
		ForgePRState:       t.forgePRState,
		ForgeIssue:         t.ForgeIssue,
		CIStatus:           t.ciStatus,
		CIChecks:           append([]forge.Check(nil), t.ciChecks...),
		RateLimit:          t.rateLimit,
		TurnError:          t.exitStatus.TurnError,
		Background:         counts,
		BackgroundItems:    t.backgroundItemsLocked(counts),
	}
}

func (t *Task) backgroundCountsLocked() BackgroundCounts {
	return BackgroundCounts{
		Commands: t.backgroundCommands.Running(),
		Watches:  t.backgroundCommands.ActiveWatches(),
		Agents:   t.nativeSubagents.ActiveBackground(),
	}
}

func (t *Task) backgroundItemsLocked(counts BackgroundCounts) []BackgroundItem {
	n := counts.Commands + counts.Watches + counts.Agents
	if n == 0 {
		return nil
	}
	items := make([]BackgroundItem, 0, n)
	for c := range t.backgroundCommands.All {
		if c.Status != agent.BackgroundCommandStatusRunning {
			continue
		}
		kind := BackgroundKindCommand
		if c.Watch {
			kind = BackgroundKindWatch
		}
		items = append(items, BackgroundItem{Kind: kind, ID: c.ID, Label: c.Label, StartedAt: t.backgroundStarts[c.ID].at})
	}
	for s := range t.nativeSubagents.All {
		if s.Background && s.Status == agent.NativeSubagentStatusRunning {
			items = append(items, BackgroundItem{
				Kind: BackgroundKindAgent, ID: s.ID, Label: s.Label, StartedAt: t.backgroundStarts[s.ID].at,
				AgentKind: s.Kind, AgentScope: s.Scope,
			})
		}
	}
	slices.SortStableFunc(items, func(a, b BackgroundItem) int { return a.StartedAt.Compare(b.StartedAt) })
	return items
}

// foldBackgroundLocked folds a background lifecycle message into the card
// timelines and records the first running observation of each card's run.
// It reports changes to the summary of work already running.
func (t *Task) foldBackgroundLocked(m agent.Message, at time.Time) bool {
	switch m := m.(type) {
	case *agent.NativeSubagentMessage:
		before, _ := t.nativeSubagents.Subagent(m.Subagent.ID)
		startedAt := t.backgroundStarts[m.Subagent.ID].at
		t.nativeSubagents.Apply(&m.Subagent)
		s, _ := t.nativeSubagents.Subagent(m.Subagent.ID)
		t.observeBackgroundStartLocked(s.ID, s.RunID, s.Status == agent.NativeSubagentStatusRunning, at)
		return s.Background && s.Status == agent.NativeSubagentStatusRunning &&
			(s.Label != before.Label || s.Kind != before.Kind || s.Scope != before.Scope ||
				!t.backgroundStarts[s.ID].at.Equal(startedAt))
	case *agent.BackgroundCommandMessage:
		before, _ := t.backgroundCommands.Command(m.Command.ID)
		t.backgroundCommands.Apply(&m.Command)
		c, _ := t.backgroundCommands.Command(m.Command.ID)
		t.observeBackgroundStartLocked(c.ID, "", c.Status == agent.BackgroundCommandStatusRunning, at)
		return c.Status == agent.BackgroundCommandStatusRunning && c.Label != before.Label
	case *agent.BackgroundRetiredMessage:
		t.nativeSubagents.Retire(m.ID)
		t.backgroundCommands.Retire(m.ID)
	}
	return false
}

// observeBackgroundStartLocked applies the task history's StartedAt rule: a
// card's run starts at its first running observation, and only a new run
// forgets that start.
func (t *Task) observeBackgroundStartLocked(id, run string, running bool, at time.Time) {
	if id == "" {
		return
	}
	if s, ok := t.backgroundStarts[id]; ok {
		if s.run == run {
			return
		}
		delete(t.backgroundStarts, id)
	}
	// History also ignores a zero time and takes the next observation's.
	if !running || at.IsZero() {
		return
	}
	if t.backgroundStarts == nil {
		t.backgroundStarts = make(map[string]backgroundStart)
	}
	t.backgroundStarts[id] = backgroundStart{run: run, at: at}
}

// hostExitedLocked ends the open root turn, every running card, and the
// queued turn: the root harness process hosted them and can no longer report
// them. Unanswered questions and a presented plan stay until a turn starts.
func (t *Task) hostExitedLocked() {
	t.nativeSubagents.HostExited()
	t.backgroundCommands.HostExited()
	t.turnContinues = false
	t.turnEnded = true
}

// latestCommitSnapshot returns a copy of the most recently recorded repository
// tips, whether it is a pre-session baseline or a completed-turn snapshot.
func (t *Task) latestCommitSnapshot() *agent.TurnCommitSnapshotMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	view := t.timelineViewLocked()
	for _, entry := range view.Backward() {
		snapshot, ok := entry.Message.(*agent.TurnCommitSnapshotMessage)
		if !ok {
			continue
		}
		clone := *snapshot
		clone.RepositoryCommits = slices.Clone(snapshot.RepositoryCommits)
		if snapshot.ChangeStat != nil {
			change := *snapshot.ChangeStat
			clone.ChangeStat = &change
		}
		return &clone
	}
	return nil
}

// acceptGitReadLocked advances the applied read only for the current instance.
func (t *Task) acceptGitReadLocked(read repo.GitRead) bool {
	if read.InstanceID == "" || read.InstanceID != t.runtimeInstanceID || !read.NewerThan(t.lastGitRead) {
		return false
	}
	t.lastGitRead = read
	return true
}

func (t *Task) setLiveDiffStatLocked(ds v3.DiffStat) {
	t.liveDiffStat = ds
	if len(ds) > 0 {
		t.diffCreated = true
	}
}

// setLiveRepoStatesLocked overwrites the compact per-repo git state. An empty
// update keeps the previous state: the relay watcher emits DiffStatMessages
// without per-repo sections.
func (t *Task) setLiveRepoStatesLocked(states []v3.RepoState) {
	if len(states) == 0 {
		return
	}
	t.liveRepoStates = states
}

// observeContextLocked folds root context snapshots independently from billing.
// The caller holds t.mu.
func (t *Task) observeContextLocked(msg agent.Message, at time.Time, priorSessionID string) bool {
	change := t.contextMeter.Observe(msg, priorSessionID)
	if m, ok := msg.(*agent.UsageMessage); ok && change != agent.ContextUnchanged {
		if change == agent.ContextAPICall {
			t.cacheExpiresAt = time.Time{}
			if m.Usage.CacheTTLSeconds > 0 {
				t.cacheExpiresAt = at.Add(time.Duration(m.Usage.CacheTTLSeconds) * time.Second)
			}
		}
		if m.ContextWindow > 0 {
			t.reportedContextWindow = m.ContextWindow
		}
	}
	return change != agent.ContextUnchanged
}

// observeUsageLocked accumulates root token contributions while leaving
// context snapshots available independently of accounting.
func (t *Task) observeUsageLocked(msg agent.Message) {
	switch m := msg.(type) {
	case *agent.UsageMessage:
		if m.Additive {
			t.liveUsage.Add(m.Usage)
		}
	case *agent.ResultMessage:
		if !m.UsageIsSnapshot {
			t.liveUsage.Add(m.Usage)
		}
	}
}

// stateLocked derives the published state: the lifecycle, or the activity the
// fold derived for a starting or live session once it reports one. A live
// session that has not reported a turn waits for input. Callers hold t.mu.
func (t *Task) stateLocked() task.State {
	switch {
	case t.activity != "":
		return t.activity
	case t.lifecycle == live:
		return task.StateWaiting
	default:
		return t.lifecycle
	}
}

// transitionLocked moves the lifecycle to `to` when lifecycleSources allows it.
// Callers hold t.mu.
func (t *Task) transitionLocked(to task.State) bool {
	if !slices.Contains(lifecycleSources[to], t.lifecycle) {
		return false
	}
	t.moveLocked(to)
	return true
}

// moveLocked moves the lifecycle to `to` without checking lifecycleSources. A
// new lifecycle phase starts without activity, except a starting session that
// goes live keeps the activity it already reported. Callers hold t.mu.
func (t *Task) moveLocked(to task.State) {
	prev := t.stateLocked()
	if t.lifecycle != task.StateStarting || to != live {
		t.activity = ""
	}
	t.lifecycle = to
	if t.stateLocked() != prev {
		t.publishLocked(prev, time.Now().UTC())
	}
}

// setActivityLocked records the fold's activity for the starting or live
// session at its transition time; a zero time means now. Callers hold t.mu.
func (t *Task) setActivityLocked(a task.State, at time.Time) {
	prev := t.stateLocked()
	t.activity = a
	t.publishLocked(prev, at)
}

// publishLocked records the published state as of at, which is now when zero.
// Entering Running records the turn start; leaving it clears the turn start. A
// change appends to the bounded transition journal. Callers hold t.mu.
func (t *Task) publishLocked(prev task.State, at time.Time) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s := t.stateLocked()
	if s == task.StateRunning && prev != task.StateRunning {
		t.turnStartedAt = at
	} else if s != task.StateRunning {
		t.turnStartedAt = time.Time{}
	}
	t.stateUpdatedAt = at
	if s == prev {
		return
	}
	t.stateSeq++
	t.stateTransitions = append(t.stateTransitions, StateTransition{Seq: t.stateSeq, State: s, At: at})
	if len(t.stateTransitions) > stateTransitionHistory {
		t.stateTransitions = t.stateTransitions[len(t.stateTransitions)-stateTransitionHistory:]
	}
}

// settledTurnStateLocked returns the state a parent turn settles into once its
// trailing ResultMessage has arrived: asking or presenting a plan when the user
// still owes input, running while the harness has queued a turn by itself or a
// detached subagent or a shell command whose end wakes the agent is active, and
// waiting otherwise. A harness can end the parent turn while background work
// continues, so such a card keeps the task busy after the parent stops
// producing output. When that work ends, the harness queues a turn to report
// it; the task stays running until that turn's result or a host exit. A
// running watch or a command whose end wakes no agent never keeps the task
// running: the agent waits for the user meanwhile. A foreground card that is
// still running at the result is stale evidence and falls back to waiting.
//
// The caller must hold t.mu.
func (t *Task) settledTurnStateLocked() task.State {
	switch {
	case len(t.turnAsks) > 0:
		return task.StateAsking
	case t.turnExitPlan && t.planContent != "":
		return task.StateHasPlan
	case t.turnContinues || t.nativeSubagents.ActiveBackground() > 0 || t.backgroundCommands.Active() > 0:
		return task.StateRunning
	default:
		return task.StateWaiting
	}
}

// settleTurnLocked settles the activity of an ended root turn as of at. A
// task still starting settles too: the agent can report a result before its
// session goes live. The caller holds t.mu.
func (t *Task) settleTurnLocked(at time.Time) {
	cur := t.stateLocked()
	if cur != task.StateRunning && cur != task.StateStarting && cur != task.StateWaiting && cur != task.StateAsking {
		return
	}
	next := t.settledTurnStateLocked()
	t.setActivityLocked(next, at)
	if next == task.StateRunning {
		// Duration now includes the settled turn, so the live delta starts at
		// its result rather than at the turn that just ended.
		t.turnStartedAt = t.stateUpdatedAt
	}
}

// addParsedMessage folds an accepted semantic message into task state and the
// timeline, then wakes timeline subscribers.
func (t *Task) addParsedMessage(parsed agent.TimedMessage) (stateChanged bool) {
	m := parsed.Message
	t.mu.Lock()
	if pr, ok := m.(*agent.MetaPRMessage); ok {
		changed := t.forgeOwner != pr.ForgeOwner || t.forgeRepo != pr.ForgeRepo || t.forgePR != pr.ForgePR
		t.forgeOwner = pr.ForgeOwner
		t.forgeRepo = pr.ForgeRepo
		t.forgePR = pr.ForgePR
		t.forgePRState = forge.PRStateOpen
		t.mu.Unlock()
		return changed
	}
	switch m.(type) {
	case *agent.MetaMessage:
		t.mu.Unlock()
		return false
	case *agent.MetaResultMessage:
		// The settlement trailer ends the agent process, which can die with its
		// container without recording an exit. It is not part of the timeline.
		before := t.backgroundCountsLocked()
		t.hostExitedLocked()
		changed := t.backgroundCountsLocked() != before
		t.mu.Unlock()
		return changed
	}
	initialState := t.stateLocked()
	initialPlanMode, initialPlanID, initialPlanContent := t.inPlanMode, t.planExitID, t.planContent
	summaryChanged := false
	defer func() {
		stateChanged = summaryChanged || t.stateLocked() != initialState || t.inPlanMode != initialPlanMode || t.planExitID != initialPlanID || t.planContent != initialPlanContent
		t.mu.Unlock()
	}()
	producerAt := parsed.ProducerTime
	at := producerAt
	if at.IsZero() {
		at = time.Now()
	}
	timeline := !agent.IsSessionMetadataUpdate(m)
	if timeline {
		parsed.ProducerTime = at
		t.timeline = append(t.timeline, parsed)
		if isDeltaFinal(m) {
			if t.timelineReaders == 0 {
				view := t.timelineViewLocked()
				compactFinalizedDeltaAt(view, view.Len()-1)
			} else {
				t.timelineCompactPending = append(t.timelineCompactPending, t.timelineLenLocked()-1)
			}
		}
	}
	summaryChanged = t.foldMessageLocked(m, at, producerAt, false)
	if timeline && !agent.IsInputDeliveryControl(m) {
		wakeLocked(t.wakeSubs)
	}
	return stateChanged
}

// foldMessageLocked folds one ordered message into the task's derived state.
// Live ingestion and SeedTimelineParts both apply it, so a restored task
// reaches the state the live task had. at is when the message took effect,
// zero when unknown; producerAt is the recorded producer time, zero when
// unknown. It reports
// whether the message is published and whether it changed the task summary
// without a state change. The caller holds t.mu.
func (t *Task) foldMessageLocked(m agent.Message, at, producerAt time.Time, replayed bool) (summaryChanged bool) {
	child := m.Provenance().IsChild()
	priorMetadata := t.sessionMetadata
	t.sessionMetadata.Apply(m)
	if agent.IsSessionMetadataUpdate(m) {
		t.costTracker.Observe(m, producerAt, t.Pricer)
		return t.sessionMetadata != priorMetadata
	}
	// State transitions take the producer time so live and restored tasks
	// agree; a message without one inherits the latest recorded producer time.
	stateAt := producerAt
	if stateAt.IsZero() {
		stateAt = t.lastProducerAt
	} else {
		t.lastProducerAt = producerAt
	}
	t.turnMetrics.Observe(m, producerAt)
	t.exitStatus.Observe(m)
	if !child {
		t.observeTurnLocked(m, stateAt)
	}
	if agent.IsInputDeliveryControl(m) {
		return false
	}
	if rateLimit, ok := m.(*agent.RateLimitMessage); ok && !rateLimit.IsChildThreadGoal() {
		t.recordRateLimitLocked(rateLimit)
	}
	if !child {
		if tu, ok := m.(*agent.ToolUseMessage); ok {
			t.trackToolUse(tu)
		}
		// A compaction boundary rewrites the summary's live context fill, so the
		// task list must refresh even though the task state is unchanged.
		summaryChanged = t.observeContextLocked(m, at, priorMetadata.SessionID)
	}

	// Fold harness-native child activity into the canonical card set and let it
	// settle the parent state. A harness can end the parent turn when it
	// delegates in the background, so a trailing ResultMessage is not enough
	// evidence that the task is idle: a detached child keeps it running, and the
	// last detached child settling returns it to waiting.
	// Detached shell commands whose end wakes the agent keep the task busy after
	// the agent ends its turn, and a card the harness stopped listing ends as on
	// host exit.
	switch m.(type) {
	case *agent.NativeSubagentMessage, *agent.BackgroundCommandMessage, *agent.BackgroundRetiredMessage, *agent.ExitMessage:
		before := t.backgroundCountsLocked()
		detailsChanged := t.foldBackgroundLocked(m, at)
		// The root harness process hosts its native subagents, so its exit ends
		// their observable lifecycle and they no longer keep the task running.
		if _, ok := m.(*agent.ExitMessage); ok && !child {
			t.hostExitedLocked()
		}
		summaryChanged = summaryChanged || detailsChanged || t.backgroundCountsLocked() != before
		if cur := t.stateLocked(); (cur == task.StateWaiting || cur == task.StateRunning) && t.turnEnded {
			if next := t.settledTurnStateLocked(); next != cur {
				t.setActivityLocked(next, stateAt)
			}
		}
	}
	// Update live diff stat from relay polling.
	if ds, ok := m.(*agent.DiffStatMessage); ok {
		t.lastGitRead = repo.NewGitRead(t.runtimeInstanceID)
		t.setLiveDiffStatLocked(ds.DiffStat)
		t.setLiveRepoStatesLocked(ds.Repos)
		// Diff data refreshes without a state change, but the task-list stream
		// must still push the new card summary.
		summaryChanged = true
	}
	if sm, ok := m.(*agent.SystemMessage); ok && !child && sm.Subtype == "context_cleared" {
		t.clearContextLocked(stateAt)
	}
	// Transition to waiting/asking when a result arrives.
	if rm, ok := m.(*agent.ResultMessage); ok && !child {
		t.completedRootTurns++
		t.turnContinues = rm.TurnContinues
		if len(rm.DiffStat) > 0 {
			t.setLiveDiffStatLocked(rm.DiffStat)
		}
		t.lastUsage = rm.Usage
		if rm.ContextWindow > 0 {
			t.reportedContextWindow = rm.ContextWindow
		}
		t.planDismissed = false
		t.settleTurnLocked(stateAt)
	}
	if _, ok := m.(*agent.TurnEndedMessage); ok && !child {
		t.turnContinues = false
		t.settleTurnLocked(stateAt)
	}
	if !child {
		t.observeUsageLocked(m)
	}
	t.costTracker.Observe(m, at, t.Pricer)
	// Forward the folded message to the usage rollup with the cost snapshot
	// reflecting every prior fold in this message.
	t.observeRollupLocked(m, at, producerAt, replayed)
	return summaryChanged
}

// observeTurnLocked folds a root message into the turn boundaries and the
// evidence of the current turn: an accepted prompt or a TurnStartedMessage
// starts a turn, its trailing result ends it, and the turn collects its
// unanswered questions and whether it presented a plan. Agent output neither
// starts nor ends a turn. A root input whose delivery is unresolved takes
// effect once delivery completes or root output proves it was accepted; a
// failed delivery discards it, so the task keeps the state the attempt started
// from. The caller holds t.mu.
func (t *Task) observeTurnLocked(m agent.Message, at time.Time) {
	if task.AcceptedInputOutput(m) {
		t.resolveInputLocked(true)
	}
	switch m := m.(type) {
	case *agent.SystemMessage:
		switch m.Subtype {
		case agent.SystemSubtypeInputDeliveryStarted:
			t.resolveInputLocked(false)
			t.inputDelivering = true
		case agent.SystemSubtypeInputDeliveryCompleted:
			t.resolveInputLocked(true)
		case agent.SystemSubtypeInputDeliveryFailed:
			t.resolveInputLocked(false)
		}
	case *agent.TurnStartedMessage:
		t.startTurnLocked(at)
	case *agent.ResultMessage:
		if t.turnEnded {
			t.turnAsks, t.turnExitPlan = nil, false
		}
		t.turnEnded = true
	case *agent.TurnEndedMessage:
		t.turnEnded = true
	case *agent.UserInputMessage:
		if t.inputDelivering {
			t.deferredInput, t.deferredInputAt = true, at
		} else {
			t.acceptInputLocked(at)
		}
	case *agent.AskMessage:
		t.continueTurnLocked()
		key := agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)
		if !slices.ContainsFunc(t.turnAsks, func(a turnAsk) bool { return a.key == key && m.ToolUseID != "" }) {
			t.turnAsks = append(t.turnAsks, turnAsk{key: key})
		}
		if cur := t.stateLocked(); cur == task.StateStarting || cur == task.StateRunning || cur == task.StateWaiting || cur == task.StateHasPlan {
			t.setActivityLocked(task.StateAsking, at)
		}
	case *agent.ToolResultMessage:
		if m.ToolUseID != "" && m.Error == "" {
			key := agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)
			t.turnAsks = slices.DeleteFunc(t.turnAsks, func(a turnAsk) bool { return a.key == key })
		}
	case *agent.PendingUserActionMessage:
		if m.Action.Kind != agent.PendingUserActionAskUserQuestion || m.Action.ToolUseID == "" {
			return
		}
		key := agent.ToolKeyForSource(m.Provenance(), m.Action.ToolUseID)
		if i := slices.IndexFunc(t.turnAsks, func(a turnAsk) bool { return a.key == key }); i >= 0 {
			t.turnAsks[i].action = &m.Action
		}
	case *agent.ToolUseMessage:
		if m.Name == "ExitPlanMode" {
			t.turnExitPlan = true
		}
	}
}

// resolveInputLocked ends an unresolved input delivery and applies its input,
// as of its arrival, once accepted.
func (t *Task) resolveInputLocked(accepted bool) {
	if accepted && t.deferredInput {
		t.acceptInputLocked(t.deferredInputAt)
	}
	t.inputDelivering, t.deferredInput, t.deferredInputAt = false, false, time.Time{}
}

// acceptInputLocked applies an accepted root input: it answers every open
// question and starts a turn.
func (t *Task) acceptInputLocked(at time.Time) {
	t.turnAsks = nil
	t.startTurnLocked(at)
}

// startTurnLocked starts a root turn after a trailing result; during a turn it
// changes nothing. A waiting task, or one that a restart left starting, runs.
// So does an asking task without unanswered questions; a question of the
// current turn keeps asking until input answers it. The caller holds t.mu.
func (t *Task) startTurnLocked(at time.Time) {
	t.continueTurnLocked()
	switch t.stateLocked() {
	case task.StateStarting, task.StateWaiting, task.StateHasPlan:
	case task.StateAsking:
		if len(t.turnAsks) > 0 {
			return
		}
	default:
		return
	}
	t.setActivityLocked(task.StateRunning, at)
}

// continueTurnLocked keeps the current root turn; after a trailing result it
// starts a new one.
func (t *Task) continueTurnLocked() {
	if t.turnEnded {
		t.turnEnded = false
		t.turnAsks, t.turnExitPlan = nil, false
	}
}

func rateLimitFromMessage(m *agent.RateLimitMessage) RateLimit {
	rateLimit := RateLimit{
		Status:         m.Status,
		RateLimitType:  m.RateLimitType,
		Utilization:    m.Utilization,
		IsUsingOverage: m.IsUsingOverage,
		QuotaProvider:  m.QuotaProvider,
		QuotaLabel:     m.QuotaLabel,
		QuotaWindow:    m.QuotaWindow,
		ObservedAt:     time.Now(),
	}
	rateLimit.ResetsAt = m.ResetsAt
	rateLimit.OverageResetsAt = m.OverageResetsAt
	return rateLimit
}

// recordRateLimitLocked records one provider-window update and refreshes the
// active task block. The caller must hold t.mu.
func (t *Task) recordRateLimitLocked(m *agent.RateLimitMessage) {
	rateLimit := rateLimitFromMessage(m)
	if t.rateLimits == nil {
		t.rateLimits = make(map[quotaWindowKey]RateLimit)
	}
	t.rateLimits[rateLimitKey(&rateLimit)] = rateLimit
	t.rateLimit = activeRateLimit(t.rateLimits, time.Now())
}

func rateLimitKey(rateLimit *RateLimit) quotaWindowKey {
	window := rateLimit.QuotaWindow
	if window == "" {
		window = rateLimit.RateLimitType
	}
	return quotaWindowKey{provider: rateLimit.QuotaProvider, window: window}
}

func activeRateLimit(rateLimits map[quotaWindowKey]RateLimit, now time.Time) RateLimit {
	var active RateLimit
	for key := range rateLimits {
		rateLimit := rateLimits[key]
		if rateLimit.Status != agent.RateLimitStatusRejected || rateLimit.IsUsingOverage || !rateLimit.ResetsAt.After(now) {
			continue
		}
		if active.ResetsAt.IsZero() || rateLimit.ResetsAt.After(active.ResetsAt) {
			active = rateLimit
		}
	}
	return active
}

// writeToolInput is the JSON input schema for the Write tool_use block.
type writeToolInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// editToolInput is the JSON input schema for the Edit tool_use block.
type editToolInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// trackToolUse inspects a ToolUseMessage for plan-related tools and updates
// PlanFile, InPlanMode, and which ExitPlanMode carries the current plan
// (planExitID in the task snapshot). The caller must hold t.mu.
func (t *Task) trackToolUse(tu *agent.ToolUseMessage) {
	switch tu.Name {
	case "EnterPlanMode":
		t.inPlanMode = true
	case "ExitPlanMode":
		t.inPlanMode = false
		t.planExitID = tu.ToolUseID
	case "Write":
		if t.planDismissed {
			return
		}
		var input writeToolInput
		if json.Unmarshal(tu.Input, &input) == nil && strings.Contains(input.FilePath, ".claude/plans/") {
			t.planFile = input.FilePath
			t.planContent = input.Content
		}
	case "Edit":
		if t.planDismissed {
			return
		}
		var input editToolInput
		if json.Unmarshal(tu.Input, &input) == nil && t.planFile == input.FilePath && t.planContent != "" {
			if input.ReplaceAll {
				t.planContent = strings.ReplaceAll(t.planContent, input.OldString, input.NewString)
			} else {
				t.planContent = strings.Replace(t.planContent, input.OldString, input.NewString, 1)
			}
		}
	}
}

// terminalLogSummary projects the live task and its bounded terminal result
// into the same LoadedTask shape used by startup. Completion already has these
// values in memory, so persisting this projection avoids decoding the newly
// compressed message body on the next process start.
func (t *Task) terminalLogSummary(res *task.Result) *taskhistory.LoadedTask {
	snapshot := t.Snapshot()
	// GitRoot is process-local checkout state and is absent from the durable
	// log header. Do not let the fast completion path persist a stale host path
	// that a normal log scan would never reconstruct.
	for i := range snapshot.Repos {
		snapshot.Repos[i].GitRoot = ""
	}
	parentTaskID := ""
	if t.ParentTaskID != 0 {
		parentTaskID = t.ParentTaskID.String()
	}
	return &taskhistory.LoadedTask{
		SessionMetadata:                snapshot.SessionMetadata,
		TaskID:                         t.ID.String(),
		Prompt:                         t.InitialPrompt.Text,
		Title:                          snapshot.Title,
		Repos:                          snapshot.Repos,
		Harness:                        t.Harness,
		StartedAt:                      t.StartedAt,
		LastActivityAt:                 snapshot.StateUpdatedAt,
		State:                          res.State,
		ForgeIssue:                     snapshot.ForgeIssue,
		OwnerID:                        t.OwnerID,
		ForkedFromTaskID:               t.ForkedFromTaskID.String(),
		ParentTaskID:                   parentTaskID,
		CaicMCP:                        t.CaicMCP,
		ForgeOwner:                     snapshot.ForgeOwner,
		ForgeRepo:                      snapshot.ForgeRepo,
		ForgePR:                        snapshot.ForgePR,
		Tailscale:                      snapshot.Tailscale,
		USB:                            snapshot.USB,
		Display:                        snapshot.Display,
		Sudo:                           snapshot.Sudo,
		GitHubToken:                    snapshot.GitHubToken,
		RuntimeName:                    snapshot.RuntimeName,
		BaseImage:                      t.BaseImage,
		ContainerPlatform:              t.ContainerPlatform,
		MaxCPUs:                        t.MaxCPUs,
		CacheMounts:                    slices.Clone(t.CacheMounts),
		Mounts:                         slices.Clone(t.Mounts),
		RequestedModel:                 t.RequestedModel,
		RequestedEffort:                t.RequestedEffort,
		RequestedAutoCompactTokenLimit: t.RequestedAutoCompactTokenLimit,
		RequestedCompactionBudget:      t.RequestedCompactionBudget,
		DiffCreated:                    t.DiffCreated(),
		LastTrailer:                    res,
	}
}

// activeModel returns the model to price usage against: the harness-reported
// model when known, else the user-requested one.
func (t *Task) activeModel() string {
	if t.sessionMetadata.ReportedModel != "" {
		return t.sessionMetadata.ReportedModel
	}
	return t.RequestedModel
}

// observeRollupLocked forwards one agent message to the task's usage rollup
// sink, which is never nil. The caller holds t.mu.
func (t *Task) observeRollupLocked(m agent.Message, at, producerAt time.Time, replayed bool) {
	count, confirmed := t.skillReads.Confirm(m)
	if count {
		t.observeRollupMessageLocked(m, at, producerAt, replayed)
	}
	for _, c := range confirmed {
		t.observeRollupMessageLocked(c, at, producerAt, replayed)
	}
}

func (t *Task) observeRollupMessageLocked(m agent.Message, at, producerAt time.Time, replayed bool) {
	if q, ok := m.(*agent.RateLimitMessage); ok {
		if q.IsChildThreadGoal() {
			return
		}
		c := quotaChange(q, at)
		t.Rollup.ObserveQuota(&c)
		return
	}
	model, effort := t.rollupAttributionLocked(m)
	e, ok := rollupEvent(m, at, producerAt, replayed, model, effort, t.costTracker.Snapshot(), t.costTracker.TurnAPIMs)
	if ok {
		t.Rollup.Observe(t.rollupMetaLocked(), &e)
	}
}

// rollupMetaLocked snapshots the task identity for the rollup. The caller
// holds t.mu.
func (t *Task) rollupMetaLocked() usagedb.TaskMeta {
	repos := make([]string, len(t.Repos))
	for i, r := range t.Repos {
		repos[i] = r.Name
	}
	return usagedb.TaskMeta{
		TaskID:         t.ID,
		Harness:        string(t.Harness),
		Repos:          repos,
		RequestedModel: t.RequestedModel,
	}
}

// rollupAttributionLocked resolves the model and thinking effort one message
// attributes to the rollup: what the message reports when present, else the
// task's active values. A child actor reports its own model on usage messages
// and never its effort, so it inherits neither from the task. The caller holds
// t.mu.
func (t *Task) rollupAttributionLocked(m agent.Message) (model, effort string) {
	if u, ok := m.(*agent.UsageMessage); ok && u.ReportedModel != "" {
		model = u.ReportedModel
	}
	if m.Provenance().IsChild() {
		return model, ""
	}
	if model == "" {
		model = t.activeModel()
	}
	return model, t.activeEffort()
}

// activeEffort returns the thinking effort the task currently runs at.
func (t *Task) activeEffort() string {
	if t.sessionMetadata.ReportedEffort != "" {
		return t.sessionMetadata.ReportedEffort
	}
	return t.RequestedEffort
}

func (t *Task) timelineViewLocked() timelineEntries {
	return timelineEntries{prefix: t.timelinePrefix, suffix: t.timeline}
}

func (t *Task) timelineLenLocked() int { return len(t.timelinePrefix) + len(t.timeline) }

func (t *Task) acquireTimelineReaderLocked() timelineEntries {
	t.timelineReaders++
	return t.timelineViewLocked()
}

func (t *Task) releaseTimelineReader() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timelineReaders--
	if t.timelineReaders < 0 {
		panic("task: negative timeline reader count")
	}
	if t.timelineReaders == 0 && len(t.timelineCompactPending) > 0 {
		view := t.timelineViewLocked()
		for _, i := range t.timelineCompactPending {
			compactFinalizedDeltaAt(view, i)
		}
		t.timelineCompactPending = t.timelineCompactPending[:0]
	}
}

// titleRefreshDue keeps early titles responsive while reducing requests on long
// tasks. The count is folded from history, so restarts do not reset the cadence.
func (t *Task) titleRefreshDue() bool {
	t.mu.Lock()
	n := t.completedRootTurns
	t.mu.Unlock()
	switch n {
	case 1, 2, 5, 10, 20:
		return true
	default:
		return n > 0 && n%50 == 0
	}
}

type titleTurn struct{ prompt, answer string }

// titleInput reads only the most recent ten root turns and bounds each prompt
// and answer before building the request. Tool payloads and child activity are
// excluded; an empty result uses the turn's latest assistant text.
func (t *Task) titleInput() string {
	t.mu.Lock()
	view := t.acquireTimelineReaderLocked()
	title := t.title
	t.mu.Unlock()
	defer t.releaseTimelineReader()

	const maxTextBytes = 2000
	start, completed := 0, 0
	for i, entry := range view.Backward() {
		if entry.Message.Provenance().IsChild() {
			continue
		}
		if _, ok := entry.Message.(*agent.ResultMessage); ok {
			completed++
			if completed > 10 {
				start = i + 1
				break
			}
		}
	}
	turns := make([]titleTurn, 0, 10)
	failedInputs := unacceptedInputs(view, start)
	var current titleTurn
	finalText := false
	for i := start; i < view.Len(); i++ {
		m := view.At(i).Message
		if m.Provenance().IsChild() {
			continue
		}
		switch m := m.(type) {
		case *agent.UserInputMessage:
			if _, rejected := failedInputs[m]; rejected {
				continue
			}
			current.prompt = truncateUTF8(m.Text, maxTextBytes)
		case *agent.TextMessage:
			current.answer = truncateUTF8(m.Text, maxTextBytes)
			finalText = true
		case *agent.TextDeltaMessage:
			if !finalText {
				current.answer += truncateUTF8(m.Text, maxTextBytes-len(current.answer))
			}
		case *agent.ResultMessage:
			if m.Result != "" {
				current.answer = truncateUTF8(m.Result, maxTextBytes)
			}
			turns = append(turns, current)
			current = titleTurn{}
			finalText = false
		}
	}
	if current.prompt != "" || current.answer != "" {
		turns = append(turns, current)
	}
	if len(turns) == 0 {
		turns = append(turns, titleTurn{})
	}
	if start == 0 && turns[0].prompt == "" {
		turns[0].prompt = truncateUTF8(t.InitialPrompt.Text, maxTextBytes)
	}
	if len(turns) > 10 {
		turns = turns[len(turns)-10:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Current title: %s\nRecent activity:\n", title)
	for _, turn := range turns {
		fmt.Fprintf(&b, "Prompt: %s\nResult: %s\n", turn.prompt, turn.answer)
	}
	return b.String()
}

func (t *Task) setTitleLocked(title string) {
	if utf8.RuneCountInString(title) > maxTitleChars {
		title = string([]rune(title)[:maxTitleChars-1]) + "…"
	}
	t.title = title
}

var (
	compactedTextDelta     = &agent.TextDeltaMessage{}
	compactedThinkingDelta = &agent.ThinkingDeltaMessage{}
)

type timelineEntries struct {
	prefix []agent.TimedMessage
	suffix []agent.TimedMessage
}

func (v timelineEntries) Len() int { return len(v.prefix) + len(v.suffix) }

func (v timelineEntries) At(i int) agent.TimedMessage {
	if i < len(v.prefix) {
		return v.prefix[i]
	}
	return v.suffix[i-len(v.prefix)]
}

func (v timelineEntries) Forward() iter.Seq2[int, agent.TimedMessage] {
	return func(yield func(int, agent.TimedMessage) bool) {
		for i, entry := range v.prefix {
			if !yield(i, entry) {
				return
			}
		}
		for i, entry := range v.suffix {
			if !yield(len(v.prefix)+i, entry) {
				return
			}
		}
	}
}

func (v timelineEntries) Backward() iter.Seq2[int, agent.TimedMessage] {
	return func(yield func(int, agent.TimedMessage) bool) {
		for i, v0 := range slices.Backward(v.suffix) {
			if !yield(len(v.prefix)+i, v0) {
				return
			}
		}
		for i, v0 := range slices.Backward(v.prefix) {
			if !yield(i, v0) {
				return
			}
		}
	}
}

func (v timelineEntries) SetMessage(i int, message agent.Message) {
	if i < len(v.prefix) {
		v.prefix[i].Message = message
		return
	}
	v.suffix[i-len(v.prefix)].Message = message
}

func (v timelineEntries) Slice(end int) timelineEntries {
	if end <= len(v.prefix) {
		return timelineEntries{prefix: v.prefix[:end]}
	}
	return timelineEntries{prefix: v.prefix, suffix: v.suffix[:end-len(v.prefix)]}
}

// fallbackResultText returns the visible assistant text of the turn preceding
// the trailing ResultMessage in msgs, using the same window rules as
// ResultTextWindow. The input may include the trailing ResultMessage.
func fallbackResultText(entries timelineEntries) string {
	end := entries.Len()
	if end > 0 {
		if _, ok := entries.At(end - 1).Message.(*agent.ResultMessage); ok {
			end--
		}
	}
	var w task.ResultTextWindow
	for _, entry := range entries.Slice(end).Forward() {
		w.Update(entry.Message)
	}
	return w.Value()
}

const maxTitleChars = 80

const titleSystemPrompt = "Summarize this coding task conversation in 3-8 words as a short title of at most %d characters, including spaces and punctuation. Keep the current title if it still accurately describes the recent activity. Reply with ONLY the title, no quotes."
