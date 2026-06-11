// Tests for the MCP JSON-RPC endpoint.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// fakeMCPRegistry is a minimal mcpRegistry that surfaces canned errors so the
// dispatcher's error categorization can be tested in isolation.
type fakeMCPRegistry struct {
	callErr error
	readErr error
}

func (f fakeMCPRegistry) tools(context.Context) ([]mcpToolDescriptor, error) {
	return nil, nil
}

func (f fakeMCPRegistry) callTool(context.Context, string, json.RawMessage) (rawToolResult, error) {
	return rawToolResult{}, f.callErr
}

func (f fakeMCPRegistry) listResources(context.Context) resourcesListResult {
	return resourcesListResult{}
}

func (f fakeMCPRegistry) readResource(context.Context, string) (resourcesReadResult, error) {
	return resourcesReadResult{}, f.readErr
}

func TestMCPHandlers(t *testing.T) {
	t.Parallel()

	postMCP := func(t *testing.T, h *mcpHandlers, method, name, body string) (*httptest.ResponseRecorder, jsonRPCResponse) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcpProtocolVersion)
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
		w := httptest.NewRecorder()
		h.handleMCP(w, req)
		var resp jsonRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		return w, resp
	}

	t.Run("serverDiscover", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		w, resp := postMCP(t, s.mcpHandlers, "server/discover", "", mcpRequestJSON("server/discover", `{}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", resp.Result)
		}
		if result["resultType"] != "complete" {
			t.Errorf("resultType = %v, want complete", result["resultType"])
		}
		versions, ok := result["supportedVersions"].([]any)
		if !ok || len(versions) != 1 || versions[0] != mcpProtocolVersion {
			t.Fatalf("supportedVersions = %#v, want [%q]", result["supportedVersions"], mcpProtocolVersion)
		}
		caps, ok := result["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("capabilities type = %T", result["capabilities"])
		}
		if _, ok := caps["tools"]; !ok {
			t.Error("tools capability missing")
		}
	})

	t.Run("toolsList", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		w, resp := postMCP(t, s.mcpHandlers, "tools/list", "", mcpRequestJSON("tools/list", `{}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", resp.Result)
		}
		if result["resultType"] != "complete" {
			t.Errorf("resultType = %v, want complete", result["resultType"])
		}
		if result["cacheScope"] != string(mcpCacheScopePrivate) {
			t.Errorf("cacheScope = %v, want %q", result["cacheScope"], mcpCacheScopePrivate)
		}
		tools, ok := result["tools"].([]any)
		if !ok {
			t.Fatalf("tools type = %T", result["tools"])
		}
		if len(tools) == 0 {
			t.Fatal("tools is empty")
		}
		var found bool
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("tool type = %T", item)
			}
			if tool["name"] == "tasks_list" {
				found = true
				if _, ok := tool["inputSchema"].(map[string]any); !ok {
					t.Fatalf("inputSchema type = %T", tool["inputSchema"])
				}
				outputSchema, ok := tool["outputSchema"].(map[string]any)
				if !ok {
					t.Fatalf("outputSchema type = %T", tool["outputSchema"])
				}
				if outputSchema["type"] != "object" {
					t.Fatalf("outputSchema.type = %v, want object", outputSchema["type"])
				}
			}
		}
		if !found {
			t.Fatal("tasks_list not found")
		}
	})

	t.Run("resourceTemplatesList", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		w, resp := postMCP(t, s.mcpHandlers, "resources/templates/list", "", mcpRequestJSON("resources/templates/list", `{}`))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", resp.Result)
		}
		if result["resultType"] != "complete" {
			t.Errorf("resultType = %v, want complete", result["resultType"])
		}
		templates, ok := result["resourceTemplates"].([]any)
		if !ok {
			t.Fatalf("resourceTemplates type = %T", result["resourceTemplates"])
		}
		if len(templates) == 0 {
			t.Fatal("resourceTemplates is empty")
		}
	})

	t.Run("subscriptionsListenAcknowledges", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"toolsListChanged":true,"promptsListChanged":true,"resourcesListChanged":true,"resourceSubscriptions":["caic://tasks"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcpProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		w := httptest.NewRecorder()
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		data := w.Body.String()
		if !strings.Contains(data, `"method":"notifications/subscriptions/acknowledged"`) {
			t.Fatalf("subscription response = %s, want acknowledgment", data)
		}
		if strings.Contains(data, "promptsListChanged") {
			t.Fatalf("subscription response = %s, want unsupported prompts omitted", data)
		}
	})

	t.Run("toolsCall", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		id := ksid.NewID()
		tk := &task.Task{ID: id, InitialPrompt: agent.Prompt{Text: "test"}, Harness: harness.Claude}
		tk.SetTitle("Fix tests")
		tk.SetState(task.StateWaiting)
		insertTestTask(t, s, id.String(), tk)

		body := mcpRequestJSON("tools/call", `"name":"tasks_list","arguments":{},"inputResponses":{},"requestState":"retry-state"`)
		body = strings.Replace(body, `"io.modelcontextprotocol/clientCapabilities":{}`, `"io.modelcontextprotocol/clientCapabilities":{},"example.com/clientTrace":"trace-1"`, 1)
		w, resp := postMCP(t, s.mcpHandlers, "tools/call", "tasks_list", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		data, err := json.Marshal(resp.Result)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !bytes.Contains(data, []byte(`"resultType":"complete"`)) {
			t.Fatalf("result = %s, want complete resultType", data)
		}
		if !bytes.Contains(data, []byte("Fix tests")) {
			t.Fatalf("result = %s, want task title", data)
		}
		if !bytes.Contains(data, []byte("structuredContent")) {
			t.Fatalf("result = %s, want structuredContent", data)
		}
	})

	t.Run("headerMismatch", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/list", `{}`)))
		req.Header.Set("Mcp-Protocol-Version", mcpProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		w := httptest.NewRecorder()
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		var resp jsonRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != mcpHeaderMismatchCode {
			t.Fatalf("error = %#v, want header mismatch", resp.Error)
		}
	})

	t.Run("toolParamHeaderMismatch", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		body := mcpRequestJSON("tools/call", `"name":"task_get_detail","arguments":{"task_number":1}`)
		w, resp := postMCP(t, s.mcpHandlers, "tools/call", "task_get_detail", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if resp.Error == nil || resp.Error.Code != mcpHeaderMismatchCode {
			t.Fatalf("error = %#v, want header mismatch", resp.Error)
		}
	})

	t.Run("toolExecutionErrorOmitsStructuredContent", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		body := mcpRequestJSON("tools/call", `"name":"task_get_detail","arguments":{"task_number":1}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcpProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "task_get_detail")
		req.Header.Set("Mcp-Param-Task-Number", "1")
		w := httptest.NewRecorder()
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var rpc map[string]any
		if err := json.NewDecoder(w.Body).Decode(&rpc); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		result, ok := rpc["result"].(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", rpc["result"])
		}
		if result["isError"] != true {
			t.Fatalf("isError = %v, want true", result["isError"])
		}
		if _, ok := result["structuredContent"]; ok {
			t.Fatalf("structuredContent present on tool execution error: %#v", result["structuredContent"])
		}
	})

	t.Run("unsupportedProtocolVersion", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		body := strings.ReplaceAll(mcpRequestJSON("tools/list", `{}`), mcpProtocolVersion, "2099-01-01")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", "2099-01-01")
		req.Header.Set("Mcp-Method", "tools/list")
		w := httptest.NewRecorder()
		s.mcpHandlers.handleMCP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		var resp jsonRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != mcpUnsupportedProtocolVersionCode {
			t.Fatalf("error = %#v, want unsupported protocol version", resp.Error)
		}
	})

	// The draft Streamable HTTP transport maps an unknown RPC method to HTTP 404
	// with a -32601 JSON-RPC error body.
	t.Run("initializeRemoved", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		w, resp := postMCP(t, s.mcpHandlers, "initialize", "", mcpRequestJSON("initialize", `{}`))
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
		if resp.Error == nil || resp.Error.Code != -32601 {
			t.Fatalf("error = %#v, want method not found", resp.Error)
		}
	})

	// A registry fault (e.g. a backend lookup failing while building the catalog)
	// must surface as an internal error, not as invalid params.
	t.Run("toolCallBackendError", func(t *testing.T) {
		t.Parallel()
		h := &mcpHandlers{Registry: fakeMCPRegistry{callErr: errors.New("backend unavailable")}, ServerInfo: mcpImplementation{Name: "caic"}}
		body := mcpRequestJSON("tools/call", `"name":"tasks_list","arguments":{}`)
		w, resp := postMCP(t, h, "tools/call", "tasks_list", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error == nil || resp.Error.Code != mcpInternalErrorCode {
			t.Fatalf("error = %#v, want internal error", resp.Error)
		}
	})

	// Bad client input (unknown tool) stays an invalid-params error.
	t.Run("toolCallInvalidParams", func(t *testing.T) {
		t.Parallel()
		h := &mcpHandlers{Registry: fakeMCPRegistry{callErr: errInvalidParams("unknown tool: nope")}, ServerInfo: mcpImplementation{Name: "caic"}}
		body := mcpRequestJSON("tools/call", `"name":"nope","arguments":{}`)
		_, resp := postMCP(t, h, "tools/call", "nope", body)
		if resp.Error == nil || resp.Error.Code != mcpInvalidParamsCode {
			t.Fatalf("error = %#v, want invalid params", resp.Error)
		}
	})

	// A resource backend fault is internal; an unknown resource is invalid params.
	t.Run("resourceReadErrors", func(t *testing.T) {
		t.Parallel()
		internal := &mcpHandlers{Registry: fakeMCPRegistry{readErr: errors.New("snapshot failed")}, ServerInfo: mcpImplementation{Name: "caic"}}
		_, resp := postMCP(t, internal, "resources/read", "caic://tasks", mcpRequestJSON("resources/read", `"uri":"caic://tasks"`))
		if resp.Error == nil || resp.Error.Code != mcpInternalErrorCode {
			t.Fatalf("error = %#v, want internal error", resp.Error)
		}
		notFound := &mcpHandlers{Registry: fakeMCPRegistry{readErr: errInvalidParams("unknown resource: caic://nope")}, ServerInfo: mcpImplementation{Name: "caic"}}
		_, resp = postMCP(t, notFound, "resources/read", "caic://nope", mcpRequestJSON("resources/read", `"uri":"caic://nope"`))
		if resp.Error == nil || resp.Error.Code != mcpInvalidParamsCode {
			t.Fatalf("error = %#v, want invalid params", resp.Error)
		}
	})
}
