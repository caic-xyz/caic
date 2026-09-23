// Shared types and message definitions for the agent abstraction layer.

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/mcp"
)

const (
	messageTypeSystem                = "system"
	messageSubtypeContextCleared     = "context_cleared"
	messageTypeText                  = "text"
	messageTypeUserInput             = "user_input"
	messageTypeProvisioningLog       = "log"
	messageTypeMeta                  = "caic_meta"
	messageTypeDiffStat              = "caic_diff_stat"
	messageTypeExit                  = "caic_exit"
	messageTypeStrippedEnv           = "caic_stripped_env"
	messageTypeSession               = "caic_session"
	messageTypeLegacyInit            = "caic_init"
	messageTypeModelInfo             = "caic_model_info"
	messageTypePR                    = "caic_pr"
	messageTypeResult                = "caic_result"
	messageTypeTurnCommitSnapshot    = "caic_turn_commit_snapshot"
	messageTypePendingUserAction     = "caic_pending_user_action"
	messageTypeProvisioningLogRecord = "caic_log"
	messageTypeMCPRequest            = "caic_mcp_request"
	messageTypeRelayGeneration       = "caic_relay_generation"
)

// SystemSubtypeModelRerouted identifies a system message reporting that the
// harness changed the active model.
const SystemSubtypeModelRerouted = "model_rerouted"

// SystemSubtypeCompactStart marks the beginning of a harness-initiated context
// compaction. It lets the UI show progress while the summary is generated.
const SystemSubtypeCompactStart = "compact_start"

// SystemSubtypeCompactBoundary marks the point where the harness replaced the
// conversation with a summary.
const SystemSubtypeCompactBoundary = "compact_boundary"

// SystemSubtypeCompactError reports a compaction that failed or was cancelled,
// so the context was left unchanged.
const SystemSubtypeCompactError = "compact_error"

// DiffFileStat describes changes to a single file.
//
// The added/deleted JSON keys are the relay wire contract, so the Go field names
// are intentionally different from the JSON tags.
type DiffFileStat struct {
	Path         string `json:"path"`
	LinesAdded   int    `json:"added"`
	LinesDeleted int    `json:"deleted"`
	Binary       bool   `json:"binary,omitempty"`
	OldSize      int64  `json:"oldSize,omitempty"` // Byte size of the binary pre-image; zero for added paths.
	NewSize      int64  `json:"newSize,omitempty"` // Byte size of the binary post-image; zero for deleted paths.
}

// MCPRequestMessage carries one task-local MCP request from the relay.
type MCPRequestMessage struct {
	ID        string          `json:"id"`
	Method    mcp.Method      `json:"method"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Type implements Message.
func (*MCPRequestMessage) Type() string { return messageTypeMCPRequest }

// DiffStat summarises the changes in a branch relative to its base.
type DiffStat []DiffFileStat

// Message is the interface for all agent streaming messages.
type Message interface {
	// Type returns the message type string.
	Type() string
}

// TimedMessage pairs a semantic message with its producer timestamp. A zero
// ProducerTime means the physical record did not provide producer time.
type TimedMessage struct {
	Message      Message
	ProducerTime time.Time
}

// RelayRecordBoundary identifies one relay-owned physical record by its end
// offset within a relay generation and the following semantic timeline position.
type RelayRecordBoundary struct {
	Generation  string
	RelayEnd    int64
	MessageEnd  int
	ByteEnd     int
	Fingerprint [32]byte
}

// ParsedTimeline contains semantic messages and adoption-only relay record
// boundaries from one ordered physical-record scan. Encoded holds validated
// relay bytes when the scan came directly from a bounded relay snapshot.
type ParsedTimeline struct {
	Messages     []TimedMessage
	RelayRecords []RelayRecordBoundary
	Encoded      []byte
}

// NativeDurationMessage is implemented by messages that carry an authoritative
// duration reported by the agent or harness.
type NativeDurationMessage interface {
	Message
	NativeDuration() (time.Duration, bool)
}

// NativeDuration returns a message's authoritative agent- or harness-reported
// duration. The boolean is false when that message type or record has none.
func NativeDuration(message Message) (time.Duration, bool) {
	timed, ok := message.(NativeDurationMessage)
	if !ok {
		return 0, false
	}
	return timed.NativeDuration()
}

// InitMessage is emitted when a session starts.
//
// Its JSON encoding is persisted in task logs. Keep JSON field names and their
// meanings backward-compatible with logs written by released binaries.
type InitMessage struct {
	SessionID      string   `json:"session_id"`
	Cwd            string   `json:"cwd"`
	Tools          []string `json:"tools"`
	ReportedModel  string   `json:"model"`
	ReportedEffort string   `json:"reported_effort,omitempty"` // Thinking effort (e.g. "low", "medium", "high", "max"). Empty when not supported.
	Version        string   `json:"claude_code_version"`
}

// Type implements Message.
func (m *InitMessage) Type() string { return "init" }

// SystemMessage is a generic system message (status, compact_boundary, etc.).
//
// Its JSON encoding is persisted in task logs. Keep JSON field names and their
// meanings backward-compatible with logs written by released binaries.
type SystemMessage struct {
	MessageType   string `json:"type"`
	Subtype       string `json:"subtype"`
	SessionID     string `json:"session_id"`
	UUID          string `json:"uuid"`
	Detail        string `json:"detail,omitempty"` // Optional human-readable detail (e.g. model names for SystemSubtypeModelRerouted).
	ReportedModel string `json:"model,omitempty"`  // Active model after SystemSubtypeModelRerouted; used to update task.reportedModel.
	// ContextTokensBefore is the harness-reported context size before a
	// SystemSubtypeCompactBoundary. Zero means the harness did not report it.
	ContextTokensBefore int64 `json:"context_tokens_before,omitempty"`
	// ContextTokensAfter is the harness's context size after a
	// SystemSubtypeCompactBoundary, measured or estimated by the harness. Zero
	// means the harness did not report it.
	ContextTokensAfter int64 `json:"context_tokens_after,omitempty"`
}

// ContextCleared creates the persisted context-clear system marker.
func ContextCleared() *SystemMessage {
	return &SystemMessage{MessageType: messageTypeSystem, Subtype: messageSubtypeContextCleared}
}

// Type implements Message.
func (m *SystemMessage) Type() string { return messageTypeSystem }

// TextMessage is emitted when the agent produces text output.
type TextMessage struct {
	Text  string `json:"text"`
	Phase string `json:"phase,omitempty"` // Codex only: "commentary" | "final_answer" | "".
}

// Type implements Message.
func (m *TextMessage) Type() string { return messageTypeText }

// ToolUseMessage is emitted when the agent invokes a tool (except
// AskUserQuestion and TodoWrite which have their own types).
type ToolUseMessage struct {
	ToolUseID string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input,omitempty"`
	Detail    string          `json:"detail,omitempty"` // Backend-normalized short display detail for tool headers.
	InputView ToolInputView   `json:"input_view,omitzero"`
}

// Type implements Message.
func (m *ToolUseMessage) Type() string { return "tool_use" }

// SkillReadMessage records that the harness loaded a skill into the
// conversation context (Claude Code's Skill tool). It is usage metadata for
// analytics and is never rendered in the visible timeline; the tool call and
// its result stay out of the transcript.
//
// Its JSON encoding is derived from harness records on replay; keep JSON
// field names and their meanings backward-compatible with logs written by
// released binaries.
type SkillReadMessage struct {
	ToolUseID string `json:"id"`
	Skill     string `json:"skill"`
	Args      string `json:"args,omitempty"`
	// SourceToolUseID links an inferred read to the file-opening tool's
	// outcome. It is separate from ToolUseID, which suppresses Claude's
	// reported Skill tool result in the timeline.
	SourceToolUseID string `json:"source_tool_use_id,omitempty"`
	// Inferred marks a read recovered from the path a tool opened rather than
	// reported by the harness. An inferred read accompanies its tool call
	// instead of replacing it, so it carries no tool use ID and never
	// suppresses a tool result.
	Inferred bool `json:"inferred,omitempty"`
}

// Type implements Message.
func (m *SkillReadMessage) Type() string { return "skill_read" }

// ToolInputViewKind identifies a normalized tool input view.
type ToolInputViewKind string

const (
	// ToolInputFileChanges renders changed files as unified patches.
	ToolInputFileChanges ToolInputViewKind = "fileChanges"
	// ToolInputSubagents renders one or more spawned subagents.
	ToolInputSubagents ToolInputViewKind = "subagents"
)

// ToolInputView is a backend-normalized rendering model for known tool inputs.
type ToolInputView struct {
	Kind      ToolInputViewKind `json:"kind"`
	Files     []FileChange      `json:"files,omitzero"`
	Subagents []SubagentSpawn   `json:"subagents,omitzero"`
}

// FileChange is one changed file rendered from a unified patch.
type FileChange struct {
	Path  string `json:"path"`
	Patch string `json:"patch"`
}

// TextReplacement is one exact text replacement reported by an edit tool.
type TextReplacement struct {
	OldText string
	NewText string
}

// SubagentSpawn is one backend-normalized subagent invocation.
type SubagentSpawn struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
	Label string `json:"label,omitempty"`
	Phase string `json:"phase,omitempty"`
}

// FileChangesInputView returns a normalized file-change rendering model.
func FileChangesInputView(files []FileChange) ToolInputView {
	if len(files) == 0 {
		return ToolInputView{}
	}
	return ToolInputView{
		Kind:  ToolInputFileChanges,
		Files: files,
	}
}

// FileChangesInputViewFromReplacements converts exact replacements to a
// synthetic unified patch so every edit-like tool uses the same display model.
func FileChangesInputViewFromReplacements(path string, replacements []TextReplacement) ToolInputView {
	if path == "" || len(replacements) == 0 {
		return ToolInputView{}
	}
	return FileChangesInputView([]FileChange{{
		Path:  path,
		Patch: textReplacementsPatch(path, replacements),
	}})
}

func textReplacementsPatch(path string, replacements []TextReplacement) string {
	var b strings.Builder
	b.WriteString("--- ")
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString("+++ ")
	b.WriteString(path)
	b.WriteByte('\n')
	for i, r := range replacements {
		b.WriteString("@@ replacement ")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(" @@\n")
		writePatchLines(&b, '-', r.OldText)
		writePatchLines(&b, '+', r.NewText)
	}
	return b.String()
}

func writePatchLines(b *strings.Builder, prefix byte, text string) {
	if text == "" {
		return
	}
	for line := range strings.Lines(text) {
		b.WriteByte(prefix)
		b.WriteString(strings.TrimSuffix(line, "\n"))
		b.WriteByte('\n')
	}
}

// AskMessage is emitted when the agent asks the user a question via the
// AskUserQuestion tool.
type AskMessage struct {
	ToolUseID string        `json:"id"`
	Questions []AskQuestion `json:"questions"`
}

// Type implements Message.
func (m *AskMessage) Type() string { return "ask" }

// PendingUserActionMessageType identifies a persisted pending user action.
const PendingUserActionMessageType = messageTypePendingUserAction

// PendingUserActionKind identifies the user action caic is waiting for.
type PendingUserActionKind string

const (
	// PendingUserActionAskUserQuestion means the agent invoked AskUserQuestion
	// and caic still needs the user's answer.
	PendingUserActionAskUserQuestion PendingUserActionKind = "ask_user_question"
)

// PendingUserAction records one user-facing action that must be completed
// before the agent can continue after a reconnect.
//
// This is intentionally user-facing state, not a generic backend control
// protocol bucket. Permission auto-allow, keepalive, environment updates, and
// other backend-only control messages should not be represented here.
type PendingUserAction struct {
	Kind PendingUserActionKind `json:"kind"`

	// RequestID is the backend request ID needed to complete the action.
	RequestID string `json:"request_id,omitempty"`

	// ToolUseID is the user-visible tool call that created the action.
	ToolUseID string `json:"tool_use_id,omitempty"`

	Ask PendingAskAction `json:"ask,omitzero"`
}

// PendingAskAction is the payload for PendingUserActionAskUserQuestion. It
// stores the rendered questions so reconnect can answer the original backend
// control request without replaying provider-specific raw JSON.
type PendingAskAction struct {
	Questions []AskQuestion `json:"questions,omitzero"`
}

// PendingUserActionMessage persists a PendingUserAction in task history. It is
// metadata for reconnect and should not be rendered as a chat message.
type PendingUserActionMessage struct {
	MessageType string            `json:"type"`
	Action      PendingUserAction `json:"action"`
}

// Type implements Message.
func (m *PendingUserActionMessage) Type() string { return PendingUserActionMessageType }

// ClonePendingUserAction returns a deep copy of a.
func ClonePendingUserAction(a PendingUserAction) PendingUserAction {
	a.Ask.Questions = cloneAskQuestions(a.Ask.Questions)
	return a
}

// TodoMessage is emitted when the agent updates its todo list via the
// TodoWrite tool.
type TodoMessage struct {
	ToolUseID string     `json:"id"`
	Todos     []TodoItem `json:"todos"`
}

// Type implements Message.
func (m *TodoMessage) Type() string { return "todo" }

// UserInputMessage represents direct user text/image input (not a tool result).
type UserInputMessage struct {
	Text   string      `json:"text,omitempty"`
	Images []ImageData `json:"images,omitempty"`
}

// Type implements Message.
func (m *UserInputMessage) Type() string { return messageTypeUserInput }

// ToolResultMessage is emitted when a tool returns its result.
type ToolResultMessage struct {
	ToolUseID  string `json:"tool_use_id"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"` // Non-empty when the tool reported an error.
}

// Type implements Message.
func (m *ToolResultMessage) Type() string { return "tool_result" }

// NativeDuration returns the tool duration when the harness reported one.
func (m *ToolResultMessage) NativeDuration() (time.Duration, bool) {
	return time.Duration(m.DurationMs) * time.Millisecond, m.DurationMs > 0
}

// UsageMessage reports token consumption for a single API call.
//
// Its JSON encoding is persisted in task logs. Keep JSON field names and their
// meanings backward-compatible with logs written by released binaries.
type UsageMessage struct {
	Usage         Usage  `json:"usage"`
	ReportedModel string `json:"model,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"` // Non-zero when the backend reports the active context window size.
	// ModelDerived reports that ReportedModel was derived by caic from session
	// state because the harness record carried no model. Such records must not
	// be priced per call: the harness prices this usage in its turn result, or
	// another harness-reported record already covers the same API call. The
	// model stamp remains valid for per-model usage analytics.
	ModelDerived bool `json:"model_derived,omitempty"`
}

// Type implements Message.
func (m *UsageMessage) Type() string { return "usage" }

// AskOption is a single option in an AskUserQuestion.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AskQuestion is a single question from AskUserQuestion.
type AskQuestion struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	Options     []AskOption `json:"options"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
}

func cloneAskQuestions(qs []AskQuestion) []AskQuestion {
	if len(qs) == 0 {
		return nil
	}
	out := make([]AskQuestion, len(qs))
	for i := range qs {
		out[i] = qs[i]
		out[i].Options = append([]AskOption(nil), qs[i].Options...)
	}
	return out
}

// TodoItem is a single todo entry from a TodoWrite tool call.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // "pending", "in_progress", "completed".
	ActiveForm string `json:"activeForm,omitempty"`
}

// Usage tracks per-API-call token consumption as reported by the Anthropic API.
//
// The three input token fields are disjoint; total input context for one call
// equals InputTokens + CacheCreationInputTokens + CacheReadInputTokens.
// InputTokens is only the small non-cached, non-cache-creation portion
// (typically single-digit). The bulk of the input context lands in cache
// fields.
//
// In ResultMessage these values are per-query (sum of all API calls in the turn).
// Task.liveUsage sums them across all queries for cumulative totals.
//
// ReasoningOutputTokens is a subset of OutputTokens used for extended thinking
// (Claude) or reasoning summaries (Codex). Zero when the harness does not report it.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	ReasoningOutputTokens    int `json:"reasoning_output_tokens,omitempty"`
	CacheTTLSeconds          int `json:"cache_ttl_seconds,omitempty"` // Effective cache TTL from last API call; 0 = unknown.
}

// RepositoryCommit identifies a committed repository branch tip recorded at a
// turn boundary.
type RepositoryCommit struct {
	// RepositoryPath is the repository's absolute path inside the task runtime.
	RepositoryPath string `json:"repository_path"`
	// BranchName is the short local branch name, such as "main" or "caic-1".
	BranchName string `json:"branch_name"`
	// CommitHash is the full Git object ID of the branch tip.
	CommitHash string `json:"commit_hash"`
}

// ChangeStat summarizes the net committed file changes between two repository snapshots.
type ChangeStat struct {
	Files        int `json:"files"`
	LinesAdded   int `json:"added"`
	LinesDeleted int `json:"deleted"`
	BinaryFiles  int `json:"binary_files"`
}

// TurnCommitSnapshotMessage is a standalone durable record of the committed
// repository branch tips fetched from a task runtime when a turn finishes.
type TurnCommitSnapshotMessage struct {
	MessageType string `json:"type"`
	// Baseline marks the snapshot taken before a newly started agent session.
	Baseline bool `json:"baseline,omitempty"`
	// RepositoryCommits contains the immutable Git branch tips fetched from
	// every repository in the task runtime at this boundary.
	RepositoryCommits []RepositoryCommit `json:"repository_commits"`
	// ChangeStat is the completed turn's net committed change since the prior
	// snapshot. It is nil when no complete comparison was available.
	ChangeStat *ChangeStat `json:"change_stat,omitempty"`
}

// NewTurnCommitSnapshotMessage creates a durable commit snapshot.
func NewTurnCommitSnapshotMessage(commits []RepositoryCommit, baseline bool, changeStat *ChangeStat) *TurnCommitSnapshotMessage {
	return &TurnCommitSnapshotMessage{
		MessageType:       messageTypeTurnCommitSnapshot,
		Baseline:          baseline,
		RepositoryCommits: append([]RepositoryCommit(nil), commits...),
		ChangeStat:        changeStat,
	}
}

// Type implements Message.
func (m *TurnCommitSnapshotMessage) Type() string { return messageTypeTurnCommitSnapshot }

// ResultMessage is the terminal message for a query.
type ResultMessage struct {
	MessageType   string  `json:"type"`
	Subtype       string  `json:"subtype"`
	IsError       bool    `json:"is_error"`
	DurationMs    int64   `json:"duration_ms"`
	DurationAPIMs int64   `json:"duration_api_ms"`
	NumTurns      int     `json:"num_turns"`
	Result        string  `json:"result"`
	SessionID     string  `json:"session_id"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	Usage         Usage   `json:"usage"`
	// ContextWindow is the active model's context window size in tokens. It is
	// set by harnesses that report the window with the turn result (Claude Code)
	// and is 0 when they do not.
	ContextWindow int      `json:"context_window,omitempty"`
	UUID          string   `json:"uuid"`
	DiffStat      DiffStat `json:"diff_stat,omitzero"` // Set by caic after running container diff.
}

// Type implements Message.
func (m *ResultMessage) Type() string { return "result" }

// NativeDuration returns the invocation duration when the harness reported one.
func (m *ResultMessage) NativeDuration() (time.Duration, bool) {
	return time.Duration(m.DurationMs) * time.Millisecond, m.DurationMs > 0
}

// TextDeltaMessage is a streaming text fragment, emitted when
// --include-partial-messages is enabled. Extracted from the nested wire
// format (stream_event → content_block_delta → text_delta) during parsing.
type TextDeltaMessage struct {
	Text string
}

// Type implements Message.
func (m *TextDeltaMessage) Type() string { return "text_delta" }

// ThinkingMessage is emitted when the agent produces a thinking block.
type ThinkingMessage struct {
	Text string `json:"text"`
}

// Type implements Message.
func (m *ThinkingMessage) Type() string { return "thinking" }

// ThinkingDeltaMessage is a streaming thinking fragment.
type ThinkingDeltaMessage struct {
	Text string
}

// Type implements Message.
func (m *ThinkingDeltaMessage) Type() string { return "thinking_delta" }

// ToolOutputDeltaMessage is a streaming output fragment from a tool execution.
// Codex only: emitted via item/commandExecution/outputDelta (Bash stdout) and
// item/mcpToolCall/progress (MCP tool progress messages).
type ToolOutputDeltaMessage struct {
	ToolUseID string
	Delta     string
}

// Type implements Message.
func (m *ToolOutputDeltaMessage) Type() string { return "tool_output_delta" }

// NativeSubagentStatus describes the lifecycle state a harness actually
// reported for a native subagent. Unknown means the harness proved the spawn
// but did not expose a lifecycle state; it is not an inferred running state.
type NativeSubagentStatus string

// Terminal reports whether the status ends a native-subagent lifecycle.
func (s NativeSubagentStatus) Terminal() bool {
	switch s {
	case NativeSubagentStatusCompleted, NativeSubagentStatusFailed, NativeSubagentStatusInterrupted:
		return true
	default:
		return false
	}
}

const (
	// NativeSubagentStatusUnknown means the harness proved a spawn but not its lifecycle state.
	NativeSubagentStatusUnknown NativeSubagentStatus = "unknown"
	// NativeSubagentStatusRunning means the harness reported an active lifecycle.
	NativeSubagentStatusRunning NativeSubagentStatus = "running"
	// NativeSubagentStatusPaused means the harness reported work that is not
	// running but also not finished, such as a resumable detached run. It is not
	// terminal: a later running observation may resume the lifecycle.
	NativeSubagentStatusPaused NativeSubagentStatus = "paused"
	// NativeSubagentStatusCompleted means the harness reported successful completion.
	NativeSubagentStatusCompleted NativeSubagentStatus = "completed"
	// NativeSubagentStatusFailed means the harness reported a failed lifecycle.
	NativeSubagentStatusFailed NativeSubagentStatus = "failed"
	// NativeSubagentStatusInterrupted means the harness reported an interrupted lifecycle.
	NativeSubagentStatusInterrupted NativeSubagentStatus = "interrupted"
)

// NativeSubagentScope distinguishes an individual agent from aggregate orchestration.
type NativeSubagentScope string

const (
	// NativeSubagentScopeAgent represents one delegated agent.
	NativeSubagentScopeAgent NativeSubagentScope = "agent"
	// NativeSubagentScopeBatch represents a batch or chain without per-agent lifecycle evidence.
	NativeSubagentScopeBatch NativeSubagentScope = "batch"
)

// NativeSubagent is one harness-owned native subagent lifecycle. ID and
// GroupID are opaque harness identities. Optional fields are omitted when the
// harness does not expose them.
type NativeSubagent struct {
	ToolUseID string               `json:"tool_use_id,omitempty"`
	Scope     NativeSubagentScope  `json:"scope,omitempty"`
	ID        string               `json:"id"`
	GroupID   string               `json:"group_id,omitempty"`
	Label     string               `json:"label,omitempty"`
	Prompt    string               `json:"prompt,omitempty"`
	Status    NativeSubagentStatus `json:"status"`
	Result    string               `json:"result,omitempty"`
	// Background reports that the harness launched the delegation independently
	// of the parent turn, so it can still be running after that turn's result.
	// Only such a card justifies keeping a task running past its trailing result;
	// a foreground delegation always settles before the parent result arrives.
	Background bool `json:"background,omitempty"`
}

// NativeSubagentMessage records an observed native-subagent lifecycle update.
// It is harness activity within the parent session, not an independently
// runnable session.
type NativeSubagentMessage struct {
	Subagent NativeSubagent `json:"subagent"`
}

// Type implements Message.
func (m *NativeSubagentMessage) Type() string { return "native_subagent" }

// BackgroundCommandStatus describes the lifecycle state a harness actually
// reported for a detached shell command. The vocabulary is deliberately
// minimal: a shell command only runs and then settles, so there is no paused
// or unknown state and none of NativeSubagentStatus's resumable semantics.
type BackgroundCommandStatus string

// Terminal reports whether the status ends a background-command lifecycle.
func (s BackgroundCommandStatus) Terminal() bool {
	switch s {
	case BackgroundCommandStatusCompleted, BackgroundCommandStatusFailed, BackgroundCommandStatusInterrupted:
		return true
	default:
		return false
	}
}

const (
	// BackgroundCommandStatusRunning means the harness reported the command as active.
	BackgroundCommandStatusRunning BackgroundCommandStatus = "running"
	// BackgroundCommandStatusCompleted means the harness reported the command finished.
	BackgroundCommandStatusCompleted BackgroundCommandStatus = "completed"
	// BackgroundCommandStatusFailed means the harness reported the command failed.
	BackgroundCommandStatusFailed BackgroundCommandStatus = "failed"
	// BackgroundCommandStatusInterrupted means the harness reported the command
	// was killed, stopped, or otherwise cut short before finishing.
	BackgroundCommandStatusInterrupted BackgroundCommandStatus = "interrupted"
)

// BackgroundCommand is one harness-owned detached shell command. ID is an
// opaque harness identity, prefixed per harness ("claude:shell:", ...) so
// card sets from different adapters never collide in logs. ExitCode is the
// canonical numeric outcome the harness reported; it stays nil when the
// harness did not expose one, even for a terminal status.
type BackgroundCommand struct {
	ID     string                  `json:"id"`
	Label  string                  `json:"label,omitempty"`
	Status BackgroundCommandStatus `json:"status"`
	Result string                  `json:"result,omitempty"`
	// ExitCode is the command's exit status when the harness reported one.
	ExitCode *int `json:"exit_code,omitempty"`
	// OutputRef is an opaque reference to the command's captured output (a
	// harness-owned path or buffer id); it is not a caic-owned resource.
	OutputRef string `json:"output_ref,omitempty"`
	// ToolUseID correlates the command with the tool call that spawned it.
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// BackgroundCommandMessage records an observed background-command lifecycle
// update. It is informational harness activity: unlike a background subagent,
// a detached shell command never justifies keeping its task running.
type BackgroundCommandMessage struct {
	Command BackgroundCommand `json:"command"`
}

// Type implements Message.
func (m *BackgroundCommandMessage) Type() string { return "background_command" }

// MaxWidgetHTMLBytes is the maximum size of widget HTML the backend will
// forward to clients. Widgets exceeding this limit are replaced with an
// error message.
const MaxWidgetHTMLBytes = 256 * 1024 // 256 KB

// WidgetToolNames is the set of tool names that produce HTML widgets.
// Each harness parser checks this set to decide whether a tool_use block
// should emit WidgetMessage instead of ToolUseMessage.
var WidgetToolNames = map[string]struct{}{
	"show_widget":              {}, // Direct tool name.
	"mcp__widget__show_widget": {}, // MCP-prefixed (server "widget", tool "show_widget").
	"mcp__plugin_caic-widget_widget__show_widget": {}, // Plugin MCP-prefixed.
}

// WidgetMessage is emitted when the agent produces an interactive HTML widget
// via a tool call (e.g. Claude's show_widget). The HTML is the complete,
// renderable widget code.
type WidgetMessage struct {
	ToolUseID string `json:"id"`
	Title     string `json:"title"`
	HTML      string `json:"html"`
}

// NewWidgetMessage creates a WidgetMessage from raw tool input JSON. It
// extracts the title and widget_code fields and enforces MaxWidgetHTMLBytes.
// Shared by all backend parsers.
func NewWidgetMessage(toolUseID string, input json.RawMessage) *WidgetMessage {
	var w widgetInput
	if len(input) > 0 {
		_ = json.Unmarshal(input, &w)
	}
	html := w.WidgetCode
	if len(html) > MaxWidgetHTMLBytes {
		html = `<p style="color:red;font-family:system-ui">Widget too large (256 KB limit exceeded)</p>`
	}
	return &WidgetMessage{
		ToolUseID: toolUseID,
		Title:     w.Title,
		HTML:      html,
	}
}

// Type implements Message.
func (m *WidgetMessage) Type() string { return "widget" }

// widgetInput is the expected JSON schema for the show_widget tool's input.
type widgetInput struct {
	Title      string `json:"title"`
	WidgetCode string `json:"widget_code"`
}

// WidgetDeltaMessage is a streaming fragment of widget HTML, emitted as the
// agent generates the widget code. Clients accumulate deltas for progressive
// rendering; the final WidgetMessage replaces them.
type WidgetDeltaMessage struct {
	ToolUseID string
	Delta     string // Partial HTML fragment.
}

// Type implements Message.
func (m *WidgetDeltaMessage) Type() string { return "widget_delta" }

// QuotaProvider identifies a monitored quota source. Add a value here when
// adding a provider fetcher or harness quota adapter so their identifiers stay
// coupled at compile time.
type QuotaProvider string

// QuotaProviderForModel maps a harness-reported model ID (for example
// "zai/glm-5.3-flash" or "openrouter/z-ai/glm-5.3-flash") to the quota
// provider that bills it. Returns "" when no quota provider matches.
func QuotaProviderForModel(model string) QuotaProvider {
	provider, _, _ := strings.Cut(strings.ToLower(model), "/")
	return quotaProviderModelAliases[provider]
}

// Valid reports whether p is a supported quota provider.
func (p QuotaProvider) Valid() bool {
	switch p {
	case QuotaProviderAlibaba, QuotaProviderAnthropic, QuotaProviderCerebras,
		QuotaProviderClaudeCode, QuotaProviderCodex, QuotaProviderDeepSeek,
		QuotaProviderGemini, QuotaProviderGrok, QuotaProviderGroq,
		QuotaProviderOpenRouter, QuotaProviderRunInfra, QuotaProviderTypeSafe,
		QuotaProviderXiaomi, QuotaProviderZai:
		return true
	default:
		return false
	}
}

const (
	// QuotaProviderAnthropic identifies direct Anthropic API usage.
	QuotaProviderAnthropic QuotaProvider = "anthropic"
	// QuotaProviderClaudeCode identifies a Claude Code OAuth subscription.
	QuotaProviderClaudeCode QuotaProvider = "claudecode"
	// QuotaProviderCodex identifies Codex usage.
	QuotaProviderCodex QuotaProvider = "codex"
	// QuotaProviderAlibaba identifies Alibaba Cloud Model Studio (DashScope) API usage.
	QuotaProviderAlibaba QuotaProvider = "alibaba"
	// QuotaProviderCerebras identifies Cerebras Inference API usage.
	QuotaProviderCerebras QuotaProvider = "cerebras"
	// QuotaProviderDeepSeek identifies DeepSeek API usage.
	QuotaProviderDeepSeek QuotaProvider = "deepseek"
	// QuotaProviderGemini identifies Google Gemini API (AI Studio) usage.
	QuotaProviderGemini QuotaProvider = "gemini"
	// QuotaProviderGrok identifies xAI Grok API usage.
	QuotaProviderGrok QuotaProvider = "grok"
	// QuotaProviderGroq identifies Groq API usage.
	QuotaProviderGroq QuotaProvider = "groq"
	// QuotaProviderOpenRouter identifies OpenRouter API usage.
	QuotaProviderOpenRouter QuotaProvider = "openrouter"
	// QuotaProviderRunInfra identifies RunInfra Model APIs usage.
	QuotaProviderRunInfra QuotaProvider = "runinfra"
	// QuotaProviderTypeSafe identifies TypeSafe API usage.
	QuotaProviderTypeSafe QuotaProvider = "typesafe"
	// QuotaProviderXiaomi identifies Xiaomi MiMo API usage.
	QuotaProviderXiaomi QuotaProvider = "xiaomi"
	// QuotaProviderZai identifies Z.ai (Zhipu) API usage.
	QuotaProviderZai QuotaProvider = "zai"
)

// String returns the human-readable provider name shown in the UI.
func (p QuotaProvider) String() string {
	switch p {
	case QuotaProviderAlibaba:
		return "Alibaba"
	case QuotaProviderAnthropic:
		return "Anthropic"
	case QuotaProviderCerebras:
		return "Cerebras"
	case QuotaProviderClaudeCode:
		return "Claude Code"
	case QuotaProviderCodex:
		return "Codex"
	case QuotaProviderDeepSeek:
		return "DeepSeek"
	case QuotaProviderGemini:
		return "Gemini"
	case QuotaProviderGrok:
		return "Grok"
	case QuotaProviderGroq:
		return "Groq"
	case QuotaProviderOpenRouter:
		return "OpenRouter"
	case QuotaProviderRunInfra:
		return "RunInfra"
	case QuotaProviderTypeSafe:
		return "TypeSafe"
	case QuotaProviderXiaomi:
		return "Xiaomi MiMo"
	case QuotaProviderZai:
		return "Z.ai"
	default:
		return string(p)
	}
}

// quotaProviderModelAliases maps harness model provider prefixes (the part of
// a model ID before the first "/") to the quota provider that bills them.
var quotaProviderModelAliases = map[string]QuotaProvider{
	"alibaba":         QuotaProviderAlibaba,
	"anthropic":       QuotaProviderAnthropic,
	"bigmodel":        QuotaProviderZai,
	"cerebras":        QuotaProviderCerebras,
	"claudecode":      QuotaProviderClaudeCode,
	"codex":           QuotaProviderCodex,
	"dashscope":       QuotaProviderAlibaba,
	"deepseek":        QuotaProviderDeepSeek,
	"gemini":          QuotaProviderGemini,
	"google":          QuotaProviderGemini,
	"grok":            QuotaProviderGrok,
	"groq":            QuotaProviderGroq,
	"openai-codex":    QuotaProviderCodex,
	"openrouter":      QuotaProviderOpenRouter,
	"runinfra":        QuotaProviderRunInfra,
	"typesafe":        QuotaProviderTypeSafe,
	"xiaomi":          QuotaProviderXiaomi,
	"z-ai":            QuotaProviderZai,
	"zai":             QuotaProviderZai,
	"zai-coding-plan": QuotaProviderZai,
	"zhipu":           QuotaProviderZai,
}

// RateLimitStatus describes whether a provider accepted or rejected a request
// for a quota window.
type RateLimitStatus string

// Valid reports whether s is a supported rate-limit status.
func (s RateLimitStatus) Valid() bool {
	switch s {
	case RateLimitStatusAllowed, RateLimitStatusAllowedWarning, RateLimitStatusRejected:
		return true
	default:
		return false
	}
}

const (
	// QuotaWindowRequest marks a per-request rejection reported without a
	// quota window, such as a harness relaying only an error message.
	QuotaWindowRequest = "request"
)

const (
	// RateLimitStatusAllowed means the provider accepted the request.
	RateLimitStatusAllowed RateLimitStatus = "allowed"
	// RateLimitStatusAllowedWarning means the provider accepted the request and warned of high usage.
	RateLimitStatusAllowedWarning RateLimitStatus = "allowed_warning"
	// RateLimitStatusRejected means the provider rejected the request for quota exhaustion.
	RateLimitStatusRejected RateLimitStatus = "rejected"
)

// RateLimitMessage is emitted when the CLI reports a rate limit status change.
type RateLimitMessage struct {
	Status          RateLimitStatus `json:"status"`            // "allowed", "allowed_warning", "rejected".
	ResetsAt        time.Time       `json:"resets_at"`         // When the quota window resets; zero if unknown.
	RateLimitType   string          `json:"rate_limit_type"`   // Harness-native window ID (for example, "five_hour"); use QuotaWindow for the canonical ID.
	Utilization     float64         `json:"utilization"`       // Fraction of the window used in [0, 1], not a percentage; 0 if unknown.
	IsUsingOverage  bool            `json:"is_using_overage"`  // True when extra/overage usage is active.
	OverageResetsAt time.Time       `json:"overage_resets_at"` // When overage resets; zero if unknown.
	QuotaProvider   QuotaProvider   `json:"quota_provider"`    // Canonical usage-provider ID that matches ProviderQuota.Provider; empty when the harness cannot identify it.
	QuotaLabel      string          `json:"quota_label"`       // Human-readable canonical provider label.
	QuotaWindow     string          `json:"quota_window"`      // Canonical provider window ID; empty when unknown.
}

// Type implements Message.
func (m *RateLimitMessage) Type() string { return "rate_limit" }

// RawMessage is a pass-through for message types we don't need to inspect
// (tool_progress, etc.).
type RawMessage struct {
	MessageType string
	Raw         []byte
}

// Type implements Message.
func (m *RawMessage) Type() string { return m.MessageType }

// ParseErrorMessage is emitted when a backend output line cannot be decoded.
// It carries the error and the raw line for diagnostic display.
type ParseErrorMessage struct {
	Err  string
	Line string
}

// Type implements Message.
func (m *ParseErrorMessage) Type() string { return "parse_error" }

// LogMessage is a provisioning/startup log line from the container backend.
type LogMessage struct {
	MessageType string `json:"type"`
	Line        string `json:"line"`
}

// Type implements Message.
func (m *LogMessage) Type() string { return messageTypeProvisioningLog }

// StrippedEnvMessage is emitted by the relay when it strips environment
// variables (e.g. ANTHROPIC_API_KEY) before spawning the agent subprocess.
// The backend uses these values to re-inject them after auth completes.
type StrippedEnvMessage struct {
	MessageType string            `json:"type"`
	Variables   map[string]string `json:"variables"`
}

// Type implements Message.
func (m *StrippedEnvMessage) Type() string { return messageTypeStrippedEnv }

// DiffStatMessage is emitted periodically by the relay's diff watcher thread
// and by the backend after mutating tool calls, with the current in-container
// git diff stats.
type DiffStatMessage struct {
	MessageType string   `json:"type"`
	DiffStat    DiffStat `json:"diff_stat"`
	// Repos carries one compact git state per task repository, filled by the
	// backend's post-tool probe. The relay watcher omits it.
	Repos []RepoState `json:"repos,omitempty"`
	Ts    float64     `json:"ts,omitempty"` // Unix epoch seconds (ms precision) when the relay emitted this record.
}

// Type implements Message.
func (m *DiffStatMessage) Type() string { return messageTypeDiffStat }

// RepoState is the compact git state of one task repository: exactly what the
// task card and detail header render, without per-file details or history.
type RepoState struct {
	RepoIndex        int    `json:"repo_index"`
	Branch           string `json:"branch"`
	Operation        string `json:"operation,omitempty"` // runtime.RepositoryOperation while a merge/rebase is in progress.
	Ahead            int    `json:"ahead"`
	Behind           int    `json:"behind"`
	ChangedFiles     int    `json:"changed_files"`
	LinesAdded       int    `json:"added"`
	LinesDeleted     int    `json:"deleted"`
	UncommittedFiles int    `json:"uncommitted"`
	Conflicts        int    `json:"conflicts"`
}

// ExitMessage is written by the relay to output.jsonl when the agent
// subprocess exits, regardless of shutdown reason (crash, sentinel, EOF).
// It carries the exit code, command, signal, stderr, and timestamp so the
// backend can diagnose why a relay session ended without parsing relay.log.
type ExitMessage struct {
	MessageType     string   `json:"type"`
	ExitCode        int      `json:"exit_code"`
	Command         []string `json:"cmd,omitempty"`
	Signal          int      `json:"signal,omitempty"`
	Error           string   `json:"error,omitempty"`
	StderrTruncated bool     `json:"stderr_truncated,omitempty"`
	Ts              float64  `json:"ts,omitempty"`
}

// Type implements Message.
func (m *ExitMessage) Type() string { return messageTypeExit }

// ExitError returns the user-facing diagnostic for a non-zero process exit.
func (m *ExitMessage) ExitError() string {
	if m.Error != "" {
		return m.Error
	}
	return fmt.Sprintf("agent subprocess exited with code %d", m.ExitCode)
}

// MetaRepo describes one repository entry in a MetaMessage.
type MetaRepo struct {
	Name          string `json:"name"`
	BaseBranch    string `json:"base_branch,omitempty"`
	Branch        string `json:"branch"`
	ContainerPath string `json:"containerPath,omitempty"`
}

// MetaCacheMount describes one cache mount in a MetaMessage.
type MetaCacheMount struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	HostPath    string `json:"hostPath,omitempty"`
	// ContainerPath is the resolved target path in the runtime container.
	ContainerPath string `json:"containerPath,omitempty"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
	Shallow       bool   `json:"shallow,omitempty"`
}

// MetaMount describes one custom bind mount in a MetaMessage.
type MetaMount struct {
	HostPath string `json:"hostPath,omitempty"`
	// ContainerPath is the resolved target path in the runtime container.
	ContainerPath string `json:"containerPath,omitempty"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
}

// LogVersion identifies a physical task-log format.
type LogVersion int

// Validate rejects unsupported task-log versions.
func (v LogVersion) Validate() error {
	switch v {
	case LogVersionV1, LogVersionV2, LogVersionV3:
		return nil
	default:
		return fmt.Errorf("unsupported log version %d", v)
	}
}

const (
	// LogVersionV1 is the legacy bare-harness task-log format.
	LogVersionV1 LogVersion = 1
	// LogVersionV2 is the caic-enveloped task-log format.
	LogVersionV2 LogVersion = 2
	// LogVersionV3 distinguishes relay output from caic-to-harness input.
	LogVersionV3 LogVersion = 3
)

// MetaMessage is written as the first line of a JSONL log file. It captures
// task-level metadata so logs can be reloaded on restart.
//
// Its serialized task-log schema must remain backward-readable; API DTOs may
// evolve independently.
type MetaMessage struct {
	MessageType       string           `json:"type"`
	Version           int              `json:"version"`
	Prompt            string           `json:"prompt"`
	Title             string           `json:"title,omitempty"`
	Repos             []MetaRepo       `json:"repos"`
	Harness           harness.Name     `json:"harness"`
	RequestedModel    string           `json:"model,omitempty"`
	RequestedEffort   string           `json:"effort,omitempty"`
	StartedAt         time.Time        `json:"started_at"`
	ForgeIssue        int              `json:"forge_issue,omitempty"` // Originating issue/PR number for bot comment callbacks.
	OwnerID           string           `json:"owner_id,omitempty"`    // Human authorization principal; distinct from task lineage.
	ForkedFromTaskID  string           `json:"forked_from_task_id,omitempty"`
	ParentTaskID      string           `json:"parent_task_id,omitempty"` // Delegating task identity; empty for roots and ordinary forks.
	CaicMCP           bool             `json:"caic_mcp,omitempty"`       // Enables task-scoped CAIC MCP delegation.
	Tailscale         bool             `json:"tailscale,omitempty"`
	USB               bool             `json:"usb,omitempty"`
	Display           bool             `json:"display,omitempty"`
	Sudo              bool             `json:"sudo,omitempty"`
	GitHubToken       bool             `json:"gitHubToken,omitempty"`
	RuntimeName       string           `json:"runtimeName,omitempty"`
	BaseImage         string           `json:"baseImage,omitempty"`
	ContainerPlatform string           `json:"containerPlatform,omitempty"`
	MaxCPUs           int              `json:"maxCPUs,omitempty"`
	CacheMounts       []MetaCacheMount `json:"cacheMounts,omitempty"`
	Mounts            []MetaMount      `json:"mounts,omitempty"`
}

// Type implements Message.
func (m *MetaMessage) Type() string { return messageTypeMeta }

// Validate checks that all required fields are present and the version is supported.
func (m *MetaMessage) Validate() error {
	if m.MessageType != messageTypeMeta {
		return fmt.Errorf("unexpected type %q", m.MessageType)
	}
	if err := LogVersion(m.Version).Validate(); err != nil {
		return err
	}
	if m.Prompt == "" {
		return errors.New("missing prompt")
	}
	if m.Harness == "" {
		return errors.New("missing harness")
	}
	return nil
}

// MetaSessionMessage records the backend-native session identifier needed to
// resume a stateful harness after server restart.
//
// Its serialized task-log schema must remain backward-readable; API DTOs may
// evolve independently.
type MetaSessionMessage struct {
	MessageType    string `json:"type"`
	SessionID      string `json:"session_id"`
	ReportedModel  string `json:"model,omitempty"`
	ReportedEffort string `json:"reported_effort,omitempty"`
	AgentVersion   string `json:"agent_version,omitempty"`
}

// Type implements Message.
func (m *MetaSessionMessage) Type() string { return messageTypeSession }

// RelayGenerationMessage marks the durable-log boundary corresponding to a
// newly created relay output file. It is excluded from the visible timeline.
type RelayGenerationMessage struct {
	MessageType string `json:"type"`
	Generation  string `json:"generation"`
}

// Type implements Message.
func (m *RelayGenerationMessage) Type() string { return messageTypeRelayGeneration }

// LogSink appends complete task-log records through the task-owned physical
// log authority. Native records are already encoded physical records; semantic
// messages are backend controls encoded according to the owned log version.
type LogSink interface {
	LogVersion() LogVersion
	AppendNative(data []byte) error
	AppendMessage(message Message) error
	Close() error
}

// DiscardLogSink ignores task-log records for non-persistent agent operations.
// Version must be set to the physical record version represented by the caller.
type DiscardLogSink struct {
	Version LogVersion
}

// LogVersion returns the physical record version represented by the discarded records.
func (s DiscardLogSink) LogVersion() LogVersion { return s.Version }

// AppendNative discards one native physical record.
func (DiscardLogSink) AppendNative([]byte) error { return nil }

// AppendMessage discards one caic control record.
func (DiscardLogSink) AppendMessage(Message) error { return nil }

// Close releases no resources.
func (DiscardLogSink) Close() error { return nil }

var _ LogSink = DiscardLogSink{}

// AppendInputNativeRecord appends bytes caic sent to a harness's stdin in the
// exact physical task-log format. V3 persists them separately from relay
// output so protocol commands remain durable without becoming relay offsets.
func AppendInputNativeRecord(log LogSink, version LogVersion, data []byte) error {
	token := logRecordAgent
	if version == LogVersionV3 {
		token = logRecordInput
	}
	return appendNativeRecord(log, version, token, data)
}

func appendNativeRecord(log LogSink, version LogVersion, token logRecordType, data []byte) error {
	if err := version.Validate(); err != nil {
		return err
	}
	data = bytes.TrimSuffix(data, []byte{'\n'})
	if len(data) == 0 {
		return nil
	}
	if version != LogVersionV1 {
		ts := time.Now().UTC()
		data = fmt.Appendf(nil, `{"t":"%s","ts":%d.%03d,"msg":%s}`, token, ts.Unix(), ts.Nanosecond()/int(time.Millisecond), data)
		data = append(data, '\n')
	} else {
		data = append(data, '\n')
	}
	return log.AppendNative(data)
}

// WriteMetaSession appends a caic_session control record for init metadata.
func WriteMetaSession(log LogSink, init *InitMessage) error {
	if init.SessionID == "" && init.ReportedModel == "" && init.ReportedEffort == "" && init.Version == "" {
		return nil
	}
	return log.AppendMessage(&MetaSessionMessage{
		MessageType:    messageTypeSession,
		SessionID:      init.SessionID,
		ReportedModel:  init.ReportedModel,
		ReportedEffort: init.ReportedEffort,
		AgentVersion:   init.Version,
	})
}

// ModelInfoMessage records a harness-reported context window for replay.
type ModelInfoMessage struct {
	MessageType   string `json:"type"`
	ContextWindow int64  `json:"context_window"`
}

// Type implements Message.
func (m *ModelInfoMessage) Type() string { return messageTypeModelInfo }

// MetaResultMessage is appended as the last line of a JSONL log file when a
// task reaches a terminal state.
type MetaResultMessage struct {
	MessageType              string   `json:"type"`
	State                    string   `json:"state"`
	Title                    string   `json:"title,omitempty"`
	CostUSD                  float64  `json:"cost_usd,omitempty"`
	Duration                 float64  `json:"duration,omitempty"` // Seconds.
	NumTurns                 int      `json:"num_turns,omitempty"`
	InputTokens              int      `json:"input_tokens,omitempty"`
	OutputTokens             int      `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int      `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int      `json:"cache_read_input_tokens,omitempty"`
	ReasoningOutputTokens    int      `json:"reasoning_output_tokens,omitempty"`
	DiffStat                 DiffStat `json:"diff_stat,omitzero"`
	// TODO(2026-10-01): Make DiskUsedBytes an int64 value using -1 for
	// unavailable measurements after legacy result records have aged out.
	DiskUsedBytes  *int64          `json:"disk_used_bytes,omitempty"`
	Error          string          `json:"error,omitempty"`
	AgentResult    string          `json:"agent_result,omitempty"`
	StartupFailure *StartupFailure `json:"startup_failure,omitempty"`
}

// Type implements Message.
func (m *MetaResultMessage) Type() string { return messageTypeResult }

// StartupFailure identifies the failed task-start phase and original agent diagnostic.
type StartupFailure struct {
	Harness string `json:"harness"`
	Phase   string `json:"phase"`
	Cause   string `json:"cause"`
}

// MetaPRMessage is written to the JSONL log when a PR is created so that the
// PR number can be restored on server restart.
type MetaPRMessage struct {
	MessageType string `json:"type"`
	ForgeOwner  string `json:"forge_owner"`
	ForgeRepo   string `json:"forge_repo"`
	ForgePR     int    `json:"forge_pr"`
}

// Type implements Message.
func (m *MetaPRMessage) Type() string { return messageTypePR }

// MarshalMessage serializes a Message to JSON. For RawMessage, returns the
// original bytes to preserve unknown fields. For typed messages, uses
// json.Marshal.
//
// Typed message JSON is a durable task-log schema when written through a
// LogSink. Keep JSON tags and their meanings backward-compatible with logs
// written by released binaries; rename Go fields without renaming their tags.
func MarshalMessage(m Message) ([]byte, error) {
	if rm, ok := m.(*RawMessage); ok {
		return rm.Raw, nil
	}
	return json.Marshal(m)
}

// MarshalLogMessage encodes one semantic backend control record for version.
func MarshalLogMessage(version LogVersion, m Message) ([]byte, error) {
	if err := version.Validate(); err != nil {
		return nil, err
	}
	if _, ok := m.(*RawMessage); ok {
		return nil, errors.New("raw messages must be appended as native records")
	}
	if version == LogVersionV1 {
		return marshalV1LogMessage(m)
	}
	data, err := MarshalMessage(m)
	if err != nil {
		return data, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	delete(fields, "type")
	token, err := v2ControlToken(m)
	if err != nil {
		return nil, err
	}
	fields["t"], err = json.Marshal(token)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func v2ControlToken(m Message) (logRecordType, error) {
	switch m.Type() {
	case messageTypeMeta:
		return logRecordMeta, nil
	case messageTypeDiffStat:
		return logRecordDiffStat, nil
	case messageTypeExit:
		return logRecordExit, nil
	case messageTypeStrippedEnv:
		return logRecordStrippedEnv, nil
	case messageTypeSession:
		return logRecordSession, nil
	case messageTypeModelInfo:
		return logRecordModelInfo, nil
	case messageTypePR:
		return logRecordPR, nil
	case messageTypeResult:
		return logRecordResult, nil
	case messageTypeTurnCommitSnapshot:
		return logRecordTurnCommitSnapshot, nil
	case messageTypePendingUserAction:
		return logRecordPendingUserAction, nil
	case messageTypeProvisioningLog:
		return logRecordProvisioningLog, nil
	case messageTypeText:
		return logRecordText, nil
	case messageTypeUserInput:
		return logRecordUserInput, nil
	case messageTypeRelayGeneration:
		return logRecordRelayGeneration, nil
	case messageTypeSystem:
		if m, ok := m.(*SystemMessage); ok && m.Subtype == messageSubtypeContextCleared {
			return logRecordContextCleared, nil
		}
	}
	return "", fmt.Errorf("message type %q is not a task-log control", m.Type())
}
