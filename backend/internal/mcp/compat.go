// Released Streamable HTTP protocol compatibility for shipped MCP clients.
//
// Version ownership map:
//   - 2026-07-28: native caic path in mcp.go. It uses server/discover,
//     Mcp-Method/Mcp-Name headers, _meta-nested params, resultType, ttlMs, and
//     cacheScope.
//   - 2025-11-25: Claude Code 2.1.177 fixture. Needs this file's released
//     initialize handshake, notifications/initialized, GET -> 405 SSE probe,
//     flat request params, and released list/call/read result envelopes.
//   - 2025-06-18: Codex 0.137.0 fixture. Needs the same released wire shape as
//     2025-11-25.
//   - 2025-03-26 and 2024-11-05: recognized only for protocol negotiation. We
//     have no current shipped-client fixture for version-specific behavior.
//
// When dropping support for 2025-11-25 and 2025-06-18 clients, delete this file,
// HandleMCP's released fallback, server.go's GET route, and compat_test.go. When
// dropping only 2025-03-26 or 2024-11-05, remove just the corresponding
// negotiation entry below.

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// compatDefaultProtocolVersion is the version returned when a client requests
// one caic does not recognize. 2025-06-18 is the oldest released Streamable
// HTTP revision with a current shipped-client fixture (Codex 0.137.0).
const compatDefaultProtocolVersion = "2025-06-18"

// compatProtocolVersions enumerates released MCP revisions caic answers in the
// released wire format. The native 2026-07-28 revision is handled by the primary
// endpoint. Entries marked negotiation-only can be removed independently once we
// stop accepting that version in initialize.
var compatProtocolVersions = map[string]struct{}{
	"2024-11-05": {}, // negotiation-only; HTTP+SSE-era schema, no current shipped-client fixture
	"2025-03-26": {}, // negotiation-only; first released Streamable HTTP schema
	"2025-06-18": {}, // Codex 0.137.0 fixture
	"2025-11-25": {}, // Claude Code 2.1.177 fixture
}

// compatInitializeResult is the result for a released `initialize` request.
// Needed for 2025-06-18 Codex and 2025-11-25 Claude Code; native 2026-07-28
// uses ServerDiscoverResult instead of initialize.
type compatInitializeResult struct {
	ProtocolVersion string                   `json:"protocolVersion"`
	Capabilities    compatServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation           `json:"serverInfo"`
	Instructions    string                   `json:"instructions,omitempty"`
}

// compatServerCapabilities advertises caic's released features for initialize.
// The tools and resources structs marshal to `{}` when empty, which signals
// support per the 2025-06-18 and 2025-11-25 schemas.
type compatServerCapabilities struct {
	Tools     compatToolsCapability     `json:"tools"`
	Resources compatResourcesCapability `json:"resources"`
}

type compatToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type compatResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// compatInitializeParams captures the released initialize fields. It is needed
// for 2025-06-18 and 2025-11-25 clients, whose params are flat rather than
// nested under _meta as in native 2026-07-28 requests. Unknown fields are
// ignored for forward compatibility.
type compatInitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	ClientInfo      Implementation `json:"clientInfo"`
}

// compatListToolsResult is the 2025-06-18/2025-11-25 tools/list envelope.
// Native 2026-07-28 uses ToolsListResult with resultType, ttlMs, and cacheScope.
type compatListToolsResult struct {
	Tools      []ToolDescriptor `json:"tools"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// compatCallToolResult is the 2025-06-18/2025-11-25 tools/call envelope.
// Native 2026-07-28 uses ToolsCallResult with resultType.
type compatCallToolResult struct {
	Content           []ContentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

// compatListResourcesResult is the 2025-06-18/2025-11-25 resources/list
// envelope. Native 2026-07-28 uses ResourcesListResult with resultType, ttlMs,
// and cacheScope.
type compatListResourcesResult struct {
	Resources  []ResourceDescriptor `json:"resources"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

// compatListResourceTemplatesResult is the 2025-06-18/2025-11-25
// resources/templates/list envelope. Native 2026-07-28 uses
// ResourceTemplatesListResult with resultType, ttlMs, and cacheScope.
type compatListResourceTemplatesResult struct {
	ResourceTemplates []ResourceTemplateDescriptor `json:"resourceTemplates"`
	NextCursor        string                       `json:"nextCursor,omitempty"`
}

// compatReadResourceResult is the 2025-06-18/2025-11-25 resources/read envelope.
// Native 2026-07-28 uses ResourcesReadResult with resultType, ttlMs, and
// cacheScope.
type compatReadResourceResult struct {
	Contents []ResourceContent `json:"contents"`
}

// handleCompat serves one released Streamable HTTP request. caic runs as a
// stateless server: it does not issue an Mcp-Session-Id and offers no
// server-initiated SSE stream, so a GET returns 405 (permitted by the released
// transport spec) and clients fall back to plain POST request/response.
func (h *Handler) handleCompat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "no server-initiated SSE stream", http.StatusMethodNotAllowed)
		return
	}
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeResponse(w, http.StatusOK, JSONRPCResponse{JSONRPC: jsonRPCVersion, Error: rpcError(ParseErrorCode, "Parse error")})
		return
	}
	// Requests without a valid id are notifications (e.g.
	// notifications/initialized); they take no response body.
	if !validJSONRPCRequestID(req.ID) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := h.dispatchCompat(r.Context(), req.Method, req.Params)
	if rpcErr != nil {
		logMCPFailure(r, http.StatusOK, &req, rpcErr, nil)
		h.writeResponse(w, http.StatusOK, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	h.writeResponse(w, http.StatusOK, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result})
}

// dispatchCompat routes a released request to its handler and builds the
// released result envelope.
func (h *Handler) dispatchCompat(ctx context.Context, method Method, params json.RawMessage) (any, *JSONRPCError) {
	switch method {
	case "initialize":
		var p compatInitializeParams
		_ = decodeCompatParams(params, &p)
		instructions, err := h.Registry.Instructions(ctx)
		if err != nil {
			return nil, rpcError(InternalErrorCode, err.Error())
		}
		return compatInitializeResult{
			ProtocolVersion: negotiateCompatVersion(p.ProtocolVersion),
			Capabilities:    compatServerCapabilities{},
			ServerInfo:      h.serverInfo(),
			Instructions:    instructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case MethodToolsList:
		var p PaginatedRequestParams
		if err := decodeCompatParams(params, &p); err != nil {
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
		return compatListToolsResult{Tools: page, NextCursor: next}, nil
	case MethodToolsCall:
		var p ToolsCallParams
		if err := decodeCompatParams(params, &p); err != nil || p.Name == "" {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		res, err := h.Registry.CallTool(ctx, p.Name, p.Arguments)
		if err != nil {
			return nil, registryError(err)
		}
		out := compatCallToolResult{
			Content: []ContentBlock{{Type: ContentTypeText, Text: toolResultText(res.Structured)}},
			IsError: res.IsError,
		}
		if !res.IsError {
			out.StructuredContent = res.Structured
		}
		for i := range out.Content {
			if err := out.Content[i].Validate(); err != nil {
				return nil, rpcError(InternalErrorCode, "invalid tool content")
			}
		}
		return out, nil
	case MethodResourcesList:
		var p PaginatedRequestParams
		if err := decodeCompatParams(params, &p); err != nil {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		res := h.Registry.ListResources(ctx)
		page, next, err := paginate(res.Resources, p.Cursor)
		if err != nil {
			return nil, rpcError(InvalidParamsCode, err.Error())
		}
		return compatListResourcesResult{Resources: page, NextCursor: next}, nil
	case MethodResourceTemplatesList:
		var p PaginatedRequestParams
		if err := decodeCompatParams(params, &p); err != nil {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		page, next, err := paginate(h.resourceTemplates(), p.Cursor)
		if err != nil {
			return nil, rpcError(InvalidParamsCode, err.Error())
		}
		return compatListResourceTemplatesResult{ResourceTemplates: page, NextCursor: next}, nil
	case MethodResourcesRead:
		var p ResourcesReadParams
		if err := decodeCompatParams(params, &p); err != nil || p.URI == "" {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		res, err := h.Registry.ReadResource(ctx, p.URI)
		if err != nil {
			return nil, registryError(err)
		}
		for i := range res.Contents {
			if err := res.Contents[i].Validate(); err != nil {
				return nil, rpcError(InternalErrorCode, "invalid resource content")
			}
		}
		return compatReadResourceResult{Contents: res.Contents}, nil
	default:
		return nil, rpcError(MethodNotFoundCode, "Method not found")
	}
}

// negotiateCompatVersion echoes the client's requested protocol version when
// caic recognizes it, otherwise falls back to a known-good default. The released
// spec lets the server answer with a different supported version.
func negotiateCompatVersion(requested string) string {
	if _, ok := compatProtocolVersions[requested]; ok {
		return requested
	}
	return compatDefaultProtocolVersion
}

// decodeCompatParams decodes released request params leniently: unknown fields
// are tolerated so newer client revisions remain compatible.
func decodeCompatParams(data json.RawMessage, out any) error {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	return json.Unmarshal(data, out)
}
