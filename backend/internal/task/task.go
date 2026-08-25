// Package task orchestrates a single coding agent task: branch creation,
// instance lifecycle, agent execution, and git integration.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// CreateRequest describes an automated task creation request for one managed repository.
type CreateRequest struct {
	Repo       string
	Prompt     string
	OwnerID    string
	ForgeIssue int
}

const statsRingSize = 60

type statsSub struct {
	ch   chan runtime.Stats
	once sync.Once
}

func (s *statsSub) close() { s.once.Do(func() { close(s.ch) }) }

// SessionHandle bundles the resources associated with an active agent session:
// the SSH session, the message dispatch channel, and the log writer.
// DispatchDone is closed when the dispatch goroutine exits after MsgCh is closed.
type SessionHandle struct {
	Session      *agent.Session
	MsgCh        chan agent.ParsedMessage
	DispatchDone <-chan struct{}
	Log          agent.LogSink
	closeMsgCh   sync.Once
}

// CloseMsgCh closes MsgCh exactly once. Safe to call concurrently; subsequent
// calls are no-ops. Use this instead of close(h.MsgCh) to prevent double-close
// panics when StopTask and EnsureSession race on the same handle.
func (h *SessionHandle) CloseMsgCh() {
	h.closeMsgCh.Do(func() { close(h.MsgCh) })
}

// Done returns the channel that closes when the session's underlying process
// exits. Exposed so callers outside this file can watch for session death
// without reaching into Session directly.
func (h *SessionHandle) Done() <-chan struct{} {
	return h.Session.Done()
}

// GracefulStop sends the shutdown sentinel and waits for the agent to exit
// (up to timeout). Returns nil on success or the context error on timeout.
//
// The relay consumes the sentinel and sends SIGINT to the subprocess, so the
// agent never produces a ResultMessage in this path — callers should use
// live stats from the Task instead.
//
// On timeout, the caller should kill the instance/SSH, then call Drain to
// unblock the read loop.
func (h *SessionHandle) GracefulStop(ctx context.Context, timeout time.Duration) error {
	stopCtx, stopCancel := context.WithTimeout(ctx, timeout)
	err := h.Session.Stop(stopCtx)
	stopCancel()
	return err
}

// Drain waits for the session read loop to finish (useful after a timeout
// where the instance was killed externally), then closes the message channel
// and waits for the dispatch goroutine to complete. Returns the session's
// exit error, if any.
func (h *SessionHandle) Drain() error {
	err := h.Session.Wait()
	h.CloseMsgCh()
	<-h.DispatchDone
	return err
}

// Task represents a single unit of work.
type Task struct {
	// Immutable fields — set at creation, never modified.
	ID                ksid.ID
	InitialPrompt     agent.Prompt         // Initial prompt text and optional images.
	Harness           harness.Name         // Agent harness ("claude", "codex", etc.).
	Model             string               // User-requested model; passed to agent CLI.
	Effort            string               // Thinking effort; passed to agent CLI. Empty = default.
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
	OwnerID           string               // Internal user ID of the creator; empty in no-auth mode.
	ForgeIssue        int                  // Originating issue number for bot comment callbacks; 0 = none.
	ForkedFromTaskID  ksid.ID              // Parent task ID when created by fork; zero otherwise.
	Provider          genai.Provider

	// Mutable task metadata. These fields are populated at construction, setup, or
	// import. After a task is published in the Manager registry, access them
	// through Task methods so readers and async lifecycle goroutines synchronize.
	Repos            []taskslog.RepoMount // index 0 = primary; empty = no-repo
	TailscaleFQDN    string               // Tailscale FQDN assigned to the instance (empty if not available).
	TailscaleAuthURL string               // Tailscale browser auth URL when no pre-auth key was available.
	RelayOffset      int64                // Bytes received from relay output.jsonl, for reconnect.
	SudoPassword     string               // Random sudo password; empty if sudo is not enabled.
	VNCPort          int                  // VNC WebSocket port inside the instance (0 = no VNC). Set during launch.
	GitHubToken      bool                 // Inject GitHub token into the instance's environment.

	// mu protects mutable task metadata above and all fields below.
	mu                    sync.Mutex
	runtimeInstanceID     runtime.ID
	runtimeConnection     runtime.ConnectionTarget
	statsRing             [statsRingSize]runtime.Stats
	statsLen              int
	statsHead             int
	statsSubs             []*statsSub
	state                 taskslog.State
	stateUpdatedAt        time.Time // UTC timestamp of the last state transition.
	sessionID             string    // Agent session ID, captured from InitMessage.
	reportedModel         string    // Model reported by InitMessage (may differ from Model).
	agentVersion          string    // Agent version, captured from InitMessage.
	reportedContextWindow int       // Context window size reported by the agent (0 = unknown).
	planFile              string    // Path to plan file inside instance, captured from Write tool_use.
	planContent           string    // Content of the plan file, captured from Write tool_use input.
	planExitID            string    // ToolUseID of the ExitPlanMode carrying planContent; "" after context_cleared.
	planDismissed         bool      // True after ClearMessages; suppresses plan tracking until the next ResultMessage.
	inPlanMode            bool      // True while the agent is in plan mode (between EnterPlanMode and ExitPlanMode).
	title                 string    // LLM-generated short title; set via SetTitle.
	msgs                  []agent.Message
	subs                  []*sub          // active SSE subscribers
	rateLimitSubs         []*rateLimitSub // active lossless quota subscribers
	handle                *SessionHandle  // current active session; nil when no session is attached
	priorCostUSD          float64         // accumulated cost from all cleared sessions
	priorNumTurns         int             // accumulated turns from all cleared sessions
	priorDuration         time.Duration   // accumulated duration from all cleared sessions
	turnStartedAt         time.Time       // when the current running turn started; zero when not running
	liveCostUSD           float64
	liveNumTurns          int
	liveDuration          time.Duration
	liveUsage             agent.Usage
	lastUsage             agent.Usage    // Most recent ResultMessage usage (active context).
	lastAPIUsage          agent.Usage    // Most recent per-API-call usage from AssistantMessage (context window fill).
	cacheExpiresAt        time.Time      // When the prompt cache from the last API call expires.
	liveDiffStat          agent.DiffStat // Updated by DiffStatMessage from relay.
	diffCreated           bool           // True after any non-empty diff was reported for the task.
	lastExitError         string         // Most recent non-zero relay exit diagnostic.
	forgeOwner            string
	forgeRepo             string
	forgePR               int
	forgePRState          forge.PRState // "open", "closed", "merged"; empty when no PR.
	ciStatus              forge.CIStatus
	ciChecks              []forge.Check
	rateLimit             RateLimit                    // Active task quota block resolved across provider windows.
	rateLimits            map[quotaWindowKey]RateLimit // Latest quota status for each provider window.
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
		InitialPrompt:     prompt,
		Harness:           h,
		Model:             model,
		Effort:            effort,
		BaseImage:         baseImage,
		ContainerPlatform: containerPlatform,
		StartedAt:         time.Now().UTC(),
	}
	t.SetState(taskslog.StatePending)
	if title == "" {
		title = prompt.Text
	}
	t.SetTitle(title)
	return t, nil
}

// syntheticUserInput builds the UserInputMessage recorded in the task log for
// a prompt that wasn't itself parsed from agent output.
func syntheticUserInput(p agent.Prompt) *agent.UserInputMessage {
	var images []agent.ImageData
	if len(p.Images) > 0 {
		images = make([]agent.ImageData, len(p.Images))
		copy(images, p.Images)
	}
	return &agent.UserInputMessage{
		Text:   p.Text,
		Images: images,
	}
}

// lastAgentMessage scans backwards through msgs, skipping non-semantic
// messages (DiffStatMessage, ExitMessage, PendingUserActionMessage,
// TextDeltaMessage, RawMessage), and returns the trailing ResultMessage if the
// last semantically meaningful message is a result. Returns nil if it is not a
// ResultMessage (agent still producing output) or msgs is empty.
func lastAgentMessage(msgs []agent.Message) *agent.ResultMessage {
	for _, msg := range slices.Backward(msgs) {
		switch m := msg.(type) {
		case *agent.DiffStatMessage:
			continue // Relay metadata; skip.
		case *agent.ExitMessage:
			continue // Relay metadata; skip.
		case *agent.PendingUserActionMessage:
			continue // Reconnect metadata; skip.
		case *agent.TextDeltaMessage:
			continue // Streaming delta; skip.
		case *agent.RawMessage:
			continue // tool_progress, etc.; skip.
		case *agent.UsageMessage:
			continue // Token usage metadata; skip.
		case *agent.ResultMessage:
			return m
		default:
			return nil
		}
	}
	return nil
}

// ResultTextWindow accumulates the visible assistant text of the current
// turn so a streaming consumer can produce the fallback result text at the
// point a ResultMessage arrives. Any other message type (a boundary, see
// fallbackBoundary) resets the window; finalized TextMessages accumulate
// with consecutive duplicates collapsed, and TextDeltaMessages count only
// until the first finalized text.
type ResultTextWindow struct {
	texts []string
	delta strings.Builder
}

// Update folds one message into the window.
func (w *ResultTextWindow) Update(msg agent.Message) {
	if fallbackBoundary(msg) {
		w.reset()
		return
	}
	switch m := msg.(type) {
	case *agent.TextMessage:
		text := strings.TrimSpace(m.Text)
		if text == "" {
			return
		}
		if len(w.texts) == 0 || w.texts[len(w.texts)-1] != text {
			w.texts = append(w.texts, text)
		}
	case *agent.TextDeltaMessage:
		if len(w.texts) == 0 {
			w.delta.WriteString(m.Text)
		}
	}
}

// Value returns the accumulated fallback text: finalized messages joined by
// blank lines, or the trimmed delta stream when none arrived.
func (w *ResultTextWindow) Value() string {
	if len(w.texts) > 0 {
		return strings.Join(w.texts, "\n\n")
	}
	return strings.TrimSpace(w.delta.String())
}

func (w *ResultTextWindow) reset() {
	w.texts = nil
	w.delta.Reset()
}

// fallbackResultText returns the visible assistant text of the turn preceding
// the trailing ResultMessage in msgs, using the same window rules as
// ResultTextWindow. The input may include the trailing ResultMessage.
func fallbackResultText(msgs []agent.Message) string {
	end := len(msgs)
	if end > 0 {
		if _, ok := msgs[end-1].(*agent.ResultMessage); ok {
			end--
		}
	}
	var w ResultTextWindow
	for _, msg := range msgs[:end] {
		w.Update(msg)
	}
	return w.Value()
}

func fallbackBoundary(msg agent.Message) bool {
	switch msg.(type) {
	case *agent.ResultMessage, *agent.ToolUseMessage, *agent.ToolResultMessage,
		*agent.ThinkingMessage, *agent.ThinkingDeltaMessage:
		return true
	default:
		return false
	}
}

// ClearsExitError reports whether a message clears the last exit error from a
// prior turn. Messages that accompany a turn without starting a new one (exit,
// diff stat, raw relay lines, pending user actions, parse errors, log output,
// stripped env) never clear it; a ResultMessage clears it only when the turn
// succeeded; every other message starts a new turn. The live fold
// (addParsedMessage), the seed fold (SeedTimeline), and the server SSE replay
// filter must all agree on this rule, so it lives in one place.
func ClearsExitError(msg agent.Message) bool {
	switch m := msg.(type) {
	case *agent.ExitMessage, *agent.DiffStatMessage, *agent.RawMessage,
		*agent.PendingUserActionMessage, *agent.ParseErrorMessage,
		*agent.LogMessage, *agent.StrippedEnvMessage:
		return false
	case *agent.ResultMessage:
		return !m.IsError
	default:
		return true
	}
}

// lastTurnHasUnansweredAsk reports whether the current turn contains an
// AskMessage that has not been followed by a successful ToolResultMessage.
// It scans backwards from the end until it hits the previous turn's
// ResultMessage boundary. If the current turn's ResultMessage is present, it is
// skipped as a boundary first.
func lastTurnHasUnansweredAsk(msgs []agent.Message) bool {
	skipTrailingResult := lastAgentMessage(msgs) != nil
	answered := map[string]struct{}{}
	for _, msg := range slices.Backward(msgs) {
		switch m := msg.(type) {
		case *agent.AskMessage:
			if m.ToolUseID == "" {
				return true
			}
			if _, ok := answered[m.ToolUseID]; !ok {
				return true
			}
		case *agent.ToolResultMessage:
			if m.ToolUseID != "" && m.Error == "" {
				answered[m.ToolUseID] = struct{}{}
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
func pendingUserActionsFromMessages(msgs []agent.Message) []agent.PendingUserAction {
	skipTrailingResult := lastAgentMessage(msgs) != nil
	answered := map[string]struct{}{}
	restored := map[string]struct{}{}
	pending := map[string]agent.PendingUserAction{}
	var actions []agent.PendingUserAction
	for _, msg := range slices.Backward(msgs) {
		switch m := msg.(type) {
		case *agent.AskMessage:
			if m.ToolUseID == "" {
				continue
			}
			if _, ok := answered[m.ToolUseID]; ok {
				continue
			}
			if _, ok := restored[m.ToolUseID]; ok {
				continue
			}
			action, ok := pending[m.ToolUseID]
			if ok {
				actions = append(actions, agent.ClonePendingUserAction(action))
				restored[m.ToolUseID] = struct{}{}
				delete(pending, m.ToolUseID)
			}
		case *agent.PendingUserActionMessage:
			switch m.Action.Kind {
			case agent.PendingUserActionAskUserQuestion:
			default:
				continue
			}
			if m.Action.ToolUseID != "" {
				pending[m.Action.ToolUseID] = m.Action
			}
		case *agent.ToolResultMessage:
			if m.ToolUseID != "" && m.Error == "" {
				answered[m.ToolUseID] = struct{}{}
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
func lastTurnHasExitPlan(msgs []agent.Message) bool {
	skippedResult := false
	for _, msg := range slices.Backward(msgs) {
		switch m := msg.(type) {
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

// sub is an SSE subscriber with a once-guarded close to prevent double-close
// panics when both the fan-out (slow subscriber drop) and context cancellation
// race to close the channel.
type sub struct {
	ch   chan agent.Message
	once sync.Once
}

func (s *sub) close() {
	s.once.Do(func() { close(s.ch) })
}

type rateLimitSub struct {
	ch chan *agent.RateLimitMessage
}

// computeCost returns the true USD cost for a Claude API result by adding the
// cache-read surcharge that TotalCostUSD omits.
//
// Claude Code's TotalCostUSD correctly prices input, output, and cache-write
// tokens but excludes cache_read_input_tokens. All Claude models share the same
// structural price ratios, so we derive the per-token input price from
// TotalCostUSD and the non-cache-read token counts, then add the missing term.
//
// If there are no non-cache-read tokens to derive a unit price from,
// TotalCostUSD is returned unchanged.
func computeCost(totalCostUSD float64, u agent.Usage) float64 {
	// Express all non-cache-read tokens as an equivalent number of input tokens.
	nonCREquiv := float64(u.InputTokens) + 5*float64(u.OutputTokens) + 1.25*float64(u.CacheCreationInputTokens)
	if nonCREquiv == 0 {
		return totalCostUSD
	}
	inputPricePerTok := totalCostUSD / nonCREquiv
	return totalCostUSD + float64(u.CacheReadInputTokens)*0.10*inputPricePerTok
}

const titleSystemPrompt = "Summarize this coding task conversation in 3-8 words as a short title. Reply with ONLY the title, no quotes."

// Primary returns a copy of the primary RepoMount (Repos[0]), or nil for no-repo tasks.
func (t *Task) Primary() *taskslog.RepoMount {
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

// ExtraRuntimeRepos returns all repos after the primary.
func (t *Task) ExtraRuntimeRepos() []runtime.Repo {
	repos := t.RuntimeRepos()
	if len(repos) <= 1 {
		return nil
	}
	return repos[1:]
}

// ReposSnapshot returns a copy of the task repo mounts.
func (t *Task) ReposSnapshot() []taskslog.RepoMount {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]taskslog.RepoMount(nil), t.Repos...)
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

// SetRelayOffset records the byte offset used when reconnecting to the relay.
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
	metaRepos := make([]agent.MetaRepo, len(repos))
	for i, r := range repos {
		metaRepos[i] = agent.MetaRepo{Name: r.Name, BaseBranch: r.BaseBranch, Branch: r.Branch, ContainerPath: r.ContainerPath}
	}
	return &agent.MetaMessage{
		MessageType:       "caic_meta",
		Prompt:            t.InitialPrompt.Text,
		Title:             t.Title(),
		Repos:             metaRepos,
		Harness:           t.Harness,
		Model:             t.Model,
		Effort:            t.Effort,
		StartedAt:         t.StartedAt,
		ForgeIssue:        t.ForgeIssue,
		ForkedFromTaskID:  t.ForkedFromTaskID.String(),
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
func (t *Task) SetState(s taskslog.State) {
	t.mu.Lock()
	t.setState(s)
	t.mu.Unlock()
}

// SetStateAt updates the state under the mutex with an explicit timestamp.
// Used during import to preserve the original transition time.
func (t *Task) SetStateAt(s taskslog.State, at time.Time) {
	t.mu.Lock()
	if s != taskslog.StateRunning {
		t.turnStartedAt = time.Time{}
	}
	t.state = s
	t.stateUpdatedAt = at
	t.mu.Unlock()
}

// SetTurnStartedAt sets the turn start time if the task is currently running.
// Called during import to estimate when the current mid-turn started.
func (t *Task) SetTurnStartedAt(at time.Time) {
	t.mu.Lock()
	if t.state == taskslog.StateRunning {
		t.turnStartedAt = at
	}
	t.mu.Unlock()
}

// SetStateIf atomically transitions the state to next only if the current
// state equals expected. Returns true if the transition occurred.
func (t *Task) SetStateIf(expected, next taskslog.State) bool {
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
func (t *Task) SetStateIfAny(next taskslog.State, allowed ...taskslog.State) (prev taskslog.State, changed bool) {
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
func (t *Task) SetStateUnless(next taskslog.State, excluded ...taskslog.State) (prev taskslog.State, changed bool) {
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
func (t *Task) GetState() taskslog.State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// GetSessionID returns the agent session ID under the mutex.
func (t *Task) GetSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// SetSessionMetadata records persisted agent session metadata.
func (t *Task) SetSessionMetadata(sessionID, model, agentVersion string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if sessionID != "" {
		t.sessionID = sessionID
	}
	if model != "" && t.reportedModel == "" {
		t.reportedModel = model
	}
	if agentVersion != "" {
		t.agentVersion = agentVersion
	}
}

// GetModel returns the agent-reported model if available, otherwise the
// user-requested model. Read under the mutex.
func (t *Task) GetModel() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reportedModel != "" {
		return t.reportedModel
	}
	return t.Model
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

// LiveStats returns the latest cost, turn count, duration, cumulative token
// usage, and the most recent turn's usage (active context).
func (t *Task) LiveStats() (costUSD float64, numTurns int, duration time.Duration, cumulativeUsage, lastTurnUsage agent.Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.liveCostUSD, t.liveNumTurns, t.liveDuration, t.liveUsage, t.lastUsage
}

// LastAgentResult returns the result text from the most recent ResultMessage,
// falling back to the visible text of that turn when the agent reported
// none, or "" if no result was received. Used by Cleanup/StopTask for the
// log trailer.
func (t *Task) LastAgentResult() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Walk messages in reverse to find the last ResultMessage.
	for i, v := range slices.Backward(t.msgs) {
		if rm, ok := v.(*agent.ResultMessage); ok {
			if rm.Result != "" {
				return rm.Result
			}
			return fallbackResultText(t.msgs[:i+1])
		}
	}
	return ""
}

// PlanContentFor returns the plan content to display on an ExitPlanMode
// message with the given tool use ID: the current plan content when this is
// the visible plan (most recent ExitPlanMode, not cleared by
// context_cleared), else "". SSE conversion consumes this projection because
// the immutable message no longer carries a plan snapshot.
func (t *Task) PlanContentFor(toolUseID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if toolUseID == "" || toolUseID != t.planExitID {
		return ""
	}
	return t.planContent
}

// LastExitError returns the most recent non-zero relay exit diagnostic.
func (t *Task) LastExitError() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastExitError
}

// LiveDiffStat returns the latest diff stat from the relay's periodic polling.
func (t *Task) LiveDiffStat() agent.DiffStat {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.liveDiffStat
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

// SetLiveDiffStat overwrites the live diff stat. Used by task import to set
// the host-side branch diff after SeedTimeline, because the relay's
// diff_watcher only tracks uncommitted changes (git diff HEAD) which
// becomes empty after the agent commits.
func (t *Task) SetLiveDiffStat(ds agent.DiffStat) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setLiveDiffStatLocked(ds)
}

// SetPR stores the forge owner, repo, and PR/MR number. Does not change task state.
func (t *Task) SetPR(owner, repo string, pr int) {
	t.mu.Lock()
	t.forgeOwner = owner
	t.forgeRepo = repo
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

// WriteToLog appends one backend-owned control through the active task log.
func (t *Task) WriteToLog(m agent.Message) error {
	t.mu.Lock()
	var log agent.LogSink
	if t.handle != nil {
		log = t.handle.Log
	}
	t.mu.Unlock()
	if log == nil {
		return taskslog.ErrNoLog
	}
	return log.AppendMessage(m)
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

// Snapshot holds volatile task fields read under the mutex. Used by the
// server to build API responses without data races on fields that
// addMessage/SeedTimeline modify concurrently.
type Snapshot struct {
	State              taskslog.State
	StateUpdatedAt     time.Time
	TurnStartedAt      time.Time // non-zero only while state is Running
	Repos              []taskslog.RepoMount
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
	SessionID          string
	Model              string
	AgentVersion       string
	ContextWindowLimit int // Non-zero when reported by the agent at runtime.
	InPlanMode         bool
	PlanFile           string
	PlanContent        string
	CostUSD            float64
	NumTurns           int
	Duration           time.Duration
	Usage              agent.Usage
	LastUsage          agent.Usage
	LastAPIUsage       agent.Usage
	CacheExpiresAt     time.Time
	DiffStat           agent.DiffStat
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
	Utilization     float64 // Fraction of the window used in [0, 1], not a percentage.
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
	model := t.reportedModel
	if model == "" {
		model = t.Model
	}
	return Snapshot{
		State:              t.state,
		StateUpdatedAt:     t.stateUpdatedAt,
		TurnStartedAt:      t.turnStartedAt,
		Repos:              append([]taskslog.RepoMount(nil), t.Repos...),
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
		SessionID:          t.sessionID,
		Model:              model,
		AgentVersion:       t.agentVersion,
		ContextWindowLimit: t.reportedContextWindow,
		InPlanMode:         t.inPlanMode,
		PlanFile:           t.planFile,
		PlanContent:        t.planContent,
		CostUSD:            t.liveCostUSD,
		NumTurns:           t.liveNumTurns,
		Duration:           t.liveDuration,
		Usage:              t.liveUsage,
		LastUsage:          t.lastUsage,
		LastAPIUsage:       t.lastAPIUsage,
		CacheExpiresAt:     t.cacheExpiresAt,
		DiffStat:           t.liveDiffStat,
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

// Messages returns a copy of all received agent messages.
func (t *Task) Messages() []agent.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]agent.Message(nil), t.msgs...)
}

// PendingUserActions returns current user-facing actions that still need input.
func (t *Task) PendingUserActions() []agent.PendingUserAction {
	t.mu.Lock()
	defer t.mu.Unlock()
	return pendingUserActionsFromMessages(t.msgs)
}

// SeedTimeline fills an empty task with the message history from previously
// saved logs, then derives session metadata, plan state, live stats and task
// state from it.
//
// It panics if the task already holds messages. Seeding is one-shot
// initialization, not a merge: replaying a second batch onto an existing
// timeline would double-count cost, turns and token usage. A live timeline
// grows through the session message pump instead; the disk-reload callers
// The task import paths run exactly once per task.
//
// State inference rules (applied only for non-terminal states):
//   - Current turn has unanswered AskUserQuestion → StateAsking
//   - Trailing ResultMessage (no ask) → StateWaiting
//   - No trailing ResultMessage → state unchanged (agent was mid-output)
//
// Metadata-only messages (DiffStatMessage, PendingUserActionMessage,
// RawMessage) after the ResultMessage are skipped during inference. For
// import, the caller must handle the case where state remains StateRunning
// with no relay alive.
func (t *Task) SeedTimeline(msgs []agent.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.msgs) > 0 {
		panic(fmt.Sprintf("task %s: SeedTimeline on a timeline that already holds %d messages", t.ID, len(t.msgs)))
	}
	t.msgs = msgs
	// One forward pass: the task starts empty, so every field below is derived
	// from msgs alone and the per-concern handlers touch disjoint fields.
	// Later entries (model_rerouted) override earlier ones. The fold only reads
	// msgs — projections like plan content are resolved later via
	// PlanContentFor instead of being written into messages.
	//
	// Plan state: a context_cleared marker resets it — it means ClearMessages
	// was called (e.g. "Clear and execute plan"), so plan data before the
	// marker is stale and plan tracking is suppressed until the next
	// ResultMessage. planExitID holds the ToolUseID of the ExitPlanMode that
	// carries the current plan; a new ExitPlanMode or context_cleared
	// supersedes it so only the latest plan is visible.
	//
	// Live stats: TotalCostUSD is cumulative per-session (resets on
	// compact_boundary), so cost uses priorCostUSD + currentSessionTotal.
	// DurationMs and NumTurns are per-invocation, so they always accumulate.
	// Token usage is always summed.
	cleanTurnComplete := false
	for _, msg := range msgs {
		if exit, ok := msg.(*agent.ExitMessage); ok {
			if exit.ExitCode != 0 && !cleanTurnComplete {
				t.lastExitError = exit.ExitError()
			} else {
				t.lastExitError = ""
			}
			continue
		}
		if ClearsExitError(msg) {
			t.lastExitError = ""
			if _, ok := msg.(*agent.ResultMessage); !ok {
				cleanTurnComplete = false
			}
		}
		switch m := msg.(type) {
		case *agent.MetaSessionMessage:
			if m.SessionID != "" {
				t.sessionID = m.SessionID
			}
			if m.AgentVersion != "" {
				t.agentVersion = m.AgentVersion
			}
			if m.Model != "" && t.reportedModel == "" {
				t.reportedModel = m.Model
			}
		case *agent.InitMessage:
			if m.SessionID != "" {
				t.sessionID = m.SessionID
			}
			if m.Version != "" {
				t.agentVersion = m.Version
			}
			if m.Model != "" {
				t.reportedModel = m.Model
			}
		case *agent.SystemMessage:
			switch m.Subtype {
			case "model_rerouted":
				if m.Model != "" {
					t.reportedModel = m.Model
				}
			case "context_cleared", "compact_boundary":
				if m.Subtype == "context_cleared" {
					t.inPlanMode = false
					t.planFile = ""
					t.planContent = ""
					t.planDismissed = true
					t.planExitID = ""
				}
				t.priorCostUSD = t.liveCostUSD
				t.priorNumTurns = t.liveNumTurns
				t.priorDuration = t.liveDuration
			}
		case *agent.RateLimitMessage:
			t.recordRateLimitLocked(m)
		case *agent.ToolUseMessage:
			t.trackToolUse(m)
		case *agent.UsageMessage:
			t.lastAPIUsage = m.Usage
			if m.Usage.CacheTTLSeconds > 0 {
				t.cacheExpiresAt = time.Now().Add(time.Duration(m.Usage.CacheTTLSeconds) * time.Second)
			}
			if m.ContextWindow > 0 {
				t.reportedContextWindow = m.ContextWindow
			}
		case *agent.DiffStatMessage:
			if len(m.DiffStat) > 0 {
				t.diffCreated = true
			}
		case *agent.ResultMessage:
			t.planDismissed = false
			cleanTurnComplete = !m.IsError
			if len(m.DiffStat) > 0 {
				t.diffCreated = true
			}
			t.liveUsage.InputTokens += m.Usage.InputTokens
			t.liveUsage.OutputTokens += m.Usage.OutputTokens
			t.liveUsage.CacheCreationInputTokens += m.Usage.CacheCreationInputTokens
			t.liveUsage.CacheReadInputTokens += m.Usage.CacheReadInputTokens
			t.liveUsage.ReasoningOutputTokens += m.Usage.ReasoningOutputTokens
			t.lastUsage = m.Usage
			// Compute cost from token counts: TotalCostUSD from Claude Code excludes
			// cache_read_input_tokens, which are charged but omitted from its total.
			t.liveCostUSD = t.priorCostUSD + computeCost(m.TotalCostUSD, m.Usage)
			t.liveNumTurns += m.NumTurns
			t.liveDuration += time.Duration(m.DurationMs) * time.Millisecond
		}
	}
	// Restore live diff stat from the last DiffStatMessage or ResultMessage,
	// whichever appears later. ResultMessage carries the authoritative
	// host-side diff stat but a DiffStatMessage from the relay may follow it.
	for _, msg := range slices.Backward(msgs) {
		if ds, ok := msg.(*agent.DiffStatMessage); ok {
			t.liveDiffStat = ds.DiffStat
			break
		}
		if rm, ok := msg.(*agent.ResultMessage); ok && len(rm.DiffStat) > 0 {
			t.liveDiffStat = rm.DiffStat
			break
		}
	}
	// Infer state: if the last agent-emitted message is a ResultMessage, the
	// agent finished its turn and is waiting for user input (or asking a
	// question). Skip trailing DiffStatMessages — the relay emits periodic
	// diff stats that can appear after the ResultMessage.
	// Only override non-terminal states — purged/crashed/failed tasks loaded
	// from logs must keep their recorded state.
	if len(msgs) > 0 && t.state != taskslog.StatePurged && t.state != taskslog.StateCrashed && t.state != taskslog.StateFailed && t.state != taskslog.StatePurging {
		if lastAgentMessage(msgs) != nil {
			switch {
			case lastTurnHasUnansweredAsk(msgs):
				t.setState(taskslog.StateAsking)
			case lastTurnHasExitPlan(msgs) && t.planContent != "":
				t.setState(taskslog.StateHasPlan)
			default:
				t.setState(taskslog.StateWaiting)
			}
		} else if lastTurnHasUnansweredAsk(msgs) {
			t.setState(taskslog.StateAsking)
		}
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
func (t *Task) CloseAndDetachSession(ctx context.Context) *SessionHandle {
	h := t.DetachSession()
	if h == nil {
		return nil
	}
	_ = h.GracefulStop(ctx, 10*time.Second)
	// Wait for ReadMessages to finish so callers can safely close MsgCh.
	_ = h.Session.Wait()
	return h
}

// GracefulStopSession detaches the currently attached session (if any) and
// sends the relay's shutdown sentinel, waiting up to timeout for the agent to
// exit gracefully. Returns the detached handle (nil if no session was
// attached) and any error from the graceful stop, e.g. a timeout. Used by
// callers (Cleanup, StopTask) that need the handle afterward to reuse its
// Log for a trailer, or to Drain it once other teardown steps complete.
func (t *Task) GracefulStopSession(ctx context.Context, timeout time.Duration) (*SessionHandle, error) {
	h := t.DetachSession()
	if h == nil {
		return nil, nil //nolint:nilnil // no session attached is not an error
	}
	err := h.GracefulStop(ctx, timeout)
	return h, err
}

// ClearMessages injects a context_cleared boundary marker into the message
// stream and resets live stats. Message history is preserved so that SSE
// subscribers (including reconnecting clients) can see the full timeline.
func (t *Task) ClearMessages(ctx context.Context) {
	t.addMessage(ctx, agent.ContextCleared(), false)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionID = ""
	t.priorCostUSD = t.liveCostUSD
	t.priorNumTurns = t.liveNumTurns
	t.priorDuration = t.liveDuration
	t.inPlanMode = false
	t.planFile = ""
	t.planContent = ""
	t.planDismissed = true
	// Drop the plan projection: PlanContentFor returns "" until a new plan
	// is written and exited.
	t.planExitID = ""
}

// Subscribe returns a snapshot of all past messages plus a live channel for
// new messages. The caller must call unsubFn when done to release resources.
func (t *Task) Subscribe(ctx context.Context) (history []agent.Message, live <-chan agent.Message, unsubFn func()) {
	s := &sub{ch: make(chan agent.Message, 256)}

	t.mu.Lock()
	// Snapshot history under lock — no channel writes, so no deadlock risk
	// regardless of history size.
	history = append([]agent.Message(nil), t.msgs...)
	t.subs = append(t.subs, s)
	t.mu.Unlock()

	return history, s.ch, unsubscribeMessages(t, ctx, s)
}

// SubscribeLiveMessages returns only messages produced after subscription. It
// avoids copying retained task history for consumers with another authoritative
// history source, such as a stopped task's raw log.
func (t *Task) SubscribeLiveMessages(ctx context.Context) (live <-chan agent.Message, unsubFn func()) {
	s := &sub{ch: make(chan agent.Message, 256)}
	t.mu.Lock()
	t.subs = append(t.subs, s)
	t.mu.Unlock()
	return s.ch, unsubscribeMessages(t, ctx, s)
}

func unsubscribeMessages(t *Task, ctx context.Context, s *sub) func() {
	unsub := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		for i, ss := range t.subs {
			if ss == s {
				t.subs = append(t.subs[:i], t.subs[i+1:]...)
				break
			}
		}
	}
	// Close channel when context is done.
	go func() {
		<-ctx.Done()
		unsub()
		s.close()
	}()
	return unsub
}

// SubscribeRateLimits returns historical quota messages and a lossless stream
// of future quota messages. The caller must call unsubFn at most once when
// done; context cancellation automatically unsubscribes.
func (t *Task) SubscribeRateLimits(ctx context.Context) (history []*agent.RateLimitMessage, live <-chan *agent.RateLimitMessage, unsubFn func()) {
	s := &rateLimitSub{ch: make(chan *agent.RateLimitMessage, 16)}

	t.mu.Lock()
	for _, message := range t.msgs {
		if rateLimit, ok := message.(*agent.RateLimitMessage); ok {
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
	idx := (t.statsHead + t.statsLen) % statsRingSize
	t.statsRing[idx] = *s
	if t.statsLen < statsRingSize {
		t.statsLen++
	} else {
		t.statsHead = (t.statsHead + 1) % statsRingSize
	}
	val := *s
	for _, sub := range t.statsSubs {
		select {
		case sub.ch <- val:
		default:
		}
	}
	t.mu.Unlock()
}

// SubscribeStats returns a snapshot of the stats ring buffer and a channel that
// receives only live stats arriving after the snapshot. The context cancellation
// closes the channel and removes the subscriber.
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

// SubscribeLiveStats returns only stats produced after subscription. It avoids
// allocating a ring-buffer snapshot when raw disk history is authoritative.
func (t *Task) SubscribeLiveStats(ctx context.Context) (live <-chan runtime.Stats, unsubFn func()) {
	s := &statsSub{ch: make(chan runtime.Stats, 64)}
	t.mu.Lock()
	t.statsSubs = append(t.statsSubs, s)
	t.mu.Unlock()
	return s.ch, unsubscribeStats(t, ctx, s)
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
	}
	go func() {
		<-ctx.Done()
		unsub()
		s.close()
	}()
	return unsub
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

// SendInput sends a user message to the running agent.
//
// Returns an error if no session is active. The error includes the task state
// and a SessionStatus so the caller can diagnose why the session is missing
// (e.g. relay died vs. never connected). The session watcher now handles
// dead-session detection proactively, so SendInput no longer does lazy
// cleanup.
func (t *Task) SendInput(ctx context.Context, p agent.Prompt) error {
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
	if err := h.Session.SendPrompt(p); err != nil {
		return err
	}
	if state == taskslog.StateWaiting || state == taskslog.StateAsking || state == taskslog.StateHasPlan {
		t.SetStateIfAny(taskslog.StateRunning, taskslog.StateWaiting, taskslog.StateAsking, taskslog.StateHasPlan)
		// Plan content is preserved — the UI hides naturally while the
		// task is Running (isWaiting is false). When the agent finishes,
		// the plan reappears (original or updated via Write/Edit).
		// ClearMessages (the "Clear and execute plan" path) is the only
		// place that erases plan state.
	}
	t.addMessage(ctx, syntheticUserInput(p), false)
	return nil
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

// SetTitle sets the title under the mutex. Empty strings are ignored to
// preserve the prompt-fallback invariant.
func (t *Task) SetTitle(title string) {
	if title == "" {
		return
	}
	t.mu.Lock()
	t.title = title
	t.mu.Unlock()
}

// SetAgentVersion sets the agent version reported by the init message.
// Used when restoring purged tasks from logs without full message parsing.
func (t *Task) SetAgentVersion(v string) {
	t.mu.Lock()
	t.agentVersion = v
	t.mu.Unlock()
}

// GenerateTitle asks the LLM for a short title from the prompt and any result
// messages, logging through the task-scoped log. No-op when the provider is
// unconfigured.
func (t *Task) GenerateTitle(ctx context.Context, log *slog.Logger) {
	if t.Provider == nil {
		return
	}
	msgs := t.Messages()
	var b strings.Builder
	var window ResultTextWindow
	for _, m := range msgs {
		if v, ok := m.(*agent.ResultMessage); ok {
			text := v.Result
			if text == "" {
				text = window.Value()
			}
			if text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString("Result: ")
				b.WriteString(text)
			}
			// A result is a turn boundary: reset the window for the next turn.
			window.Update(m)
			continue
		}
		window.Update(m)
	}
	// Prepend the original prompt.
	// TODO: Use the images too.
	input := "Prompt: " + t.InitialPrompt.Text
	if b.Len() > 0 {
		input += "\n" + b.String()
	}
	// Truncate to keep it working on most providers.
	const maxChars = 50000
	if len(input) > maxChars {
		input = input[:maxChars]
	}

	start := time.Now()
	res, err := t.Provider.GenSync(ctx,
		genai.Messages{genai.NewTextMessage(input)},
		&genai.GenOptionText{SystemPrompt: titleSystemPrompt},
	)
	dur := time.Since(start).Round(time.Millisecond)
	if err != nil {
		log.WarnContext(ctx, "title failed", "err", err, "dur", dur)
		return
	}
	// Strip surrounding quotes if the model adds them despite instructions.
	title := strings.Trim(strings.TrimSpace(res.String()), "\"'`")
	if title == "" {
		log.WarnContext(ctx, "title empty", "dur", dur)
		return
	}
	log.InfoContext(ctx, "title generated", "title", title, "dur", dur)
	t.SetTitle(title)
}

// RecordSessionCrash marks an agent session as crashed and emits a
// user-visible error result. It returns false if the task is no longer in a
// state owned by a recoverable agent session.
func (t *Task) RecordSessionCrash(ctx context.Context, err error) bool {
	if _, changed := t.SetStateIfAny(taskslog.StateCrashed, taskslog.StateRunning, taskslog.StateWaiting, taskslog.StateAsking, taskslog.StateHasPlan); !changed {
		return false
	}
	msg := "Agent session crashed: " + err.Error()
	if exitErr := t.LastExitError(); exitErr != "" {
		msg = "Agent session crashed: " + exitErr
	}
	t.addMessage(ctx, &agent.ResultMessage{
		MessageType: "result",
		Subtype:     "error",
		IsError:     true,
		Result:      msg,
	}, true)
	return true
}

// RecordSessionFailure marks an active startup session as failed and emits a
// user-visible error result. It returns false if the task is no longer in a
// startup state.
func (t *Task) RecordSessionFailure(ctx context.Context, err error) bool {
	if _, changed := t.SetStateIfAny(taskslog.StateFailed, taskslog.StateStarting); !changed {
		return false
	}
	msg := "Agent session failed: " + err.Error()
	if exitErr := t.LastExitError(); exitErr != "" {
		msg = "Agent session failed: " + exitErr
	}
	t.addMessage(ctx, &agent.ResultMessage{
		MessageType: "result",
		Subtype:     "error",
		IsError:     true,
		Result:      msg,
	}, true)
	return true
}

func (t *Task) setLiveDiffStatLocked(ds agent.DiffStat) {
	t.liveDiffStat = ds
	if len(ds) > 0 {
		t.diffCreated = true
	}
}

// setState updates the state and records the transition time. The caller must
// hold t.mu when called from a locked context, or ensure exclusive access.
func (t *Task) setState(s taskslog.State) {
	if s == taskslog.StateRunning && t.state != taskslog.StateRunning {
		t.turnStartedAt = time.Now().UTC()
	} else if s != taskslog.StateRunning {
		t.turnStartedAt = time.Time{}
	}
	t.state = s
	t.stateUpdatedAt = time.Now().UTC()
}

func (t *Task) recordStartupFailure(ctx context.Context, err error) {
	t.SetState(taskslog.StateFailed)
	t.addMessage(ctx, &agent.LogMessage{Line: "Task startup failed: " + err.Error()}, false)
}

// addMessage records a synthetic server message with an explicit zero producer time.
func (t *Task) addMessage(_ context.Context, m agent.Message, skipTitleGen bool) {
	t.addParsedMessage(agent.ParsedMessage{Message: m}, skipTitleGen)
}

// addParsedMessage records one physical relay record while task state and
// subscribers consume its Message.
func (t *Task) addParsedMessage(parsed agent.ParsedMessage, skipTitleGen bool) (stateChanged, generateTitle bool) {
	m := parsed.Message
	t.mu.Lock()
	initialState := t.state
	defer func() {
		stateChanged = t.state != initialState
		t.mu.Unlock()
	}()
	if meta, ok := m.(*agent.MetaSessionMessage); ok {
		if meta.SessionID != "" {
			t.sessionID = meta.SessionID
		}
		if meta.AgentVersion != "" {
			t.agentVersion = meta.AgentVersion
		}
		if meta.Model != "" && t.reportedModel == "" {
			t.reportedModel = meta.Model
		}
		return stateChanged, generateTitle
	}
	t.msgs = append(t.msgs, m)
	if rateLimit, ok := m.(*agent.RateLimitMessage); ok {
		t.recordRateLimitLocked(rateLimit)
		for _, sub := range t.rateLimitSubs {
			sub.ch <- rateLimit
		}
	}

	// Capture metadata from the init message.
	if init, ok := m.(*agent.InitMessage); ok {
		if init.SessionID != "" {
			t.sessionID = init.SessionID
		}
		if init.Version != "" {
			t.agentVersion = init.Version
		}
		if init.Model != "" {
			t.reportedModel = init.Model
		}
		// Inject the user-requested thinking effort into the init message
		// so the frontend can display it. The agent CLI doesn't report it.
		if init.Effort == "" && t.Effort != "" {
			init.Effort = t.Effort
		}
	}
	// Track model rerouting (codex): update reportedModel to the active model.
	if sm, ok := m.(*agent.SystemMessage); ok && sm.Subtype == "model_rerouted" && sm.Model != "" {
		t.reportedModel = sm.Model
	}
	// Track plan mode and plan file from tool_use events.
	if tu, ok := m.(*agent.ToolUseMessage); ok {
		t.trackToolUse(tu)
	}
	if u, ok := m.(*agent.UsageMessage); ok {
		t.lastAPIUsage = u.Usage
		ttl := u.Usage.CacheTTLSeconds
		if ttl <= 0 {
			ttl = 3600
		}
		t.cacheExpiresAt = time.Now().Add(time.Duration(ttl) * time.Second)
		if u.ContextWindow > 0 {
			t.reportedContextWindow = u.ContextWindow
		}
	}
	// Transition to running when the agent starts producing output.
	// Handles three cases:
	//   - Normal turn: awaiting user input (Waiting/Asking/HasPlan).
	//   - Server restart: SeedTimeline inferred a waiting state,
	//     but the relay already started a new turn before reattach.
	//   - First turn: the agent may produce output before Checkout.Start
	//     sets StateRunning (race between backend subprocess and
	//     SetState on the main goroutine).
	switch m.(type) {
	case *agent.AskMessage:
		if t.state == taskslog.StateStarting || t.state == taskslog.StateRunning || t.state == taskslog.StateWaiting || t.state == taskslog.StateHasPlan {
			t.setState(taskslog.StateAsking)
		}
	case *agent.TextMessage, *agent.ToolUseMessage, *agent.TodoMessage:
		if t.state == taskslog.StateStarting || t.state == taskslog.StateWaiting || t.state == taskslog.StateAsking || t.state == taskslog.StateHasPlan {
			if t.state == taskslog.StateAsking && lastTurnHasUnansweredAsk(t.msgs) {
				break
			}
			t.setState(taskslog.StateRunning)
		}
	}
	// Update live diff stat from relay polling.
	if ds, ok := m.(*agent.DiffStatMessage); ok {
		t.setLiveDiffStatLocked(ds.DiffStat)
	}
	if exit, ok := m.(*agent.ExitMessage); ok {
		if rm := lastAgentMessage(t.msgs); exit.ExitCode != 0 && (rm == nil || rm.IsError) {
			t.lastExitError = exit.ExitError()
		} else {
			t.lastExitError = ""
		}
	} else if ClearsExitError(m) {
		t.lastExitError = ""
	}
	// compact_boundary resets TotalCostUSD in Claude Code's subsequent
	// ResultMessages (same as context_cleared). Snapshot priors so the
	// cost accumulation across the boundary is correct. DurationMs and
	// NumTurns are per-invocation and always use +=, so priors just carry
	// the running total forward.
	if sm, ok := m.(*agent.SystemMessage); ok && sm.Subtype == "compact_boundary" {
		t.priorCostUSD = t.liveCostUSD
		t.priorNumTurns = t.liveNumTurns
		t.priorDuration = t.liveDuration
	}
	// Transition to waiting/asking when a result arrives.
	if rm, ok := m.(*agent.ResultMessage); ok {
		if len(rm.DiffStat) > 0 {
			t.setLiveDiffStatLocked(rm.DiffStat)
		}
		t.liveUsage.InputTokens += rm.Usage.InputTokens
		t.liveUsage.OutputTokens += rm.Usage.OutputTokens
		t.liveUsage.CacheCreationInputTokens += rm.Usage.CacheCreationInputTokens
		t.liveUsage.CacheReadInputTokens += rm.Usage.CacheReadInputTokens
		t.liveUsage.ReasoningOutputTokens += rm.Usage.ReasoningOutputTokens
		t.lastUsage = rm.Usage
		// Compute cost from token counts: TotalCostUSD from Claude Code excludes
		// cache_read_input_tokens, which are charged but omitted from its total.
		t.liveCostUSD = t.priorCostUSD + computeCost(rm.TotalCostUSD, rm.Usage)
		t.liveNumTurns += rm.NumTurns
		t.liveDuration += time.Duration(rm.DurationMs) * time.Millisecond
		t.planDismissed = false
		// Transition Running→Waiting/Asking/HasPlan. Also handle
		// Running/Waiting because watchSession may have already set
		// Waiting before the dispatch goroutine processed this
		// ResultMessage (it does a blocking Fetch first). In that case
		// we still need to distinguish Waiting from Asking/HasPlan.
		// StateStarting is also handled: the agent subprocess may
		// produce a result before Checkout.Start calls SetState(Running).
		if t.state == taskslog.StateRunning || t.state == taskslog.StateStarting || t.state == taskslog.StateWaiting || t.state == taskslog.StateAsking {
			switch {
			case lastTurnHasUnansweredAsk(t.msgs):
				t.setState(taskslog.StateAsking)
			case lastTurnHasExitPlan(t.msgs) && t.planContent != "":
				t.setState(taskslog.StateHasPlan)
			default:
				t.setState(taskslog.StateWaiting)
			}
		}
		if !skipTitleGen {
			generateTitle = true
		}
	}
	// Fan out to subscribers (non-blocking). Skip a non-zero exit message that
	// follows a cleanly completed turn: it is a spurious termination artifact
	// (e.g. SIGINT from a user-requested stop) and is already dropped from the
	// persisted replay, so the live stream must match to avoid a transient
	// "Parse error" that disappears when the task log is reloaded.
	if exit, ok := m.(*agent.ExitMessage); ok && exit.ExitCode != 0 && t.lastExitError == "" {
		return stateChanged, generateTitle
	}
	for i := 0; i < len(t.subs); i++ {
		select {
		case t.subs[i].ch <- m:
		default:
			// Slow subscriber — drop and remove.
			t.subs[i].close()
			t.subs = append(t.subs[:i], t.subs[i+1:]...)
			i--
		}
	}
	return stateChanged, generateTitle
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
// (planExitID, resolved via PlanContentFor). The caller must hold t.mu.
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
