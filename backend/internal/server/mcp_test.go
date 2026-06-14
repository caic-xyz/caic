// Tests for the MCP JSON-RPC endpoint.

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/task"
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
		if !ok || len(versions) != 1 || versions[0] != mcp.ProtocolVersion {
			t.Fatalf("supportedVersions = %#v, want [%q]", result["supportedVersions"], mcp.ProtocolVersion)
		}
		caps, ok := result["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("capabilities type = %T", result["capabilities"])
		}
		if _, ok := caps["tools"]; !ok {
			t.Error("tools capability missing")
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
		_, resp := postMCP(t, s.mcpHandlers, "server/discover", "", mcpRequestJSON("server/discover", `{}`))
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
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		w := httptest.NewRecorder()
		s.mcpHandlers.HandleMCP(w, req)
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
		req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		w := httptest.NewRecorder()
		s.mcpHandlers.HandleMCP(w, req)
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
		w, resp := postMCP(t, s.mcpHandlers, "tools/call", "task_get_detail", body)
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
		s.mcpHandlers.HandleMCP(w, req)
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
		s.mcpHandlers.HandleMCP(w, req)
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

func newAuthEnabledRouter(t *testing.T) (*Router, *auth.Store, auth.User) {
	s := newTestRouter(t)
	s.hostState = auth.NewHostState("https://caic.example.com")
	s.sessionSecret = []byte("0123456789abcdef0123456789abcdef")
	usersPath := filepath.Join(t.TempDir(), "users.json")
	store, err := auth.Open(usersPath)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	user, err := store.UpsertUser(&auth.User{
		Provider:    forge.KindGitHub,
		ProviderID:  "1",
		Username:    "alice",
		AccessToken: "forge-token",
		AvatarURL:   "https://github.com/avatar/alice",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	s.authStore = store
	return s.Router, store, user
}

func registerTestClient(t *testing.T, h http.Handler, clientName string, redirectURIs []string) mcp.OAuthRegisterResponse {
	body := strings.NewReader(`{"client_name":"` + clientName + `","redirect_uris":["` + strings.Join(redirectURIs, `","`) + `"],"token_endpoint_auth_method":"none"}`)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthRegisterPath, body)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "caic.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp mcp.OAuthRegisterResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return resp
}

func consentTokenFromHTML(t *testing.T, body string) string {
	_, tokenSuffix, ok := strings.Cut(body, `name="consent_token" value="`)
	if !ok {
		t.Fatal("consent token missing")
	}
	consentToken, _, ok := strings.Cut(tokenSuffix, `"`)
	if !ok {
		t.Fatal("consent token value is not terminated")
	}
	return consentToken
}

func TestMCPConsentPage(t *testing.T) {
	t.Parallel()

	s, _, user := newAuthEnabledRouter(t)
	h, err := s.buildHandler()
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}

	verifier := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])

	t.Run("renders consent page with user info and scopes", func(t *testing.T) {
		t.Parallel()
		s2, _, user2 := newAuthEnabledRouter(t)
		h2, err := s2.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		registered := registerTestClient(t, h2, "Claude Code", []string{"https://claude.ai/api/mcp/auth_callback"})

		form := url.Values{
			"response_type":         {"code"},
			"client_id":             {registered.ClientID},
			"redirect_uri":          {"https://claude.ai/api/mcp/auth_callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
			"scope":                 {"caic:mcp.read caic:tasks.read"},
		}
		jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		body := w.Body.String()

		// Verify client identity is shown as unverified, with redirect URI and client ID.
		if !strings.Contains(body, "Claude Code") {
			t.Errorf("body missing client name: %s", body)
		}
		if !strings.Contains(body, "self-declared") {
			t.Errorf("body missing unverified client warning: %s", body)
		}
		if !strings.Contains(body, "https://claude.ai/api/mcp/auth_callback") {
			t.Errorf("body missing redirect URI: %s", body)
		}
		if !strings.Contains(body, registered.ClientID) {
			t.Errorf("body missing client ID: %s", body)
		}

		// Verify username.
		if !strings.Contains(body, user2.Username) {
			t.Errorf("body missing username %q: %s", user2.Username, body)
		}

		// Verify provider.
		if !strings.Contains(body, "GitHub") {
			t.Errorf("body missing provider: %s", body)
		}

		// Verify the avatar does not load a third-party URL and leak OAuth request details.
		if user2.AvatarURL != "" && strings.Contains(body, user2.AvatarURL) {
			t.Errorf("body contains external avatar URL: %s", body)
		}

		// Verify resource.
		if !strings.Contains(body, "caic.example.com/api/caic/v1/mcp") {
			t.Errorf("body missing resource URL: %s", body)
		}

		// Verify scope descriptions.
		if !strings.Contains(body, "caic:mcp.read") {
			t.Error("body missing scope caic:mcp.read")
		}
		if !strings.Contains(body, "web search/fetch") {
			t.Error("body missing scope description for caic:mcp.read")
		}
		if !strings.Contains(body, "caic:tasks.read") {
			t.Error("body missing scope caic:tasks.read")
		}
		if !strings.Contains(body, "Read task information") {
			t.Error("body missing scope description for caic:tasks.read")
		}

		// Verify security warning.
		if !strings.Contains(body, "caic MCP only") {
			t.Error("body missing security warning")
		}
		if !strings.Contains(body, "GitHub") && !strings.Contains(body, "GitLab") {
			t.Error("body missing forge credential disclaimer")
		}

		// Verify consent token is present.
		if !strings.Contains(body, `name="consent_token"`) {
			t.Error("body missing consent_token field")
		}

		// Verify form action.
		if !strings.Contains(body, `action="/api/caic/v1/oauth/authorize"`) {
			t.Error("body missing form action")
		}

		// Verify Deny and Authorize buttons.
		if !strings.Contains(body, "Deny") {
			t.Error("body missing Deny button")
		}
		if !strings.Contains(body, "Authorize") {
			t.Error("body missing Authorize button")
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
		}
		if w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("Referrer-Policy = %q, want no-referrer", w.Header().Get("Referrer-Policy"))
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("Content-Security-Policy = %q, want locked-down default-src", w.Header().Get("Content-Security-Policy"))
		}
	})

	t.Run("shows spoofable client name as unverified", func(t *testing.T) {
		t.Parallel()
		s2, _, user2 := newAuthEnabledRouter(t)
		h2, err := s2.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		registered := registerTestClient(t, h2, "Claude Code", []string{"https://evil.example/callback"})
		form := url.Values{
			"response_type":         {"code"},
			"client_id":             {registered.ClientID},
			"redirect_uri":          {"https://evil.example/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
			"scope":                 {"caic:mcp.read"},
		}
		jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		body := w.Body.String()
		for _, want := range []string{"Claude Code", "self-declared", "https://evil.example/callback", registered.ClientID} {
			if !strings.Contains(body, want) {
				t.Fatalf("body missing %q: %s", want, body)
			}
		}
	})

	t.Run("renders with default scope when empty", func(t *testing.T) {
		t.Parallel()
		s2, _, user2 := newAuthEnabledRouter(t)
		h2, err := s2.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		registered := registerTestClient(t, h2, "Test Client", []string{"http://localhost:9999/callback"})

		form := url.Values{
			"response_type":         {"code"},
			"client_id":             {registered.ClientID},
			"redirect_uri":          {"http://localhost:9999/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
			"scope":                 {""},
		}
		jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		// Default scope should be shown.
		if !strings.Contains(w.Body.String(), "caic:mcp.read") {
			t.Error("body missing default scope caic:mcp.read")
		}
	})

	t.Run("rejects unknown client", func(t *testing.T) {
		t.Parallel()
		form := url.Values{
			"response_type":         {"code"},
			"client_id":             {"unknown_client"},
			"redirect_uri":          {"https://example.com/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
			"scope":                 {"caic:mcp.read"},
		}
		jwt, err := auth.IssueToken(&user, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("rejects unauthenticated request", func(t *testing.T) {
		t.Parallel()
		form := url.Values{
			"response_type":         {"code"},
			"client_id":             {"any"},
			"redirect_uri":          {"https://example.com/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
			"scope":                 {"caic:mcp.read"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
		req.Host = "caic.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d (login_required)", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("rejects invalid redirect URI", func(t *testing.T) {
		t.Parallel()
		s2, _, user2 := newAuthEnabledRouter(t)
		h2, err := s2.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		registered := registerTestClient(t, h2, "Test Client", []string{"https://example.com/callback"})

		form := url.Values{
			"response_type":         {"code"},
			"client_id":             {registered.ClientID},
			"redirect_uri":          {"https://different.com/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
			"scope":                 {"caic:mcp.read"},
		}
		jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("POST approves and redirects with authorization code", func(t *testing.T) {
		t.Parallel()
		s2, _, user2 := newAuthEnabledRouter(t)
		h2, err := s2.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		registered := registerTestClient(t, h2, "Test Client", []string{"https://example.com/callback"})
		form := url.Values{
			"response_type":         {"code"},
			"client_id":             {registered.ClientID},
			"redirect_uri":          {"https://example.com/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
			"scope":                 {"caic:mcp.read"},
			"state":                 {"client-state"},
		}
		jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("consent status = %d, want %d", w.Code, http.StatusOK)
		}
		consentForm := url.Values{"consent_token": {consentTokenFromHTML(t, w.Body.String())}, "decision": {"approve"}}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthAuthorizePath, strings.NewReader(consentForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w = httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
		}
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		if location.Scheme != "https" || location.Host != "example.com" || location.Path != "/callback" {
			t.Fatalf("Location = %q, want callback redirect", location.String())
		}
		if location.Query().Get("code") == "" {
			t.Fatalf("Location = %q, missing code", location.String())
		}
		if location.Query().Get("state") != "client-state" {
			t.Fatalf("state = %q, want client-state", location.Query().Get("state"))
		}
		if location.Query().Get("iss") != "https://caic.example.com" {
			t.Fatalf("iss = %q, want https://caic.example.com", location.Query().Get("iss"))
		}
	})

	t.Run("POST denies and redirects with access_denied", func(t *testing.T) {
		t.Parallel()
		s2, _, user2 := newAuthEnabledRouter(t)
		h2, err := s2.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		registered := registerTestClient(t, h2, "Test Client", []string{"https://example.com/callback"})
		form := url.Values{
			"response_type":         {"code"},
			"client_id":             {registered.ClientID},
			"redirect_uri":          {"https://example.com/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
			"scope":                 {"caic:mcp.read"},
			"state":                 {"deny-state"},
		}
		jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("consent status = %d, want %d", w.Code, http.StatusOK)
		}
		consentForm := url.Values{"consent_token": {consentTokenFromHTML(t, w.Body.String())}, "decision": {"deny"}}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthAuthorizePath, strings.NewReader(consentForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w = httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
		}
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		if location.Query().Get("error") != "access_denied" {
			t.Fatalf("error = %q, want access_denied", location.Query().Get("error"))
		}
		if location.Query().Get("state") != "deny-state" {
			t.Fatalf("state = %q, want deny-state", location.Query().Get("state"))
		}
		if location.Query().Get("iss") != "https://caic.example.com" {
			t.Fatalf("iss = %q, want https://caic.example.com", location.Query().Get("iss"))
		}
	})

	t.Run("POST rejects invalid consent token", func(t *testing.T) {
		t.Parallel()
		form := url.Values{"consent_token": {"invalid-token"}}
		jwt, err := auth.IssueToken(&user, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthAuthorizePath, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("POST rejects wrong user", func(t *testing.T) {
		t.Parallel()
		s2, store2, user2 := newAuthEnabledRouter(t)
		h2, err := s2.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}
		registered := registerTestClient(t, h2, "Test Client", []string{"https://example.com/callback"})

		// Create consent as user2.
		form := url.Values{
			"response_type":         {"code"},
			"client_id":             {registered.ClientID},
			"redirect_uri":          {"https://example.com/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
			"scope":                 {"caic:mcp.read"},
		}
		jwt2, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt2, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("consent status = %d, want %d", w.Code, http.StatusOK)
		}
		consentToken := consentTokenFromHTML(t, w.Body.String())

		// Try to submit consent as a different user.
		otherUser, err := store2.UpsertUser(&auth.User{
			Provider:    forge.KindGitLab,
			ProviderID:  "999",
			Username:    "bob",
			AccessToken: "other-token",
		})
		if err != nil {
			t.Fatalf("upsert other user: %v", err)
		}
		jwtOther, err := auth.IssueToken(&otherUser, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		consentForm := url.Values{"consent_token": {consentToken}}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthAuthorizePath, strings.NewReader(consentForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "caic.example.com"
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwtOther, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		w = httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d (wrong user)", w.Code, http.StatusBadRequest)
		}
	})
}
