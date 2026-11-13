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

const statsRingSize = 60

// stateTransitionHistory bounds each task's journal of recent state
// transitions. It only needs to cover the states a task-list connection can
// miss between two snapshots, so a short window is sufficient.
const stateTransitionHistory = 16

type statsSub struct {
	ch   chan runtime.Stats
	once sync.Once
}

func (s *statsSub) close() { s.once.Do(func() { close(s.ch) }) }

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
	ID                ksid.ID
	InitialPrompt     agent.Prompt         // Initial prompt text and optional images.
	Harness           harness.Name         // Agent harness ("claude", "codex", etc.).
	RequestedModel    string               // User-requested model; passed to agent CLI.
	RequestedEffort   string               // Thinking effort; passed to agent CLI. Empty = default.
	RuntimeName       runtime.Name         // Runtime backend used for this task.
	BaseImage         string               // Custom runtime base image; empty means use the default.
	ContainerPlatform string               // Container CPU architecture; empty means use the host default.
	MaxCPUs           int                  // Max CPU cores for the instance; 0 means use the default.
	CacheMounts       []runtime.CacheMount // Build cache mounts baked into the runtime image.
	Mounts            []runtime.Mount      // Host directories bind-mounted into the runtime instance.
	Tailscale         bool                 // Enable Tailscale networking in the instance.
	USB               bool                 // Enable USB passthrough in the instance.
	Display           bool                 // Enable Xvfb display in the instance.
	Sudo              bool                 // Enable root access (password-based sudo) in the instance.
	StartedAt         time.Time            // When the task was created.
	OwnerID           string               // Human authorization principal; distinct from the delegating task's identity.
	ForgeIssue        int                  // Originating issue number for bot comment callbacks; 0 = none.
	ForkedFromTaskID  ksid.ID              // Parent task ID when created by fork; zero otherwise.
	ParentTaskID      ksid.ID              // Delegating task ID for a child task; zero for root tasks and ordinary forks.
	CaicMCP           bool                 // Enables task-scoped CAIC MCP delegation.
	Pricer            usage.ModelPricer    // Quota-provider per-model pricing for cost reporting; set by the task manager before the task runs. Nil keeps harness-reported totals only.
	Rollup            task.RollupSink      // Cross-task usage rollup sink; NewTask defaults to DiscardRollup, the task manager replaces it with the wired store.
	Provider          genai.Provider

	timelineID string

	// Mutable task metadata. These fields are populated at construction, setup, or
	// import. After a task is published in the Manager registry, access them
	// through Task methods so readers and async lifecycle goroutines synchronize.
	Repos            []repo.Mount // index 0 = primary; empty = no-repo
	TailscaleFQDN    string       // Tailscale FQDN assigned to the instance (empty if not available).
	TailscaleAuthURL string       // Tailscale browser auth URL when no pre-auth key was available.
	RelayOffset      int64        // Committed relay output bytes in the current generation, for reconnect.
	SudoPassword     string       // Random sudo password; empty if sudo is not enabled.
	VNCPort          int          // VNC WebSocket port inside the instance (0 = no VNC). Set during launch.
	GitHubToken      bool         // Inject GitHub token into the instance's environment.

	inputMu sync.Mutex // Serializes follow-up delivery attempts.

	// mu protects mutable task metadata above and all fields below.
	mu                     sync.Mutex
	runtimeInstanceID      runtime.ID
	runtimeConnection      runtime.ConnectionTarget
	statsRing              [statsRingSize]runtime.Stats
	statsLen               int
	statsHead              int
	statsSubs              []*statsSub
	diskUsed               int64
	diskKnown              bool
	state                  task.State
	stateUpdatedAt         time.Time         // UTC timestamp of the last state transition.
	stateSeq               uint64            // Monotonic sequence of recorded state transitions.
	stateTransitions       []StateTransition // Bounded journal of recent state transitions.
	sessionMetadata        agent.SessionMetadata
	contextUsed            *int                            // Latest native occupancy; nil means unknown.
	reportedContextWindow  int                             // Context window size reported by the agent (0 = unknown).
	planFile               string                          // Path to plan file inside instance, captured from Write tool_use.
	planContent            string                          // Content of the plan file, captured from Write tool_use input.
	planExitID             string                          // ToolUseID of the ExitPlanMode carrying planContent; "" after context_cleared.
	planDismissed          bool                            // True after a context boundary; suppresses plan tracking until the next ResultMessage.
	inPlanMode             bool                            // True while the agent is in plan mode (between EnterPlanMode and ExitPlanMode).
	completedRootTurns     int                             // Completed root turns; reconstructed from history for title refresh cadence.
	titleGeneration        uint64                          // Orders concurrent title requests so stale responses cannot replace newer titles.
	title                  string                          // LLM-generated short title; set via SetTitle.
	timelinePrefix         []agent.TimedMessage            // Imported durable history; ownership transfers at adoption.
	timeline               []agent.TimedMessage            // Live tail and ordinary one-slice histories.
	timelineReaders        int                             // Active zero-copy iterators and snapshots.
	timelineCompactPending []int                           // Final-message indexes waiting for readers to release timeline slots.
	nativeSubagents        agent.NativeSubagentTimeline    // harness-native subagent cards folded from timeline
	backgroundCommands     agent.BackgroundCommandTimeline // harness-native detached shell cards folded from timeline
	skillReads             agent.SkillReadTracker

	subs            []*sub          // active sequenced message subscribers
	rateLimitSubs   []*rateLimitSub // active lossless quota subscribers
	handle          *SessionHandle  // current active session; nil when no session is attached
	inputPriorState task.State      // Nonzero only while a committed attempt has no accepted root output.
	turnStartedAt   time.Time       // when the current running turn started; zero when not running
	costTracker     task.CostTracker
	turnMetrics     task.TurnMetrics
	liveUsage       agent.Usage
	lastUsage       agent.Usage  // Most recent ResultMessage usage (active context).
	lastAPIUsage    agent.Usage  // Most recent per-API-call usage from AssistantMessage (context window fill).
	cacheExpiresAt  time.Time    // When the prompt cache from the last API call expires.
	liveDiffStat    v3.DiffStat  // Updated by DiffStatMessage from relay.
	lastGitRead     repo.GitRead // Rejects deferred snapshots older than the applied Git state.
	liveRepoStates  []v3.RepoState
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
	t.SetState(task.StatePending)
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
func (t *Task) SetRuntimeConnectionInfo(id runtime.ID, target runtime.ConnectionTarget, tailscaleFQDN, tailscaleAuthURL string, vncPort int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.runtimeInstanceID != id {
		t.lastGitRead = repo.NewGitRead(id)
	}
	t.runtimeInstanceID = id
	t.runtimeConnection = target
	t.TailscaleFQDN = tailscaleFQDN
	t.TailscaleAuthURL = tailscaleAuthURL
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
		MessageType:       "caic_meta",
		Prompt:            t.InitialPrompt.Text,
		Title:             t.Title(),
		Repos:             metaRepos,
		Harness:           string(t.Harness),
		RequestedModel:    t.RequestedModel,
		RequestedEffort:   t.RequestedEffort,
		StartedAt:         t.StartedAt,
		ForgeIssue:        t.ForgeIssue,
		OwnerID:           t.OwnerID,
		ForkedFromTaskID:  t.ForkedFromTaskID.String(),
		ParentTaskID:      parentTaskID,
		CaicMCP:           t.CaicMCP,
		Tailscale:         t.Tailscale,
		USB:               t.USB,
		Display:           t.Display,
		Sudo:              t.Sudo,
		GitHubToken:       t.GitHubTokenEnabled(),
		RuntimeName:       string(t.RuntimeName),
		BaseImage:         t.BaseImage,
		ContainerPlatform: t.ContainerPlatform,
		MaxCPUs:           t.MaxCPUs,
		CacheMounts:       metaCacheMountsFromRuntime(t.CacheMounts),
		Mounts:            metaMountsFromRuntime(t.Mounts),
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

// SetState updates the state under the mutex and records the transition time.
func (t *Task) SetState(s task.State) {
	t.mu.Lock()
	t.setState(s)
	t.mu.Unlock()
}

// SetStateAt updates the state under the mutex with an explicit timestamp.
// Used during import to preserve the original transition time.
func (t *Task) SetStateAt(s task.State, at time.Time) {
	t.mu.Lock()
	if s != task.StateRunning {
		t.turnStartedAt = time.Time{}
	}
	t.recordStateTransition(s, at)
	t.state = s
	t.stateUpdatedAt = at
	t.mu.Unlock()
}

// SetTurnStartedAt sets the turn start time if the task is currently running.
// Called during import to estimate when the current mid-turn started.
func (t *Task) SetTurnStartedAt(at time.Time) {
	t.mu.Lock()
	if t.state == task.StateRunning {
		t.turnStartedAt = at
	}
	t.mu.Unlock()
}

// SetStateIf atomically transitions the state to next only if the current
// state equals expected. Returns true if the transition occurred.
func (t *Task) SetStateIf(expected, next task.State) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != expected {
		return false
	}
	t.setState(next)
	return true
}

// SetStateIfAny atomically transitions the state to next only if the current
// state is one of allowed. Returns the previous state and whether the
// transition occurred.
func (t *Task) SetStateIfAny(next task.State, allowed ...task.State) (prev task.State, changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev = t.state
	if !slices.Contains(allowed, t.state) {
		return prev, false
	}
	t.setState(next)
	return prev, true
}

// SetStateUnless atomically transitions the state to next unless the current
// state is one of excluded, in which case it is left unchanged. Returns the
// previous state and whether the transition occurred. Performing the guard and
// the transition under a single lock closes the check-then-set race that a
// separate GetState/SetState pair leaves open against concurrent transitions.
func (t *Task) SetStateUnless(next task.State, excluded ...task.State) (prev task.State, changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev = t.state
	if slices.Contains(excluded, t.state) {
		return prev, false
	}
	t.setState(next)
	return prev, true
}

// GetState returns the current state under the mutex.
func (t *Task) GetState() task.State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
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
	return InvalidationState{State: t.state, Sequence: t.stateSeq, Title: t.title, RateLimit: t.rateLimit, CIStatus: t.ciStatus, ForgePR: t.forgePR}
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
	TailscaleAuthURL   string
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
func (t *Task) PendingUserActions() []v3.PendingUserAction {
	t.mu.Lock()
	defer t.mu.Unlock()
	return pendingUserActionsFromMessages(t.timelineViewLocked())
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

// SeedTimelineParts fills an empty task with the message history from previously
// saved logs, then derives session metadata, plan state, live stats and task
// state from it.
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
// State inference rules (applied only for non-terminal states):
//   - Current turn has unanswered AskUserQuestion → StateAsking
//   - Trailing ResultMessage and active detached work → StateRunning
//   - Trailing ResultMessage (no ask, no detached work) → StateWaiting
//   - No trailing ResultMessage → state unchanged (agent was mid-output)
//
// Metadata-only messages (DiffStatMessage, TurnCommitSnapshotMessage, PendingUserActionMessage,
// NativeSubagentMessage, BackgroundCommandMessage, RawMessage) after the ResultMessage are skipped during inference. For
// import, the caller must handle the case where state remains StateRunning
// with no relay alive.
// The owned durable prefix and live suffix remain logically contiguous without
// concatenation. Both slices transfer to the task.
func (t *Task) SeedTimelineParts(prefix, suffix []agent.TimedMessage) {
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
	// One forward pass: the task starts empty, so every field below is derived
	// from msgs alone and the per-concern handlers touch disjoint fields.
	// Later entries (model_rerouted) override earlier ones. The fold only reads
	// msgs; the current plan is projected through the task snapshot.
	//
	// Plan state: a context_cleared marker makes previous plan data stale
	// and suppresses plan tracking until the next ResultMessage. planExitID
	// holds the ToolUseID of the ExitPlanMode that
	// carries the current plan; a new ExitPlanMode or context_cleared
	// supersedes it so only the latest plan is visible.
	//
	// Live stats: Claude's TotalCostUSD is cumulative until context_cleared.
	// Completed root metrics preserve native values or measure producer time.
	// Token usage is always summed.
	for _, entry := range view.Forward() {
		msg := entry.Message
		t.turnMetrics.Observe(msg, entry.ProducerTime)
		t.exitStatus.Observe(msg)
		if msg.Provenance().IsChild() {
			switch m := msg.(type) {
			case *agent.NativeSubagentMessage:
				t.nativeSubagents.Apply(&m.Subagent)
			case *agent.BackgroundCommandMessage:
				t.backgroundCommands.Apply(&m.Command)
			case *agent.RateLimitMessage:
				if !m.IsChildThreadGoal() {
					t.recordRateLimitLocked(m)
				}
			}
			t.costTracker.Observe(msg, entry.ProducerTime, t.Pricer)
			t.observeRollupLocked(msg, entry.ProducerTime, entry.ProducerTime, true)
			continue
		}

		t.observeContextLocked(msg, entry.ProducerTime, t.sessionMetadata.SessionID)
		t.sessionMetadata.Apply(msg)
		t.observeUsageLocked(msg)
		if _, ok := msg.(*agent.ExitMessage); ok {
			t.nativeSubagents.HostExited()
			t.backgroundCommands.HostExited()
			t.costTracker.Observe(msg, entry.ProducerTime, t.Pricer)
			continue
		}
		switch m := msg.(type) {
		case *agent.SystemMessage:
			switch m.Subtype {
			case "context_cleared", "compact_boundary":
				if m.Subtype == "context_cleared" {
					t.clearContextLocked()
				}
			}
		case *agent.RateLimitMessage:
			t.recordRateLimitLocked(m)
		case *agent.NativeSubagentMessage:
			t.nativeSubagents.Apply(&m.Subagent)
		case *agent.BackgroundCommandMessage:
			t.backgroundCommands.Apply(&m.Command)
		case *agent.ToolUseMessage:
			t.trackToolUse(m)
		case *agent.DiffStatMessage:
			if len(m.DiffStat) > 0 {
				t.diffCreated = true
			}
		case *agent.ResultMessage:
			t.completedRootTurns++
			t.planDismissed = false
			if m.ContextWindow > 0 {
				t.reportedContextWindow = m.ContextWindow
			}
			if len(m.DiffStat) > 0 {
				t.diffCreated = true
			}
			t.lastUsage = m.Usage
		}
		t.costTracker.Observe(msg, entry.ProducerTime, t.Pricer)
		// Forward the folded message to the usage rollup with the cost
		// snapshot reflecting every prior entry, so resumed replays carry the
		// correct cost delta for the unflushed tail.
		t.observeRollupLocked(msg, entry.ProducerTime, entry.ProducerTime, true)
	}
	// Restore live diff stat from the last DiffStatMessage or ResultMessage,
	// whichever appears later. ResultMessage carries the authoritative
	// host-side diff stat but a DiffStatMessage from the relay may follow it.
	// Repo states restore separately: only the backend's post-tool probe fills
	// them, and a newer watcher-only DiffStatMessage must not hide it.
	for _, entry := range view.Backward() {
		if ds, ok := entry.Message.(*agent.DiffStatMessage); ok {
			t.liveDiffStat = ds.DiffStat
			break
		}
		if rm, ok := entry.Message.(*agent.ResultMessage); ok && len(rm.DiffStat) > 0 {
			t.liveDiffStat = rm.DiffStat
			break
		}
	}
	for _, entry := range view.Backward() {
		if ds, ok := entry.Message.(*agent.DiffStatMessage); ok {
			if len(ds.Repos) > 0 {
				t.liveRepoStates = ds.Repos
				break
			}
		}
	}
	// Infer state: if the last agent-emitted message is a ResultMessage, the
	// agent finished its turn and is waiting for user input (or asking a
	// question). A harness-native subagent still running keeps the task Running,
	// because a background delegation ends the parent turn before the child
	// settles. Skip trailing metadata — the relay emits periodic diff stats and
	// native child updates that can appear after the ResultMessage.
	// Only override non-terminal states — purged/crashed/failed tasks loaded
	// from logs must keep their recorded state.
	if view.Len() > 0 && t.state != task.StatePurged && t.state != task.StateCrashed && t.state != task.StateFailed && t.state != task.StatePurging {
		var next task.State
		switch {
		case lastAgentMessage(view) != nil:
			next = t.settledTurnStateLocked()
		case lastTurnHasUnansweredAsk(view):
			next = task.StateAsking
		}
		if next != "" {
			// Producer timestamps keep the adopted task's state duration and turn
			// start equal to the live values the restart replaced.
			if at := seedStateTime(view, next); !at.IsZero() {
				t.seedStateAtLocked(next, at)
			} else {
				t.setState(next)
			}
		} else if t.state == task.StateRunning || t.state == task.StateStarting {
			// The log ends mid-turn. Record the turn start because import set a
			// provisional Running state without a turn timestamp.
			if at := turnStartTime(view); !at.IsZero() {
				t.seedStateAtLocked(task.StateRunning, at)
			}
		}
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
		if final.Source != (agent.MessageSource{}) {
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
		if final.Source != (agent.MessageSource{}) {
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

// Subscribe returns a snapshot of all past messages plus a live channel for
// new messages. The caller must call unsubFn when done to release resources.
func (t *Task) Subscribe(ctx context.Context) (history []TimelineMessage, live <-chan TimelineMessage, unsubFn func()) {
	s := &sub{ch: make(chan TimelineMessage, 256)}

	t.mu.Lock()
	// Snapshot history under lock — no channel writes, so no deadlock risk
	// regardless of history size.
	view := t.timelineViewLocked()
	history = make([]TimelineMessage, view.Len())
	for i, entry := range view.Forward() {
		history[i] = TimelineMessage{Message: entry.Message, Sequence: uint64(i + 1), ObservedAt: entry.ProducerTime} //nolint:gosec // A timeline cannot approach uint64 capacity.
	}
	t.subs = append(t.subs, s)
	t.mu.Unlock()

	return history, s.ch, unsubscribeMessages(t, ctx, s)
}

// SubscribeSnapshot returns a zero-copy view of past messages plus a live
// channel for messages appended after the snapshot. The view remains stable
// until history.Release or unsubFn is called. The caller must call one of them;
// context cancellation only unsubscribes the live channel and does not release
// the snapshot lease.
func (t *Task) SubscribeSnapshot(ctx context.Context) (history TimelineSnapshot, live <-chan TimelineMessage, unsubFn func()) {
	s := &sub{ch: make(chan TimelineMessage, 256)}
	t.mu.Lock()
	history.entries = t.acquireTimelineReaderLocked()
	history.lease = &timelineSnapshotLease{release: t.releaseTimelineReader}
	t.subs = append(t.subs, s)
	t.mu.Unlock()
	unsubMessages := unsubscribeMessages(t, ctx, s)
	return history, s.ch, func() {
		history.Release()
		unsubMessages()
	}
}

// SubscribeLiveMessages returns the current timeline position and sequenced
// messages produced after subscription. It avoids copying retained task history
// for consumers with another authoritative history source.
func (t *Task) SubscribeLiveMessages(ctx context.Context) (after uint64, live <-chan TimelineMessage, unsubFn func()) {
	s := &sub{ch: make(chan TimelineMessage, 256)}
	t.mu.Lock()
	after = uint64(t.timelineLenLocked()) //nolint:gosec // A timeline cannot approach uint64 capacity.
	t.subs = append(t.subs, s)
	t.mu.Unlock()
	return after, s.ch, unsubscribeMessages(t, ctx, s)
}

func unsubscribeMessages(t *Task, ctx context.Context, s *sub) func() {
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			t.mu.Lock()
			for i, ss := range t.subs {
				if ss == s {
					t.subs = append(t.subs[:i], t.subs[i+1:]...)
					break
				}
			}
			t.mu.Unlock()
			s.close()
		})
	}
	stop := context.AfterFunc(ctx, cleanup)
	return func() {
		stop()
		cleanup()
	}
}

// SubscribeRateLimits returns historical quota messages and a lossless stream
// of future quota messages. The caller must call unsubFn at most once when
// done; context cancellation automatically unsubscribes.
func (t *Task) SubscribeRateLimits(ctx context.Context) (history []*agent.RateLimitMessage, live <-chan *agent.RateLimitMessage, unsubFn func()) {
	s := &rateLimitSub{ctx: ctx, ch: make(chan *agent.RateLimitMessage, 16)}

	t.mu.Lock()
	view := t.timelineViewLocked()
	for _, entry := range view.Forward() {
		if rateLimit, ok := entry.Message.(*agent.RateLimitMessage); ok && !rateLimit.IsChildThreadGoal() {
			history = append(history, rateLimit)
		}
	}
	t.rateLimitSubs = append(t.rateLimitSubs, s)
	t.mu.Unlock()

	unsub := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		for i, ss := range t.rateLimitSubs {
			if ss == s {
				t.rateLimitSubs = append(t.rateLimitSubs[:i], t.rateLimitSubs[i+1:]...)
				close(s.ch)
				return
			}
		}
		panic("rate limit subscriber already unsubscribed")
	}
	stop := context.AfterFunc(ctx, unsub)
	return history, s.ch, func() {
		if !stop() {
			panic("rate limit subscriber already unsubscribed")
		}
		unsub()
	}
}

// PushStats records a runtime stats snapshot and notifies live subscribers.
func (t *Task) PushStats(s *runtime.Stats) {
	t.mu.Lock()
	val := *s
	if val.DiskUsed >= 0 {
		t.diskUsed = val.DiskUsed
		t.diskKnown = true
	} else if t.diskKnown {
		val.DiskUsed = t.diskUsed
	}
	idx := (t.statsHead + t.statsLen) % statsRingSize
	t.statsRing[idx] = val
	if t.statsLen < statsRingSize {
		t.statsLen++
	} else {
		t.statsHead = (t.statsHead + 1) % statsRingSize
	}
	for _, sub := range t.statsSubs {
		select {
		case sub.ch <- val:
		default:
		}
	}
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
	idx := (t.statsHead + t.statsLen - 1) % statsRingSize
	t.statsRing[idx].DiskUsed = bytes
	val := t.statsRing[idx]
	for _, sub := range t.statsSubs {
		select {
		case sub.ch <- val:
		default:
		}
	}
	t.mu.Unlock()
}

// SubscribeStats returns a snapshot of the stats ring buffer and a channel that
// receives only live stats arriving after the snapshot. Cancellation or
// unsubscribing closes the channel and removes the subscriber.
func (t *Task) SubscribeStats(ctx context.Context) (history []runtime.Stats, live <-chan runtime.Stats, unsubFn func()) {
	s := &statsSub{ch: make(chan runtime.Stats, 64)}
	t.mu.Lock()
	history = make([]runtime.Stats, t.statsLen)
	for i := range t.statsLen {
		history[i] = t.statsRing[(t.statsHead+i)%statsRingSize]
	}
	t.statsSubs = append(t.statsSubs, s)
	t.mu.Unlock()
	return history, s.ch, unsubscribeStats(t, ctx, s)
}

func unsubscribeStats(t *Task, ctx context.Context, s *statsSub) func() {
	unsub := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		for i, ss := range t.statsSubs {
			if ss == s {
				t.statsSubs = append(t.statsSubs[:i], t.statsSubs[i+1:]...)
				break
			}
		}
		s.close()
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
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
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
	state := t.state
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
	_ = ctx
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
	state := t.state
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

// MarkSessionCrashed records operational health when a recoverable session ends.
// The execution owner separately reports its error through the lifecycle result.
func (t *Task) MarkSessionCrashed() bool {
	if _, changed := t.SetStateIfAny(task.StateCrashed, task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan); !changed {
		return false
	}

	return true
}

// MarkSessionFailed records operational health when startup transport fails.
// The execution owner separately reports its error through the lifecycle result.
func (t *Task) MarkSessionFailed() bool {
	if _, changed := t.SetStateIfAny(task.StateFailed, task.StateStarting); !changed {
		return false
	}

	return true
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
// is applied. The caller holds t.mu.
func (t *Task) clearContextLocked() {
	t.inPlanMode = false
	t.planFile = ""
	t.planContent = ""
	t.planDismissed = true
	t.planExitID = ""
}

// snapshotLocked builds a Snapshot. Callers hold t.mu.
func (t *Task) snapshotLocked() Snapshot {
	var sample *runtime.Stats
	if t.statsLen > 0 {
		sample = new(t.statsRing[(t.statsHead+t.statsLen-1)%statsRingSize])
	}
	return Snapshot{
		SessionMetadata:    t.sessionMetadata,
		State:              t.state,
		StateUpdatedAt:     t.stateUpdatedAt,
		TurnStartedAt:      t.turnStartedAt,
		Repos:              append([]repo.Mount(nil), t.Repos...),
		RuntimeName:        t.RuntimeName,
		RuntimeInstanceID:  t.runtimeInstanceID,
		Tailscale:          t.Tailscale,
		TailscaleFQDN:      t.TailscaleFQDN,
		TailscaleAuthURL:   t.TailscaleAuthURL,
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
		ContextUsed:        t.contextUsed,
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
		LastAPIUsage:       t.lastAPIUsage.Clone(),
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
	}
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
	switch m := msg.(type) {
	case *agent.InitMessage:
		if m.SessionID == priorSessionID {
			return false
		}
		t.contextUsed = nil
	case *agent.UsageMessage:
		// Native occupancy alone does not replace the billed API-call snapshot.
		if m.ContextUsed == nil || !m.Usage.Equal(agent.Usage{}) || m.PerCall {
			t.lastAPIUsage = m.Usage
			t.cacheExpiresAt = time.Time{}
			if m.Usage.CacheTTLSeconds > 0 {
				t.cacheExpiresAt = at.Add(time.Duration(m.Usage.CacheTTLSeconds) * time.Second)
			}
		}
		if m.ContextUsed != nil {
			t.contextUsed = m.ContextUsed
		}
		if m.ContextWindow > 0 {
			t.reportedContextWindow = m.ContextWindow
		}
	case *agent.SystemMessage:
		if m.Subtype != agent.SystemSubtypeCompactBoundary && m.Subtype != "context_cleared" {
			return false
		}
		t.contextUsed = nil
		if m.ContextTokensAfter > 0 {
			t.lastAPIUsage = agent.Usage{InputTokens: int(m.ContextTokensAfter)}
		}
	default:
		return false
	}
	return true
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

// setState updates the state and records the transition time. The caller must
// hold t.mu when called from a locked context, or ensure exclusive access.
func (t *Task) setState(s task.State) {
	t.setStateAtLocked(s, time.Now().UTC())
}

// setStateAtLocked updates the state at an explicit transition time. Entering
// Running records the turn start; leaving it clears the turn start. Callers
// hold t.mu.
func (t *Task) setStateAtLocked(s task.State, at time.Time) {
	if s == task.StateRunning && t.state != task.StateRunning {
		t.turnStartedAt = at
	} else if s != task.StateRunning {
		t.turnStartedAt = time.Time{}
	}
	t.recordStateTransition(s, at)
	t.state = s
	t.stateUpdatedAt = at
}

// seedStateAtLocked applies a timeline-inferred state at its recorded
// transition time. Seeding starts from a provisional Running state, so a
// Running result always records the derived turn start. Callers hold t.mu.
func (t *Task) seedStateAtLocked(s task.State, at time.Time) {
	if s == task.StateRunning {
		t.turnStartedAt = at
	} else {
		t.turnStartedAt = time.Time{}
	}
	t.recordStateTransition(s, at)
	t.state = s
	t.stateUpdatedAt = at
}

// recordStateTransition appends a state change to the bounded transition
// journal. Callers hold t.mu.
func (t *Task) recordStateTransition(s task.State, at time.Time) {
	if s == t.state {
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
// still owes input, running while a detached subagent or shell command is
// active, and waiting otherwise. A harness can end the parent turn while
// background work continues, so a running background card keeps the task busy
// after the parent stops producing output; a foreground card that
// is still running then is stale evidence and falls back to waiting.
//
// The caller must hold t.mu.
func (t *Task) settledTurnStateLocked() task.State {
	switch {
	case lastTurnHasUnansweredAsk(t.timelineViewLocked()):
		return task.StateAsking
	case lastTurnHasExitPlan(t.timelineViewLocked()) && t.planContent != "":
		return task.StateHasPlan
	case t.nativeSubagents.ActiveBackground() > 0 || t.backgroundCommands.Active() > 0:
		return task.StateRunning
	default:
		return task.StateWaiting
	}
}

// addParsedMessage folds an accepted semantic message into task state and
// the subscriber timeline.
func (t *Task) addParsedMessage(parsed agent.TimedMessage) (stateChanged bool) {
	m := parsed.Message
	child := m.Provenance().IsChild()
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
	case *agent.MetaMessage, *agent.MetaResultMessage:
		t.mu.Unlock()
		return false
	}
	initialState := t.state
	initialPlanMode, initialPlanID, initialPlanContent := t.inPlanMode, t.planExitID, t.planContent
	// A compaction boundary rewrites the summary's live context fill, so the
	// task list must refresh even though the task state is unchanged.
	summaryChanged := false
	defer func() {
		stateChanged = summaryChanged || t.state != initialState || t.inPlanMode != initialPlanMode || t.planExitID != initialPlanID || t.planContent != initialPlanContent
		t.mu.Unlock()
	}()
	priorMetadata := t.sessionMetadata
	t.sessionMetadata.Apply(m)
	if agent.IsSessionMetadataUpdate(m) {
		summaryChanged = t.sessionMetadata != priorMetadata
		t.costTracker.Observe(m, parsed.ProducerTime, t.Pricer)
		return stateChanged
	}
	producerAt := parsed.ProducerTime
	at := producerAt
	if at.IsZero() {
		at = time.Now()
	}
	parsed.ProducerTime = at
	t.timeline = append(t.timeline, parsed)
	t.turnMetrics.Observe(m, producerAt)
	visible := t.exitStatus.Observe(m)
	if sm, ok := m.(*agent.SystemMessage); ok && !child {
		switch sm.Subtype {
		case agent.SystemSubtypeInputDeliveryStarted:
			t.inputPriorState = t.state
		case agent.SystemSubtypeInputDeliveryCompleted:
			t.inputPriorState = ""
		case agent.SystemSubtypeInputDeliveryFailed:
			if t.inputPriorState != "" && t.state == task.StateRunning {
				t.setState(t.inputPriorState)
			}
			t.inputPriorState = ""
		}
	} else if task.AcceptedInputOutput(m) {
		t.inputPriorState = ""
	}

	if agent.IsInputDeliveryControl(m) {
		return stateChanged
	}
	if isDeltaFinal(m) {
		if t.timelineReaders == 0 {
			view := t.timelineViewLocked()
			compactFinalizedDeltaAt(view, view.Len()-1)
		} else {
			t.timelineCompactPending = append(t.timelineCompactPending, t.timelineLenLocked()-1)
		}
	}
	if rateLimit, ok := m.(*agent.RateLimitMessage); ok && !rateLimit.IsChildThreadGoal() {
		t.recordRateLimitLocked(rateLimit)
		for _, sub := range t.rateLimitSubs {
			select {
			case sub.ch <- rateLimit:
			case <-sub.ctx.Done():
			}
		}
	}

	// Track plan mode and plan file from tool_use events.
	if tu, ok := m.(*agent.ToolUseMessage); ok && !child {
		t.trackToolUse(tu)
	}
	if !child {
		summaryChanged = t.observeContextLocked(m, at, priorMetadata.SessionID) || summaryChanged
	}
	// Transition to running when the agent starts producing output.
	// Handles three cases:
	//   - Normal turn: awaiting user input (Waiting/Asking/HasPlan).
	//   - Server restart: history restoration inferred a waiting state,
	//     but the relay already started a new turn before reattach.
	//   - First turn: the agent may produce output before Checkout.Start
	//     sets StateRunning (race between backend subprocess and
	//     SetState on the main goroutine).
	if !child {
		switch m.(type) {
		case *agent.AskMessage:
			if t.state == task.StateStarting || t.state == task.StateRunning || t.state == task.StateWaiting || t.state == task.StateHasPlan {
				t.setState(task.StateAsking)
			}
		case *agent.UserInputMessage:
			if t.state == task.StateStarting || t.state == task.StateWaiting || t.state == task.StateAsking || t.state == task.StateHasPlan {
				t.setState(task.StateRunning)
			}
		case *agent.TextMessage, *agent.TextDeltaMessage,
			*agent.ThinkingMessage, *agent.ThinkingDeltaMessage,
			*agent.ToolUseMessage, *agent.ToolOutputDeltaMessage, *agent.TodoMessage,
			*agent.WidgetMessage, *agent.WidgetDeltaMessage:
			if t.state == task.StateStarting || t.state == task.StateWaiting || t.state == task.StateAsking || t.state == task.StateHasPlan {
				if t.state == task.StateAsking && lastTurnHasUnansweredAsk(t.timelineViewLocked()) {
					break
				}
				t.setState(task.StateRunning)
			}
		}
	}

	// Fold harness-native child activity into the canonical card set and let it
	// settle the parent state. A harness can end the parent turn when it
	// delegates in the background, so a trailing ResultMessage is not enough
	// evidence that the task is idle: a detached child keeps it running, and the
	// last detached child settling returns it to waiting.
	if ns, ok := m.(*agent.NativeSubagentMessage); ok {
		t.nativeSubagents.Apply(&ns.Subagent)
	}
	// The root harness process hosts its native subagents, so its exit ends
	// their observable lifecycle and they no longer keep the task running.
	if _, ok := m.(*agent.ExitMessage); ok && !child {
		t.nativeSubagents.HostExited()
		t.backgroundCommands.HostExited()
	}
	// Detached shell checks keep the task busy after the agent ends its turn.
	if bc, ok := m.(*agent.BackgroundCommandMessage); ok {
		t.backgroundCommands.Apply(&bc.Command)
	}
	switch m.(type) {
	case *agent.NativeSubagentMessage, *agent.BackgroundCommandMessage, *agent.ExitMessage:
		if (t.state == task.StateWaiting || t.state == task.StateRunning) && lastAgentMessage(t.timelineViewLocked()) != nil {
			if next := t.settledTurnStateLocked(); next != t.state {
				t.setState(next)
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
	if sm, ok := m.(*agent.SystemMessage); ok && !child && (sm.Subtype == "compact_boundary" || sm.Subtype == "context_cleared") {
		if sm.Subtype == "context_cleared" {
			t.clearContextLocked()
		}
	}
	// Transition to waiting/asking when a result arrives.
	if rm, ok := m.(*agent.ResultMessage); ok && !child {
		t.completedRootTurns++
		if len(rm.DiffStat) > 0 {
			t.setLiveDiffStatLocked(rm.DiffStat)
		}
		t.lastUsage = rm.Usage
		if rm.ContextWindow > 0 {
			t.reportedContextWindow = rm.ContextWindow
		}
		t.planDismissed = false
		// Settle the parent turn. Also handle Running/Waiting because
		// watchSession may have already set Waiting before the dispatch
		// goroutine processed this ResultMessage (it does a blocking Fetch
		// first). In that case we still need to distinguish Asking/HasPlan and
		// to keep Running while a native subagent is active.
		// StateStarting is also handled: the agent subprocess may
		// produce a result before Checkout.Start calls SetState(Running).
		if t.state == task.StateRunning || t.state == task.StateStarting || t.state == task.StateWaiting || t.state == task.StateAsking {
			next := t.settledTurnStateLocked()
			t.setState(next)
			if next == task.StateRunning {
				// Duration now includes the settled turn, so the live delta starts
				// at its result rather than at the turn that just ended.
				t.turnStartedAt = at
			}
		}
	}
	if !child {
		t.observeUsageLocked(m)
	}
	t.costTracker.Observe(m, at, t.Pricer)
	// Forward the folded message to the usage rollup with the cost snapshot
	// reflecting every prior fold in this message.
	t.observeRollupLocked(m, at, producerAt, false)
	// Fan out to subscribers (non-blocking). Skip a non-zero exit message that
	// follows a cleanly completed turn: it is a spurious termination artifact
	// (e.g. SIGINT from a user-requested stop) and is already dropped from the
	// persisted replay, so the live stream must match to avoid a transient
	// "Agent exited" error that disappears when the task log is reloaded.
	if !visible {
		return stateChanged
	}
	t.broadcastTimelineMessageLocked(m, at)
	return stateChanged
}

func (t *Task) broadcastTimelineMessageLocked(m agent.Message, at time.Time) {
	event := TimelineMessage{Message: m, Sequence: uint64(t.timelineLenLocked()), ObservedAt: at} //nolint:gosec // A timeline cannot approach uint64 capacity.
	for i := 0; i < len(t.subs); i++ {
		select {
		case t.subs[i].ch <- event:
		default:
			// Slow subscriber — drop and remove.
			t.subs[i].close()
			t.subs = append(t.subs[:i], t.subs[i+1:]...)
			i--
		}
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
		SessionMetadata:   snapshot.SessionMetadata,
		TaskID:            t.ID.String(),
		Prompt:            t.InitialPrompt.Text,
		Title:             snapshot.Title,
		Repos:             snapshot.Repos,
		Harness:           t.Harness,
		StartedAt:         t.StartedAt,
		LastActivityAt:    snapshot.StateUpdatedAt,
		State:             res.State,
		ForgeIssue:        snapshot.ForgeIssue,
		OwnerID:           t.OwnerID,
		ForkedFromTaskID:  t.ForkedFromTaskID.String(),
		ParentTaskID:      parentTaskID,
		CaicMCP:           t.CaicMCP,
		ForgeOwner:        snapshot.ForgeOwner,
		ForgeRepo:         snapshot.ForgeRepo,
		ForgePR:           snapshot.ForgePR,
		Tailscale:         snapshot.Tailscale,
		USB:               snapshot.USB,
		Display:           snapshot.Display,
		Sudo:              snapshot.Sudo,
		GitHubToken:       snapshot.GitHubToken,
		RuntimeName:       snapshot.RuntimeName,
		BaseImage:         t.BaseImage,
		ContainerPlatform: t.ContainerPlatform,
		MaxCPUs:           t.MaxCPUs,
		CacheMounts:       slices.Clone(t.CacheMounts),
		Mounts:            slices.Clone(t.Mounts),
		RequestedModel:    t.RequestedModel,
		RequestedEffort:   t.RequestedEffort,
		DiffCreated:       t.DiffCreated(),
		LastTrailer:       res,
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
	e, ok := rollupEvent(m, at, producerAt, replayed, model, effort, t.costTracker.Snapshot())
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

// TimelineMessage is an immutable task message and its stable, one-based
// position in the task timeline.
type TimelineMessage struct {
	Message    agent.Message
	Sequence   uint64
	ObservedAt time.Time
}

// TimelineSnapshot is a stable view of a task timeline at subscription time.
// It retains the underlying entries without copying them until Release or the
// subscription's unsubscribe function is called.
type TimelineSnapshot struct {
	entries timelineEntries
	lease   *timelineSnapshotLease
}

// Len returns the number of messages retained by the snapshot.
func (s TimelineSnapshot) Len() int { return s.entries.Len() }

// At returns the indexed message with its stable one-based sequence number.
func (s TimelineSnapshot) At(i int) TimelineMessage {
	entry := s.entries.At(i)
	return TimelineMessage{Message: entry.Message, Sequence: uint64(i + 1), ObservedAt: entry.ProducerTime} //nolint:gosec // A timeline cannot approach uint64 capacity.
}

// Release allows finalized live deltas retained for this zero-copy snapshot to
// be compacted. It must be called once after replay finishes.
func (s TimelineSnapshot) Release() {
	if s.lease != nil {
		s.lease.once.Do(s.lease.release)
	}
}

type timelineSnapshotLease struct {
	once    sync.Once
	release func()
}

// seedStateTime returns the recorded time the loaded timeline entered state
// next. Producer timestamps survive a restart, so the adopted task reports the
// same state duration and running turn start as the live task did. A running
// state comes from a trailing result that settled the parent turn while a
// background subagent stays active; its duration already includes that turn, so
// the live delta starts at the result.
func seedStateTime(entries timelineEntries, next task.State) time.Time {
	if next == task.StateAsking {
		return lastAskTime(entries)
	}
	return lastResultTime(entries)
}

// lastResultTime returns the producer time of the trailing root result.
func lastResultTime(entries timelineEntries) time.Time {
	for _, entry := range entries.Backward() {
		if entry.Message.Provenance().IsChild() {
			continue
		}
		if _, ok := entry.Message.(*agent.ResultMessage); ok {
			return entry.ProducerTime
		}
	}
	return time.Time{}
}

// lastAskTime returns the producer time of the trailing root question.
func lastAskTime(entries timelineEntries) time.Time {
	for _, entry := range entries.Backward() {
		if entry.Message.Provenance().IsChild() {
			continue
		}
		if _, ok := entry.Message.(*agent.AskMessage); ok {
			return entry.ProducerTime
		}
	}
	return time.Time{}
}

// turnStartTime returns when the turn that is still running began. A trailing
// ResultMessage belongs to that turn when a background subagent outlived it, so
// skip it and measure from the turn before.
func turnStartTime(entries timelineEntries) time.Time {
	skipResult := lastAgentMessage(entries) != nil
	start := time.Time{}
	for _, entry := range entries.Backward() {
		if entry.Message.Provenance().IsChild() || nonSemanticMessage(entry.Message) {
			continue
		}
		if _, ok := entry.Message.(*agent.SystemMessage); ok {
			continue
		}
		if _, ok := entry.Message.(*agent.ResultMessage); ok {
			if skipResult {
				skipResult = false
				continue
			}
			return start
		}
		if !entry.ProducerTime.IsZero() {
			start = entry.ProducerTime
		}
	}
	return start
}

// nonSemanticMessage reports whether m is relay or caic bookkeeping that
// neither settles a turn nor starts one.
func nonSemanticMessage(m agent.Message) bool {
	switch m.(type) {
	case *agent.BackgroundCommandMessage, *agent.DiffStatMessage, *agent.ExitMessage,
		*agent.MetaMessage, *agent.MetaPRMessage, *agent.MetaResultMessage, *agent.MetaSessionMessage,
		*agent.ModelInfoMessage, *agent.NativeSubagentMessage, *agent.PendingUserActionMessage,
		*agent.RawMessage, *agent.RelayGenerationMessage, *agent.StrippedEnvMessage,
		*agent.TextDeltaMessage, *agent.TurnCommitSnapshotMessage, *agent.UsageMessage:
		return true
	default:
		return false
	}
}

// lastAgentMessage scans backwards through msgs, skipping non-semantic
// messages, and returns the trailing ResultMessage if the last semantically
// meaningful message is a result. Returns nil if it is not a ResultMessage
// (agent still producing output) or msgs is empty.
func lastAgentMessage(entries timelineEntries) *agent.ResultMessage {
	var rejected map[*agent.UserInputMessage]struct{}
	checkedInputs := false
	for _, entry := range entries.Backward() {
		if entry.Message.Provenance().IsChild() || nonSemanticMessage(entry.Message) {
			continue
		}
		switch m := entry.Message.(type) {
		case *agent.SystemMessage:
			if m.Subtype == agent.SystemSubtypeInputDeliveryFailed || agent.IsInputDeliveryControl(m) {
				continue
			}
			return nil
		case *agent.ResultMessage:
			return m
		case *agent.UserInputMessage:
			if !checkedInputs {
				rejected = unacceptedInputs(entries, 0)
				checkedInputs = true
			}
			if _, rejected := rejected[m]; rejected {
				continue
			}
			return nil
		default:
			return nil
		}
	}
	return nil
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

// lastTurnHasUnansweredAsk reports whether the current turn contains an
// AskMessage that has not been followed by a successful ToolResultMessage or
// accepted user answer.
// It scans backwards from the end until it hits the previous turn's
// ResultMessage boundary. If the current turn's ResultMessage is present, it is
// skipped as a boundary first.
func lastTurnHasUnansweredAsk(entries timelineEntries) bool {
	skipTrailingResult := lastAgentMessage(entries) != nil
	answered := map[agent.ToolKey]struct{}{}
	var rejected map[*agent.UserInputMessage]struct{}
	checkedInputs := false
	for _, entry := range entries.Backward() {
		if entry.Message.Provenance().IsChild() {
			continue
		}
		switch m := entry.Message.(type) {
		case *agent.UserInputMessage:
			if !checkedInputs {
				rejected = unacceptedInputs(entries, 0)
				checkedInputs = true
			}
			if _, failed := rejected[m]; !failed {
				return false
			}
		case *agent.AskMessage:
			if m.ToolUseID == "" {
				return true
			}
			if _, ok := answered[agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)]; !ok {
				return true
			}
		case *agent.ToolResultMessage:
			if m.ToolUseID != "" && m.Error == "" {
				answered[agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)] = struct{}{}
			}
		case *agent.ResultMessage:
			if skipTrailingResult {
				skipTrailingResult = false
			} else {
				return false
			}
		}
	}
	return false
}

// pendingUserActionsFromMessages derives reconnect state from the current turn.
// Today AskUserQuestion is the only pending action kind; adding a new kind
// should add its close condition here instead of preserving provider-specific
// control messages directly.
func pendingUserActionsFromMessages(entries timelineEntries) []v3.PendingUserAction {
	skipTrailingResult := lastAgentMessage(entries) != nil
	answered := map[agent.ToolKey]struct{}{}
	restored := map[agent.ToolKey]struct{}{}
	pending := map[agent.ToolKey]v3.PendingUserAction{}
	var actions []v3.PendingUserAction
	var rejected map[*agent.UserInputMessage]struct{}
	checkedInputs := false
	for _, entry := range entries.Backward() {
		if entry.Message.Provenance().IsChild() {
			continue
		}
		switch m := entry.Message.(type) {
		case *agent.UserInputMessage:
			if !checkedInputs {
				rejected = unacceptedInputs(entries, 0)
				checkedInputs = true
			}
			if _, failed := rejected[m]; !failed {
				// Accepted answers close earlier Asks even before their tool
				// results arrive. Keep only newer, still-pending questions.
				slices.Reverse(actions)
				return actions
			}
		case *agent.AskMessage:
			if m.ToolUseID == "" {
				continue
			}
			if _, ok := answered[agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)]; ok {
				continue
			}
			if _, ok := restored[agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)]; ok {
				continue
			}
			action, ok := pending[agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)]
			if ok {
				actions = append(actions, agent.ClonePendingUserAction(action))
				restored[agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)] = struct{}{}
				delete(pending, agent.ToolKeyForSource(m.Provenance(), m.ToolUseID))
			}
		case *agent.PendingUserActionMessage:
			switch m.Action.Kind {
			case agent.PendingUserActionAskUserQuestion:
			default:
				continue
			}
			if m.Action.ToolUseID != "" {
				pending[agent.ToolKeyForSource(m.Provenance(), m.Action.ToolUseID)] = m.Action
			}
		case *agent.ToolResultMessage:
			if m.ToolUseID != "" && m.Error == "" {
				answered[agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)] = struct{}{}
			}
		case *agent.ResultMessage:
			if skipTrailingResult {
				skipTrailingResult = false
			} else {
				slices.Reverse(actions)
				return actions
			}
		}
	}
	slices.Reverse(actions)
	return actions
}

// lastTurnHasExitPlan reports whether the current turn contains an ExitPlanMode
// tool call. It scans backwards from the end until it hits a previous turn's
// ResultMessage boundary.
func lastTurnHasExitPlan(entries timelineEntries) bool {
	skippedResult := false
	for _, entry := range entries.Backward() {
		if entry.Message.Provenance().IsChild() {
			continue
		}
		switch m := entry.Message.(type) {
		case *agent.ToolUseMessage:
			if m.Name == "ExitPlanMode" {
				return true
			}
		case *agent.ResultMessage:
			if skippedResult {
				return false
			}
			skippedResult = true
		}
	}
	return false
}

// sub is a message subscriber with a once-guarded close to prevent double-close
// panics when both the fan-out (slow subscriber drop) and context cancellation
// race to close the channel.
type sub struct {
	ch   chan TimelineMessage
	once sync.Once
}

func (s *sub) close() {
	s.once.Do(func() { close(s.ch) })
}

type rateLimitSub struct {
	ctx context.Context
	ch  chan *agent.RateLimitMessage
}

const maxTitleChars = 80

const titleSystemPrompt = "Summarize this coding task conversation in 3-8 words as a short title of at most %d characters, including spaces and punctuation. Keep the current title if it still accurately describes the recent activity. Reply with ONLY the title, no quotes."
