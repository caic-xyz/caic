// Wire types for the Codex CLI app-server JSON-RPC 2.0 protocol.
package codex

import (
	"encoding/json"

	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// JSON-RPC notification method constants for codex app-server.
const (
	MethodThreadStarted     = "thread/started"
	MethodTurnStarted       = "turn/started"
	MethodTurnCompleted     = "turn/completed"
	MethodItemStarted       = "item/started"
	MethodItemCompleted     = "item/completed"
	MethodItemUpdated       = "item/updated"
	MethodItemDelta         = "item/agentMessage/delta"
	MethodTokenUsageUpdated = "thread/tokenUsage/updated"

	MethodCommandOutputDelta        = "item/commandExecution/outputDelta"
	MethodCommandTerminalInteract   = "item/commandExecution/terminalInteraction"
	MethodFileChangeOutputDelta     = "item/fileChange/outputDelta"
	MethodReasoningSummaryTextDelta = "item/reasoning/summaryTextDelta"
	MethodReasoningSummaryPartAdded = "item/reasoning/summaryPartAdded"
	MethodReasoningTextDelta        = "item/reasoning/textDelta"
	MethodPlanDelta                 = "item/plan/delta"
	MethodMcpToolCallProgress       = "item/mcpToolCall/progress"
	MethodTurnDiffUpdated           = "turn/diff/updated"
	MethodTurnPlanUpdated           = "turn/plan/updated"
	MethodThreadStatusChanged       = "thread/status/changed"
	MethodThreadNameUpdated         = "thread/name/updated"
	MethodModelRerouted             = "model/rerouted"
	MethodErrorNotification         = "error"
)

// Item type constants (camelCase as emitted by Codex v2).
const (
	ItemTypeUserMessage         = "userMessage"
	ItemTypeAgentMessage        = "agentMessage"
	ItemTypePlan                = "plan"
	ItemTypeReasoning           = "reasoning"
	ItemTypeCommandExecution    = "commandExecution"
	ItemTypeFileChange          = "fileChange"
	ItemTypeMCPToolCall         = "mcpToolCall"
	ItemTypeWebSearch           = "webSearch"
	ItemTypeImageView           = "imageView"
	ItemTypeContextCompaction   = "contextCompaction"
	ItemTypeDynamicToolCall     = "dynamicToolCall"
	ItemTypeCollabAgentToolCall = "collabAgentToolCall"
	ItemTypeEnteredReviewMode   = "enteredReviewMode"
	ItemTypeExitedReviewMode    = "exitedReviewMode"
)

// ---------- JSON-RPC envelope ----------

// JSONRPCMessage is the JSON-RPC 2.0 envelope for codex app-server messages.
// Notifications have Method set and ID nil. Responses have ID set.
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

// messageProbe extracts routing fields from a codex app-server line to
// distinguish caic-injected JSON (has "type") from JSON-RPC (has "method"/"id").
type messageProbe struct {
	Type   string           `json:"type,omitzero"`
	Method string           `json:"method,omitzero"`
	ID     *json.RawMessage `json:"id,omitzero"`
}

// methodProbe extracts the method field from a JSON-RPC message.
type methodProbe struct {
	Method string `json:"method,omitzero"`
}

// ---------- Handshake types ----------

// threadStartResult is the result object from a thread/start JSON-RPC response.
type threadStartResult struct {
	Thread threadStartThread `json:"thread"`
}

// threadStartThread is the thread object inside a threadStartResult.
type threadStartThread struct {
	ID string `json:"id"`
}

// ---------- Thread lifecycle ----------

// ThreadStartedParams holds the params for thread/started notifications.
type ThreadStartedParams struct {
	Thread ThreadInfo `json:"thread"`
	jsonutil.Overflow
}

var threadStartedParamsKnown = jsonutil.KnownFields(ThreadStartedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ThreadStartedParams) UnmarshalJSON(data []byte) error {
	type Alias ThreadStartedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, threadStartedParamsKnown, "ThreadStartedParams")
}

// ThreadInfo describes a thread in thread/started params.
type ThreadInfo struct {
	ID            string          `json:"id"`
	CLIVersion    string          `json:"cliVersion,omitzero"`
	CreatedAt     int64           `json:"createdAt,omitzero"`
	CWD           string          `json:"cwd,omitzero"`
	GitInfo       json.RawMessage `json:"gitInfo,omitzero"`
	ModelProvider string          `json:"modelProvider,omitzero"`
	Path          string          `json:"path,omitzero"`
	Preview       string          `json:"preview,omitzero"`
	Source        string          `json:"source,omitzero"`
	UpdatedAt     int64           `json:"updatedAt,omitzero"`
	Status        string          `json:"status,omitzero"`
	Name          string          `json:"name,omitzero"`
	AgentNickname string          `json:"agentNickname,omitzero"`
	AgentRole     string          `json:"agentRole,omitzero"`
	Turns         json.RawMessage `json:"turns,omitzero"`
	jsonutil.Overflow
}

var threadInfoKnown = jsonutil.KnownFields(ThreadInfo{})

// UnmarshalJSON implements json.Unmarshaler.
func (t *ThreadInfo) UnmarshalJSON(data []byte) error {
	type Alias ThreadInfo
	return jsonutil.UnmarshalRecord(data, (*Alias)(t), &t.Overflow, threadInfoKnown, "ThreadInfo")
}

// ---------- Turn lifecycle ----------

// TurnStartedParams holds the params for turn/started notifications.
type TurnStartedParams struct {
	ThreadID string   `json:"threadId"`
	Turn     TurnInfo `json:"turn"`
	jsonutil.Overflow
}

var turnStartedParamsKnown = jsonutil.KnownFields(TurnStartedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *TurnStartedParams) UnmarshalJSON(data []byte) error {
	type Alias TurnStartedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, turnStartedParamsKnown, "TurnStartedParams")
}

// TurnCompletedParams holds the params for turn/completed notifications.
type TurnCompletedParams struct {
	ThreadID string   `json:"threadId"`
	Turn     TurnInfo `json:"turn"`
	jsonutil.Overflow
}

var turnCompletedParamsKnown = jsonutil.KnownFields(TurnCompletedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *TurnCompletedParams) UnmarshalJSON(data []byte) error {
	type Alias TurnCompletedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, turnCompletedParamsKnown, "TurnCompletedParams")
}

// TurnInfo describes a turn in turn/started and turn/completed params.
type TurnInfo struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Error  *TurnError      `json:"error,omitzero"`
	Items  json.RawMessage `json:"items,omitzero"`
	jsonutil.Overflow
}

var turnInfoKnown = jsonutil.KnownFields(TurnInfo{})

// UnmarshalJSON implements json.Unmarshaler.
func (t *TurnInfo) UnmarshalJSON(data []byte) error {
	type Alias TurnInfo
	return jsonutil.UnmarshalRecord(data, (*Alias)(t), &t.Overflow, turnInfoKnown, "TurnInfo")
}

// TurnError describes a turn failure.
type TurnError struct {
	Message           string          `json:"message"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo,omitzero"`
	AdditionalDetails string          `json:"additionalDetails,omitzero"`
	jsonutil.Overflow
}

var turnErrorKnown = jsonutil.KnownFields(TurnError{})

// UnmarshalJSON implements json.Unmarshaler.
func (e *TurnError) UnmarshalJSON(data []byte) error {
	type Alias TurnError
	return jsonutil.UnmarshalRecord(data, (*Alias)(e), &e.Overflow, turnErrorKnown, "TurnError")
}

// ---------- Item envelope ----------

// ItemParams holds the params for item/started, item/completed, and
// item/updated notifications. Item is raw JSON dispatched by ItemHeader.Type.
type ItemParams struct {
	Item     json.RawMessage `json:"item"`
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	jsonutil.Overflow
}

var itemParamsKnown = jsonutil.KnownFields(ItemParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ItemParams) UnmarshalJSON(data []byte) error {
	type Alias ItemParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, itemParamsKnown, "ItemParams")
}

// ItemHeader extracts the discriminant fields from a raw item for dispatch.
type ItemHeader struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// ItemDeltaParams holds the params for item/agentMessage/delta notifications.
type ItemDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
	jsonutil.Overflow
}

var itemDeltaParamsKnown = jsonutil.KnownFields(ItemDeltaParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ItemDeltaParams) UnmarshalJSON(data []byte) error {
	type Alias ItemDeltaParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, itemDeltaParamsKnown, "ItemDeltaParams")
}

// ---------- Per-item-type structs ----------

// AgentMessageItem is an agent text response item.
type AgentMessageItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Text   string `json:"text,omitzero"`
	Phase  string `json:"phase,omitzero"`
	Status string `json:"status,omitzero"`
	jsonutil.Overflow
}

var agentMessageItemKnown = jsonutil.KnownFields(AgentMessageItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *AgentMessageItem) UnmarshalJSON(data []byte) error {
	type Alias AgentMessageItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, agentMessageItemKnown, "AgentMessageItem")
}

// PlanItem is an agent plan item.
type PlanItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Text   string `json:"text,omitzero"`
	Status string `json:"status,omitzero"`
	jsonutil.Overflow
}

var planItemKnown = jsonutil.KnownFields(PlanItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *PlanItem) UnmarshalJSON(data []byte) error {
	type Alias PlanItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, planItemKnown, "PlanItem")
}

// ReasoningItem is an agent reasoning/thinking item.
type ReasoningItem struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Summary []string        `json:"summary,omitzero"`
	Content json.RawMessage `json:"content,omitzero"`
	Status  string          `json:"status,omitzero"`
	jsonutil.Overflow
}

var reasoningItemKnown = jsonutil.KnownFields(ReasoningItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *ReasoningItem) UnmarshalJSON(data []byte) error {
	type Alias ReasoningItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, reasoningItemKnown, "ReasoningItem")
}

// CommandExecutionItem is a shell command execution item.
type CommandExecutionItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Command          string          `json:"command,omitzero"`
	Cwd              string          `json:"cwd,omitzero"`
	ProcessID        string          `json:"processId,omitzero"`
	Status           string          `json:"status,omitzero"`
	CommandActions   json.RawMessage `json:"commandActions,omitzero"`
	AggregatedOutput *string         `json:"aggregatedOutput,omitzero"`
	ExitCode         *int            `json:"exitCode,omitzero"`
	DurationMs       *int64          `json:"durationMs,omitzero"`
	jsonutil.Overflow
}

var commandExecutionItemKnown = jsonutil.KnownFields(CommandExecutionItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *CommandExecutionItem) UnmarshalJSON(data []byte) error {
	type Alias CommandExecutionItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, commandExecutionItemKnown, "CommandExecutionItem")
}

// FileChangeItem is a file creation/modification/deletion item.
type FileChangeItem struct {
	ID      string             `json:"id"`
	Type    string             `json:"type"`
	Changes []FileUpdateChange `json:"changes,omitzero"`
	Status  string             `json:"status,omitzero"`
	jsonutil.Overflow
}

var fileChangeItemKnown = jsonutil.KnownFields(FileChangeItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *FileChangeItem) UnmarshalJSON(data []byte) error {
	type Alias FileChangeItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, fileChangeItemKnown, "FileChangeItem")
}

// McpToolCallItem is an MCP tool call item.
type McpToolCallItem struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Server     string             `json:"server,omitzero"`
	Tool       string             `json:"tool,omitzero"`
	Status     string             `json:"status,omitzero"`
	Arguments  json.RawMessage    `json:"arguments,omitzero"`
	Result     *McpToolCallResult `json:"result,omitzero"`
	Error      *McpToolCallError  `json:"error,omitzero"`
	DurationMs *int64             `json:"durationMs,omitzero"`
	jsonutil.Overflow
}

var mcpToolCallItemKnown = jsonutil.KnownFields(McpToolCallItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *McpToolCallItem) UnmarshalJSON(data []byte) error {
	type Alias McpToolCallItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, mcpToolCallItemKnown, "McpToolCallItem")
}

// DynamicToolCallItem is a dynamically registered tool call item.
type DynamicToolCallItem struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Tool         string          `json:"tool,omitzero"`
	Arguments    json.RawMessage `json:"arguments,omitzero"`
	Status       string          `json:"status,omitzero"`
	ContentItems json.RawMessage `json:"contentItems,omitzero"`
	Success      *bool           `json:"success,omitzero"`
	DurationMs   *int64          `json:"durationMs,omitzero"`
	jsonutil.Overflow
}

var dynamicToolCallItemKnown = jsonutil.KnownFields(DynamicToolCallItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *DynamicToolCallItem) UnmarshalJSON(data []byte) error {
	type Alias DynamicToolCallItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, dynamicToolCallItemKnown, "DynamicToolCallItem")
}

// CollabAgentToolCallItem is a collaborative multi-agent tool call item.
type CollabAgentToolCallItem struct {
	ID                string          `json:"id"`
	Type              string          `json:"type"`
	Tool              string          `json:"tool,omitzero"`
	Status            string          `json:"status,omitzero"`
	SenderThreadID    string          `json:"senderThreadId,omitzero"`
	ReceiverThreadIDs json.RawMessage `json:"receiverThreadIds,omitzero"`
	Prompt            string          `json:"prompt,omitzero"`
	AgentsStates      json.RawMessage `json:"agentsStates,omitzero"`
	jsonutil.Overflow
}

var collabAgentToolCallItemKnown = jsonutil.KnownFields(CollabAgentToolCallItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *CollabAgentToolCallItem) UnmarshalJSON(data []byte) error {
	type Alias CollabAgentToolCallItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, collabAgentToolCallItemKnown, "CollabAgentToolCallItem")
}

// WebSearchItem is a web search item.
type WebSearchItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Query  string `json:"query,omitzero"`
	Action string `json:"action,omitzero"`
	Status string `json:"status,omitzero"`
	jsonutil.Overflow
}

var webSearchItemKnown = jsonutil.KnownFields(WebSearchItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *WebSearchItem) UnmarshalJSON(data []byte) error {
	type Alias WebSearchItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, webSearchItemKnown, "WebSearchItem")
}

// ImageViewItem is an image viewing item.
type ImageViewItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Path   string `json:"path,omitzero"`
	Status string `json:"status,omitzero"`
	jsonutil.Overflow
}

var imageViewItemKnown = jsonutil.KnownFields(ImageViewItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *ImageViewItem) UnmarshalJSON(data []byte) error {
	type Alias ImageViewItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, imageViewItemKnown, "ImageViewItem")
}

// EnteredReviewModeItem signals the agent entered review mode.
type EnteredReviewModeItem struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Review json.RawMessage `json:"review,omitzero"`
	jsonutil.Overflow
}

var enteredReviewModeItemKnown = jsonutil.KnownFields(EnteredReviewModeItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *EnteredReviewModeItem) UnmarshalJSON(data []byte) error {
	type Alias EnteredReviewModeItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, enteredReviewModeItemKnown, "EnteredReviewModeItem")
}

// ExitedReviewModeItem signals the agent exited review mode.
type ExitedReviewModeItem struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Review json.RawMessage `json:"review,omitzero"`
	jsonutil.Overflow
}

var exitedReviewModeItemKnown = jsonutil.KnownFields(ExitedReviewModeItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *ExitedReviewModeItem) UnmarshalJSON(data []byte) error {
	type Alias ExitedReviewModeItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, exitedReviewModeItemKnown, "ExitedReviewModeItem")
}

// ContextCompactionItem signals a context window compaction.
type ContextCompactionItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	jsonutil.Overflow
}

var contextCompactionItemKnown = jsonutil.KnownFields(ContextCompactionItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *ContextCompactionItem) UnmarshalJSON(data []byte) error {
	type Alias ContextCompactionItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, contextCompactionItemKnown, "ContextCompactionItem")
}

// UserMessageItem is a user-submitted message item.
type UserMessageItem struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content,omitzero"`
	Status  string          `json:"status,omitzero"`
	jsonutil.Overflow
}

var userMessageItemKnown = jsonutil.KnownFields(UserMessageItem{})

// UnmarshalJSON implements json.Unmarshaler.
func (item *UserMessageItem) UnmarshalJSON(data []byte) error {
	type Alias UserMessageItem
	return jsonutil.UnmarshalRecord(data, (*Alias)(item), &item.Overflow, userMessageItemKnown, "UserMessageItem")
}

// ---------- Item field types ----------

// FileUpdateChange describes a single file change within a fileChange item.
type FileUpdateChange struct {
	Path string          `json:"path"`
	Kind PatchChangeKind `json:"kind"`
	Diff string          `json:"diff,omitzero"`
}

// PatchChangeKind is the discriminated kind for FileUpdateChange.
type PatchChangeKind struct {
	Type     string  `json:"type"`
	MovePath *string `json:"movePath,omitzero"`
}

// McpToolCallResult holds the result of a successful MCP tool call.
type McpToolCallResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitzero"`
}

// McpToolCallError holds the error from a failed MCP tool call.
type McpToolCallError struct {
	Message string `json:"message"`
}

// ---------- Token usage ----------

// TokenUsageUpdatedParams holds params for thread/tokenUsage/updated notifications.
type TokenUsageUpdatedParams struct {
	ThreadID   string           `json:"threadId"`
	TurnID     string           `json:"turnId"`
	TokenUsage ThreadTokenUsage `json:"tokenUsage"`
	jsonutil.Overflow
}

var tokenUsageUpdatedParamsKnown = jsonutil.KnownFields(TokenUsageUpdatedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *TokenUsageUpdatedParams) UnmarshalJSON(data []byte) error {
	type Alias TokenUsageUpdatedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, tokenUsageUpdatedParamsKnown, "TokenUsageUpdatedParams")
}

// ThreadTokenUsage holds cumulative and per-turn token usage for a thread.
type ThreadTokenUsage struct {
	Total              TokenUsageBreakdown `json:"total"`
	Last               TokenUsageBreakdown `json:"last"`
	ModelContextWindow *int64              `json:"modelContextWindow,omitzero"`
}

// TokenUsageBreakdown contains a detailed breakdown of token counts.
type TokenUsageBreakdown struct {
	TotalTokens           int64 `json:"totalTokens"`
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
}

// ---------- Delta notification params ----------

// CommandOutputDeltaParams holds params for item/commandExecution/outputDelta.
type CommandOutputDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
	jsonutil.Overflow
}

var commandOutputDeltaParamsKnown = jsonutil.KnownFields(CommandOutputDeltaParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *CommandOutputDeltaParams) UnmarshalJSON(data []byte) error {
	type Alias CommandOutputDeltaParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, commandOutputDeltaParamsKnown, "CommandOutputDeltaParams")
}

// TerminalInteractionParams holds params for item/commandExecution/terminalInteraction.
type TerminalInteractionParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	ItemID    string `json:"itemId"`
	ProcessID string `json:"processId"`
	Stdin     string `json:"stdin"`
	jsonutil.Overflow
}

var terminalInteractionParamsKnown = jsonutil.KnownFields(TerminalInteractionParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *TerminalInteractionParams) UnmarshalJSON(data []byte) error {
	type Alias TerminalInteractionParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, terminalInteractionParamsKnown, "TerminalInteractionParams")
}

// FileChangeOutputDeltaParams holds params for item/fileChange/outputDelta.
type FileChangeOutputDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
	jsonutil.Overflow
}

var fileChangeOutputDeltaParamsKnown = jsonutil.KnownFields(FileChangeOutputDeltaParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *FileChangeOutputDeltaParams) UnmarshalJSON(data []byte) error {
	type Alias FileChangeOutputDeltaParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, fileChangeOutputDeltaParamsKnown, "FileChangeOutputDeltaParams")
}

// ReasoningSummaryTextDeltaParams holds params for item/reasoning/summaryTextDelta.
type ReasoningSummaryTextDeltaParams struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	Delta        string `json:"delta"`
	SummaryIndex int    `json:"summaryIndex"`
	jsonutil.Overflow
}

var reasoningSummaryTextDeltaParamsKnown = jsonutil.KnownFields(ReasoningSummaryTextDeltaParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ReasoningSummaryTextDeltaParams) UnmarshalJSON(data []byte) error {
	type Alias ReasoningSummaryTextDeltaParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, reasoningSummaryTextDeltaParamsKnown, "ReasoningSummaryTextDeltaParams")
}

// ReasoningSummaryPartAddedParams holds params for item/reasoning/summaryPartAdded.
type ReasoningSummaryPartAddedParams struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	SummaryIndex int    `json:"summaryIndex"`
	jsonutil.Overflow
}

var reasoningSummaryPartAddedParamsKnown = jsonutil.KnownFields(ReasoningSummaryPartAddedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ReasoningSummaryPartAddedParams) UnmarshalJSON(data []byte) error {
	type Alias ReasoningSummaryPartAddedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, reasoningSummaryPartAddedParamsKnown, "ReasoningSummaryPartAddedParams")
}

// ReasoningTextDeltaParams holds params for item/reasoning/textDelta.
type ReasoningTextDeltaParams struct {
	ThreadID     string `json:"threadId"`
	TurnID       string `json:"turnId"`
	ItemID       string `json:"itemId"`
	Delta        string `json:"delta"`
	ContentIndex int    `json:"contentIndex"`
	jsonutil.Overflow
}

var reasoningTextDeltaParamsKnown = jsonutil.KnownFields(ReasoningTextDeltaParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ReasoningTextDeltaParams) UnmarshalJSON(data []byte) error {
	type Alias ReasoningTextDeltaParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, reasoningTextDeltaParamsKnown, "ReasoningTextDeltaParams")
}

// PlanDeltaParams holds params for item/plan/delta.
type PlanDeltaParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
	jsonutil.Overflow
}

var planDeltaParamsKnown = jsonutil.KnownFields(PlanDeltaParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *PlanDeltaParams) UnmarshalJSON(data []byte) error {
	type Alias PlanDeltaParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, planDeltaParamsKnown, "PlanDeltaParams")
}

// McpToolCallProgressParams holds params for item/mcpToolCall/progress.
type McpToolCallProgressParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Message  string `json:"message"`
	jsonutil.Overflow
}

var mcpToolCallProgressParamsKnown = jsonutil.KnownFields(McpToolCallProgressParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *McpToolCallProgressParams) UnmarshalJSON(data []byte) error {
	type Alias McpToolCallProgressParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, mcpToolCallProgressParamsKnown, "McpToolCallProgressParams")
}

// ---------- Other notification params ----------

// TurnDiffUpdatedParams holds params for turn/diff/updated.
type TurnDiffUpdatedParams struct {
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	Diff     json.RawMessage `json:"diff"`
	jsonutil.Overflow
}

var turnDiffUpdatedParamsKnown = jsonutil.KnownFields(TurnDiffUpdatedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *TurnDiffUpdatedParams) UnmarshalJSON(data []byte) error {
	type Alias TurnDiffUpdatedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, turnDiffUpdatedParamsKnown, "TurnDiffUpdatedParams")
}

// TurnPlanUpdatedParams holds params for turn/plan/updated.
type TurnPlanUpdatedParams struct {
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	Explanation string          `json:"explanation,omitzero"`
	Plan        json.RawMessage `json:"plan,omitzero"`
	jsonutil.Overflow
}

var turnPlanUpdatedParamsKnown = jsonutil.KnownFields(TurnPlanUpdatedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *TurnPlanUpdatedParams) UnmarshalJSON(data []byte) error {
	type Alias TurnPlanUpdatedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, turnPlanUpdatedParamsKnown, "TurnPlanUpdatedParams")
}

// ThreadStatusChangedParams holds params for thread/status/changed.
type ThreadStatusChangedParams struct {
	ThreadID string `json:"threadId"`
	Status   string `json:"status"`
	jsonutil.Overflow
}

var threadStatusChangedParamsKnown = jsonutil.KnownFields(ThreadStatusChangedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ThreadStatusChangedParams) UnmarshalJSON(data []byte) error {
	type Alias ThreadStatusChangedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, threadStatusChangedParamsKnown, "ThreadStatusChangedParams")
}

// ThreadNameUpdatedParams holds params for thread/name/updated.
type ThreadNameUpdatedParams struct {
	ThreadID   string `json:"threadId"`
	ThreadName string `json:"threadName"`
	jsonutil.Overflow
}

var threadNameUpdatedParamsKnown = jsonutil.KnownFields(ThreadNameUpdatedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ThreadNameUpdatedParams) UnmarshalJSON(data []byte) error {
	type Alias ThreadNameUpdatedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, threadNameUpdatedParamsKnown, "ThreadNameUpdatedParams")
}

// ModelReroutedParams holds params for model/rerouted.
type ModelReroutedParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	FromModel string `json:"fromModel"`
	ToModel   string `json:"toModel"`
	Reason    string `json:"reason,omitzero"`
	jsonutil.Overflow
}

var modelReroutedParamsKnown = jsonutil.KnownFields(ModelReroutedParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ModelReroutedParams) UnmarshalJSON(data []byte) error {
	type Alias ModelReroutedParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, modelReroutedParamsKnown, "ModelReroutedParams")
}

// ErrorNotificationParams holds params for error notifications.
type ErrorNotificationParams struct {
	Error     *TurnError `json:"error"`
	WillRetry bool       `json:"willRetry,omitzero"`
	ThreadID  string     `json:"threadId,omitzero"`
	TurnID    string     `json:"turnId,omitzero"`
	jsonutil.Overflow
}

var errorNotificationParamsKnown = jsonutil.KnownFields(ErrorNotificationParams{})

// UnmarshalJSON implements json.Unmarshaler.
func (p *ErrorNotificationParams) UnmarshalJSON(data []byte) error {
	type Alias ErrorNotificationParams
	return jsonutil.UnmarshalRecord(data, (*Alias)(p), &p.Overflow, errorNotificationParamsKnown, "ErrorNotificationParams")
}
