// Tests for the released Streamable HTTP compatibility handler.

package mcp

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/invopop/jsonschema"
)

// fakeRegistry is a minimal Registry with one tool and one resource, enough to
// exercise the released protocol envelopes.
type fakeRegistry struct{}

func (fakeRegistry) Instructions(context.Context) (string, error) { return "be helpful", nil }

func (fakeRegistry) Tools(context.Context) ([]ToolDescriptor, error) {
	return []ToolDescriptor{{
		Name:        "echo",
		Description: "Echo the input back",
		InputSchema: &jsonschema.Schema{Type: "object"},
	}}, nil
}

func (fakeRegistry) CallTool(_ context.Context, name string, _ json.RawMessage) (RawToolResult, error) {
	if name != "echo" {
		return RawToolResult{}, ErrInvalidParams("unknown tool: %s", name)
	}
	return RawToolResult{Structured: TextOutput{Result: "ok"}}, nil
}

func (fakeRegistry) ListResources(context.Context) ResourcesListResult {
	return ResourcesListResult{
		ResultType: ResultTypeComplete,
		Resources:  []ResourceDescriptor{{URI: "caic://tasks", Name: "tasks", MimeType: "application/json"}},
	}
}

func (fakeRegistry) ReadResource(_ context.Context, uri string) (ResourcesReadResult, error) {
	return ResourceJSON(uri, map[string]any{"ok": true})
}

func (fakeRegistry) SubscribeResourceUpdates(context.Context, SubscriptionFilter) (iter.Seq2[ResourceUpdate, error], error) {
	return func(func(ResourceUpdate, error) bool) {}, nil
}

// compatTestHandler returns a Handler backed by the package's fake registry.
func compatTestHandler() *Handler {
	return &Handler{Registry: fakeRegistry{}, ServerInfo: Implementation{Name: "caic", Version: "test"}}
}

func postCompat(t *testing.T, h *Handler, body string) (*httptest.ResponseRecorder, JSONRPCResponse) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.HandleMCP(w, req)
	var resp JSONRPCResponse
	if w.Body.Len() > 0 {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v (body %q)", err, w.Body.String())
		}
	}
	return w, resp
}

func TestCompatHandler(t *testing.T) {
	t.Parallel()

	// Captured verbatim from `claude --version 2.1.177` and `codex 0.137.0`
	// connecting to a local MCP server over the released Streamable HTTP
	// transport. These are the exact initialize bodies each client sends.
	clients := []struct {
		name        string
		initialize  string
		wantVersion string
	}{
		{
			name:        "claude_code",
			initialize:  `{"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{},"elicitation":{}},"clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.177","description":"Anthropic's agentic coding tool","websiteUrl":"https://claude.com/claude-code"}},"jsonrpc":"2.0","id":0}`,
			wantVersion: "2025-11-25",
		},
		{
			name:        "codex",
			initialize:  `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"elicitation":{}},"clientInfo":{"name":"codex-mcp-client","title":"Codex","version":"0.137.0"}}}`,
			wantVersion: "2025-06-18",
		},
	}

	for _, c := range clients {
		t.Run(c.name+"_initialize", func(t *testing.T) {
			t.Parallel()
			h := compatTestHandler()
			w, resp := postCompat(t, h, c.initialize)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if resp.Error != nil {
				t.Fatalf("unexpected error: %#v", resp.Error)
			}
			result, _ := resp.Result.(map[string]any)
			if got := result["protocolVersion"]; got != c.wantVersion {
				t.Fatalf("protocolVersion = %v, want %s", got, c.wantVersion)
			}
			caps, ok := result["capabilities"].(map[string]any)
			if !ok {
				t.Fatalf("capabilities missing: %#v", result)
			}
			if _, ok := caps["tools"]; !ok {
				t.Fatalf("capabilities.tools missing: %#v", caps)
			}
			info, _ := result["serverInfo"].(map[string]any)
			if info["name"] != "caic" {
				t.Fatalf("serverInfo.name = %v, want caic", info["name"])
			}
		})
	}

	// The released handler must NOT require the Mcp-Method/Mcp-Name headers or
	// the _meta-nested params that the native 2026-07-28 endpoint enforces.
	t.Run("notifications_initialized_no_body", func(t *testing.T) {
		t.Parallel()
		h := compatTestHandler()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp",
			strings.NewReader(`{"method":"notifications/initialized","jsonrpc":"2.0"}`))
		w := httptest.NewRecorder()
		h.HandleMCP(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Fatalf("notification produced a body: %q", w.Body.String())
		}
	})

	t.Run("tools_list", func(t *testing.T) {
		t.Parallel()
		h := compatTestHandler()
		// Codex sends tools/list with a _meta progressToken; it must be tolerated.
		_, resp := postCompat(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"progressToken":0}}}`)
		if resp.Error != nil {
			t.Fatalf("unexpected error: %#v", resp.Error)
		}
		result, _ := resp.Result.(map[string]any)
		tools, ok := result["tools"].([]any)
		if !ok || len(tools) == 0 {
			t.Fatalf("tools = %#v, want non-empty list", result["tools"])
		}
		// Released result must not carry envelope fields exclusive to the
		// 2026-07-28 revision.
		for _, k := range []string{"resultType", "ttlMs", "cacheScope"} {
			if _, ok := result[k]; ok {
				t.Fatalf("released tools/list result leaked 2026-07-28 field %q: %#v", k, result)
			}
		}
	})

	t.Run("tools_call", func(t *testing.T) {
		t.Parallel()
		h := compatTestHandler()
		_, resp := postCompat(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
		if resp.Error != nil {
			t.Fatalf("unexpected error: %#v", resp.Error)
		}
		result, _ := resp.Result.(map[string]any)
		content, ok := result["content"].([]any)
		if !ok || len(content) == 0 {
			t.Fatalf("content = %#v, want non-empty", result["content"])
		}
		if _, ok := result["resultType"]; ok {
			t.Fatalf("released tools/call result leaked resultType: %#v", result)
		}
	})

	// Claude Code calls resources/list during its handshake (Codex does not),
	// so the released path must answer it with a released-shaped result.
	t.Run("resources_list", func(t *testing.T) {
		t.Parallel()
		h := compatTestHandler()
		_, resp := postCompat(t, h, `{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{}}`)
		if resp.Error != nil {
			t.Fatalf("unexpected error: %#v", resp.Error)
		}
		result, _ := resp.Result.(map[string]any)
		resources, ok := result["resources"].([]any)
		if !ok || len(resources) == 0 {
			t.Fatalf("resources = %#v, want non-empty list", result["resources"])
		}
		// The released envelope must not leak the resultType discriminator the
		// 2026-07-28 revision uses.
		if _, ok := result["resultType"]; ok {
			t.Fatalf("released resources/list result leaked resultType: %#v", result)
		}
	})

	t.Run("resources_read", func(t *testing.T) {
		t.Parallel()
		h := compatTestHandler()
		_, resp := postCompat(t, h, `{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"caic://tasks"}}`)
		if resp.Error != nil {
			t.Fatalf("unexpected error: %#v", resp.Error)
		}
		result, _ := resp.Result.(map[string]any)
		contents, ok := result["contents"].([]any)
		if !ok || len(contents) == 0 {
			t.Fatalf("contents = %#v, want non-empty list", result["contents"])
		}
	})

	t.Run("get_returns_405", func(t *testing.T) {
		t.Parallel()
		h := compatTestHandler()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/mcp", nil)
		req.Header.Set("Accept", "text/event-stream")
		w := httptest.NewRecorder()
		h.HandleMCP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET status = %d, want 405", w.Code)
		}
	})

	t.Run("unknown_method", func(t *testing.T) {
		t.Parallel()
		h := compatTestHandler()
		_, resp := postCompat(t, h, `{"jsonrpc":"2.0","id":3,"method":"prompts/list","params":{}}`)
		if resp.Error == nil || resp.Error.Code != MethodNotFoundCode {
			t.Fatalf("error = %#v, want method not found", resp.Error)
		}
	})
}

func TestNegotiateCompatVersion(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"2025-11-25": "2025-11-25",
		"2025-06-18": "2025-06-18",
		"2024-11-05": "2024-11-05",
		"2099-01-01": compatDefaultProtocolVersion,
		"":           compatDefaultProtocolVersion,
		"2026-07-28": compatDefaultProtocolVersion, // caic's native revision is not a released version
	}
	for requested, want := range cases {
		if got := negotiateCompatVersion(requested); got != want {
			t.Errorf("negotiateCompatVersion(%q) = %q, want %q", requested, got, want)
		}
	}
}
