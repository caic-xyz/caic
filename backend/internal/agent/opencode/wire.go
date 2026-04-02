// Wire types for the OpenCode ACP (Agent Client Protocol) JSON-RPC 2.0 protocol.
//
// Type names follow the upstream ACP SDK definitions:
//
//	packages/opencode/src/acp/agent.ts — session update types and request/response handling
//
// Source: https://github.com/anomalyco/opencode
// Spec:   https://agentclientprotocol.com
package opencode

import "encoding/json"

// ============================================================
// Shared types: enums, JSON-RPC envelope, routing probes.
// ============================================================

// Method is a JSON-RPC method string for the ACP protocol.
type Method string

// JSON-RPC method constants for the ACP protocol.
const (
	// Request methods (client → agent).
	MethodInitialize              Method = "initialize"
	MethodSessionNew              Method = "session/new"
	MethodSessionLoad             Method = "session/load"
	MethodSessionPrompt           Method = "session/prompt"
	MethodSessionCancel           Method = "session/cancel"
	MethodSessionSetModel         Method = "session/set_model"
	MethodSessionSetMode          Method = "session/set_mode"
	MethodUnstableSetSessionModel Method = "unstable_setSessionModel"

	// Notification methods (agent → client).
	MethodSessionUpdate            Method = "session/update"
	MethodSessionRequestPermission Method = "session/request_permission"
)

// UpdateType is the session update discriminator (sessionUpdate field).
type UpdateType string

// Session update type constants.
const (
	UpdateAgentMessageChunk       UpdateType = "agent_message_chunk"
	UpdateAgentThoughtChunk       UpdateType = "agent_thought_chunk"
	UpdateUserMessageChunk        UpdateType = "user_message_chunk"
	UpdateToolCall                UpdateType = "tool_call"
	UpdateToolCallUpdate          UpdateType = "tool_call_update"
	UpdatePlan                    UpdateType = "plan"
	UpdateUsageUpdate             UpdateType = "usage_update"
	UpdateCurrentModeUpdate       UpdateType = "current_mode_update"
	UpdateSessionInfoUpdate       UpdateType = "session_info_update"
	UpdateAvailableCommandsUpdate UpdateType = "available_commands_update"
	UpdateConfigOptionUpdate      UpdateType = "config_option_update"
)

// ToolStatus is the status of a tool call.
type ToolStatus string

// Tool call status constants.
const (
	StatusPending    ToolStatus = "pending"
	StatusInProgress ToolStatus = "in_progress"
	StatusCompleted  ToolStatus = "completed"
	StatusFailed     ToolStatus = "failed"
)

// ToolKind is the kind of tool operation.
type ToolKind string

// Tool call kind constants.
const (
	KindRead       ToolKind = "read"
	KindEdit       ToolKind = "edit"
	KindDelete     ToolKind = "delete"
	KindMove       ToolKind = "move"
	KindSearch     ToolKind = "search"
	KindExecute    ToolKind = "execute"
	KindThink      ToolKind = "think"
	KindFetch      ToolKind = "fetch"
	KindSwitchMode ToolKind = "switch_mode"
	KindOther      ToolKind = "other"
)

// PlanStatus is the status of a plan entry.
type PlanStatus string

// Plan entry status constants.
const (
	PlanStatusPending    PlanStatus = "pending"
	PlanStatusInProgress PlanStatus = "in_progress"
	PlanStatusCompleted  PlanStatus = "completed"
	PlanStatusCancelled  PlanStatus = "cancelled"
)

// ContentType is the type discriminator for content blocks and prompt items.
type ContentType string

// Content type constants.
const (
	ContentText         ContentType = "text"
	ContentImage        ContentType = "image"
	ContentResource     ContentType = "resource"
	ContentResourceLink ContentType = "resource_link"
)

// ---------- JSON-RPC envelope ----------

// JSONRPCMessage is the JSON-RPC 2.0 envelope for ACP messages.
type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  Method          `json:"method,omitzero"`
	ID      json.RawMessage `json:"id,omitzero"`
	Params  json.RawMessage `json:"params,omitzero"`
	Result  json.RawMessage `json:"result,omitzero"`
	Error   *JSONRPCError   `json:"error,omitzero"`
}

// IsResponse returns true if this is a response (has an ID).
func (m *JSONRPCMessage) IsResponse() bool { return m.ID != nil }

// JSONRPCError is a JSON-RPC 2.0 error object.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------- Routing probes ----------

// messageProbe extracts routing fields from an ACP line to distinguish
// caic-injected JSON (has "type") from JSON-RPC (has "method"/"id").
type messageProbe struct {
	Type   string          `json:"type,omitzero"`
	Method Method          `json:"method,omitzero"`
	ID     json.RawMessage `json:"id,omitzero"`
}

// paramsProbe extracts the raw params field from a JSON-RPC message.
type paramsProbe struct {
	Params json.RawMessage `json:"params,omitzero"`
}

// updateProbe extracts the discriminator from a session update.
type updateProbe struct {
	SessionUpdate UpdateType `json:"sessionUpdate"`
}

// ============================================================
// Input types: requests sent to OpenCode (stdin).
// ============================================================

// ---------- caic-injected synthetic lines ----------

// caicInit is written to output.jsonl during handshake so replay can
// reconstruct an InitMessage (handshake responses aren't otherwise logged).
type caicInit struct {
	Type      string `json:"type"` // always "caic_init"
	SessionID string `json:"session_id"`
	Model     string `json:"model,omitzero"`
	Version   string `json:"version,omitzero"`
}

// ---------- JSON-RPC request envelope ----------

// jsonrpcRequest is the envelope for all JSON-RPC 2.0 requests sent to OpenCode.
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitzero"`
	Method  Method `json:"method"`
	Params  any    `json:"params,omitzero"`
}

// ---------- Handshake request params ----------

// initializeParams holds the params for the initialize request.
type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         clientInfo         `json:"clientInfo"`
}

type clientCapabilities struct {
	Terminal bool `json:"terminal"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

// ---------- Session management request params ----------

// sessionNewParams holds the params for session/new.
type sessionNewParams struct {
	Cwd        string      `json:"cwd"`
	McpServers []mcpServer `json:"mcpServers"`
}

// sessionLoadParams holds the params for session/load.
type sessionLoadParams struct {
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	McpServers []mcpServer `json:"mcpServers"`
}

// mcpServer describes an MCP server to register with the session.
// ACP supports three variants (stdio, http, sse) discriminated by the Type
// field. Only stdio is used by caic (for the widget MCP server).
type mcpServer struct {
	Type    string        `json:"type,omitzero"` // "http", "sse", or empty for stdio.
	Name    string        `json:"name"`
	Command string        `json:"command,omitzero"` // Stdio only.
	Args    []string      `json:"args,omitzero"`    // Stdio only.
	Env     []envVariable `json:"env,omitzero"`     // Stdio only.
	URL     string        `json:"url,omitzero"`     // HTTP/SSE only.
	Headers []httpHeader  `json:"headers,omitzero"` // HTTP/SSE only.
}

type envVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type httpHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ---------- Prompt request params ----------

// promptContent is a single item in the session/prompt content array.
// This is a flat union discriminated by Type:
//
//   - ContentText:         Text
//   - ContentImage:        Data (base64), MimeType
//   - ContentResource:     Resource (embedded resource)
//   - ContentResourceLink: URI, Name, MimeType
type promptContent struct {
	Type     ContentType     `json:"type"`
	Text     string          `json:"text,omitzero"`
	Data     string          `json:"data,omitzero"`     // Base64 image data.
	MimeType string          `json:"mimeType,omitzero"` // e.g. "image/png".
	URI      string          `json:"uri,omitzero"`
	Name     string          `json:"name,omitzero"`
	Resource json.RawMessage `json:"resource,omitzero"` // Embedded resource object.
}

// sessionPromptParams holds the params for session/prompt.
type sessionPromptParams struct {
	SessionID string          `json:"sessionId"`
	Prompt    []promptContent `json:"prompt"`
}

// ---------- Model switching ----------

// setSessionModelParams holds the params for unstable_setSessionModel.
type setSessionModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

// ============================================================
// Output types: notifications and responses received from OpenCode (stdout).
//
// Unknown field detection is centralized in unmarshalNotification
// (parse.go) rather than per-struct UnmarshalJSON methods.
// ============================================================

// ---------- Session update envelope ----------

// SessionUpdateParams holds the params for session/update notifications.
type SessionUpdateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// ---------- Content types ----------

// ContentBlock is a content block in message chunks. This is a flat union:
// fields are populated depending on Type.
//
//   - ContentText:         Text, Annotations
//   - ContentImage:        Data, MimeType, URI
//   - ContentResource:     Resource
//   - ContentResourceLink: URI, Name, MimeType
type ContentBlock struct {
	Type        ContentType     `json:"type"`
	Text        string          `json:"text,omitzero"`
	Data        string          `json:"data,omitzero"` // Base64 image data.
	MimeType    string          `json:"mimeType,omitzero"`
	URI         string          `json:"uri,omitzero"`
	Name        string          `json:"name,omitzero"`
	Resource    json.RawMessage `json:"resource,omitzero"`
	Annotations json.RawMessage `json:"annotations,omitzero"`
}

// ---------- Session update types ----------

// AgentMessageChunkUpdate is a streaming text chunk from the agent.
type AgentMessageChunkUpdate struct {
	SessionUpdate UpdateType   `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
}

// AgentThoughtChunkUpdate is a streaming reasoning chunk from the agent.
type AgentThoughtChunkUpdate struct {
	SessionUpdate UpdateType   `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
}

// UserMessageChunkUpdate is a replayed user message (during session/load).
type UserMessageChunkUpdate struct {
	SessionUpdate UpdateType   `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
}

// ToolCallLocation is a file location associated with a tool call.
type ToolCallLocation struct {
	Path string `json:"path,omitzero"`
	Line int    `json:"line,omitzero"`
}

// ToolCallUpdate is the initial tool call announcement.
type ToolCallUpdate struct {
	SessionUpdate UpdateType         `json:"sessionUpdate"`
	ToolCallID    string             `json:"toolCallId"`
	Title         string             `json:"title,omitzero"`
	Kind          ToolKind           `json:"kind,omitzero"`
	Status        ToolStatus         `json:"status,omitzero"`
	Locations     []ToolCallLocation `json:"locations,omitzero"`
	RawInput      json.RawMessage    `json:"rawInput,omitzero"`
}

// ToolCallContent is a content entry in a tool call update result. This is a
// flat union discriminated by Type:
//
//   - "content":       Content (text block)
//   - "diff":          Path, OldText, NewText
//   - "image":         Content.Data, Content.MimeType
//   - "resource":      Content.Resource
//   - "resource_link": Content.URI, Content.Name, Content.MimeType
type ToolCallContent struct {
	Type    string       `json:"type"`
	Content ContentBlock `json:"content,omitzero"`
	// Diff fields.
	Path    string `json:"path,omitzero"`
	OldText string `json:"oldText,omitzero"`
	NewText string `json:"newText,omitzero"`
}

// ToolCallRawOutput is the structured raw output from a tool call.
type ToolCallRawOutput struct {
	Output   string          `json:"output,omitzero"`
	Error    string          `json:"error,omitzero"`
	Metadata json.RawMessage `json:"metadata,omitzero"`
}

// ToolCallUpdateUpdate is a tool call progress/completion update.
type ToolCallUpdateUpdate struct {
	SessionUpdate UpdateType         `json:"sessionUpdate"`
	ToolCallID    string             `json:"toolCallId"`
	Title         string             `json:"title,omitzero"`
	Kind          ToolKind           `json:"kind,omitzero"`
	Status        ToolStatus         `json:"status,omitzero"`
	Locations     []ToolCallLocation `json:"locations,omitzero"`
	RawInput      json.RawMessage    `json:"rawInput,omitzero"`
	RawOutput     *ToolCallRawOutput `json:"rawOutput,omitzero"`
	Content       []ToolCallContent  `json:"content,omitzero"`
}

// PlanEntry is a single entry in a plan update.
type PlanEntry struct {
	Priority string     `json:"priority,omitzero"`
	Status   PlanStatus `json:"status"`
	Content  string     `json:"content"`
}

// PlanUpdate is a todo/plan update from the agent.
type PlanUpdate struct {
	SessionUpdate UpdateType  `json:"sessionUpdate"`
	Entries       []PlanEntry `json:"entries"`
}

// UsageCost describes the cost of usage.
type UsageCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// UsageUpdateUpdate is a context window / cost update.
type UsageUpdateUpdate struct {
	SessionUpdate UpdateType `json:"sessionUpdate"`
	Used          int        `json:"used"`
	Size          int        `json:"size"`
	Cost          UsageCost  `json:"cost,omitzero"`
}

// CurrentModeUpdate is a mode change notification.
type CurrentModeUpdate struct {
	SessionUpdate UpdateType `json:"sessionUpdate"`
	ModeID        string     `json:"modeId,omitzero"`
	ModeName      string     `json:"modeName,omitzero"`
}

// AvailableCommand is a single command in an available_commands_update.
type AvailableCommand struct {
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
}

// AvailableCommandsUpdate lists commands available in the current session.
type AvailableCommandsUpdate struct {
	SessionUpdate     UpdateType         `json:"sessionUpdate"`
	AvailableCommands []AvailableCommand `json:"availableCommands"`
}

// ---------- Permission request ----------

// PermissionToolCall describes the tool call in a permission request.
type PermissionToolCall struct {
	ToolCallID string             `json:"toolCallId"`
	Status     ToolStatus         `json:"status,omitzero"`
	Title      string             `json:"title,omitzero"`
	Kind       ToolKind           `json:"kind,omitzero"`
	RawInput   json.RawMessage    `json:"rawInput,omitzero"`
	Locations  []ToolCallLocation `json:"locations,omitzero"`
}

// PermissionOption is a single option in a permission request.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"` // "allow_once", "allow_always", "reject_once".
	Name     string `json:"name"`
}

// PermissionRequestParams holds params for session/request_permission.
type PermissionRequestParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  PermissionToolCall `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// ---------- Response types ----------

// initializeResult is the result of an initialize request.
type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities,omitzero"`
	AgentInfo         agentInfo         `json:"agentInfo,omitzero"`
	AuthMethods       json.RawMessage   `json:"authMethods,omitzero"`
}

type agentCapabilities struct {
	PromptCapabilities  promptCapabilities `json:"promptCapabilities,omitzero"`
	LoadSession         bool               `json:"loadSession,omitzero"`
	McpCapabilities     json.RawMessage    `json:"mcpCapabilities,omitzero"`
	SessionCapabilities json.RawMessage    `json:"sessionCapabilities,omitzero"`
}

type promptCapabilities struct {
	Image           bool `json:"image,omitzero"`
	EmbeddedContext bool `json:"embeddedContext,omitzero"`
}

type agentInfo struct {
	Name    string `json:"name,omitzero"`
	Version string `json:"version,omitzero"`
}

// sessionNewResult is the result of a session/new request.
type sessionNewResult struct {
	SessionID string          `json:"sessionId"`
	Models    modelsInfo      `json:"models,omitzero"`
	Modes     modesInfo       `json:"modes,omitzero"`
	Meta      json.RawMessage `json:"_meta,omitzero"`
}

type modelsInfo struct {
	CurrentModelID  string      `json:"currentModelId,omitzero"`
	AvailableModels []modelInfo `json:"availableModels,omitzero"`
}

type modelInfo struct {
	ModelID string `json:"modelId"`
	Name    string `json:"name,omitzero"`
}

type modesInfo struct {
	CurrentModeID  string     `json:"currentModeId,omitzero"`
	AvailableModes []modeInfo `json:"availableModes,omitzero"`
}

type modeInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitzero"`
}

// promptResult is the result of a session/prompt response.
type promptResult struct {
	StopReason string      `json:"stopReason,omitzero"` // "end_turn", "max_tokens", "cancelled", "refusal".
	Usage      promptUsage `json:"usage,omitzero"`
}

// promptUsage holds the token usage from a session/prompt response.
type promptUsage struct {
	TotalTokens       int `json:"totalTokens,omitzero"`
	InputTokens       int `json:"inputTokens,omitzero"`
	OutputTokens      int `json:"outputTokens,omitzero"`
	ThoughtTokens     int `json:"thoughtTokens,omitzero"`
	CachedReadTokens  int `json:"cachedReadTokens,omitzero"`
	CachedWriteTokens int `json:"cachedWriteTokens,omitzero"`
}
