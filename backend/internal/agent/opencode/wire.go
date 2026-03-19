// Wire types for the OpenCode ACP (Agent Client Protocol) JSON-RPC 2.0 protocol.
package opencode

import (
	"encoding/json"

	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// JSON-RPC notification method constants for the ACP protocol.
const (
	MethodSessionUpdate            = "session/update"
	MethodSessionRequestPermission = "session/request_permission"
	MethodSessionCancel            = "session/cancel"
	MethodSessionSetModel          = "session/set_model"
	MethodSessionSetMode           = "session/set_mode"
)

// Session update type discriminators (sessionUpdate field).
const (
	UpdateAgentMessageChunk       = "agent_message_chunk"
	UpdateAgentThoughtChunk       = "agent_thought_chunk"
	UpdateUserMessageChunk        = "user_message_chunk"
	UpdateToolCall                = "tool_call"
	UpdateToolCallUpdate          = "tool_call_update"
	UpdatePlan                    = "plan"
	UpdateUsageUpdate             = "usage_update"
	UpdateCurrentModeUpdate       = "current_mode_update"
	UpdateSessionInfoUpdate       = "session_info_update"
	UpdateAvailableCommandsUpdate = "available_commands_update"
	UpdateConfigOptionUpdate      = "config_option_update"
)

// Tool call status constants.
const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// Tool call kind constants.
const (
	KindRead       = "read"
	KindEdit       = "edit"
	KindDelete     = "delete"
	KindMove       = "move"
	KindSearch     = "search"
	KindExecute    = "execute"
	KindThink      = "think"
	KindFetch      = "fetch"
	KindSwitchMode = "switch_mode"
	KindOther      = "other"
)

// Plan entry status constants.
const (
	PlanStatusPending    = "pending"
	PlanStatusInProgress = "in_progress"
	PlanStatusCompleted  = "completed"
	PlanStatusCancelled  = "cancelled"
)

// ---------- JSON-RPC envelope ----------

// JSONRPCMessage is the JSON-RPC 2.0 envelope for ACP messages.
type JSONRPCMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	Method  string           `json:"method,omitzero"`
	ID      *json.RawMessage `json:"id,omitzero"`
	Params  json.RawMessage  `json:"params,omitzero"`
	Result  json.RawMessage  `json:"result,omitzero"`
	Error   *JSONRPCError    `json:"error,omitzero"`
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
	Type   string           `json:"type,omitzero"`
	Method string           `json:"method,omitzero"`
	ID     *json.RawMessage `json:"id,omitzero"`
}

// paramsProbe extracts the raw params field from a JSON-RPC message.
type paramsProbe struct {
	Params json.RawMessage `json:"params,omitzero"`
}

// ---------- Session update envelope ----------

// SessionUpdateParams holds the params for session/update notifications.
type SessionUpdateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// updateProbe extracts the discriminator from a session update.
type updateProbe struct {
	SessionUpdate string `json:"sessionUpdate"`
}

// ---------- Content types ----------

// ContentBlock is a content block in message chunks. This is a flat union:
// fields are populated depending on Type.
//
//   - "text":          Text, Annotations
//   - "image":         Data, MimeType, URI
//   - "resource":      Resource
//   - "resource_link": URI, Name, MimeType
type ContentBlock struct {
	Type        string          `json:"type"`
	Text        string          `json:"text,omitzero"`
	Data        string          `json:"data,omitzero"` // Base64 image data.
	MimeType    string          `json:"mimeType,omitzero"`
	URI         string          `json:"uri,omitzero"`
	Name        string          `json:"name,omitzero"`
	Resource    json.RawMessage `json:"resource,omitzero"`
	Annotations json.RawMessage `json:"annotations,omitzero"`
	jsonutil.Overflow
}

var contentBlockKnown = jsonutil.KnownFields(ContentBlock{})

// UnmarshalJSON implements json.Unmarshaler.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type Alias ContentBlock
	return jsonutil.UnmarshalRecord(data, (*Alias)(b), &b.Overflow, contentBlockKnown, "ContentBlock")
}

// ---------- Session update types ----------

// AgentMessageChunkUpdate is a streaming text chunk from the agent.
type AgentMessageChunkUpdate struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
	jsonutil.Overflow
}

var agentMessageChunkUpdateKnown = jsonutil.KnownFields(AgentMessageChunkUpdate{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *AgentMessageChunkUpdate) UnmarshalJSON(data []byte) error {
	type Alias AgentMessageChunkUpdate
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, agentMessageChunkUpdateKnown, "AgentMessageChunkUpdate")
}

// AgentThoughtChunkUpdate is a streaming reasoning chunk from the agent.
type AgentThoughtChunkUpdate struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
	jsonutil.Overflow
}

var agentThoughtChunkUpdateKnown = jsonutil.KnownFields(AgentThoughtChunkUpdate{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *AgentThoughtChunkUpdate) UnmarshalJSON(data []byte) error {
	type Alias AgentThoughtChunkUpdate
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, agentThoughtChunkUpdateKnown, "AgentThoughtChunkUpdate")
}

// UserMessageChunkUpdate is a replayed user message (during session/load).
type UserMessageChunkUpdate struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
	jsonutil.Overflow
}

var userMessageChunkUpdateKnown = jsonutil.KnownFields(UserMessageChunkUpdate{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *UserMessageChunkUpdate) UnmarshalJSON(data []byte) error {
	type Alias UserMessageChunkUpdate
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, userMessageChunkUpdateKnown, "UserMessageChunkUpdate")
}

// ToolCallLocation is a file location associated with a tool call.
type ToolCallLocation struct {
	Path string `json:"path,omitzero"`
	Line int    `json:"line,omitzero"`
}

// ToolCallUpdate is the initial tool call announcement.
type ToolCallUpdate struct {
	SessionUpdate string             `json:"sessionUpdate"`
	ToolCallID    string             `json:"toolCallId"`
	Title         string             `json:"title,omitzero"`
	Kind          string             `json:"kind,omitzero"`
	Status        string             `json:"status,omitzero"`
	Locations     []ToolCallLocation `json:"locations,omitzero"`
	RawInput      json.RawMessage    `json:"rawInput,omitzero"`
	jsonutil.Overflow
}

var toolCallUpdateKnown = jsonutil.KnownFields(ToolCallUpdate{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *ToolCallUpdate) UnmarshalJSON(data []byte) error {
	type Alias ToolCallUpdate
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, toolCallUpdateKnown, "ToolCallUpdate")
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
	jsonutil.Overflow
}

var toolCallContentKnown = jsonutil.KnownFields(ToolCallContent{})

// UnmarshalJSON implements json.Unmarshaler.
func (c *ToolCallContent) UnmarshalJSON(data []byte) error {
	type Alias ToolCallContent
	return jsonutil.UnmarshalRecord(data, (*Alias)(c), &c.Overflow, toolCallContentKnown, "ToolCallContent")
}

// ToolCallRawOutput is the structured raw output from a tool call.
type ToolCallRawOutput struct {
	Output   string          `json:"output,omitzero"`
	Error    string          `json:"error,omitzero"`
	Metadata json.RawMessage `json:"metadata,omitzero"`
	jsonutil.Overflow
}

var toolCallRawOutputKnown = jsonutil.KnownFields(ToolCallRawOutput{})

// UnmarshalJSON implements json.Unmarshaler.
func (o *ToolCallRawOutput) UnmarshalJSON(data []byte) error {
	type Alias ToolCallRawOutput
	return jsonutil.UnmarshalRecord(data, (*Alias)(o), &o.Overflow, toolCallRawOutputKnown, "ToolCallRawOutput")
}

// ToolCallUpdateUpdate is a tool call progress/completion update.
type ToolCallUpdateUpdate struct {
	SessionUpdate string             `json:"sessionUpdate"`
	ToolCallID    string             `json:"toolCallId"`
	Title         string             `json:"title,omitzero"`
	Kind          string             `json:"kind,omitzero"`
	Status        string             `json:"status,omitzero"`
	Locations     []ToolCallLocation `json:"locations,omitzero"`
	RawInput      json.RawMessage    `json:"rawInput,omitzero"`
	RawOutput     *ToolCallRawOutput `json:"rawOutput,omitzero"`
	Content       []ToolCallContent  `json:"content,omitzero"`
	jsonutil.Overflow
}

var toolCallUpdateUpdateKnown = jsonutil.KnownFields(ToolCallUpdateUpdate{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *ToolCallUpdateUpdate) UnmarshalJSON(data []byte) error {
	type Alias ToolCallUpdateUpdate
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, toolCallUpdateUpdateKnown, "ToolCallUpdateUpdate")
}

// PlanEntry is a single entry in a plan update.
type PlanEntry struct {
	Priority string `json:"priority,omitzero"`
	Status   string `json:"status"`
	Content  string `json:"content"`
	jsonutil.Overflow
}

var planEntryKnown = jsonutil.KnownFields(PlanEntry{})

// UnmarshalJSON implements json.Unmarshaler.
func (e *PlanEntry) UnmarshalJSON(data []byte) error {
	type Alias PlanEntry
	return jsonutil.UnmarshalRecord(data, (*Alias)(e), &e.Overflow, planEntryKnown, "PlanEntry")
}

// PlanUpdate is a todo/plan update from the agent.
type PlanUpdate struct {
	SessionUpdate string      `json:"sessionUpdate"`
	Entries       []PlanEntry `json:"entries"`
	jsonutil.Overflow
}

var planUpdateKnown = jsonutil.KnownFields(PlanUpdate{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *PlanUpdate) UnmarshalJSON(data []byte) error {
	type Alias PlanUpdate
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, planUpdateKnown, "PlanUpdate")
}

// UsageCost describes the cost of usage.
type UsageCost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// UsageUpdateUpdate is a context window / cost update.
type UsageUpdateUpdate struct {
	SessionUpdate string    `json:"sessionUpdate"`
	Used          int       `json:"used"`
	Size          int       `json:"size"`
	Cost          UsageCost `json:"cost,omitzero"`
	jsonutil.Overflow
}

var usageUpdateUpdateKnown = jsonutil.KnownFields(UsageUpdateUpdate{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *UsageUpdateUpdate) UnmarshalJSON(data []byte) error {
	type Alias UsageUpdateUpdate
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, usageUpdateUpdateKnown, "UsageUpdateUpdate")
}

// CurrentModeUpdate is a mode change notification.
type CurrentModeUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	ModeID        string `json:"modeId,omitzero"`
	ModeName      string `json:"modeName,omitzero"`
	jsonutil.Overflow
}

var currentModeUpdateKnown = jsonutil.KnownFields(CurrentModeUpdate{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *CurrentModeUpdate) UnmarshalJSON(data []byte) error {
	type Alias CurrentModeUpdate
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, currentModeUpdateKnown, "CurrentModeUpdate")
}

// AvailableCommand is a single command in an available_commands_update.
type AvailableCommand struct {
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
}

// AvailableCommandsUpdate lists commands available in the current session.
type AvailableCommandsUpdate struct {
	SessionUpdate     string             `json:"sessionUpdate"`
	AvailableCommands []AvailableCommand `json:"availableCommands"`
	jsonutil.Overflow
}

var availableCommandsUpdateKnown = jsonutil.KnownFields(AvailableCommandsUpdate{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *AvailableCommandsUpdate) UnmarshalJSON(data []byte) error {
	type Alias AvailableCommandsUpdate
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, availableCommandsUpdateKnown, "AvailableCommandsUpdate")
}

// ---------- Permission request ----------

// PermissionToolCall describes the tool call in a permission request.
type PermissionToolCall struct {
	ToolCallID string             `json:"toolCallId"`
	Status     string             `json:"status,omitzero"`
	Title      string             `json:"title,omitzero"`
	Kind       string             `json:"kind,omitzero"`
	RawInput   json.RawMessage    `json:"rawInput,omitzero"`
	Locations  []ToolCallLocation `json:"locations,omitzero"`
	jsonutil.Overflow
}

var permissionToolCallKnown = jsonutil.KnownFields(PermissionToolCall{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *PermissionToolCall) UnmarshalJSON(data []byte) error {
	type Alias PermissionToolCall
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, permissionToolCallKnown, "PermissionToolCall")
}

// PermissionOption is a single option in a permission request.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"` // "allow_once", "allow_always", "reject_once".
	Name     string `json:"name"`
	jsonutil.Overflow
}

var permissionOptionKnown = jsonutil.KnownFields(PermissionOption{})

// UnmarshalJSON implements json.Unmarshaler.
func (o *PermissionOption) UnmarshalJSON(data []byte) error {
	type Alias PermissionOption
	return jsonutil.UnmarshalRecord(data, (*Alias)(o), &o.Overflow, permissionOptionKnown, "PermissionOption")
}

// PermissionRequestParams holds params for session/request_permission.
type PermissionRequestParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  PermissionToolCall `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
	jsonutil.Overflow
}

var permissionRequestParamsKnown = jsonutil.KnownFields(PermissionRequestParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *PermissionRequestParams) UnmarshalJSON(data []byte) error {
	type Alias PermissionRequestParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, permissionRequestParamsKnown, "PermissionRequestParams")
}

// ---------- Outbound request types ----------

// jsonrpcRequest is the envelope for all JSON-RPC 2.0 requests sent to OpenCode.
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitzero"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitzero"`
}

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

// promptContent is a single item in the session/prompt content array.
// This is a flat union discriminated by Type:
//
//   - "text":          Text
//   - "image":         Data (base64), MimeType
//   - "resource":      Resource (embedded resource)
//   - "resource_link": URI, Name, MimeType
type promptContent struct {
	Type     string          `json:"type"`
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

// ---------- Response types ----------

// initializeResult is the result of an initialize request.
type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities,omitzero"`
	AgentInfo         agentInfo         `json:"agentInfo,omitzero"`
	AuthMethods       json.RawMessage   `json:"authMethods,omitzero"`
	jsonutil.Overflow
}

var initializeResultKnown = jsonutil.KnownFields(initializeResult{})

// UnmarshalJSON implements json.Unmarshaler.
func (r *initializeResult) UnmarshalJSON(data []byte) error {
	type Alias initializeResult
	return jsonutil.UnmarshalRecord(data, (*Alias)(r), &r.Overflow, initializeResultKnown, "initializeResult")
}

type agentCapabilities struct {
	PromptCapabilities  *promptCapabilities `json:"promptCapabilities,omitzero"`
	LoadSession         bool                `json:"loadSession,omitzero"`
	McpCapabilities     json.RawMessage     `json:"mcpCapabilities,omitzero"`
	SessionCapabilities json.RawMessage     `json:"sessionCapabilities,omitzero"`
	jsonutil.Overflow
}

var agentCapabilitiesKnown = jsonutil.KnownFields(agentCapabilities{})

// UnmarshalJSON implements json.Unmarshaler.
func (c *agentCapabilities) UnmarshalJSON(data []byte) error {
	type Alias agentCapabilities
	return jsonutil.UnmarshalRecord(data, (*Alias)(c), &c.Overflow, agentCapabilitiesKnown, "agentCapabilities")
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
	Models    *modelsInfo     `json:"models,omitzero"`
	Modes     *modesInfo      `json:"modes,omitzero"`
	Meta      json.RawMessage `json:"_meta,omitzero"`
	jsonutil.Overflow
}

var sessionNewResultKnown = jsonutil.KnownFields(sessionNewResult{})

// UnmarshalJSON implements json.Unmarshaler.
func (r *sessionNewResult) UnmarshalJSON(data []byte) error {
	type Alias sessionNewResult
	return jsonutil.UnmarshalRecord(data, (*Alias)(r), &r.Overflow, sessionNewResultKnown, "sessionNewResult")
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
	StopReason string       `json:"stopReason,omitzero"` // "end_turn", "max_tokens", "cancelled", "refusal".
	Usage      *promptUsage `json:"usage,omitzero"`
	jsonutil.Overflow
}

var promptResultKnown = jsonutil.KnownFields(promptResult{})

// UnmarshalJSON implements json.Unmarshaler.
func (r *promptResult) UnmarshalJSON(data []byte) error {
	type Alias promptResult
	return jsonutil.UnmarshalRecord(data, (*Alias)(r), &r.Overflow, promptResultKnown, "promptResult")
}

// promptUsage holds the token usage from a session/prompt response.
type promptUsage struct {
	TotalTokens       int `json:"totalTokens,omitzero"`
	InputTokens       int `json:"inputTokens,omitzero"`
	OutputTokens      int `json:"outputTokens,omitzero"`
	ThoughtTokens     int `json:"thoughtTokens,omitzero"`
	CachedReadTokens  int `json:"cachedReadTokens,omitzero"`
	CachedWriteTokens int `json:"cachedWriteTokens,omitzero"`
	jsonutil.Overflow
}

var promptUsageKnown = jsonutil.KnownFields(promptUsage{})

// UnmarshalJSON implements json.Unmarshaler.
func (u *promptUsage) UnmarshalJSON(data []byte) error {
	type Alias promptUsage
	return jsonutil.UnmarshalRecord(data, (*Alias)(u), &u.Overflow, promptUsageKnown, "promptUsage")
}
