// Shared types and message definitions for the agent abstraction layer.

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// DiffFileStat describes changes to a single file.
type DiffFileStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// DiffStat summarises the changes in a branch relative to its base.
type DiffStat []DiffFileStat

// Message is the interface for all agent streaming messages.
type Message interface {
	// Type returns the message type string.
	Type() string
}

// ParsedMessage pairs a semantic message with its producer timestamp. A zero
// ProducerTime means the physical record did not provide producer time.
type ParsedMessage struct {
	Message      Message
	ProducerTime time.Time
}

// InitMessage is emitted when a session starts.
type InitMessage struct {
	SessionID string   `json:"session_id"`
	Cwd       string   `json:"cwd"`
	Tools     []string `json:"tools"`
	Model     string   `json:"model"`
	Version   string   `json:"claude_code_version"`
	Effort    string   `json:"effort,omitempty"` // Thinking effort (e.g. "low", "medium", "high", "max"). Empty when not supported.
}

// Type implements Message.
func (m *InitMessage) Type() string { return "init" }

// SystemMessage is a generic system message (status, compact_boundary, etc.).
type SystemMessage struct {
	MessageType string `json:"type"`
	Subtype     string `json:"subtype"`
	SessionID   string `json:"session_id"`
	UUID        string `json:"uuid"`
	Detail      string `json:"detail,omitempty"` // Optional human-readable detail (e.g. model names for model_rerouted).
	Model       string `json:"model,omitempty"`  // Active model after model_rerouted; used to update task.reportedModel.
}

// Type implements Message.
func (m *SystemMessage) Type() string { return "system" }

// TextMessage is emitted when the agent produces text output.
type TextMessage struct {
	Text  string `json:"text"`
	Phase string `json:"phase,omitempty"` // Codex only: "commentary" | "final_answer" | "".
}

// Type implements Message.
func (m *TextMessage) Type() string { return "text" }

// ToolUseMessage is emitted when the agent invokes a tool (except
// AskUserQuestion and TodoWrite which have their own types).
type ToolUseMessage struct {
	ToolUseID   string          `json:"id"`
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input,omitempty"`
	Detail      string          `json:"detail,omitempty"` // Backend-normalized short display detail for tool headers.
	InputView   ToolInputView   `json:"input_view,omitzero"`
	PlanContent string          `json:"-"` // Snapshot of plan content; set by task on ExitPlanMode.
}

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

// Type implements Message.
func (m *ToolUseMessage) Type() string { return "tool_use" }

// AskMessage is emitted when the agent asks the user a question via the
// AskUserQuestion tool.
type AskMessage struct {
	ToolUseID string        `json:"id"`
	Questions []AskQuestion `json:"questions"`
}

// Type implements Message.
func (m *AskMessage) Type() string { return "ask" }

// PendingUserActionMessageType identifies a persisted pending user action.
const PendingUserActionMessageType = "caic_pending_user_action"

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

// ClonePendingUserActions returns a deep copy of actions.
func ClonePendingUserActions(actions []PendingUserAction) []PendingUserAction {
	if len(actions) == 0 {
		return nil
	}
	out := make([]PendingUserAction, len(actions))
	for i := range actions {
		out[i] = ClonePendingUserAction(actions[i])
	}
	return out
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
func (m *UserInputMessage) Type() string { return "user_input" }

// ToolResultMessage is emitted when a tool returns its result.
type ToolResultMessage struct {
	ToolUseID string `json:"tool_use_id"`
	Error     string `json:"error,omitempty"` // Non-empty when the tool reported an error.
}

// Type implements Message.
func (m *ToolResultMessage) Type() string { return "tool_result" }

// UsageMessage reports token consumption for a single API call.
type UsageMessage struct {
	Usage         Usage  `json:"usage"`
	Model         string `json:"model,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"` // Non-zero when the backend reports the active context window size.
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

// ResultMessage is the terminal message for a query.
type ResultMessage struct {
	MessageType   string   `json:"type"`
	Subtype       string   `json:"subtype"`
	IsError       bool     `json:"is_error"`
	DurationMs    int64    `json:"duration_ms"`
	DurationAPIMs int64    `json:"duration_api_ms"`
	NumTurns      int      `json:"num_turns"`
	Result        string   `json:"result"`
	SessionID     string   `json:"session_id"`
	TotalCostUSD  float64  `json:"total_cost_usd"`
	Usage         Usage    `json:"usage"`
	UUID          string   `json:"uuid"`
	DiffStat      DiffStat `json:"diff_stat,omitzero"` // Set by caic after running container diff.
}

// Type implements Message.
func (m *ResultMessage) Type() string { return "result" }

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

// SubagentStartMessage is emitted when a subagent task begins.
type SubagentStartMessage struct {
	TaskID      string `json:"task_id"`
	Description string `json:"description"`
}

// Type implements Message.
func (m *SubagentStartMessage) Type() string { return "subagent_start" }

// SubagentEndMessage is emitted when a subagent task completes, fails, or stops.
type SubagentEndMessage struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"` // "completed", "failed", "stopped"
}

// Type implements Message.
func (m *SubagentEndMessage) Type() string { return "subagent_end" }

// MaxWidgetHTMLBytes is the maximum size of widget HTML the backend will
// forward to clients. Widgets exceeding this limit are replaced with an
// error message.
const MaxWidgetHTMLBytes = 256 * 1024 // 256 KB

// WidgetToolNames is the set of tool names that produce HTML widgets.
// Each harness parser checks this set to decide whether a tool_use block
// should emit WidgetMessage instead of ToolUseMessage.
var WidgetToolNames = map[string]struct{}{
	"show_widget":                                 {}, // Direct tool name.
	"mcp__widget__show_widget":                    {}, // MCP-prefixed (server "widget", tool "show_widget").
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

// widgetInput is the expected JSON schema for the show_widget tool's input.
type widgetInput struct {
	Title      string `json:"title"`
	WidgetCode string `json:"widget_code"`
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

const (
	// QuotaProviderAnthropic identifies direct Anthropic API usage.
	QuotaProviderAnthropic QuotaProvider = "anthropic"
	// QuotaProviderClaudeCode identifies a Claude Code OAuth subscription.
	QuotaProviderClaudeCode QuotaProvider = "claudecode"
	// QuotaProviderCodex identifies Codex usage.
	QuotaProviderCodex QuotaProvider = "codex"
	// QuotaProviderDeepSeek identifies DeepSeek API usage.
	QuotaProviderDeepSeek QuotaProvider = "deepseek"
	// QuotaProviderOpenRouter identifies OpenRouter API usage.
	QuotaProviderOpenRouter QuotaProvider = "openrouter"
	// QuotaProviderXiaomi identifies Xiaomi MiMo API usage.
	QuotaProviderXiaomi QuotaProvider = "xiaomi"
)

// Valid reports whether p is a supported quota provider.
func (p QuotaProvider) Valid() bool {
	switch p {
	case QuotaProviderAnthropic, QuotaProviderClaudeCode, QuotaProviderCodex,
		QuotaProviderDeepSeek, QuotaProviderOpenRouter, QuotaProviderXiaomi:
		return true
	default:
		return false
	}
}

// RateLimitStatus describes whether a provider accepted or rejected a request
// for a quota window.
type RateLimitStatus string

const (
	// RateLimitStatusAllowed means the provider accepted the request.
	RateLimitStatusAllowed RateLimitStatus = "allowed"
	// RateLimitStatusAllowedWarning means the provider accepted the request and warned of high usage.
	RateLimitStatusAllowedWarning RateLimitStatus = "allowed_warning"
	// RateLimitStatusRejected means the provider rejected the request for quota exhaustion.
	RateLimitStatusRejected RateLimitStatus = "rejected"
)

// Valid reports whether s is a supported rate-limit status.
func (s RateLimitStatus) Valid() bool {
	switch s {
	case RateLimitStatusAllowed, RateLimitStatusAllowedWarning, RateLimitStatusRejected:
		return true
	default:
		return false
	}
}

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
func (m *LogMessage) Type() string { return "log" }

// StrippedEnvMessage is emitted by the relay when it strips environment
// variables (e.g. ANTHROPIC_API_KEY) before spawning the agent subprocess.
// The backend uses these values to re-inject them after auth completes.
type StrippedEnvMessage struct {
	MessageType string            `json:"type"`
	Variables   map[string]string `json:"variables"`
}

// Type implements Message.
func (m *StrippedEnvMessage) Type() string { return "caic_stripped_env" }

// DiffStatMessage is emitted periodically by the relay's diff watcher thread
// with the current in-container git diff stats.
type DiffStatMessage struct {
	MessageType string   `json:"type"`
	DiffStat    DiffStat `json:"diff_stat"`
	Ts          float64  `json:"ts,omitempty"` // Unix epoch seconds (ms precision) when the relay emitted this record.
}

// Type implements Message.
func (m *DiffStatMessage) Type() string { return "caic_diff_stat" }

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
func (m *ExitMessage) Type() string { return "caic_exit" }

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

const (
	// LogVersionV1 is the legacy bare-harness task-log format.
	LogVersionV1 LogVersion = 1
	// LogVersionV2 is the caic-enveloped task-log format.
	LogVersionV2 LogVersion = 2
)

// Validate rejects unsupported task-log versions.
func (v LogVersion) Validate() error {
	switch v {
	case LogVersionV1, LogVersionV2:
		return nil
	default:
		return fmt.Errorf("unsupported log version %d", v)
	}
}

// MetaMessage is written as the first line of a JSONL log file. It captures
// task-level metadata so logs can be reloaded on restart.
type MetaMessage struct {
	MessageType       string           `json:"type"`
	Version           int              `json:"version"`
	Prompt            string           `json:"prompt"`
	Title             string           `json:"title,omitempty"`
	Repos             []MetaRepo       `json:"repos"`
	Harness           harness.Name     `json:"harness"`
	Model             string           `json:"model,omitempty"`
	Effort            string           `json:"effort,omitempty"`
	StartedAt         time.Time        `json:"started_at"`
	ForgeIssue        int              `json:"forge_issue,omitempty"` // Originating issue/PR number for bot comment callbacks.
	ForkedFromTaskID  string           `json:"forked_from_task_id,omitempty"`
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
func (m *MetaMessage) Type() string { return "caic_meta" }

// Validate checks that all required fields are present and the version is supported.
func (m *MetaMessage) Validate() error {
	if m.MessageType != "caic_meta" {
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
type MetaSessionMessage struct {
	MessageType  string `json:"type"`
	SessionID    string `json:"session_id"`
	Model        string `json:"model,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
}

// Type implements Message.
func (m *MetaSessionMessage) Type() string { return "caic_session" }

// WriteMetaSession writes a caic_session control record for init metadata to w.
func WriteMetaSession(w io.Writer, init *InitMessage) error {
	if w == nil || init == nil || (init.SessionID == "" && init.Model == "" && init.Version == "") {
		return nil
	}
	data, err := json.Marshal(&MetaSessionMessage{
		MessageType:  "caic_session",
		SessionID:    init.SessionID,
		Model:        init.Model,
		AgentVersion: init.Version,
	})
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

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
	Error                    string   `json:"error,omitempty"`
	AgentResult              string   `json:"agent_result,omitempty"`
}

// Type implements Message.
func (m *MetaResultMessage) Type() string { return "caic_result" }

// MetaPRMessage is written to the JSONL log when a PR is created so that the
// PR number can be restored on server restart.
type MetaPRMessage struct {
	MessageType string `json:"type"`
	ForgeOwner  string `json:"forge_owner"`
	ForgeRepo   string `json:"forge_repo"`
	ForgePR     int    `json:"forge_pr"`
}

// Type implements Message.
func (m *MetaPRMessage) Type() string { return "caic_pr" }

// MarshalMessage serializes a Message to JSON. For RawMessage, returns the
// original bytes to preserve unknown fields. For typed messages, uses
// json.Marshal.
func MarshalMessage(m Message) ([]byte, error) {
	if rm, ok := m.(*RawMessage); ok {
		return rm.Raw, nil
	}
	return json.Marshal(m)
}
