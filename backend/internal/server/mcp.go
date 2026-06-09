// MCP endpoint handlers.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	"github.com/invopop/jsonschema"
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
	mcpMethodServerDiscover mcpMethod = "server/discover"
	mcpMethodToolsList      mcpMethod = "tools/list"
	mcpMethodToolsCall      mcpMethod = "tools/call"
	mcpMethodResourcesList  mcpMethod = "resources/list"
	mcpMethodResourcesRead  mcpMethod = "resources/read"
)

type mcpResultType string

const (
	mcpResultTypeComplete mcpResultType = "complete"
)

type mcpCacheScope string

const (
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
	Code    mcpErrorCode                   `json:"code"`
	Message string                         `json:"message"`
	Data    unsupportedProtocolVersionData `json:"data,omitzero"`
}

type unsupportedProtocolVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

type mcpRequestMeta struct {
	ProtocolVersion    string                `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         mcpImplementation     `json:"io.modelcontextprotocol/clientInfo"`
	ClientCapabilities mcpClientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
}

type mcpClientCapabilities struct {
	Experimental mcpExtensions `json:"experimental,omitempty"`
	Extensions   mcpExtensions `json:"extensions,omitempty"`
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
	Tools      mcpToolsCapability     `json:"tools"`
	Resources  mcpResourcesCapability `json:"resources"`
	Extensions mcpExtensions          `json:"extensions,omitempty"`
}

type mcpToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type mcpResourcesCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type mcpImplementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type toolsListResult struct {
	ResultType mcpResultType       `json:"resultType"`
	Tools      []mcpToolDescriptor `json:"tools"`
	TTLMS      int                 `json:"ttlMs"`
	CacheScope mcpCacheScope       `json:"cacheScope"`
}

type mcpToolDescriptor struct {
	Name         string             `json:"name"`
	Title        string             `json:"title,omitempty"`
	Description  string             `json:"description"`
	InputSchema  *jsonschema.Schema `json:"inputSchema"`
	OutputSchema *jsonschema.Schema `json:"outputSchema,omitempty"`
}

type toolsCallParams struct {
	Meta      mcpRequestMeta  `json:"_meta"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type mcpToolCallResult struct {
	ResultType        mcpResultType     `json:"resultType"`
	Content           []mcpContentBlock `json:"content"`
	StructuredContent any               `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

type mcpContentBlock struct {
	Type mcpContentType `json:"type"`
	Text string         `json:"text"`
}

type resourcesListResult struct {
	ResultType mcpResultType           `json:"resultType"`
	Resources  []mcpResourceDescriptor `json:"resources"`
	TTLMS      int                     `json:"ttlMs"`
	CacheScope mcpCacheScope           `json:"cacheScope"`
}

type mcpResourceDescriptor struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type resourcesReadParams struct {
	Meta mcpRequestMeta `json:"_meta"`
	URI  string         `json:"uri"`
}

type resourcesReadResult struct {
	ResultType mcpResultType        `json:"resultType"`
	Contents   []mcpResourceContent `json:"contents"`
	TTLMS      int                  `json:"ttlMs"`
	CacheScope mcpCacheScope        `json:"cacheScope"`
}

type mcpResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text"`
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
	if req.JSONRPC != jsonRPCVersion || req.Method == "" || len(req.ID) == 0 {
		h.writeResponse(w, http.StatusBadRequest, jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcError(mcpInvalidRequestCode, "Invalid Request")})
		return
	}
	// Transport-layer rejections (bad HTTP method, unparseable body, failed
	// _meta/header validation) carry a non-200 status. Once a request is valid
	// MCP it reaches dispatch, whose every outcome — success or a JSON-RPC error
	// (method not found, invalid params, internal) — is returned with HTTP 200.
	if status, rpcErr := validateMCPRequest(r, &req); rpcErr != nil {
		h.writeResponse(w, status, jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	result, rpcErr := h.dispatch(r.Context(), req.Method, req.Params)
	if rpcErr != nil {
		h.writeResponse(w, http.StatusOK, jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	h.writeResponse(w, http.StatusOK, jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result})
}

// dispatch routes a validated MCP request to its handler. The result is always
// delivered with HTTP 200 by handleMCP: a returned rpcErr is a JSON-RPC error
// object (method not found, invalid params, internal), not a transport failure.
func (h *mcpHandlers) dispatch(ctx context.Context, method mcpMethod, params json.RawMessage) (result any, rpcErr *jsonRPCError) {
	switch method {
	case mcpMethodServerDiscover:
		t := discoverResult{
			ResultType:        mcpResultTypeComplete,
			SupportedVersions: []string{mcpProtocolVersion},
			Capabilities:      mcpCapabilities{},
			ServerInfo:        h.ServerInfo,
			Instructions:      h.Instructions,
			TTLMS:             mcpDefaultTTLMS,
			CacheScope:        mcpCacheScopePrivate,
		}
		return t, nil
	case mcpMethodToolsList:
		tools, err := h.Registry.tools(ctx)
		if err != nil {
			return nil, rpcError(mcpInternalErrorCode, err.Error())
		}
		t := toolsListResult{
			ResultType: mcpResultTypeComplete,
			Tools:      tools,
			TTLMS:      mcpDefaultTTLMS,
			CacheScope: mcpCacheScopePrivate,
		}
		return t, nil
	case mcpMethodToolsCall:
		var p toolsCallParams
		if err := decodeParams(params, &p); err != nil || p.Name == "" {
			return nil, rpcError(mcpInvalidParamsCode, "Invalid params")
		}
		res, err := h.Registry.callTool(ctx, p.Name, p.Arguments)
		if err != nil {
			return nil, registryError(err)
		}
		t := mcpToolCallResult{
			ResultType:        mcpResultTypeComplete,
			Content:           []mcpContentBlock{{Type: mcpContentTypeText, Text: toolResultText(res.Structured)}},
			StructuredContent: res.Structured,
			IsError:           res.IsError,
		}
		return t, nil
	case mcpMethodResourcesList:
		return h.Registry.listResources(ctx), nil
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
	return toolSpec{
		Name:         name,
		Title:        title,
		Description:  description,
		InputSchema:  schemaFor[In](),
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
