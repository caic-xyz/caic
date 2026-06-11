// Package mcp implements the Model Context Protocol HTTP endpoint.
package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
)

const (
	jsonRPCVersion = "2.0"

	// ProtocolVersion is the supported MCP protocol version.
	ProtocolVersion = "2026-07-28"

	// DefaultTTLMS is the default cache lifetime returned by MCP list/read methods.
	DefaultTTLMS = 10_000
)

// ErrorCode is a JSON-RPC error code used by MCP responses.
type ErrorCode int

// JSON-RPC error codes returned by the MCP handler.
const (
	ParseErrorCode                 ErrorCode = -32700
	InvalidRequestCode             ErrorCode = -32600
	MethodNotFoundCode             ErrorCode = -32601
	InvalidParamsCode              ErrorCode = -32602
	InternalErrorCode              ErrorCode = -32603
	HeaderMismatchCode             ErrorCode = -32001
	UnsupportedProtocolVersionCode ErrorCode = -32004
)

// Method is an MCP JSON-RPC method name.
type Method string

// MCP method names supported by the handler.
const (
	MethodServerDiscover        Method = "server/discover"
	MethodToolsList             Method = "tools/list"
	MethodToolsCall             Method = "tools/call"
	MethodResourcesList         Method = "resources/list"
	MethodResourcesRead         Method = "resources/read"
	MethodResourceTemplatesList Method = "resources/templates/list"
	MethodSubscriptionsListen   Method = "subscriptions/listen"
)

// ResultType describes whether an MCP result is complete or partial.
type ResultType string

// Result type values used by MCP responses.
const (
	ResultTypeComplete ResultType = "complete"
)

// CacheScope describes who may cache an MCP result.
type CacheScope string

// Cache scope values used by MCP responses.
const (
	CacheScopePublic  CacheScope = "public"
	CacheScopePrivate CacheScope = "private"
)

// ContentType identifies the kind of content block in a tool result.
type ContentType string

// Content type values used by MCP tool results.
const (
	ContentTypeText ContentType = "text"
)

// Handler serves the MCP JSON-RPC HTTP endpoint.
type Handler struct {
	Registry     Registry
	ServerInfo   Implementation
	Instructions string
}

// Registry supplies MCP tools and resources to Handler.
type Registry interface {
	Tools(ctx context.Context) ([]ToolDescriptor, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (RawToolResult, error)
	ListResources(ctx context.Context) ResourcesListResult
	ReadResource(ctx context.Context, uri string) (ResourcesReadResult, error)
}

// RawToolResult is the transport-neutral output from a tool handler.
type RawToolResult struct {
	Structured any
	IsError    bool
}

// JSONRPCRequest is an incoming JSON-RPC request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitzero"`
	Method  Method          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse is an outgoing JSON-RPC response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitzero"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// JSONRPCError is a JSON-RPC error object.
type JSONRPCError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Data    any       `json:"data,omitempty"`
}

type unsupportedProtocolVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

// MetaObject is an MCP _meta payload.
type MetaObject map[string]any

// RequestMeta is the MCP metadata object required in request params.
type RequestMeta struct {
	ProtocolVersion    string             `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         Implementation     `json:"io.modelcontextprotocol/clientInfo"`
	ClientCapabilities ClientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
	ProgressToken      any                `json:"progressToken,omitempty"`
	LogLevel           string             `json:"io.modelcontextprotocol/logLevel,omitempty"`
}

// UnmarshalJSON decodes RequestMeta while preserving forward-compatible fields through typed members.
func (m *RequestMeta) UnmarshalJSON(data []byte) error {
	type requestMeta RequestMeta
	var meta requestMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	*m = RequestMeta(meta)
	return nil
}

// ClientCapabilities describes client-supported MCP capabilities.
type ClientCapabilities struct {
	Experimental Extensions             `json:"experimental,omitempty"`
	Roots        MetaObject             `json:"roots,omitempty"`
	Sampling     *SamplingCapability    `json:"sampling,omitempty"`
	Elicitation  *ElicitationCapability `json:"elicitation,omitempty"`
	Extensions   Extensions             `json:"extensions,omitempty"`
}

// SamplingCapability describes client sampling support.
type SamplingCapability struct {
	Context MetaObject `json:"context,omitempty"`
	Tools   MetaObject `json:"tools,omitempty"`
}

// ElicitationCapability describes client elicitation support.
type ElicitationCapability struct {
	Form MetaObject `json:"form,omitempty"`
	URL  MetaObject `json:"url,omitempty"`
}

// Extensions stores namespaced MCP extension payloads.
type Extensions map[string]json.RawMessage

// RequestParams contains common MCP request metadata.
type RequestParams struct {
	Meta RequestMeta `json:"_meta"`
}

type discoverResult struct {
	ResultType        ResultType     `json:"resultType"`
	SupportedVersions []string       `json:"supportedVersions"`
	Capabilities      Capabilities   `json:"capabilities"`
	ServerInfo        Implementation `json:"serverInfo"`
	Instructions      string         `json:"instructions,omitempty"`
	TTLMS             int            `json:"ttlMs"`
	CacheScope        CacheScope     `json:"cacheScope"`
}

// Capabilities describes server-supported MCP features.
type Capabilities struct {
	Experimental Extensions          `json:"experimental,omitempty"`
	Logging      MetaObject          `json:"logging,omitempty"`
	Completions  MetaObject          `json:"completions,omitempty"`
	Prompts      *PromptsCapability  `json:"prompts,omitempty"`
	Resources    ResourcesCapability `json:"resources"`
	Tools        ToolsCapability     `json:"tools"`
	Extensions   Extensions          `json:"extensions,omitempty"`
}

// PromptsCapability describes prompt support advertised by the server.
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ToolsCapability describes tool support advertised by the server.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability describes resource support advertised by the server.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// Implementation describes an MCP client or server implementation.
type Implementation struct {
	Icons       []Icon `json:"icons,omitempty"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	WebsiteURL  string `json:"websiteUrl,omitempty"`
}

// Icon describes an implementation or descriptor icon.
type Icon struct {
	Src      string   `json:"src"`
	MimeType string   `json:"mimeType,omitempty"`
	Sizes    []string `json:"sizes,omitempty"`
	Theme    string   `json:"theme,omitempty"`
}

type paginatedRequestParams struct {
	Meta   RequestMeta `json:"_meta"`
	Cursor string      `json:"cursor,omitempty"`
}

type toolsListResult struct {
	Meta       MetaObject       `json:"_meta,omitempty"`
	ResultType ResultType       `json:"resultType"`
	NextCursor string           `json:"nextCursor,omitempty"`
	Tools      []ToolDescriptor `json:"tools"`
	TTLMS      int              `json:"ttlMs"`
	CacheScope CacheScope       `json:"cacheScope"`
}

// ToolDescriptor describes one MCP tool.
type ToolDescriptor struct {
	Meta         MetaObject         `json:"_meta,omitempty"`
	Icons        []Icon             `json:"icons,omitempty"`
	Name         string             `json:"name"`
	Title        string             `json:"title,omitempty"`
	Description  string             `json:"description,omitempty"`
	InputSchema  *jsonschema.Schema `json:"inputSchema"`
	OutputSchema *jsonschema.Schema `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations   `json:"annotations,omitempty"`
}

// ToolAnnotations describe MCP tool behavior hints.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint bool   `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool   `json:"openWorldHint,omitempty"`
}

type toolsCallParams struct {
	Meta           RequestMeta     `json:"_meta"`
	InputResponses json.RawMessage `json:"inputResponses,omitempty"`
	RequestState   string          `json:"requestState,omitempty"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments,omitempty"`
}

// ToolCallResult is the response payload for a tool call.
type ToolCallResult struct {
	Meta              MetaObject     `json:"_meta,omitempty"`
	ResultType        ResultType     `json:"resultType"`
	Content           []ContentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

// ContentBlock is an MCP tool-result content item.
type ContentBlock struct {
	ResourceLink

	Meta        MetaObject       `json:"_meta,omitempty"`
	Type        ContentType      `json:"type"`
	Text        string           `json:"text,omitempty"`
	Data        string           `json:"data,omitempty"`
	MimeType    string           `json:"mimeType,omitempty"`
	Resource    *ResourceContent `json:"resource,omitempty"`
	Annotations *Annotations     `json:"annotations,omitempty"`
}

// Annotations provide optional metadata for MCP resources and content.
type Annotations struct {
	Audience     []Role   `json:"audience,omitempty"`
	Priority     *float64 `json:"priority,omitempty"`
	LastModified string   `json:"lastModified,omitempty"`
}

// Role identifies an MCP audience role.
type Role string

// ResourceLink identifies an MCP resource referenced from content.
type ResourceLink struct {
	Icons       []Icon `json:"icons,omitempty"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	URI         string `json:"uri,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        int64  `json:"size,omitzero"`
}

// ResourcesListResult is the response payload for resources/list.
type ResourcesListResult struct {
	Meta       MetaObject           `json:"_meta,omitempty"`
	ResultType ResultType           `json:"resultType"`
	NextCursor string               `json:"nextCursor,omitempty"`
	Resources  []ResourceDescriptor `json:"resources"`
	TTLMS      int                  `json:"ttlMs"`
	CacheScope CacheScope           `json:"cacheScope"`
}

// ResourceDescriptor describes one MCP resource.
type ResourceDescriptor struct {
	Meta        MetaObject   `json:"_meta,omitempty"`
	Icons       []Icon       `json:"icons,omitempty"`
	URI         string       `json:"uri"`
	Name        string       `json:"name"`
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	MimeType    string       `json:"mimeType,omitempty"`
	Annotations *Annotations `json:"annotations,omitempty"`
	Size        int64        `json:"size,omitzero"`
}

type resourceTemplatesListResult struct {
	Meta              MetaObject                   `json:"_meta,omitempty"`
	ResultType        ResultType                   `json:"resultType"`
	NextCursor        string                       `json:"nextCursor,omitempty"`
	ResourceTemplates []ResourceTemplateDescriptor `json:"resourceTemplates"`
	TTLMS             int                          `json:"ttlMs"`
	CacheScope        CacheScope                   `json:"cacheScope"`
}

// ResourceTemplateDescriptor describes a parameterized MCP resource.
type ResourceTemplateDescriptor struct {
	Meta        MetaObject   `json:"_meta,omitempty"`
	Icons       []Icon       `json:"icons,omitempty"`
	Name        string       `json:"name"`
	Title       string       `json:"title,omitempty"`
	URITemplate string       `json:"uriTemplate"`
	Description string       `json:"description,omitempty"`
	MimeType    string       `json:"mimeType,omitempty"`
	Annotations *Annotations `json:"annotations,omitempty"`
}

type resourcesReadParams struct {
	Meta           RequestMeta     `json:"_meta"`
	InputResponses json.RawMessage `json:"inputResponses,omitempty"`
	RequestState   string          `json:"requestState,omitempty"`
	URI            string          `json:"uri"`
}

// ResourcesReadResult is the response payload for resources/read.
type ResourcesReadResult struct {
	Meta       MetaObject        `json:"_meta,omitempty"`
	ResultType ResultType        `json:"resultType"`
	Contents   []ResourceContent `json:"contents"`
	TTLMS      int               `json:"ttlMs"`
	CacheScope CacheScope        `json:"cacheScope"`
}

// ResourceContent contains resource data returned by resources/read.
type ResourceContent struct {
	Meta     MetaObject `json:"_meta,omitempty"`
	URI      string     `json:"uri"`
	MimeType string     `json:"mimeType,omitempty"`
	Text     string     `json:"text,omitempty"`
	Blob     string     `json:"blob,omitempty"`
}

type subscriptionsListenParams struct {
	Meta          RequestMeta        `json:"_meta"`
	Notifications SubscriptionFilter `json:"notifications"`
}

// SubscriptionFilter describes MCP subscription notifications requested by a client.
type SubscriptionFilter struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
}

// JSONRPCNotification is a server-sent JSON-RPC notification.
type JSONRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// SubscriptionNotificationParams is the payload for subscription notifications.
type SubscriptionNotificationParams struct {
	Meta          MetaObject          `json:"_meta,omitempty"`
	Notifications *SubscriptionFilter `json:"notifications,omitempty"`
	URI           string              `json:"uri,omitempty"`
}

// HandleMCP handles one MCP HTTP request.
func (h *Handler) HandleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeResponse(w, http.StatusMethodNotAllowed, JSONRPCResponse{JSONRPC: jsonRPCVersion, Error: rpcError(InvalidRequestCode, "Method not allowed")})
		return
	}
	var req JSONRPCRequest
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		h.writeResponse(w, http.StatusBadRequest, JSONRPCResponse{JSONRPC: jsonRPCVersion, Error: rpcError(ParseErrorCode, "Parse error")})
		return
	}
	if req.JSONRPC != jsonRPCVersion || req.Method == "" || !validJSONRPCRequestID(req.ID) {
		h.writeResponse(w, http.StatusBadRequest, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcError(InvalidRequestCode, "Invalid Request")})
		return
	}
	// Transport-layer rejections (bad HTTP method, unparseable body, failed
	// _meta/header validation) carry a non-200 status. Once a request is valid
	// MCP it reaches dispatch, whose protocol errors use the transport status
	// mandated by the draft Streamable HTTP binding.
	if status, rpcErr := validateMCPRequest(r, &req); rpcErr != nil {
		h.writeResponse(w, status, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	if req.Method == MethodSubscriptionsListen {
		if rpcErr := h.handleSubscription(r.Context(), w, req.ID, req.Params); rpcErr != nil {
			h.writeResponse(w, rpcHTTPStatus(rpcErr), JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		}
		return
	}
	result, rpcErr := h.dispatch(r.Context(), req.Method, req.Params, r.Header)
	if rpcErr != nil {
		h.writeResponse(w, rpcHTTPStatus(rpcErr), JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	h.writeResponse(w, http.StatusOK, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result})
}

// dispatch routes a validated MCP request to its handler. Returned rpcErr values
// are JSON-RPC errors; handleMCP maps them to the transport status required by
// the draft Streamable HTTP binding.
func (h *Handler) dispatch(ctx context.Context, method Method, params json.RawMessage, header http.Header) (result any, rpcErr *JSONRPCError) {
	switch method {
	case MethodServerDiscover:
		t := discoverResult{
			ResultType:        ResultTypeComplete,
			SupportedVersions: []string{ProtocolVersion},
			Capabilities:      Capabilities{Tools: ToolsCapability{ListChanged: true}, Resources: ResourcesCapability{Subscribe: true, ListChanged: true}},
			ServerInfo:        h.serverInfo(),
			Instructions:      h.Instructions,
			TTLMS:             DefaultTTLMS,
			CacheScope:        CacheScopePrivate,
		}
		return t, nil
	case MethodToolsList:
		var p paginatedRequestParams
		if err := decodeParams(params, &p); err != nil {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		tools, err := h.Registry.Tools(ctx)
		if err != nil {
			return nil, rpcError(InternalErrorCode, err.Error())
		}
		page, next, err := paginate(tools, p.Cursor)
		if err != nil {
			return nil, rpcError(InvalidParamsCode, err.Error())
		}
		t := toolsListResult{
			ResultType: ResultTypeComplete,
			NextCursor: next,
			Tools:      page,
			TTLMS:      DefaultTTLMS,
			CacheScope: CacheScopePrivate,
		}
		return t, nil
	case MethodToolsCall:
		var p toolsCallParams
		if err := decodeParams(params, &p); err != nil || p.Name == "" {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		if err := h.validateToolParamHeaders(ctx, header, p.Name, p.Arguments); err != nil {
			return nil, rpcError(HeaderMismatchCode, err.Error())
		}
		res, err := h.Registry.CallTool(ctx, p.Name, p.Arguments)
		if err != nil {
			return nil, registryError(err)
		}
		t := ToolCallResult{
			ResultType: ResultTypeComplete,
			Content:    []ContentBlock{{Type: ContentTypeText, Text: toolResultText(res.Structured)}},
			IsError:    res.IsError,
		}
		if !res.IsError {
			t.StructuredContent = res.Structured
		}
		return t, nil
	case MethodResourcesList:
		var p paginatedRequestParams
		if err := decodeParams(params, &p); err != nil {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		res := h.Registry.ListResources(ctx)
		page, next, err := paginate(res.Resources, p.Cursor)
		if err != nil {
			return nil, rpcError(InvalidParamsCode, err.Error())
		}
		res.Resources = page
		res.NextCursor = next
		return res, nil
	case MethodResourceTemplatesList:
		var p paginatedRequestParams
		if err := decodeParams(params, &p); err != nil {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		templates := h.resourceTemplates()
		page, next, err := paginate(templates, p.Cursor)
		if err != nil {
			return nil, rpcError(InvalidParamsCode, err.Error())
		}
		return resourceTemplatesListResult{ResultType: ResultTypeComplete, NextCursor: next, ResourceTemplates: page, TTLMS: DefaultTTLMS, CacheScope: CacheScopePrivate}, nil
	case MethodResourcesRead:
		var p resourcesReadParams
		if err := decodeParams(params, &p); err != nil || p.URI == "" {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		res, err := h.Registry.ReadResource(ctx, p.URI)
		if err != nil {
			return nil, registryError(err)
		}
		return res, nil
	default:
		return nil, rpcError(MethodNotFoundCode, "Method not found")
	}
}

func (h *Handler) writeResponse(w http.ResponseWriter, status int, resp JSONRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("write mcp response", "err", err)
	}
}

func (h *Handler) serverInfo() Implementation {
	info := h.ServerInfo
	if info.Version == "" {
		info.Version = "unknown"
	}
	return info
}

func (h *Handler) resourceTemplates() []ResourceTemplateDescriptor {
	return []ResourceTemplateDescriptor{
		{Name: "repo", Title: "Repository", URITemplate: "caic://repos/{path}", Description: "Managed repository detail by path", MimeType: "application/json"},
		{Name: "task", Title: "Task", URITemplate: "caic://tasks/{id}", Description: "Coding task detail by task ID", MimeType: "application/json"},
	}
}

func (h *Handler) handleSubscription(ctx context.Context, w http.ResponseWriter, id, params json.RawMessage) *JSONRPCError {
	var p subscriptionsListenParams
	if err := decodeParams(params, &p); err != nil {
		return rpcError(InvalidParamsCode, "Invalid params")
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return rpcError(InternalErrorCode, "streaming unavailable")
	}
	subID := mcpSubscriptionID(id)
	accepted := SubscriptionFilter{
		ToolsListChanged:      p.Notifications.ToolsListChanged,
		ResourcesListChanged:  p.Notifications.ResourcesListChanged,
		ResourceSubscriptions: p.Notifications.ResourceSubscriptions,
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := writeMCPNotification(w, flusher, JSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/subscriptions/acknowledged", Params: SubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID), Notifications: &accepted}}); err != nil {
		slog.WarnContext(ctx, "write mcp subscription acknowledgment", "err", err)
		return nil
	}
	h.streamSubscriptionNotifications(ctx, w, flusher, subID, accepted)
	return nil
}

func (h *Handler) streamSubscriptionNotifications(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, subID string, filter SubscriptionFilter) {
	lastTools, lastResources, lastResourceContents := h.subscriptionSnapshot(ctx, filter)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		tools, resources, contents := h.subscriptionSnapshot(ctx, filter)
		if filter.ToolsListChanged && tools != lastTools {
			if err := writeMCPNotification(w, flusher, JSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/tools/list_changed", Params: SubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID)}}); err != nil {
				slog.WarnContext(ctx, "write mcp tools notification", "err", err)
				return
			}
		}
		if filter.ResourcesListChanged && resources != lastResources {
			if err := writeMCPNotification(w, flusher, JSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/resources/list_changed", Params: SubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID)}}); err != nil {
				slog.WarnContext(ctx, "write mcp resources notification", "err", err)
				return
			}
		}
		for uri, content := range contents {
			if content == lastResourceContents[uri] {
				continue
			}
			if err := writeMCPNotification(w, flusher, JSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/resources/updated", Params: SubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID), URI: uri}}); err != nil {
				slog.WarnContext(ctx, "write mcp resource update notification", "err", err)
				return
			}
		}
		lastTools, lastResources, lastResourceContents = tools, resources, contents
	}
}

func (h *Handler) subscriptionSnapshot(ctx context.Context, filter SubscriptionFilter) (toolsHash, resourcesHash string, contents map[string]string) {
	if filter.ToolsListChanged {
		if items, err := h.Registry.Tools(ctx); err == nil {
			toolsHash = stableJSON(items)
		}
	}
	if filter.ResourcesListChanged {
		resourcesHash = stableJSON(h.Registry.ListResources(ctx).Resources)
	}
	contents = make(map[string]string, len(filter.ResourceSubscriptions))
	for _, uri := range filter.ResourceSubscriptions {
		res, err := h.Registry.ReadResource(ctx, uri)
		if err != nil {
			contents[uri] = err.Error()
			continue
		}
		contents[uri] = stableJSON(res.Contents)
	}
	return toolsHash, resourcesHash, contents
}

func mcpSubscriptionID(id json.RawMessage) string {
	var s string
	if err := json.Unmarshal(id, &s); err == nil {
		return s
	}
	return string(id)
}

func mcpSubscriptionMeta(id string) MetaObject {
	return MetaObject{"io.modelcontextprotocol/subscriptionId": id}
}

func writeMCPNotification(w http.ResponseWriter, flusher http.Flusher, msg JSONRPCNotification) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func stableJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(data)
}

func (h *Handler) validateToolParamHeaders(ctx context.Context, header http.Header, name string, args json.RawMessage) error {
	tools, err := h.Registry.Tools(ctx)
	if err != nil {
		return err
	}
	var schema *jsonschema.Schema
	for _, tool := range tools {
		if tool.Name == name {
			schema = tool.InputSchema
			break
		}
	}
	if schema == nil {
		return nil
	}
	headers, err := mcpHeaderParams(schema)
	if err != nil {
		return err
	}
	if len(headers) == 0 {
		return nil
	}
	arguments, err := decodeJSONObject(args)
	if err != nil {
		return fmt.Errorf("invalid arguments for header validation: %w", err)
	}
	for _, hp := range headers {
		bodyValue, ok := jsonValueAtPath(arguments, hp.Path)
		gotRaw := header.Get("Mcp-Param-" + hp.Header)
		if !ok || bodyValue == nil {
			if gotRaw != "" {
				return fmt.Errorf("header mismatch: Mcp-Param-%s header is present but parameter is absent", hp.Header)
			}
			continue
		}
		if gotRaw == "" {
			return fmt.Errorf("header mismatch: Mcp-Param-%s header is required", hp.Header)
		}
		got, err := decodeMCPHeaderValue(gotRaw)
		if err != nil {
			return fmt.Errorf("header mismatch: Mcp-Param-%s header is malformed", hp.Header)
		}
		want, err := mcpPrimitiveHeaderValue(bodyValue)
		if err != nil {
			return fmt.Errorf("header mismatch: parameter for Mcp-Param-%s is not header-compatible: %w", hp.Header, err)
		}
		if got != want {
			return fmt.Errorf("header mismatch: Mcp-Param-%s header does not match request params", hp.Header)
		}
	}
	return nil
}

// HeaderParam maps an MCP input-schema property to a required HTTP header.
type HeaderParam struct {
	Header string
	Path   []string
}

func mcpHeaderParams(schema *jsonschema.Schema) ([]HeaderParam, error) {
	var params []HeaderParam
	seen := map[string]struct{}{}
	var walk func(*jsonschema.Schema, []string) error
	walk = func(s *jsonschema.Schema, path []string) error {
		if s == nil {
			return nil
		}
		if raw, ok := s.Extras["x-mcp-header"]; ok {
			header, ok := raw.(string)
			if !ok || !validMCPHeaderToken(header) {
				return fmt.Errorf("invalid x-mcp-header %q", raw)
			}
			key := strings.ToLower(header)
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate x-mcp-header %q", header)
			}
			if !mcpHeaderCompatibleSchema(s) {
				return fmt.Errorf("x-mcp-header %q is applied to a non-primitive schema", header)
			}
			seen[key] = struct{}{}
			params = append(params, HeaderParam{Header: header, Path: append([]string(nil), path...)})
		}
		if s.Properties != nil {
			for key, child := range s.Properties.FromOldest() {
				if err := walk(child, append(path, key)); err != nil {
					return err
				}
			}
		}
		if s.Items != nil {
			return walk(s.Items, path)
		}
		for _, child := range s.AnyOf {
			if err := walk(child, path); err != nil {
				return err
			}
		}
		for _, child := range s.OneOf {
			if err := walk(child, path); err != nil {
				return err
			}
		}
		for _, child := range s.AllOf {
			if err := walk(child, path); err != nil {
				return err
			}
		}
		return nil
	}
	return params, walk(schema, nil)
}

func mcpHeaderCompatibleSchema(s *jsonschema.Schema) bool {
	switch s.Type {
	case "string", "integer", "boolean":
		return true
	default:
		return false
	}
}

func validMCPHeaderToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r > 127 || !strings.ContainsRune("!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", r) {
			return false
		}
	}
	return true
}

func decodeJSONObject(data json.RawMessage) (map[string]any, error) {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return map[string]any{}, nil
	}
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func jsonValueAtPath(v any, path []string) (any, bool) {
	cur := v
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func decodeMCPHeaderValue(s string) (string, error) {
	if strings.HasPrefix(s, "=?base64?") && strings.HasSuffix(s, "?=") {
		data, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(s, "=?base64?"), "?="))
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	return s, nil
}

func mcpPrimitiveHeaderValue(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		if _, err := x.Int64(); err != nil {
			return "", err
		}
		return x.String(), nil
	default:
		return "", fmt.Errorf("unsupported type %T", v)
	}
}

func rpcHTTPStatus(err *JSONRPCError) int {
	switch err.Code {
	case MethodNotFoundCode:
		return http.StatusNotFound
	case HeaderMismatchCode:
		return http.StatusBadRequest
	default:
		return http.StatusOK
	}
}

func validJSONRPCRequestID(id json.RawMessage) bool {
	if len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return false
	}
	var v any
	d := json.NewDecoder(bytes.NewReader(id))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return false
	}
	switch v.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

const mcpDefaultPageSize = 100

func paginate[T any](items []T, cursor string) (page []T, next string, err error) {
	start := 0
	if cursor != "" {
		var convErr error
		start, convErr = strconv.Atoi(cursor)
		if convErr != nil || start < 0 {
			return nil, "", errors.New("invalid cursor")
		}
	}
	if start >= len(items) {
		return []T{}, "", nil
	}
	end := min(start+mcpDefaultPageSize, len(items))
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next, nil
}

func validateMCPRequest(r *http.Request, req *JSONRPCRequest) (int, *JSONRPCError) {
	var p RequestParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return http.StatusBadRequest, rpcError(InvalidParamsCode, "Invalid params")
	}
	meta := p.Meta
	if meta.ProtocolVersion == "" || meta.ClientInfo.Name == "" || meta.ClientInfo.Version == "" {
		return http.StatusBadRequest, rpcError(InvalidParamsCode, "Invalid params: required _meta fields missing")
	}
	headerVersion := r.Header.Get("Mcp-Protocol-Version")
	if headerVersion == "" {
		return http.StatusBadRequest, rpcError(HeaderMismatchCode, "Header mismatch: MCP-Protocol-Version header is required")
	}
	if headerVersion != meta.ProtocolVersion {
		return http.StatusBadRequest, rpcError(HeaderMismatchCode, "Header mismatch: MCP-Protocol-Version header does not match request _meta")
	}
	if meta.ProtocolVersion != ProtocolVersion {
		return http.StatusBadRequest, &JSONRPCError{
			Code:    UnsupportedProtocolVersionCode,
			Message: "Unsupported protocol version",
			Data: unsupportedProtocolVersionData{
				Supported: []string{ProtocolVersion},
				Requested: meta.ProtocolVersion,
			},
		}
	}
	if got := r.Header.Get("Mcp-Method"); got == "" {
		return http.StatusBadRequest, rpcError(HeaderMismatchCode, "Header mismatch: Mcp-Method header is required")
	} else if got != string(req.Method) {
		return http.StatusBadRequest, rpcError(HeaderMismatchCode, "Header mismatch: Mcp-Method header does not match request method")
	}
	name, required, err := mcpRequestName(req.Method, req.Params)
	if err != nil {
		return http.StatusBadRequest, rpcError(InvalidParamsCode, "Invalid params")
	}
	if !required {
		return http.StatusOK, nil
	}
	if got := r.Header.Get("Mcp-Name"); got == "" {
		return http.StatusBadRequest, rpcError(HeaderMismatchCode, "Header mismatch: Mcp-Name header is required")
	} else if got != name {
		return http.StatusBadRequest, rpcError(HeaderMismatchCode, "Header mismatch: Mcp-Name header does not match request params")
	}
	return http.StatusOK, nil
}

func mcpRequestName(method Method, params json.RawMessage) (name string, required bool, err error) {
	switch method {
	case MethodToolsCall:
		var p toolsCallParams
		if err := decodeParams(params, &p); err != nil {
			return "", true, err
		}
		if p.Name == "" {
			return "", true, errors.New("name is required")
		}
		return p.Name, true, nil
	case MethodResourcesRead:
		var p resourcesReadParams
		if err := decodeParams(params, &p); err != nil {
			return "", true, err
		}
		if p.URI == "" {
			return "", true, errors.New("uri is required")
		}
		return p.URI, true, nil
	default:
		return "", false, nil
	}
}

func decodeParams(data json.RawMessage, out any) error {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func rpcError(code ErrorCode, message string) *JSONRPCError {
	return &JSONRPCError{Code: code, Message: message}
}

// invalidParamsError marks a registry failure caused by bad client input — an
// unknown tool or resource, or undecodable arguments — so the dispatcher reports
// it as a JSON-RPC invalid-params error. Unmarked registry errors are treated as
// internal faults (e.g. a backend lookup failing while building the tool catalog).
type invalidParamsError struct{ err error }

func (e invalidParamsError) Error() string { return e.err.Error() }

func (e invalidParamsError) Unwrap() error { return e.err }

// ErrInvalidParams marks a registry error as client-caused invalid params.
func ErrInvalidParams(format string, args ...any) error {
	return invalidParamsError{err: fmt.Errorf(format, args...)}
}

// registryError maps a registry error to a JSON-RPC error: invalid params for
// client-input faults, internal for everything else.
func registryError(err error) *JSONRPCError {
	if _, ok := errors.AsType[invalidParamsError](err); ok {
		return rpcError(InvalidParamsCode, err.Error())
	}
	return rpcError(InternalErrorCode, err.Error())
}

func jsonStringField(fields map[string]json.RawMessage, key string) (string, bool) {
	data, ok := fields[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return "", false
	}
	return value, true
}

// ToolHandler executes a tool with raw JSON arguments.
type ToolHandler func(context.Context, json.RawMessage) (RawToolResult, error)

// ToolSpec describes a server-side tool implementation.
type ToolSpec struct {
	Name         string
	Title        string
	Description  string
	InputSchema  *jsonschema.Schema
	OutputSchema *jsonschema.Schema
	Annotations  *ToolAnnotations
	Handler      ToolHandler
}

// ToolResult carries a tool handler's output. The T type parameter exists only
// to drive output-schema generation in NewToolSpec (via the [0]T phantom field);
// it does NOT constrain Structured, which stays any so error paths can substitute
// a different payload. Concretely, ToolError[T] puts an ErrorOutput in
// Structured regardless of T, so the emitted structuredContent is not guaranteed
// to match the advertised outputSchema — IsError signals when it diverges. Treat
// outputSchema as a hint for success results, not a wire contract.
type ToolResult[T any] struct {
	Structured any
	IsError    bool
	_          [0]T
}

func (r ToolResult[T]) toRawToolResult() RawToolResult {
	return RawToolResult{Structured: r.Structured, IsError: r.IsError}
}

func toolResultText(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err == nil {
		if text, ok := jsonStringField(fields, "result"); ok {
			return text
		}
		if text, ok := jsonStringField(fields, "error"); ok {
			return text
		}
		if content, ok := jsonStringField(fields, "content"); ok {
			if title, ok := jsonStringField(fields, "title"); ok && title != "" {
				return strings.TrimSpace(title + "\n\n" + content)
			}
			return content
		}
	}
	return string(data)
}

// NewToolSpec builds a ToolSpec with reflected input and output schemas.
func NewToolSpec[In, Out any](name, title, description string, handler func(context.Context, In) ToolResult[Out]) ToolSpec {
	inputSchema := SchemaFor[In]()
	AddHeaderToProperty(inputSchema, "task_number", "Task-Number")
	return ToolSpec{
		Name:         name,
		Title:        title,
		Description:  description,
		InputSchema:  inputSchema,
		OutputSchema: SchemaFor[Out](),
		Handler: func(ctx context.Context, argsJSON json.RawMessage) (RawToolResult, error) {
			args, err := DecodeToolArgument[In](argsJSON)
			if err != nil {
				return RawToolResult{}, ErrInvalidParams("invalid arguments: %w", err)
			}
			return handler(ctx, args).toRawToolResult(), nil
		},
	}
}

// AddHeaderToProperty marks a primitive schema property as mirrored in an MCP parameter header.
func AddHeaderToProperty(schema *jsonschema.Schema, property, header string) {
	if schema == nil || schema.Properties == nil {
		return
	}
	prop, ok := schema.Properties.Get(property)
	if !ok || !mcpHeaderCompatibleSchema(prop) {
		return
	}
	if prop.Extras == nil {
		prop.Extras = map[string]any{}
	}
	prop.Extras["x-mcp-header"] = header
}

// DecodeToolArgument decodes strict JSON tool arguments into T.
func DecodeToolArgument[T any](data json.RawMessage) (T, error) {
	var arg T
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return arg, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&arg); err != nil {
		return arg, err
	}
	return arg, nil
}

// ResourceJSON encodes value as a JSON MCP resource read result.
func ResourceJSON(uri string, value any) (ResourcesReadResult, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ResourcesReadResult{}, err
	}
	return ResourcesReadResult{ResultType: ResultTypeComplete, Contents: []ResourceContent{{URI: uri, MimeType: "application/json", Text: string(data)}}, TTLMS: DefaultTTLMS, CacheScope: CacheScopePrivate}, nil
}

// SchemaFor reflects T into a JSON schema suitable for MCP descriptors.
func SchemaFor[T any]() *jsonschema.Schema {
	r := jsonschema.Reflector{Anonymous: true, DoNotReference: true}
	return r.ReflectFromType(reflect.TypeFor[T]())
}

// TypedToolResult returns a successful structured tool result.
func TypedToolResult[T any](structured T) ToolResult[T] {
	return ToolResult[T]{Structured: structured}
}

// TextToolResult returns a successful text output result.
func TextToolResult(message string) ToolResult[TextOutput] {
	return TypedToolResult(TextOutput{Result: message})
}

// ToolError returns an MCP tool error result.
func ToolError[T any](message string) ToolResult[T] {
	return ToolResult[T]{Structured: ErrorOutput{Error: message}, IsError: true}
}

// TextOutput is the standard human-readable successful tool payload.
type TextOutput struct {
	Result string `json:"result" jsonschema_description:"Human-readable tool result"`
}

// ErrorOutput is the standard human-readable tool error payload.
type ErrorOutput struct {
	Error string `json:"error" jsonschema_description:"Human-readable error message"`
}
