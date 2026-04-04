// Wire types for the Claude Code NDJSON streaming protocol.

package claude

import (
	"encoding/json"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// ============================================================================
// Input types (sent TO the agent via stdin)
// ============================================================================
//
// Claude Code accepts five NDJSON message types on stdin when running with
// --input-format stream-json. The StdinMessage union covers all of them.
// See controlSchemas.ts StdinMessageSchema in the Claude Code source.

// InputType is the top-level "type" discriminator for Claude Code stdin NDJSON.
type InputType string

const (
	// InputUser sends a user turn.
	InputUser InputType = "user"
	// InputControlRequest sends a control request to Claude Code.
	InputControlRequest InputType = "control_request"
	// InputControlResponse responds to a control request from Claude Code.
	InputControlResponse InputType = "control_response"
	// InputKeepAlive is a heartbeat.
	InputKeepAlive InputType = "keep_alive"
	// InputUpdateEnvVars pushes env vars at runtime.
	InputUpdateEnvVars InputType = "update_environment_variables"
)

// ---------- user message ----------

// inputUser sends a user turn to Claude Code (type:"user").
type inputUser struct {
	Type            InputType        `json:"type"` // InputUser
	Message         inputUserContent `json:"message"`
	UUID            string           `json:"uuid,omitempty"`
	SessionID       string           `json:"session_id,omitempty"`
	ParentToolUseID string           `json:"parent_tool_use_id,omitempty"`
	IsSynthetic     bool             `json:"isSynthetic,omitempty"`
	ToolUseResult   json.RawMessage  `json:"tool_use_result,omitempty"`
	Priority        string           `json:"priority,omitempty"` // "now", "next", "later"
	Timestamp       string           `json:"timestamp,omitempty"`
}

type inputUserContent struct {
	Role    string              `json:"role"` // always "user"
	Content []inputContentBlock `json:"content"`
}

// inputContentBlock is a single block in the content array sent to Claude Code.
type inputContentBlock struct {
	Type   string           `json:"type"`
	Source inputImageSource `json:"source,omitzero"`
	Text   string           `json:"text,omitempty"`
}

type inputImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ---------- control request ----------

// inputControlRequest sends a control request to Claude Code (type:"control_request").
// The Request field is a JSON object whose "subtype" discriminator determines
// its schema. Use one of the controlReq* structs below as the Request value.
type inputControlRequest struct {
	Type      InputType `json:"type"` // InputControlRequest
	RequestID string    `json:"request_id"`
	Request   any       `json:"request"`
}

// ControlSubtype is the "subtype" discriminator for control requests.
type ControlSubtype string

// ControlSubtype values for control request subtypes.
const (
	ControlInitialize         ControlSubtype = "initialize"
	ControlInterrupt          ControlSubtype = "interrupt"
	ControlCanUseTool         ControlSubtype = "can_use_tool"
	ControlSetPermissionMode  ControlSubtype = "set_permission_mode"
	ControlSetModel           ControlSubtype = "set_model"
	ControlSetMaxThinking     ControlSubtype = "set_max_thinking_tokens"
	ControlMcpStatus          ControlSubtype = "mcp_status"
	ControlGetContextUsage    ControlSubtype = "get_context_usage"
	ControlHookCallback       ControlSubtype = "hook_callback"
	ControlMcpMessage         ControlSubtype = "mcp_message"
	ControlRewindFiles        ControlSubtype = "rewind_files"
	ControlCancelAsyncMessage ControlSubtype = "cancel_async_message"
	ControlSeedReadState      ControlSubtype = "seed_read_state"
	ControlMcpSetServers      ControlSubtype = "mcp_set_servers"
	ControlReloadPlugins      ControlSubtype = "reload_plugins"
	ControlMcpReconnect       ControlSubtype = "mcp_reconnect"
	ControlMcpToggle          ControlSubtype = "mcp_toggle"
	ControlStopTask           ControlSubtype = "stop_task"
	ControlApplyFlagSettings  ControlSubtype = "apply_flag_settings"
	ControlGetSettings        ControlSubtype = "get_settings"
	ControlElicitation        ControlSubtype = "elicitation"
)

// controlReqInitialize initializes the SDK session.
type controlReqInitialize struct {
	Subtype                ControlSubtype  `json:"subtype"` // ControlInitialize
	Hooks                  json.RawMessage `json:"hooks,omitempty"`
	SDKMcpServers          []string        `json:"sdkMcpServers,omitempty"`
	JSONSchema             json.RawMessage `json:"jsonSchema,omitempty"`
	SystemPrompt           string          `json:"systemPrompt,omitempty"`
	AppendSystemPrompt     string          `json:"appendSystemPrompt,omitempty"`
	Agents                 json.RawMessage `json:"agents,omitempty"`
	PromptSuggestions      bool            `json:"promptSuggestions,omitempty"`
	AgentProgressSummaries bool            `json:"agentProgressSummaries,omitempty"`
}

// controlReqInterrupt interrupts the currently running conversation turn.
type controlReqInterrupt struct {
	Subtype ControlSubtype `json:"subtype"` // ControlInterrupt
}

// controlReqCanUseTool requests permission to use a tool.
type controlReqCanUseTool struct {
	Subtype               ControlSubtype  `json:"subtype"` // ControlCanUseTool
	ToolName              string          `json:"tool_name"`
	Input                 json.RawMessage `json:"input"`
	PermissionSuggestions json.RawMessage `json:"permission_suggestions,omitempty"`
	BlockedPath           string          `json:"blocked_path,omitempty"`
	DecisionReason        string          `json:"decision_reason,omitempty"`
	Title                 string          `json:"title,omitempty"`
	DisplayName           string          `json:"display_name,omitempty"`
	ToolUseID             string          `json:"tool_use_id"`
	AgentID               string          `json:"agent_id,omitempty"`
	Description           string          `json:"description,omitempty"`
}

// controlReqSetPermissionMode changes the tool permission mode.
type controlReqSetPermissionMode struct {
	Subtype   ControlSubtype `json:"subtype"` // ControlSetPermissionMode
	Mode      string         `json:"mode"`
	Ultraplan bool           `json:"ultraplan,omitempty"`
}

// controlReqSetModel switches the model for subsequent turns.
type controlReqSetModel struct {
	Subtype ControlSubtype `json:"subtype"`         // ControlSetModel
	Model   string         `json:"model,omitempty"` // empty = reset to default
}

// controlReqSetMaxThinkingTokens configures extended thinking token limit.
type controlReqSetMaxThinkingTokens struct {
	Subtype           ControlSubtype `json:"subtype"`             // ControlSetMaxThinking
	MaxThinkingTokens *int           `json:"max_thinking_tokens"` // null = unlimited
}

// controlReqMcpStatus queries status of all MCP server connections.
type controlReqMcpStatus struct {
	Subtype ControlSubtype `json:"subtype"` // ControlMcpStatus
}

// controlReqGetContextUsage returns a context window usage breakdown.
type controlReqGetContextUsage struct {
	Subtype ControlSubtype `json:"subtype"` // ControlGetContextUsage
}

// controlReqHookCallback delivers a hook callback with its input data.
type controlReqHookCallback struct {
	Subtype    ControlSubtype  `json:"subtype"` // ControlHookCallback
	CallbackID string          `json:"callback_id"`
	Input      json.RawMessage `json:"input"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
}

// controlReqMcpMessage sends a JSON-RPC message to a specific MCP server.
type controlReqMcpMessage struct {
	Subtype    ControlSubtype  `json:"subtype"` // ControlMcpMessage
	ServerName string          `json:"server_name"`
	Message    json.RawMessage `json:"message"`
}

// controlReqRewindFiles reverts file changes since a given user message.
type controlReqRewindFiles struct {
	Subtype       ControlSubtype `json:"subtype"` // ControlRewindFiles
	UserMessageID string         `json:"user_message_id"`
	DryRun        bool           `json:"dry_run,omitempty"`
}

// controlReqCancelAsyncMessage drops a pending async user message from
// the command queue by UUID.
type controlReqCancelAsyncMessage struct {
	Subtype     ControlSubtype `json:"subtype"` // ControlCancelAsyncMessage
	MessageUUID string         `json:"message_uuid"`
}

// controlReqSeedReadState seeds the readFileState cache with a path+mtime
// entry so Edit validation succeeds after a prior Read was removed from context.
type controlReqSeedReadState struct {
	Subtype ControlSubtype `json:"subtype"` // ControlSeedReadState
	Path    string         `json:"path"`
	Mtime   int64          `json:"mtime"`
}

// controlReqMcpSetServers replaces the set of dynamically managed MCP servers.
type controlReqMcpSetServers struct {
	Subtype ControlSubtype  `json:"subtype"` // ControlMcpSetServers
	Servers json.RawMessage `json:"servers"`
}

// controlReqReloadPlugins reloads plugins from disk.
type controlReqReloadPlugins struct {
	Subtype ControlSubtype `json:"subtype"` // ControlReloadPlugins
}

// controlReqMcpReconnect reconnects a disconnected or failed MCP server.
type controlReqMcpReconnect struct {
	Subtype    ControlSubtype `json:"subtype"` // ControlMcpReconnect
	ServerName string         `json:"serverName"`
}

// controlReqMcpToggle enables or disables an MCP server.
type controlReqMcpToggle struct {
	Subtype    ControlSubtype `json:"subtype"` // ControlMcpToggle
	ServerName string         `json:"serverName"`
	Enabled    bool           `json:"enabled"`
}

// controlReqStopTask stops a running background task.
type controlReqStopTask struct {
	Subtype ControlSubtype `json:"subtype"` // ControlStopTask
	TaskID  string         `json:"task_id"`
}

// controlReqApplyFlagSettings merges settings into the flag settings layer.
type controlReqApplyFlagSettings struct {
	Subtype  ControlSubtype  `json:"subtype"` // ControlApplyFlagSettings
	Settings json.RawMessage `json:"settings"`
}

// controlReqGetSettings returns the effective and per-source settings.
type controlReqGetSettings struct {
	Subtype ControlSubtype `json:"subtype"` // ControlGetSettings
}

// controlReqElicitation requests the SDK consumer to handle an MCP elicitation.
type controlReqElicitation struct {
	Subtype         ControlSubtype  `json:"subtype"` // ControlElicitation
	MCPServerName   string          `json:"mcp_server_name"`
	Message         string          `json:"message"`
	Mode            string          `json:"mode,omitempty"` // "form" or "url"
	URL             string          `json:"url,omitempty"`
	ElicitationID   string          `json:"elicitation_id,omitempty"`
	RequestedSchema json.RawMessage `json:"requested_schema,omitempty"`
}

// ---------- control response ----------

// ControlResponseSubtype is the "subtype" discriminator for control responses.
type ControlResponseSubtype string

// ControlResponseSubtype values.
const (
	ControlResponseSuccess ControlResponseSubtype = "success"
	ControlResponseError   ControlResponseSubtype = "error"
)

// inputControlResponse responds to a control request from Claude Code (type:"control_response").
type inputControlResponse struct {
	Type     InputType       `json:"type"` // InputControlResponse
	Response controlResponse `json:"response"`
}

// controlResponse is the inner response, either success or error.
type controlResponse struct {
	Subtype                   ControlResponseSubtype `json:"subtype"` // ControlResponseSuccess or ControlResponseError
	RequestID                 string                 `json:"request_id"`
	Response                  json.RawMessage        `json:"response,omitempty"`                    // success only
	Error                     string                 `json:"error,omitempty"`                       // error only
	PendingPermissionRequests json.RawMessage        `json:"pending_permission_requests,omitempty"` // error only
}

// ---------- keep alive / env vars ----------

// inputKeepAlive is a heartbeat (type:"keep_alive").
type inputKeepAlive struct {
	Type InputType `json:"type"` // InputKeepAlive
}

// inputUpdateEnvVars pushes env vars at runtime (type:"update_environment_variables").
type inputUpdateEnvVars struct {
	Type      InputType         `json:"type"` // InputUpdateEnvVars
	Variables map[string]string `json:"variables"`
}

// Compile-time assertions for input wire types.
var (
	_ = inputControlRequest{}
	_ = inputControlResponse{}
	_ = inputKeepAlive{}
	_ = inputUpdateEnvVars{}

	_ = controlReqInitialize{}
	_ = controlReqInterrupt{}
	_ = controlReqCanUseTool{}
	_ = controlReqSetPermissionMode{}
	_ = controlReqSetModel{}
	_ = controlReqSetMaxThinkingTokens{}
	_ = controlReqMcpStatus{}
	_ = controlReqGetContextUsage{}
	_ = controlReqHookCallback{}
	_ = controlReqMcpMessage{}
	_ = controlReqRewindFiles{}
	_ = controlReqCancelAsyncMessage{}
	_ = controlReqSeedReadState{}
	_ = controlReqMcpSetServers{}
	_ = controlReqReloadPlugins{}
	_ = controlReqMcpReconnect{}
	_ = controlReqMcpToggle{}
	_ = controlReqStopTask{}
	_ = controlReqApplyFlagSettings{}
	_ = controlReqGetSettings{}
	_ = controlReqElicitation{}
)

// ---------- slash commands in -p mode ----------
//
// Slash commands can be sent as user message content (e.g. "/compact").
// In -p (print/headless) mode, only a subset is available:
//   - type="prompt" (skills like /review, /commit) — always intercepted
//   - type="local" with supportsNonInteractive=true — listed below
//
// Unrecognized commands (disabled or not in the filtered list) are passed
// through to the model as plain text, which typically fails with
// "Unknown skill: <name>".
//
// Available local commands in -p mode:
//   /compact       — shrink context (clear history, keep summary)
//   /context       — show context window usage breakdown
//   /cost          — show session cost (subscription users only)
//   /advisor       — configure advisor model (when available)
//   /release-notes — view changelog
//   /extra-usage   — extra usage info (subscription users only)
//
// Anthropic-internal only (USER_TYPE=ant):
//   /version       — print running version
//   /files         — list files in context
//   /heapdump      — dump JS heap (hidden)
//
// NOT available in -p mode (supportsNonInteractive=false or local-jsx):
//   /model, /clear, /config, /permissions, /help, /voice, /rewind,
//   /reload-plugins, /vim, /stickers, /install-slack-app, /bridge-kick
//
// For unavailable commands, use control request subtypes instead:
//   /model          → ControlSetModel
//   /reload-plugins → ControlReloadPlugins
//   (no equivalent for /clear — start a new session instead)

// ============================================================================
// Output types (received FROM the agent via stdout)
// ============================================================================

// OutputType is the top-level "type" discriminator for Claude Code stdout NDJSON.
type OutputType string

const (
	// Core message types (SDKMessageSchema).

	// OutputAssistant is a complete assistant turn with content blocks.
	OutputAssistant OutputType = "assistant"
	// OutputUser is an echoed user message (input or tool result).
	OutputUser OutputType = "user"
	// OutputResult is a terminal message with final status and usage.
	OutputResult OutputType = "result"
	// OutputSystem is a system event; dispatch further on SystemSubtype.
	OutputSystem OutputType = "system"
	// OutputStreamEvent is a partial assistant message (streaming delta).
	OutputStreamEvent OutputType = "stream_event"
	// OutputRateLimitEvent is emitted when rate limit status transitions.
	OutputRateLimitEvent OutputType = "rate_limit_event"
	// OutputToolProgress reports elapsed time for a running tool.
	OutputToolProgress OutputType = "tool_progress"
	// OutputAuthStatus reports authentication state changes.
	OutputAuthStatus OutputType = "auth_status"
	// OutputToolUseSummary summarizes preceding tool calls.
	OutputToolUseSummary OutputType = "tool_use_summary"
	// OutputPromptSuggestion is a predicted next user prompt.
	OutputPromptSuggestion OutputType = "prompt_suggestion"

	// Streamlined output types (only with --streamlined-output).

	// OutputStreamlinedText replaces assistant messages with text only.
	OutputStreamlinedText OutputType = "streamlined_text"
	// OutputStreamlinedToolUseSummary replaces tool_use blocks with a summary.
	OutputStreamlinedToolUseSummary OutputType = "streamlined_tool_use_summary"

	// Control protocol types.

	// OutputControlRequest is a control request from Claude Code to the host.
	OutputControlRequest OutputType = "control_request"
	// OutputControlResponse is a response to a control request sent by the host.
	OutputControlResponse OutputType = "control_response"
	// OutputControlCancelRequest cancels a pending control request.
	OutputControlCancelRequest OutputType = "control_cancel_request"

	// Misc.

	// OutputKeepAlive is a heartbeat.
	OutputKeepAlive OutputType = "keep_alive"
)

// SystemSubtype is the "subtype" discriminator for type="system" messages.
type SystemSubtype string

const (
	// SystemInit is the first message in a session.
	SystemInit SystemSubtype = "init"
	// SystemTaskStarted signals a background subagent has started.
	SystemTaskStarted SystemSubtype = "task_started"
	// SystemTaskNotification signals a background subagent completed/failed/stopped.
	SystemTaskNotification SystemSubtype = "task_notification"
	// SystemTaskProgress reports progress of a background subagent.
	SystemTaskProgress SystemSubtype = "task_progress"
	// SystemCompactBoundary marks where context was compacted.
	SystemCompactBoundary SystemSubtype = "compact_boundary"
	// SystemStatus reports idle/running/requires_action transitions.
	SystemStatus SystemSubtype = "status"
	// SystemSessionStateChanged mirrors notifySessionStateChanged;
	// authoritative turn-over signal ("idle" fires after result is flushed).
	SystemSessionStateChanged SystemSubtype = "session_state_changed"
	// SystemAPIRetry is emitted when an API request fails with a retryable
	// error and will be retried after a delay.
	SystemAPIRetry SystemSubtype = "api_retry"
	// SystemLocalCommandOutput is output from a local slash command.
	SystemLocalCommandOutput SystemSubtype = "local_command_output"
	// SystemHookStarted signals a hook has started executing.
	SystemHookStarted SystemSubtype = "hook_started"
	// SystemHookProgress reports partial output from a running hook.
	SystemHookProgress SystemSubtype = "hook_progress"
	// SystemHookResponse reports a hook's final result.
	SystemHookResponse SystemSubtype = "hook_response"
	// SystemFilesPersisted reports files uploaded to cloud storage.
	SystemFilesPersisted SystemSubtype = "files_persisted"
	// SystemElicitationComplete signals an MCP URL-mode elicitation is done.
	SystemElicitationComplete SystemSubtype = "elicitation_complete"
	// SystemPostTurnSummary is a background summary emitted after each
	// assistant turn (summarizes_uuid points to the assistant message).
	SystemPostTurnSummary SystemSubtype = "post_turn_summary"
)

// ---------- envelope probe ----------

// outputTypeProbe extracts the type discriminator from a Claude Code JSONL record.
type outputTypeProbe struct {
	Type OutputType `json:"type"`
	// Subtype is untyped because its meaning varies by Type: SystemSubtype for
	// system messages, but free-form strings like "success" or "error_max_turns"
	// for result messages.
	Subtype string `json:"subtype"`
}

// ---------- system/init ----------

// outputInit is the wire representation of a system/init record.
type outputInit struct {
	Type      OutputType      `json:"type"`
	Subtype   SystemSubtype   `json:"subtype"`
	Cwd       string          `json:"cwd"`
	SessionID string          `json:"session_id"`
	Tools     []string        `json:"tools"`
	Model     string          `json:"model"`
	Version   string          `json:"claude_code_version"`
	UUID      string          `json:"uuid"`
	Timestamp json.RawMessage `json:"timestamp,omitempty"`

	Agents         json.RawMessage `json:"agents,omitempty"`
	APIKeySource   json.RawMessage `json:"apiKeySource,omitempty"`
	FastModeState  json.RawMessage `json:"fast_mode_state,omitempty"`
	MCPServers     json.RawMessage `json:"mcp_servers,omitempty"`
	OutputStyle    json.RawMessage `json:"output_style,omitempty"`
	PermissionMode json.RawMessage `json:"permissionMode,omitempty"`
	Plugins        json.RawMessage `json:"plugins,omitempty"`
	Skills         json.RawMessage `json:"skills,omitempty"`
	SlashCommands  json.RawMessage `json:"slash_commands,omitempty"`
}

// ---------- system (non-init) ----------

// outputSystem is the wire representation of a non-init system record.
type outputSystem struct {
	Type      OutputType      `json:"type"`
	Subtype   SystemSubtype   `json:"subtype"`
	SessionID string          `json:"session_id"`
	UUID      string          `json:"uuid"`
	Timestamp json.RawMessage `json:"timestamp,omitempty"`

	// task_started / task_progress / task_notification fields.
	Description  json.RawMessage `json:"description,omitempty"`
	TaskID       json.RawMessage `json:"task_id,omitempty"`
	TaskType     json.RawMessage `json:"task_type,omitempty"`
	ToolUseID    json.RawMessage `json:"tool_use_id,omitempty"`
	LastToolName json.RawMessage `json:"last_tool_name,omitempty"`
	Status       json.RawMessage `json:"status,omitempty"`
	UsageExtra   json.RawMessage `json:"usage,omitempty"`
	OutputFile   json.RawMessage `json:"output_file,omitempty"`
	Summary      json.RawMessage `json:"summary,omitempty"`

	// api_retry fields.
	Attempt      json.RawMessage `json:"attempt,omitempty"`
	MaxRetries   json.RawMessage `json:"max_retries,omitempty"`
	RetryDelayMs json.RawMessage `json:"retry_delay_ms,omitempty"`
	ErrorStatus  json.RawMessage `json:"error_status,omitempty"`
	Error        json.RawMessage `json:"error,omitempty"`

	// Other optional fields.
	PermissionMode  json.RawMessage `json:"permissionMode,omitempty"`
	CompactMetadata json.RawMessage `json:"compact_metadata,omitempty"`
	Prompt          json.RawMessage `json:"prompt,omitempty"`
}

// ---------- assistant ----------

// outputAssistant is the wire representation of an assistant record.
type outputAssistant struct {
	Type            OutputType           `json:"type"`
	SessionID       string               `json:"session_id"`
	UUID            string               `json:"uuid"`
	Timestamp       json.RawMessage      `json:"timestamp,omitempty"`
	Message         assistantMessageBody `json:"message"`
	ParentToolUseID string               `json:"parent_tool_use_id"`
	Error           string               `json:"error"`
}

// assistantMessageBody is the inner message object within an assistant record.
type assistantMessageBody struct {
	ID           string               `json:"id"`
	Type         string               `json:"type,omitempty"`
	Role         string               `json:"role"`
	Model        string               `json:"model"`
	Content      []outputContentBlock `json:"content"`
	Usage        agent.Usage          `json:"usage"`
	StopReason   string               `json:"stop_reason"`
	StopSequence string               `json:"stop_sequence"`
	StopDetails  json.RawMessage      `json:"stop_details,omitempty"`

	Container         json.RawMessage `json:"container,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
}

// contentBlockStart is the content_block field in a content_block_start streaming event.
type contentBlockStart struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// outputContentBlock is a single content block inside an assistant message.
// This is a flat union: fields are populated depending on Type.
//
//   - "text":        Text
//   - "thinking":    Thinking, Signature
//   - "tool_use":    ID, Name, Input
//   - "tool_result": ToolUseID, Content, IsError
type outputContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	// tool_result fields (inline MCP tool results).
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ---------- user (echoed) ----------

// outputUser is the wire representation of a user record.
type outputUser struct {
	Type            OutputType      `json:"type"`
	UUID            string          `json:"uuid"`
	SessionID       string          `json:"session_id,omitempty"`
	Timestamp       json.RawMessage `json:"timestamp,omitempty"`
	Message         json.RawMessage `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	ToolUseResult   json.RawMessage `json:"tool_use_result,omitempty"`
	IsSynthetic     bool            `json:"isSynthetic,omitempty"`
	IsReplay        bool            `json:"isReplay,omitempty"`
}

// ---------- result ----------

// outputResult is the wire representation of a result record.
type outputResult struct {
	Type             OutputType      `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	DurationMs       int64           `json:"duration_ms"`
	DurationAPIMs    int64           `json:"duration_api_ms"`
	NumTurns         int             `json:"num_turns"`
	Result           string          `json:"result"`
	SessionID        string          `json:"session_id"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	Usage            agent.Usage     `json:"usage"`
	UUID             string          `json:"uuid"`
	StructuredOutput *string         `json:"structured_output"`
	Timestamp        json.RawMessage `json:"timestamp,omitempty"`

	FastModeState     json.RawMessage `json:"fast_mode_state,omitempty"`
	ModelUsage        json.RawMessage `json:"modelUsage,omitempty"`
	PermissionDenials json.RawMessage `json:"permission_denials,omitempty"`
	StopReason        json.RawMessage `json:"stop_reason,omitempty"`
	TerminalReason    json.RawMessage `json:"terminal_reason,omitempty"`
}

// ---------- stream_event ----------

// outputStreamEvent is the wire representation of a stream_event record.
type outputStreamEvent struct {
	Type            OutputType      `json:"type"`
	UUID            string          `json:"uuid"`
	SessionID       string          `json:"session_id"`
	Timestamp       json.RawMessage `json:"timestamp,omitempty"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	Event           streamEventData `json:"event"`
}

// streamEventData is the nested event body inside a stream_event record.
type streamEventData struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Delta        *streamDelta    `json:"delta,omitempty"`
	ContentBlock json.RawMessage `json:"content_block,omitempty"`
	// message_start carries the full message object; message_delta carries
	// stop_reason and usage in a delta wrapper.
	Message json.RawMessage `json:"message,omitempty"`
	Usage   json.RawMessage `json:"usage,omitempty"`
}

// streamDelta is a delta object inside a stream event.
type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	// message_delta carries stop_reason.
	StopReason string `json:"stop_reason,omitempty"`
}

// ---------- rate_limit_event ----------

// outputRateLimitEvent is the wire representation of a rate_limit_event record.
// Emitted when the CLI's rate limit status transitions (e.g. allowed → allowed_warning).
type outputRateLimitEvent struct {
	Type          OutputType      `json:"type"`
	UUID          string          `json:"uuid"`
	SessionID     string          `json:"session_id"`
	Timestamp     json.RawMessage `json:"timestamp,omitempty"`
	RateLimitInfo rateLimitInfo   `json:"rate_limit_info"`
}

// rateLimitInfo is the nested rate limit info inside a rate_limit_event.
type rateLimitInfo struct {
	Status                string          `json:"status"`
	ResetsAt              json.RawMessage `json:"resets_at,omitempty"`
	RateLimitType         json.RawMessage `json:"rate_limit_type,omitempty"`
	Utilization           json.RawMessage `json:"utilization,omitempty"`
	OverageStatus         json.RawMessage `json:"overage_status,omitempty"`
	OverageResetsAt       json.RawMessage `json:"overage_resets_at,omitempty"`
	OverageDisabledReason json.RawMessage `json:"overage_disabled_reason,omitempty"`
}

// ---------- documentation-only output types ----------
//
// These types are not yet parsed by ParseMessage (they fall through to
// RawMessage) but document the full stdout wire protocol.

// outputToolProgress is emitted periodically while a tool is running.
type outputToolProgress struct {
	Type               OutputType `json:"type"` // OutputToolProgress
	ToolUseID          string     `json:"tool_use_id"`
	ToolName           string     `json:"tool_name"`
	ParentToolUseID    string     `json:"parent_tool_use_id"` // nullable
	ElapsedTimeSeconds int        `json:"elapsed_time_seconds"`
	TaskID             string     `json:"task_id,omitempty"`
	UUID               string     `json:"uuid"`
	SessionID          string     `json:"session_id"`
}

// outputAuthStatus reports authentication state changes.
type outputAuthStatus struct {
	Type             OutputType `json:"type"` // OutputAuthStatus
	IsAuthenticating bool       `json:"isAuthenticating"`
	Output           []string   `json:"output"`
	Error            string     `json:"error,omitempty"`
	UUID             string     `json:"uuid"`
	SessionID        string     `json:"session_id"`
}

// outputToolUseSummary summarizes a group of preceding tool calls.
type outputToolUseSummary struct {
	Type                OutputType `json:"type"` // OutputToolUseSummary
	Summary             string     `json:"summary"`
	PrecedingToolUseIDs []string   `json:"preceding_tool_use_ids"`
	UUID                string     `json:"uuid"`
	SessionID           string     `json:"session_id"`
}

// outputPromptSuggestion is a predicted next user prompt.
type outputPromptSuggestion struct {
	Type       OutputType `json:"type"` // OutputPromptSuggestion
	Suggestion string     `json:"suggestion"`
	UUID       string     `json:"uuid"`
	SessionID  string     `json:"session_id"`
}

// outputStreamlinedText replaces assistant messages in streamlined output mode.
type outputStreamlinedText struct {
	Type      OutputType `json:"type"` // OutputStreamlinedText
	Text      string     `json:"text"`
	SessionID string     `json:"session_id"`
	UUID      string     `json:"uuid"`
}

// outputStreamlinedToolUseSummary replaces tool_use blocks in streamlined output.
type outputStreamlinedToolUseSummary struct {
	Type        OutputType `json:"type"` // OutputStreamlinedToolUseSummary
	ToolSummary string     `json:"tool_summary"`
	SessionID   string     `json:"session_id"`
	UUID        string     `json:"uuid"`
}

// outputControlCancelRequest cancels a pending control request.
type outputControlCancelRequest struct {
	Type      OutputType `json:"type"` // OutputControlCancelRequest
	RequestID string     `json:"request_id"`
}

// --- system subtype wire types ---

// outputSessionStateChanged reports idle/running/requires_action transitions.
type outputSessionStateChanged struct {
	Type      OutputType    `json:"type"`    // OutputSystem
	Subtype   SystemSubtype `json:"subtype"` // SystemSessionStateChanged
	State     string        `json:"state"`   // "idle", "running", "requires_action"
	UUID      string        `json:"uuid"`
	SessionID string        `json:"session_id"`
}

// outputPostTurnSummary is an AI-generated summary after each assistant turn.
type outputPostTurnSummary struct {
	Type           OutputType    `json:"type"`    // OutputSystem
	Subtype        SystemSubtype `json:"subtype"` // SystemPostTurnSummary
	SummarizesUUID string        `json:"summarizes_uuid"`
	StatusCategory string        `json:"status_category"` // "blocked","waiting","completed","review_ready","failed"
	StatusDetail   string        `json:"status_detail"`
	IsNoteworthy   bool          `json:"is_noteworthy"`
	Title          string        `json:"title"`
	Description    string        `json:"description"`
	RecentAction   string        `json:"recent_action"`
	NeedsAction    string        `json:"needs_action"`
	ArtifactURLs   []string      `json:"artifact_urls"`
	UUID           string        `json:"uuid"`
	SessionID      string        `json:"session_id"`
}

// outputLocalCommandOutput is output from a local slash command (e.g. /cost).
type outputLocalCommandOutput struct {
	Type      OutputType    `json:"type"`    // OutputSystem
	Subtype   SystemSubtype `json:"subtype"` // SystemLocalCommandOutput
	Content   string        `json:"content"`
	UUID      string        `json:"uuid"`
	SessionID string        `json:"session_id"`
}

// outputHookStarted signals a hook has started executing.
type outputHookStarted struct {
	Type      OutputType    `json:"type"`    // OutputSystem
	Subtype   SystemSubtype `json:"subtype"` // SystemHookStarted
	HookID    string        `json:"hook_id"`
	HookName  string        `json:"hook_name"`
	HookEvent string        `json:"hook_event"`
	UUID      string        `json:"uuid"`
	SessionID string        `json:"session_id"`
}

// outputHookProgress reports partial output from a running hook.
type outputHookProgress struct {
	Type      OutputType    `json:"type"`    // OutputSystem
	Subtype   SystemSubtype `json:"subtype"` // SystemHookProgress
	HookID    string        `json:"hook_id"`
	HookName  string        `json:"hook_name"`
	HookEvent string        `json:"hook_event"`
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr"`
	Output    string        `json:"output"`
	UUID      string        `json:"uuid"`
	SessionID string        `json:"session_id"`
}

// outputHookResponse reports a hook's final result.
type outputHookResponse struct {
	Type      OutputType    `json:"type"`    // OutputSystem
	Subtype   SystemSubtype `json:"subtype"` // SystemHookResponse
	HookID    string        `json:"hook_id"`
	HookName  string        `json:"hook_name"`
	HookEvent string        `json:"hook_event"`
	Output    string        `json:"output"`
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr"`
	ExitCode  int           `json:"exit_code,omitempty"`
	Outcome   string        `json:"outcome"` // "success", "error", "cancelled"
	UUID      string        `json:"uuid"`
	SessionID string        `json:"session_id"`
}

// outputFilesPersisted reports files uploaded to cloud storage.
type outputFilesPersisted struct {
	Type    OutputType    `json:"type"`    // OutputSystem
	Subtype SystemSubtype `json:"subtype"` // SystemFilesPersisted
	Files   []struct {
		Filename string `json:"filename"`
		FileID   string `json:"file_id"`
	} `json:"files"`
	Failed []struct {
		Filename string `json:"filename"`
		Error    string `json:"error"`
	} `json:"failed"`
	ProcessedAt string `json:"processed_at"`
	UUID        string `json:"uuid"`
	SessionID   string `json:"session_id"`
}

// outputElicitationComplete signals an MCP URL-mode elicitation is done.
type outputElicitationComplete struct {
	Type          OutputType    `json:"type"`    // OutputSystem
	Subtype       SystemSubtype `json:"subtype"` // SystemElicitationComplete
	MCPServerName string        `json:"mcp_server_name"`
	ElicitationID string        `json:"elicitation_id"`
	UUID          string        `json:"uuid"`
	SessionID     string        `json:"session_id"`
}

// Compile-time assertions for documentation-only output wire types.
var (
	_ = outputToolProgress{}
	_ = outputAuthStatus{}
	_ = outputToolUseSummary{}
	_ = outputPromptSuggestion{}
	_ = outputStreamlinedText{}
	_ = outputStreamlinedToolUseSummary{}
	_ = outputControlCancelRequest{}
	_ = outputSessionStateChanged{}
	_ = outputPostTurnSummary{}
	_ = outputLocalCommandOutput{}
	_ = outputHookStarted{}
	_ = outputHookProgress{}
	_ = outputHookResponse{}
	_ = outputFilesPersisted{}
	_ = outputElicitationComplete{}
)

// ---------- output helper types ----------

type askInput struct {
	Questions []agent.AskQuestion `json:"questions"`
}

type todoInput struct {
	Todos []agent.TodoItem `json:"todos"`
}

type outputUserText struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type outputUserBlock struct {
	Role    string                   `json:"role"`
	Content []outputUserContentBlock `json:"content"`
}

type outputUserContentBlock struct {
	Type      string             `json:"type"`
	Text      string             `json:"text,omitempty"`
	Source    *outputImageSource `json:"source,omitempty"`
	ToolUseID string             `json:"tool_use_id,omitempty"`
	// Nested content and error flag for inline tool_result blocks (MCP tools).
	Content []toolResultContent `json:"content,omitempty"`
	IsError bool                `json:"is_error,omitempty"`
}

type outputImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// outputToolResult is the message body format for tool results delivered via
// the top-level parent_tool_use_id path (standard Claude Code tools).
type outputToolResult struct {
	Content []toolResultContent `json:"content"`
	IsError bool                `json:"is_error"`
}

type toolResultContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
