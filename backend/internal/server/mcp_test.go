// Tests for the MCP JSON-RPC endpoint.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/oauth"
)

// fakeMCPRegistry is a minimal mcpRegistry that surfaces canned errors so the
// dispatcher's error categorization can be tested in isolation.
type fakeMCPRegistry struct {
	callErr error
	readErr error
}

func (f fakeMCPRegistry) Instructions(context.Context) (string, error) {
	return "Use fake test tools.", nil
}

func (f fakeMCPRegistry) Tools(context.Context) ([]mcp.ToolDescriptor, error) {
	return nil, nil
}

func (f fakeMCPRegistry) CallTool(context.Context, string, json.RawMessage) (mcp.RawToolResult, error) {
	return mcp.RawToolResult{}, f.callErr
}

func (f fakeMCPRegistry) ListResources(context.Context) mcp.ResourcesListResult {
	return mcp.ResourcesListResult{}
}

func (f fakeMCPRegistry) ReadResource(context.Context, string) (mcp.ResourcesReadResult, error) {
	return mcp.ResourcesReadResult{}, f.readErr
}

func (f fakeMCPRegistry) SubscribeResourceUpdates(context.Context, mcp.SubscriptionFilter) (iter.Seq2[mcp.ResourceUpdate, error], error) {
	return func(func(mcp.ResourceUpdate, error) bool) {}, nil
}

func mcpRequestJSON(method, paramsFields string) string {
	if paramsFields == "{}" || paramsFields == "" {
		paramsFields = ""
	} else {
		paramsFields += ","
	}
	return `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{` + paramsFields + `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + mcp.ProtocolVersion + `","io.modelcontextprotocol/clientInfo":{"name":"caic-test","version":"1.0.0"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
}

func TestMCPHandlers(t *testing.T) {
	t.Parallel()

	postMCP := func(t *testing.T, h *mcp.Handler, method, name, body string) (*httptest.ResponseRecorder, mcp.JSONRPCResponse) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
		w := httptest.NewRecorder()
		h.HandleMCP(w, req)
		var resp mcp.JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		return w, resp
	}

	t.Run("disabledLeavesEndpointUnregistered", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		s.mcpDisabled = true
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			req := httptest.NewRequestWithContext(t.Context(), method, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/list", `{}`)))
			req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
			req.Header.Set("Mcp-Method", "tools/list")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s status = %d, want %d", method, w.Code, http.StatusNotFound)
			}
		}
	})

	t.Run("enabledServesWithoutAuth", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/list", `{}`)))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/list")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})

	t.Run("serverDiscover", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		w, resp := postMCP(t, s.mcpHandlers.protocol, "server/discover", "", mcpRequestJSON("server/discover", `{}`))
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
		if !ok || len(versions) != 1 || versions[0] != mcp.ProtocolVersion {
			t.Fatalf("supportedVersions = %#v, want [%q]", result["supportedVersions"], mcp.ProtocolVersion)
		}
		caps, ok := result["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("capabilities type = %T", result["capabilities"])
		}
		toolsCapability, ok := caps["tools"].(map[string]any)
		if !ok {
			t.Error("tools capability missing")
		}
		if _, ok := toolsCapability["listChanged"]; ok {
			t.Fatalf("tools capability = %#v, want no listChanged support", toolsCapability)
		}
		instructions, ok := result["instructions"].(string)
		if !ok {
			t.Fatalf("instructions type = %T", result["instructions"])
		}
		if !strings.Contains(instructions, "[No active tasks]") {
			t.Fatalf("instructions = %q, want no active tasks snapshot", instructions)
		}
	})

	t.Run("serverDiscoverInstructionsIncludeTaskSnapshot", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		id := ksid.NewID()
		insertTestTask(t, s, id.String(), &task.Task{ID: id, InitialPrompt: agent.Prompt{Text: "ship voice prompt"}, Harness: harness.Claude})
		_, resp := postMCP(t, s.mcpHandlers.protocol, "server/discover", "", mcpRequestJSON("server/discover", `{}`))
		if resp.Error != nil {
			t.Fatalf("error = %#v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result type = %T", resp.Result)
		}
		instructions, ok := result["instructions"].(string)
		if !ok {
			t.Fatalf("instructions type = %T", result["instructions"])
		}
		for _, want := range []string{"[Current tasks at session start]", "Task #1", id.String()} {
			if !strings.Contains(instructions, want) {
				t.Fatalf("instructions missing %q: %q", want, instructions)
			}
		}
	})

	t.Run("toolsList", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		w, resp := postMCP(t, s.mcpHandlers.protocol, "tools/list", "", mcpRequestJSON("tools/list", `{}`))
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
		if result["cacheScope"] != string(mcp.CacheScopePrivate) {
			t.Errorf("cacheScope = %v, want %q", result["cacheScope"], mcp.CacheScopePrivate)
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
		w, resp := postMCP(t, s.mcpHandlers.protocol, "resources/templates/list", "", mcpRequestJSON("resources/templates/list", `{}`))
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
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourcesListChanged":true,"resourceSubscriptions":["caic://tasks"]}`)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		data := w.Body.String()
		if !strings.Contains(data, `"method":"notifications/subscriptions/acknowledged"`) {
			t.Fatalf("subscription response = %s, want acknowledgment", data)
		}
		if !strings.Contains(data, `"resourcesListChanged":true`) {
			t.Fatalf("subscription response = %s, want supported resources list changes acknowledged", data)
		}
		if !strings.Contains(data, `"resourceSubscriptions":["caic://tasks"]`) {
			t.Fatalf("subscription response = %s, want supported task resource acknowledged", data)
		}
		// The acknowledgment is followed by an initial state burst that forces
		// the client to re-read each subscribed target through the authorized
		// resources/read path, closing the read-then-subscribe staleness gap.
		// The context is pre-cancelled, so the change loop adds nothing and the
		// body is exactly the ack plus this deterministic burst.
		if !strings.Contains(data, `"method":"notifications/resources/list_changed"`) {
			t.Fatalf("subscription response = %s, want initial resources list_changed", data)
		}
		if !strings.Contains(data, `"method":"notifications/resources/updated"`) || !strings.Contains(data, `"uri":"caic://tasks"`) {
			t.Fatalf("subscription response = %s, want initial caic://tasks update", data)
		}
	})

	t.Run("subscriptionsListenRejectsUnsupportedResource", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["caic://usage"]}`)
		_, resp := postMCP(t, s.mcpHandlers.protocol, "subscriptions/listen", "", body)
		if resp.Error == nil {
			t.Fatal("error is nil, want invalid params")
		}
		if resp.Error.Code != mcp.InvalidParamsCode {
			t.Fatalf("error code = %d, want %d", resp.Error.Code, mcp.InvalidParamsCode)
		}
		if !strings.Contains(resp.Error.Message, "caic://usage") {
			t.Fatalf("error message = %q, want caic://usage", resp.Error.Message)
		}
	})

	t.Run("subscriptionsListenRejectsEmptyFilter", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		body := mcpRequestJSON("subscriptions/listen", `"notifications":{}`)
		_, resp := postMCP(t, s.mcpHandlers.protocol, "subscriptions/listen", "", body)
		if resp.Error == nil {
			t.Fatal("error is nil, want invalid params")
		}
		if resp.Error.Code != mcp.InvalidParamsCode {
			t.Fatalf("error code = %d, want %d", resp.Error.Code, mcp.InvalidParamsCode)
		}
		if !strings.Contains(resp.Error.Message, "empty") {
			t.Fatalf("error message = %q, want empty filter", resp.Error.Message)
		}
	})

	t.Run("subscriptionChangesSignalTaskResource", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
		if !ok {
			t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		changes, err := registry.SubscribeResourceUpdates(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"caic://tasks"}})
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan mcp.ResourceUpdate, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for update := range changes {
				got <- update
				return
			}
		}()
		s.taskMgr.NotifyTaskChange()
		var update mcp.ResourceUpdate
		select {
		case update = <-got:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for subscription change")
		}
		if !slices.Equal(update.ResourceURIs, []string{"caic://tasks"}) {
			t.Fatalf("update resource uris = %#v, want caic://tasks", update.ResourceURIs)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for subscription iterator to stop")
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
		w, resp := postMCP(t, s.mcpHandlers.protocol, "tools/call", "tasks_list", body)
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
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		var resp mcp.JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != mcp.InvalidRequestCode {
			t.Fatalf("error = %#v, want invalid request", resp.Error)
		}
	})

	t.Run("toolParamHeaderMismatch", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		body := mcpRequestJSON("tools/call", `"name":"task_get_detail","arguments":{"task_number":1}`)
		w, resp := postMCP(t, s.mcpHandlers.protocol, "tools/call", "task_get_detail", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if resp.Error == nil || resp.Error.Code != mcp.InvalidRequestCode {
			t.Fatalf("error = %#v, want invalid request", resp.Error)
		}
	})

	t.Run("toolExecutionErrorOmitsStructuredContent", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		body := mcpRequestJSON("tools/call", `"name":"task_get_detail","arguments":{"task_number":1}`)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "task_get_detail")
		req.Header.Set("Mcp-Param-Task-Number", "1")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
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
		body := strings.ReplaceAll(mcpRequestJSON("tools/list", `{}`), mcp.ProtocolVersion, "2099-01-01")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
		req.Header.Set("Mcp-Protocol-Version", "2099-01-01")
		req.Header.Set("Mcp-Method", "tools/list")
		w := httptest.NewRecorder()
		s.mcpHandlers.protocol.HandleMCP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		var resp mcp.JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != mcp.UnsupportedProtocolVersionCode {
			t.Fatalf("error = %#v, want unsupported protocol version", resp.Error)
		}
	})

	// The draft Streamable HTTP transport maps an unknown RPC method to HTTP 404
	// with a -32601 JSON-RPC error body.
	t.Run("initializeRemoved", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t)
		w, resp := postMCP(t, s.mcpHandlers.protocol, "initialize", "", mcpRequestJSON("initialize", `{}`))
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
		h := &mcp.Handler{Registry: fakeMCPRegistry{callErr: errors.New("backend unavailable")}, ServerInfo: mcp.Implementation{Name: "caic"}}
		body := mcpRequestJSON("tools/call", `"name":"tasks_list","arguments":{}`)
		w, resp := postMCP(t, h, "tools/call", "tasks_list", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if resp.Error == nil || resp.Error.Code != mcp.InternalErrorCode {
			t.Fatalf("error = %#v, want internal error", resp.Error)
		}
	})

	// Bad client input (unknown tool) stays an invalid-params error.
	t.Run("toolCallInvalidParams", func(t *testing.T) {
		t.Parallel()
		h := &mcp.Handler{Registry: fakeMCPRegistry{callErr: mcp.ErrInvalidParams("unknown tool: nope")}, ServerInfo: mcp.Implementation{Name: "caic"}}
		body := mcpRequestJSON("tools/call", `"name":"nope","arguments":{}`)
		_, resp := postMCP(t, h, "tools/call", "nope", body)
		if resp.Error == nil || resp.Error.Code != mcp.InvalidParamsCode {
			t.Fatalf("error = %#v, want invalid params", resp.Error)
		}
	})

	// A resource backend fault is internal; an unknown resource is invalid params.
	t.Run("resourceReadErrors", func(t *testing.T) {
		t.Parallel()
		internal := &mcp.Handler{Registry: fakeMCPRegistry{readErr: errors.New("snapshot failed")}, ServerInfo: mcp.Implementation{Name: "caic"}}
		_, resp := postMCP(t, internal, "resources/read", "caic://tasks", mcpRequestJSON("resources/read", `"uri":"caic://tasks"`))
		if resp.Error == nil || resp.Error.Code != mcp.InternalErrorCode {
			t.Fatalf("error = %#v, want internal error", resp.Error)
		}
		notFound := &mcp.Handler{Registry: fakeMCPRegistry{readErr: mcp.ErrInvalidParams("unknown resource: caic://nope")}, ServerInfo: mcp.Implementation{Name: "caic"}}
		_, resp = postMCP(t, notFound, "resources/read", "caic://nope", mcpRequestJSON("resources/read", `"uri":"caic://nope"`))
		if resp.Error == nil || resp.Error.Code != mcp.InvalidParamsCode {
			t.Fatalf("error = %#v, want invalid params", resp.Error)
		}
	})
}

func newAuthEnabledRouter(t *testing.T) (*Router, auth.User) {
	usersPath := filepath.Join(t.TempDir(), "users.json")
	store, err := auth.Open(usersPath)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	user, err := store.UpsertUser(&auth.User{
		Provider:    auth.ProviderGitHub,
		ProviderID:  "1",
		Username:    "alice",
		AccessToken: "forge-token",
		AvatarURL:   "https://github.com/avatar/alice",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	s := newTestOAuthRouter(t, store)
	return s.Router, user
}

func registerTestClient(t *testing.T, h http.Handler, clientName string, redirectURIs []string) oauth.RegisterResponse {
	body := strings.NewReader(`{"client_name":"` + clientName + `","redirect_uris":["` + strings.Join(redirectURIs, `","`) + `"],"token_endpoint_auth_method":"none"}`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", body)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "caic.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var resp oauth.RegisterResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return resp
}
