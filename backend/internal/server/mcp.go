// MCP endpoint handlers.

package server

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

	"github.com/caic-xyz/caic/backend/internal/server/api"
)

const (
	jsonRPCVersion     = "2.0"
	mcpProtocolVersion = "2026-07-28"
	mcpDefaultTTLMS    = 10_000
)

type mcpErrorCode int

const (
	mcpParseErrorCode                 mcpErrorCode = -32700
	mcpInvalidRequestCode             mcpErrorCode = -32600
	mcpMethodNotFoundCode             mcpErrorCode = -32601
	mcpInvalidParamsCode              mcpErrorCode = -32602
	mcpInternalErrorCode              mcpErrorCode = -32603
	mcpHeaderMismatchCode             mcpErrorCode = -32001
	mcpUnsupportedProtocolVersionCode mcpErrorCode = -32004
)

type mcpMethod string

const (
	mcpMethodServerDiscover        mcpMethod = "server/discover"
	mcpMethodToolsList             mcpMethod = "tools/list"
	mcpMethodToolsCall             mcpMethod = "tools/call"
	mcpMethodResourcesList         mcpMethod = "resources/list"
	mcpMethodResourcesRead         mcpMethod = "resources/read"
	mcpMethodResourceTemplatesList mcpMethod = "resources/templates/list"
	mcpMethodSubscriptionsListen   mcpMethod = "subscriptions/listen"
)

type mcpResultType string

const (
	mcpResultTypeComplete mcpResultType = "complete"
)

type mcpCacheScope string

const (
	mcpCacheScopePublic  mcpCacheScope = "public"
	mcpCacheScopePrivate mcpCacheScope = "private"
)

type mcpContentType string

const (
	mcpContentTypeText mcpContentType = "text"
)

type mcpHandlers struct {
	Registry     mcpRegistry
	ServerInfo   mcpImplementation
	Instructions string
}

type mcpRegistry interface {
	tools(ctx context.Context) ([]mcpToolDescriptor, error)
	callTool(ctx context.Context, name string, args json.RawMessage) (rawToolResult, error)
	listResources(ctx context.Context) resourcesListResult
	readResource(ctx context.Context, uri string) (resourcesReadResult, error)
}

type rawToolResult struct {
	Structured any
	IsError    bool
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitzero"`
	Method  mcpMethod       `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitzero"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    mcpErrorCode `json:"code"`
	Message string       `json:"message"`
	Data    any          `json:"data,omitempty"`
}

type unsupportedProtocolVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

type mcpMetaObject map[string]any

type mcpRequestMeta struct {
	ProtocolVersion    string                `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         mcpImplementation     `json:"io.modelcontextprotocol/clientInfo"`
	ClientCapabilities mcpClientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
	ProgressToken      any                   `json:"progressToken,omitempty"`
	LogLevel           string                `json:"io.modelcontextprotocol/logLevel,omitempty"`
}

func (m *mcpRequestMeta) UnmarshalJSON(data []byte) error {
	type requestMeta mcpRequestMeta
	var meta requestMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	*m = mcpRequestMeta(meta)
	return nil
}

type mcpClientCapabilities struct {
	Experimental mcpExtensions             `json:"experimental,omitempty"`
	Roots        mcpMetaObject             `json:"roots,omitempty"`
	Sampling     *mcpSamplingCapability    `json:"sampling,omitempty"`
	Elicitation  *mcpElicitationCapability `json:"elicitation,omitempty"`
	Extensions   mcpExtensions             `json:"extensions,omitempty"`
}

type mcpSamplingCapability struct {
	Context mcpMetaObject `json:"context,omitempty"`
	Tools   mcpMetaObject `json:"tools,omitempty"`
}

type mcpElicitationCapability struct {
	Form mcpMetaObject `json:"form,omitempty"`
	URL  mcpMetaObject `json:"url,omitempty"`
}

type mcpExtensions map[string]json.RawMessage

type mcpRequestParams struct {
	Meta mcpRequestMeta `json:"_meta"`
}

type discoverResult struct {
	ResultType        mcpResultType     `json:"resultType"`
	SupportedVersions []string          `json:"supportedVersions"`
	Capabilities      mcpCapabilities   `json:"capabilities"`
	ServerInfo        mcpImplementation `json:"serverInfo"`
	Instructions      string            `json:"instructions,omitempty"`
	TTLMS             int               `json:"ttlMs"`
	CacheScope        mcpCacheScope     `json:"cacheScope"`
}

type mcpCapabilities struct {
	Experimental mcpExtensions          `json:"experimental,omitempty"`
	Logging      mcpMetaObject          `json:"logging,omitempty"`
	Completions  mcpMetaObject          `json:"completions,omitempty"`
	Prompts      *mcpPromptsCapability  `json:"prompts,omitempty"`
	Resources    mcpResourcesCapability `json:"resources"`
	Tools        mcpToolsCapability     `json:"tools"`
	Extensions   mcpExtensions          `json:"extensions,omitempty"`
}

type mcpPromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type mcpToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type mcpResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

type mcpImplementation struct {
	Icons       []mcpIcon `json:"icons,omitempty"`
	Name        string    `json:"name"`
	Title       string    `json:"title,omitempty"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	WebsiteURL  string    `json:"websiteUrl,omitempty"`
}

type mcpIcon struct {
	Src      string   `json:"src"`
	MimeType string   `json:"mimeType,omitempty"`
	Sizes    []string `json:"sizes,omitempty"`
	Theme    string   `json:"theme,omitempty"`
}

type paginatedRequestParams struct {
	Meta   mcpRequestMeta `json:"_meta"`
	Cursor string         `json:"cursor,omitempty"`
}

type toolsListResult struct {
	Meta       mcpMetaObject       `json:"_meta,omitempty"`
	ResultType mcpResultType       `json:"resultType"`
	NextCursor string              `json:"nextCursor,omitempty"`
	Tools      []mcpToolDescriptor `json:"tools"`
	TTLMS      int                 `json:"ttlMs"`
	CacheScope mcpCacheScope       `json:"cacheScope"`
}

type mcpToolDescriptor struct {
	Meta         mcpMetaObject       `json:"_meta,omitempty"`
	Icons        []mcpIcon           `json:"icons,omitempty"`
	Name         string              `json:"name"`
	Title        string              `json:"title,omitempty"`
	Description  string              `json:"description,omitempty"`
	InputSchema  *jsonschema.Schema  `json:"inputSchema"`
	OutputSchema *jsonschema.Schema  `json:"outputSchema,omitempty"`
	Annotations  *mcpToolAnnotations `json:"annotations,omitempty"`
}

type mcpToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint bool   `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool   `json:"openWorldHint,omitempty"`
}

type toolsCallParams struct {
	Meta           mcpRequestMeta  `json:"_meta"`
	InputResponses json.RawMessage `json:"inputResponses,omitempty"`
	RequestState   string          `json:"requestState,omitempty"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments,omitempty"`
}

type mcpToolCallResult struct {
	Meta              mcpMetaObject     `json:"_meta,omitempty"`
	ResultType        mcpResultType     `json:"resultType"`
	Content           []mcpContentBlock `json:"content"`
	StructuredContent any               `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

type mcpContentBlock struct {
	mcpResourceLink

	Meta        mcpMetaObject       `json:"_meta,omitempty"`
	Type        mcpContentType      `json:"type"`
	Text        string              `json:"text,omitempty"`
	Data        string              `json:"data,omitempty"`
	MimeType    string              `json:"mimeType,omitempty"`
	Resource    *mcpResourceContent `json:"resource,omitempty"`
	Annotations *mcpAnnotations     `json:"annotations,omitempty"`
}

type mcpAnnotations struct {
	Audience     []mcpRole `json:"audience,omitempty"`
	Priority     *float64  `json:"priority,omitempty"`
	LastModified string    `json:"lastModified,omitempty"`
}

type mcpRole string

type mcpResourceLink struct {
	Icons       []mcpIcon `json:"icons,omitempty"`
	Name        string    `json:"name,omitempty"`
	Title       string    `json:"title,omitempty"`
	URI         string    `json:"uri,omitempty"`
	Description string    `json:"description,omitempty"`
	MimeType    string    `json:"mimeType,omitempty"`
	Size        int64     `json:"size,omitzero"`
}

type resourcesListResult struct {
	Meta       mcpMetaObject           `json:"_meta,omitempty"`
	ResultType mcpResultType           `json:"resultType"`
	NextCursor string                  `json:"nextCursor,omitempty"`
	Resources  []mcpResourceDescriptor `json:"resources"`
	TTLMS      int                     `json:"ttlMs"`
	CacheScope mcpCacheScope           `json:"cacheScope"`
}

type mcpResourceDescriptor struct {
	Meta        mcpMetaObject   `json:"_meta,omitempty"`
	Icons       []mcpIcon       `json:"icons,omitempty"`
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Annotations *mcpAnnotations `json:"annotations,omitempty"`
	Size        int64           `json:"size,omitzero"`
}

type resourceTemplatesListResult struct {
	Meta              mcpMetaObject                   `json:"_meta,omitempty"`
	ResultType        mcpResultType                   `json:"resultType"`
	NextCursor        string                          `json:"nextCursor,omitempty"`
	ResourceTemplates []mcpResourceTemplateDescriptor `json:"resourceTemplates"`
	TTLMS             int                             `json:"ttlMs"`
	CacheScope        mcpCacheScope                   `json:"cacheScope"`
}

type mcpResourceTemplateDescriptor struct {
	Meta        mcpMetaObject   `json:"_meta,omitempty"`
	Icons       []mcpIcon       `json:"icons,omitempty"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	URITemplate string          `json:"uriTemplate"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Annotations *mcpAnnotations `json:"annotations,omitempty"`
}

type resourcesReadParams struct {
	Meta           mcpRequestMeta  `json:"_meta"`
	InputResponses json.RawMessage `json:"inputResponses,omitempty"`
	RequestState   string          `json:"requestState,omitempty"`
	URI            string          `json:"uri"`
}

type resourcesReadResult struct {
	Meta       mcpMetaObject        `json:"_meta,omitempty"`
	ResultType mcpResultType        `json:"resultType"`
	Contents   []mcpResourceContent `json:"contents"`
	TTLMS      int                  `json:"ttlMs"`
	CacheScope mcpCacheScope        `json:"cacheScope"`
}

type mcpResourceContent struct {
	Meta     mcpMetaObject `json:"_meta,omitempty"`
	URI      string        `json:"uri"`
	MimeType string        `json:"mimeType,omitempty"`
	Text     string        `json:"text,omitempty"`
	Blob     string        `json:"blob,omitempty"`
}

type subscriptionsListenParams struct {
	Meta          mcpRequestMeta        `json:"_meta"`
	Notifications mcpSubscriptionFilter `json:"notifications"`
}

type mcpSubscriptionFilter struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
}

type mcpJSONRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type mcpSubscriptionNotificationParams struct {
	Meta          mcpMetaObject          `json:"_meta,omitempty"`
	Notifications *mcpSubscriptionFilter `json:"notifications,omitempty"`
	URI           string                 `json:"uri,omitempty"`
}

func (h *mcpHandlers) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeResponse(w, http.StatusMethodNotAllowed, jsonRPCResponse{JSONRPC: jsonRPCVersion, Error: rpcError(mcpInvalidRequestCode, "Method not allowed")})
		return
	}
	var req jsonRPCRequest
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		h.writeResponse(w, http.StatusBadRequest, jsonRPCResponse{JSONRPC: jsonRPCVersion, Error: rpcError(mcpParseErrorCode, "Parse error")})
		return
	}
	if req.JSONRPC != jsonRPCVersion || req.Method == "" || !validJSONRPCRequestID(req.ID) {
		h.writeResponse(w, http.StatusBadRequest, jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcError(mcpInvalidRequestCode, "Invalid Request")})
		return
	}
	// Transport-layer rejections (bad HTTP method, unparseable body, failed
	// _meta/header validation) carry a non-200 status. Once a request is valid
	// MCP it reaches dispatch, whose protocol errors use the transport status
	// mandated by the draft Streamable HTTP binding.
	if status, rpcErr := validateMCPRequest(r, &req); rpcErr != nil {
		h.writeResponse(w, status, jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	if req.Method == mcpMethodSubscriptionsListen {
		if rpcErr := h.handleSubscription(r.Context(), w, req.ID, req.Params); rpcErr != nil {
			h.writeResponse(w, rpcHTTPStatus(rpcErr), jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		}
		return
	}
	result, rpcErr := h.dispatch(r.Context(), req.Method, req.Params, r.Header)
	if rpcErr != nil {
		h.writeResponse(w, rpcHTTPStatus(rpcErr), jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	h.writeResponse(w, http.StatusOK, jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result})
}

// dispatch routes a validated MCP request to its handler. Returned rpcErr values
// are JSON-RPC errors; handleMCP maps them to the transport status required by
// the draft Streamable HTTP binding.
func (h *mcpHandlers) dispatch(ctx context.Context, method mcpMethod, params json.RawMessage, header http.Header) (result any, rpcErr *jsonRPCError) {
	switch method {
	case mcpMethodServerDiscover:
		t := discoverResult{
			ResultType:        mcpResultTypeComplete,
			SupportedVersions: []string{mcpProtocolVersion},
			Capabilities:      mcpCapabilities{Tools: mcpToolsCapability{ListChanged: true}, Resources: mcpResourcesCapability{Subscribe: true, ListChanged: true}},
			ServerInfo:        h.serverInfo(),
			Instructions:      h.Instructions,
			TTLMS:             mcpDefaultTTLMS,
			CacheScope:        mcpCacheScopePrivate,
		}
		return t, nil
	case mcpMethodToolsList:
		var p paginatedRequestParams
		if err := decodeParams(params, &p); err != nil {
			return nil, rpcError(mcpInvalidParamsCode, "Invalid params")
		}
		tools, err := h.Registry.tools(ctx)
		if err != nil {
			return nil, rpcError(mcpInternalErrorCode, err.Error())
		}
		page, next, err := paginate(tools, p.Cursor)
		if err != nil {
			return nil, rpcError(mcpInvalidParamsCode, err.Error())
		}
		t := toolsListResult{
			ResultType: mcpResultTypeComplete,
			NextCursor: next,
			Tools:      page,
			TTLMS:      mcpDefaultTTLMS,
			CacheScope: mcpCacheScopePrivate,
		}
		return t, nil
	case mcpMethodToolsCall:
		var p toolsCallParams
		if err := decodeParams(params, &p); err != nil || p.Name == "" {
			return nil, rpcError(mcpInvalidParamsCode, "Invalid params")
		}
		if err := h.validateToolParamHeaders(ctx, header, p.Name, p.Arguments); err != nil {
			return nil, rpcError(mcpHeaderMismatchCode, err.Error())
		}
		res, err := h.Registry.callTool(ctx, p.Name, p.Arguments)
		if err != nil {
			return nil, registryError(err)
		}
		t := mcpToolCallResult{
			ResultType: mcpResultTypeComplete,
			Content:    []mcpContentBlock{{Type: mcpContentTypeText, Text: toolResultText(res.Structured)}},
			IsError:    res.IsError,
		}
		if !res.IsError {
			t.StructuredContent = res.Structured
		}
		return t, nil
	case mcpMethodResourcesList:
		var p paginatedRequestParams
		if err := decodeParams(params, &p); err != nil {
			return nil, rpcError(mcpInvalidParamsCode, "Invalid params")
		}
		res := h.Registry.listResources(ctx)
		page, next, err := paginate(res.Resources, p.Cursor)
		if err != nil {
			return nil, rpcError(mcpInvalidParamsCode, err.Error())
		}
		res.Resources = page
		res.NextCursor = next
		return res, nil
	case mcpMethodResourceTemplatesList:
		var p paginatedRequestParams
		if err := decodeParams(params, &p); err != nil {
			return nil, rpcError(mcpInvalidParamsCode, "Invalid params")
		}
		templates := h.resourceTemplates()
		page, next, err := paginate(templates, p.Cursor)
		if err != nil {
			return nil, rpcError(mcpInvalidParamsCode, err.Error())
		}
		return resourceTemplatesListResult{ResultType: mcpResultTypeComplete, NextCursor: next, ResourceTemplates: page, TTLMS: mcpDefaultTTLMS, CacheScope: mcpCacheScopePrivate}, nil
	case mcpMethodResourcesRead:
		var p resourcesReadParams
		if err := decodeParams(params, &p); err != nil || p.URI == "" {
			return nil, rpcError(mcpInvalidParamsCode, "Invalid params")
		}
		res, err := h.Registry.readResource(ctx, p.URI)
		if err != nil {
			return nil, registryError(err)
		}
		return res, nil
	default:
		return nil, rpcError(mcpMethodNotFoundCode, "Method not found")
	}
}

func (h *mcpHandlers) writeResponse(w http.ResponseWriter, status int, resp jsonRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("write mcp response", "err", err)
	}
}

func (h *mcpHandlers) serverInfo() mcpImplementation {
	info := h.ServerInfo
	if info.Version == "" {
		info.Version = "unknown"
	}
	return info
}

func (h *mcpHandlers) resourceTemplates() []mcpResourceTemplateDescriptor {
	return []mcpResourceTemplateDescriptor{
		{Name: "repo", Title: "Repository", URITemplate: "caic://repos/{path}", Description: "Managed repository detail by path", MimeType: "application/json"},
		{Name: "task", Title: "Task", URITemplate: "caic://tasks/{id}", Description: "Coding task detail by task ID", MimeType: "application/json"},
	}
}

func (h *mcpHandlers) handleSubscription(ctx context.Context, w http.ResponseWriter, id, params json.RawMessage) *jsonRPCError {
	var p subscriptionsListenParams
	if err := decodeParams(params, &p); err != nil {
		return rpcError(mcpInvalidParamsCode, "Invalid params")
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return rpcError(mcpInternalErrorCode, "streaming unavailable")
	}
	subID := mcpSubscriptionID(id)
	accepted := mcpSubscriptionFilter{
		ToolsListChanged:      p.Notifications.ToolsListChanged,
		ResourcesListChanged:  p.Notifications.ResourcesListChanged,
		ResourceSubscriptions: p.Notifications.ResourceSubscriptions,
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := writeMCPNotification(w, flusher, mcpJSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/subscriptions/acknowledged", Params: mcpSubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID), Notifications: &accepted}}); err != nil {
		slog.WarnContext(ctx, "write mcp subscription acknowledgment", "err", err)
		return nil
	}
	h.streamSubscriptionNotifications(ctx, w, flusher, subID, accepted)
	return nil
}

func (h *mcpHandlers) streamSubscriptionNotifications(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, subID string, filter mcpSubscriptionFilter) {
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
			if err := writeMCPNotification(w, flusher, mcpJSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/tools/list_changed", Params: mcpSubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID)}}); err != nil {
				slog.WarnContext(ctx, "write mcp tools notification", "err", err)
				return
			}
		}
		if filter.ResourcesListChanged && resources != lastResources {
			if err := writeMCPNotification(w, flusher, mcpJSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/resources/list_changed", Params: mcpSubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID)}}); err != nil {
				slog.WarnContext(ctx, "write mcp resources notification", "err", err)
				return
			}
		}
		for uri, content := range contents {
			if content == lastResourceContents[uri] {
				continue
			}
			if err := writeMCPNotification(w, flusher, mcpJSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/resources/updated", Params: mcpSubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID), URI: uri}}); err != nil {
				slog.WarnContext(ctx, "write mcp resource update notification", "err", err)
				return
			}
		}
		lastTools, lastResources, lastResourceContents = tools, resources, contents
	}
}

func (h *mcpHandlers) subscriptionSnapshot(ctx context.Context, filter mcpSubscriptionFilter) (toolsHash, resourcesHash string, contents map[string]string) {
	if filter.ToolsListChanged {
		if items, err := h.Registry.tools(ctx); err == nil {
			toolsHash = stableJSON(items)
		}
	}
	if filter.ResourcesListChanged {
		resourcesHash = stableJSON(h.Registry.listResources(ctx).Resources)
	}
	contents = make(map[string]string, len(filter.ResourceSubscriptions))
	for _, uri := range filter.ResourceSubscriptions {
		res, err := h.Registry.readResource(ctx, uri)
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

func mcpSubscriptionMeta(id string) mcpMetaObject {
	return mcpMetaObject{"io.modelcontextprotocol/subscriptionId": id}
}

func writeMCPNotification(w http.ResponseWriter, flusher http.Flusher, msg mcpJSONRPCNotification) error {
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

func (h *mcpHandlers) validateToolParamHeaders(ctx context.Context, header http.Header, name string, args json.RawMessage) error {
	tools, err := h.Registry.tools(ctx)
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

type mcpHeaderParam struct {
	Header string
	Path   []string
}

func mcpHeaderParams(schema *jsonschema.Schema) ([]mcpHeaderParam, error) {
	var params []mcpHeaderParam
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
			params = append(params, mcpHeaderParam{Header: header, Path: append([]string(nil), path...)})
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

func rpcHTTPStatus(err *jsonRPCError) int {
	switch err.Code {
	case mcpMethodNotFoundCode:
		return http.StatusNotFound
	case mcpHeaderMismatchCode:
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

func validateMCPRequest(r *http.Request, req *jsonRPCRequest) (int, *jsonRPCError) {
	var p mcpRequestParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return http.StatusBadRequest, rpcError(mcpInvalidParamsCode, "Invalid params")
	}
	meta := p.Meta
	if meta.ProtocolVersion == "" || meta.ClientInfo.Name == "" || meta.ClientInfo.Version == "" {
		return http.StatusBadRequest, rpcError(mcpInvalidParamsCode, "Invalid params: required _meta fields missing")
	}
	headerVersion := r.Header.Get("Mcp-Protocol-Version")
	if headerVersion == "" {
		return http.StatusBadRequest, rpcError(mcpHeaderMismatchCode, "Header mismatch: MCP-Protocol-Version header is required")
	}
	if headerVersion != meta.ProtocolVersion {
		return http.StatusBadRequest, rpcError(mcpHeaderMismatchCode, "Header mismatch: MCP-Protocol-Version header does not match request _meta")
	}
	if meta.ProtocolVersion != mcpProtocolVersion {
		return http.StatusBadRequest, &jsonRPCError{
			Code:    mcpUnsupportedProtocolVersionCode,
			Message: "Unsupported protocol version",
			Data: unsupportedProtocolVersionData{
				Supported: []string{mcpProtocolVersion},
				Requested: meta.ProtocolVersion,
			},
		}
	}
	if got := r.Header.Get("Mcp-Method"); got == "" {
		return http.StatusBadRequest, rpcError(mcpHeaderMismatchCode, "Header mismatch: Mcp-Method header is required")
	} else if got != string(req.Method) {
		return http.StatusBadRequest, rpcError(mcpHeaderMismatchCode, "Header mismatch: Mcp-Method header does not match request method")
	}
	name, required, err := mcpRequestName(req.Method, req.Params)
	if err != nil {
		return http.StatusBadRequest, rpcError(mcpInvalidParamsCode, "Invalid params")
	}
	if !required {
		return http.StatusOK, nil
	}
	if got := r.Header.Get("Mcp-Name"); got == "" {
		return http.StatusBadRequest, rpcError(mcpHeaderMismatchCode, "Header mismatch: Mcp-Name header is required")
	} else if got != name {
		return http.StatusBadRequest, rpcError(mcpHeaderMismatchCode, "Header mismatch: Mcp-Name header does not match request params")
	}
	return http.StatusOK, nil
}

func mcpRequestName(method mcpMethod, params json.RawMessage) (name string, required bool, err error) {
	switch method {
	case mcpMethodToolsCall:
		var p toolsCallParams
		if err := decodeParams(params, &p); err != nil {
			return "", true, err
		}
		if p.Name == "" {
			return "", true, errors.New("name is required")
		}
		return p.Name, true, nil
	case mcpMethodResourcesRead:
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

func rpcError(code mcpErrorCode, message string) *jsonRPCError {
	return &jsonRPCError{Code: code, Message: message}
}

// invalidParamsError marks a registry failure caused by bad client input — an
// unknown tool or resource, or undecodable arguments — so the dispatcher reports
// it as a JSON-RPC invalid-params error. Unmarked registry errors are treated as
// internal faults (e.g. a backend lookup failing while building the tool catalog).
type invalidParamsError struct{ err error }

func (e invalidParamsError) Error() string { return e.err.Error() }

func (e invalidParamsError) Unwrap() error { return e.err }

func errInvalidParams(format string, args ...any) error {
	return invalidParamsError{err: fmt.Errorf(format, args...)}
}

// registryError maps a registry error to a JSON-RPC error: invalid params for
// client-input faults, internal for everything else.
func registryError(err error) *jsonRPCError {
	if _, ok := errors.AsType[invalidParamsError](err); ok {
		return rpcError(mcpInvalidParamsCode, err.Error())
	}
	return rpcError(mcpInternalErrorCode, err.Error())
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

type toolHandler func(context.Context, json.RawMessage) (rawToolResult, error)

type toolSpec struct {
	Name         string
	Title        string
	Description  string
	InputSchema  *jsonschema.Schema
	OutputSchema *jsonschema.Schema
	Annotations  *mcpToolAnnotations
	Handler      toolHandler
}

// toolResult carries a tool handler's output. The T type parameter exists only
// to drive output-schema generation in newToolSpec (via the [0]T phantom field);
// it does NOT constrain Structured, which stays any so error paths can substitute
// a different payload. Concretely, toolError[T] puts an mcpErrorOutput in
// Structured regardless of T, so the emitted structuredContent is not guaranteed
// to match the advertised outputSchema — IsError signals when it diverges. Treat
// outputSchema as a hint for success results, not a wire contract.
type toolResult[T any] struct {
	Structured any
	IsError    bool
	_          [0]T
}

func (r toolResult[T]) toRawToolResult() rawToolResult {
	return rawToolResult{Structured: r.Structured, IsError: r.IsError}
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

func newToolSpec[In, Out any](name, title, description string, handler func(context.Context, In) toolResult[Out]) toolSpec {
	inputSchema := schemaFor[In]()
	addMCPHeaderToProperty(inputSchema, "task_number", "Task-Number")
	return toolSpec{
		Name:         name,
		Title:        title,
		Description:  description,
		InputSchema:  inputSchema,
		OutputSchema: schemaFor[Out](),
		Handler: func(ctx context.Context, argsJSON json.RawMessage) (rawToolResult, error) {
			args, err := decodeToolArgument[In](argsJSON)
			if err != nil {
				return rawToolResult{}, errInvalidParams("invalid arguments: %w", err)
			}
			return handler(ctx, args).toRawToolResult(), nil
		},
	}
}

func addMCPHeaderToProperty(schema *jsonschema.Schema, property, header string) {
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

func decodeToolArgument[T any](data json.RawMessage) (T, error) {
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

func resourceJSON(uri string, value any) (resourcesReadResult, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return resourcesReadResult{}, err
	}
	return resourcesReadResult{ResultType: mcpResultTypeComplete, Contents: []mcpResourceContent{{URI: uri, MimeType: "application/json", Text: string(data)}}, TTLMS: mcpDefaultTTLMS, CacheScope: mcpCacheScopePrivate}, nil
}

func schemaFor[T any]() *jsonschema.Schema {
	r := jsonschema.Reflector{Anonymous: true, DoNotReference: true}
	return r.ReflectFromType(reflect.TypeFor[T]())
}

func domainToolError[T any](err error) toolResult[T] {
	if err == nil {
		return toolResult[T]{}
	}
	if ews, ok := errors.AsType[api.ErrorWithStatus](err); ok {
		return toolError[T](ews.Error())
	}
	return toolError[T](err.Error())
}

func typedToolResult[T any](structured T) toolResult[T] {
	return toolResult[T]{Structured: structured}
}

func textToolResult(message string) toolResult[mcpTextOutput] {
	return typedToolResult(mcpTextOutput{Result: message})
}

func toolError[T any](message string) toolResult[T] {
	return toolResult[T]{Structured: mcpErrorOutput{Error: message}, IsError: true}
}

func mcpRequestJSON(method, paramsFields string) string {
	if paramsFields == "{}" || paramsFields == "" {
		paramsFields = ""
	} else {
		paramsFields += ","
	}
	return `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{` + paramsFields + `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + mcpProtocolVersion + `","io.modelcontextprotocol/clientInfo":{"name":"caic-test","version":"1.0.0"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
}

type mcpTextOutput struct {
	Result string `json:"result" jsonschema_description:"Human-readable tool result"`
}

type mcpErrorOutput struct {
	Error string `json:"error" jsonschema_description:"Human-readable error message"`
}
