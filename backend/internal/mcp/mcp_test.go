// MCP protocol DTO behavior tests.

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHandlerHandleMCP swaps the process-global slog default to capture
// logMCPFailure output, so it must run serially: a parallel sibling that emits a
// failure log would pollute the captured buffer and race on it. Running in the
// serial phase guarantees no other test logs concurrently.
//
//nolint:paralleltest // mutates the global slog default; see doc comment.
func TestHandlerHandleMCP(t *testing.T) {
	t.Run("successful tool result omits metadata", func(t *testing.T) {
		registry := &subscriptionTestRegistry{callResult: RawToolResult{Structured: TextOutput{Result: "ok"}}}
		h := &Handler{Registry: registry}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(nativeMCPRequestJSON("tools/call", `"name":"echo","arguments":{}`)))
		req.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
		req.Header.Set("Mcp-Method", string(MethodToolsCall))
		req.Header.Set("Mcp-Name", "echo")
		w := httptest.NewRecorder()
		h.HandleMCP(w, req)
		var response JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		result, ok := response.Result.(map[string]any)
		if !ok {
			t.Fatalf("result = %T, want object", response.Result)
		}
		if _, ok := result["_meta"]; ok {
			t.Fatalf("successful tools/call result has metadata: %#v", result)
		}
	})

	t.Run("tool error metadata", func(t *testing.T) {
		result := RawToolResult{
			Meta:       MetaObject{ToolErrorCodeMetaKey: "UNKNOWN_REPOSITORY"},
			Structured: ErrorOutput{Error: "unknown repository"},
			IsError:    true,
		}
		registry := &subscriptionTestRegistry{}
		registry.callResult = result
		h := &Handler{Registry: registry}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(nativeMCPRequestJSON("tools/call", `"name":"echo","arguments":{}`)))
		req.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
		req.Header.Set("Mcp-Method", string(MethodToolsCall))
		req.Header.Set("Mcp-Name", "echo")
		w := httptest.NewRecorder()
		h.HandleMCP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var response JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		toolResult, ok := response.Result.(map[string]any)
		if !ok {
			t.Fatalf("result = %T, want object", response.Result)
		}
		meta, _ := toolResult["_meta"].(map[string]any)
		if code := meta[ToolErrorCodeMetaKey]; code != "UNKNOWN_REPOSITORY" {
			t.Fatalf("error code metadata = %#v, want UNKNOWN_REPOSITORY", code)
		}
		if _, ok := toolResult["structuredContent"]; ok {
			t.Fatalf("structuredContent present on tool error: %#v", toolResult)
		}
	})

	//nolint:paralleltest // parent runs serially on purpose; see doc comment.
	t.Run("error logs failure", func(t *testing.T) {
		var logBuf bytes.Buffer
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
		t.Cleanup(func() { slog.SetDefault(oldDefault) })

		h := &Handler{}
		req := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			"/api/caic/v1/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","method":"server/discover","params":{}}`),
		)
		w := httptest.NewRecorder()

		h.HandleMCP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		var got map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(logBuf.Bytes()), &got); err != nil {
			t.Fatalf("log JSON: %v\n%s", err, logBuf.String())
		}
		if got["msg"] != "mcp request failed" {
			t.Fatalf("log msg = %v, want mcp request failed", got["msg"])
		}
		if got["mcp_method"] != "server/discover" {
			t.Fatalf("mcp_method = %v, want server/discover", got["mcp_method"])
		}
		if got["err"] != "Invalid Request" {
			t.Fatalf("err = %v, want Invalid Request", got["err"])
		}
	})

	t.Run("subscription initial state and resource update", func(t *testing.T) {
		registry := newSubscriptionTestRegistry()
		h := &Handler{Registry: registry, ServerInfo: Implementation{Name: "test", Version: "1.0.0"}}
		r := openSubscriptionStream(t, h, "test://resource")

		ack := readSSEMessage(t, r)
		if ack.Method != NotificationMethodSubscriptionsAcknowledged {
			t.Fatalf("first notification = %q, want acknowledgment", ack.Method)
		}

		// The post-ack notification is the delivered initial state, sourced from
		// the same pre-ack read that seeds the dedup baseline.
		initial := readSSEMessage(t, r)
		if initial.Method != NotificationMethodSubscriptionsInitialState {
			t.Fatalf("second notification = %q, want initial state", initial.Method)
		}
		if meta := subscriptionIDFromNotification(t, initial); meta != "test" {
			t.Fatalf("initial state subscription id = %q, want test", meta)
		}
		if uri := notificationParamURI(t, initial); uri != "test://resource" {
			t.Fatalf("initial state uri = %q, want test://resource", uri)
		}
		if text := initialStateText(t, initial); text != "initial" {
			t.Fatalf("initial state content = %q, want the pre-ack read value", text)
		}

		// The legacy re-read burst still follows the delivered state for clients
		// that only react to resources/updated.
		burst := readSSEMessage(t, r)
		if burst.Method != NotificationMethodResourcesUpdated {
			t.Fatalf("third notification = %q, want resource update", burst.Method)
		}
		if uri := resourceUpdateURI(t, burst); uri != "test://resource" {
			t.Fatalf("resource update uri = %q, want test://resource", uri)
		}

		if separator, err := r.ReadString('\n'); err != nil || separator != "\n" {
			t.Fatalf("SSE notification terminator = %q, %v; want blank line", separator, err)
		}
		registry.sendHeartbeat()
		heartbeat, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE heartbeat: %v", err)
		}
		if heartbeat != ": keepalive\n" {
			t.Fatalf("SSE heartbeat = %q, want keepalive comment", heartbeat)
		}
		if blank, err := r.ReadString('\n'); err != nil || blank != "\n" {
			t.Fatalf("SSE heartbeat terminator = %q, %v; want blank line", blank, err)
		}

		// A genuine post-subscribe change produces exactly one update even
		// though the initial state was delivered inline.
		registry.setResource("changed")
		changed := readSSEMessage(t, r)
		if changed.Method != NotificationMethodResourcesUpdated {
			t.Fatalf("notification = %q, want resource update", changed.Method)
		}
		if uri := resourceUpdateURI(t, changed); uri != "test://resource" {
			t.Fatalf("uri = %q, want test://resource", uri)
		}

		// Re-signaling the same state is deduplicated against the delivered
		// contents: no further notification may follow.
		registry.setResource("changed")
		duplicate := make(chan JSONRPCNotification, 1)
		go func() {
			if msg, err := readSSEMessageNoFatal(r); err == nil {
				duplicate <- msg
			}
		}()
		select {
		case msg := <-duplicate:
			t.Fatalf("notification for unchanged content: method %q", msg.Method)
		case <-time.After(300 * time.Millisecond):
		}
	})

	t.Run("subscription initial state falls back to re-read on read failure", func(t *testing.T) {
		registry := newSubscriptionTestRegistry()
		registry.readErr = errors.New("resource read unavailable")
		h := &Handler{Registry: registry, ServerInfo: Implementation{Name: "test", Version: "1.0.0"}}
		r := openSubscriptionStream(t, h, "test://resource")

		ack := readSSEMessage(t, r)
		if ack.Method != NotificationMethodSubscriptionsAcknowledged {
			t.Fatalf("first notification = %q, want acknowledgment", ack.Method)
		}

		// A target the pre-ack read could not serve keeps the legacy
		// resources/updated burst so the client still re-reads it.
		fallback := readSSEMessage(t, r)
		if fallback.Method != NotificationMethodResourcesUpdated {
			t.Fatalf("second notification = %q, want resource update fallback", fallback.Method)
		}
		if uri := resourceUpdateURI(t, fallback); uri != "test://resource" {
			t.Fatalf("uri = %q, want test://resource", uri)
		}
	})
}

type subscriptionTestRegistry struct {
	mu         sync.Mutex
	resource   string
	changes    chan struct{}
	heartbeats chan struct{}
	readErr    error
	callResult RawToolResult
	listCalls  int
	seqCalls   int
}

func newSubscriptionTestRegistry() *subscriptionTestRegistry {
	return &subscriptionTestRegistry{
		resource:   "initial",
		changes:    make(chan struct{}, 1),
		heartbeats: make(chan struct{}, 1),
	}
}

func (r *subscriptionTestRegistry) Instructions(context.Context) (string, error) {
	return "", nil
}

func (r *subscriptionTestRegistry) Tools(context.Context) ([]ToolDescriptor, error) {
	return nil, nil
}

func (r *subscriptionTestRegistry) CallTool(context.Context, string, json.RawMessage) (RawToolResult, error) {
	return r.callResult, nil
}

func (r *subscriptionTestRegistry) ListResources(context.Context, string) (ResourcesListResult, error) {
	r.mu.Lock()
	r.listCalls++
	r.mu.Unlock()
	return ResourcesListResult{ResultType: ResultTypeComplete, Resources: []ResourceDescriptor{{URI: "test://resource", Name: "resource", MimeType: "application/json"}}}, nil
}

func (r *subscriptionTestRegistry) Resources(context.Context) iter.Seq2[ResourceDescriptor, error] {
	r.mu.Lock()
	r.seqCalls++
	r.mu.Unlock()
	return func(yield func(ResourceDescriptor, error) bool) {
		yield(ResourceDescriptor{URI: "test://resource", Name: "resource", MimeType: "application/json"}, nil)
	}
}

func TestSubscriptionResourcesHash(t *testing.T) {
	t.Parallel()

	registry := newSubscriptionTestRegistry()
	h := &Handler{Registry: registry}
	first := h.subscriptionResourcesHash(t.Context())
	second := h.subscriptionResourcesHash(t.Context())
	if first == "" || first != second {
		t.Fatalf("subscription resource hashes = %q and %q", first, second)
	}
	registry.mu.Lock()
	listCalls, seqCalls := registry.listCalls, registry.seqCalls
	registry.mu.Unlock()
	if listCalls != 0 || seqCalls != 2 {
		t.Fatalf("catalog calls = list %d, sequence %d; want list 0, sequence 2", listCalls, seqCalls)
	}
}

func (r *subscriptionTestRegistry) ReadResource(context.Context, string) (ResourcesReadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.readErr != nil {
		return ResourcesReadResult{}, r.readErr
	}
	return ResourcesReadResult{ResultType: ResultTypeComplete, Contents: []ResourceContent{{URI: "test://resource", MimeType: "application/json", Text: r.resource}}}, nil
}

func (r *subscriptionTestRegistry) SubscribeResourceUpdates(ctx context.Context, filter SubscriptionFilter) (iter.Seq2[ResourceUpdate, error], error) {
	return func(yield func(ResourceUpdate, error) bool) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.changes:
				if !yield(ResourceUpdate{ResourceURIs: filter.ResourceSubscriptions}, nil) {
					return
				}
			case <-r.heartbeats:
				if !yield(ResourceUpdate{KeepAlive: true}, nil) {
					return
				}
			}
		}
	}, nil
}

func (r *subscriptionTestRegistry) sendHeartbeat() {
	select {
	case r.heartbeats <- struct{}{}:
	default:
	}
}

func (r *subscriptionTestRegistry) setResource(value string) {
	r.mu.Lock()
	r.resource = value
	r.mu.Unlock()
	select {
	case r.changes <- struct{}{}:
	default:
	}
}

func nativeMCPRequestJSON(method, paramsFields string) string {
	if paramsFields == "{}" || paramsFields == "" {
		paramsFields = ""
	} else {
		paramsFields += ","
	}
	return `{"jsonrpc":"2.0","id":"test","method":"` + method + `","params":{` + paramsFields + `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + ProtocolVersion + `","io.modelcontextprotocol/clientInfo":{"name":"caic-test","version":"1.0.0"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
}

// openSubscriptionStream opens a subscriptions/listen SSE stream for one
// resource target and returns the body reader.
func openSubscriptionStream(t *testing.T, h *Handler, uri string) *bufio.Reader {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(h.HandleMCP))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(nativeMCPRequestJSON("subscriptions/listen", fmt.Sprintf(`"notifications":{"resourceSubscriptions":[%q]}`, uri))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
	req.Header.Set("Mcp-Method", string(MethodSubscriptionsListen))
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	return bufio.NewReader(resp.Body)
}

func readSSEMessage(t *testing.T, r *bufio.Reader) JSONRPCNotification {
	msg, err := readSSEMessageNoFatal(r)
	if err != nil {
		t.Fatalf("read SSE: %v", err)
	}
	return msg
}

func readSSEMessageNoFatal(r *bufio.Reader) (JSONRPCNotification, error) {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return JSONRPCNotification{}, err
		}
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var msg JSONRPCNotification
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			return JSONRPCNotification{}, fmt.Errorf("decode SSE data: %w", err)
		}
		return msg, nil
	}
}

func notificationParamURI(t *testing.T, msg JSONRPCNotification) string {
	params, ok := msg.Params.(map[string]any)
	if !ok {
		t.Fatalf("params = %#v, want object", msg.Params)
	}
	uri, _ := params["uri"].(string)
	return uri
}

func subscriptionIDFromNotification(t *testing.T, msg JSONRPCNotification) string {
	params, ok := msg.Params.(map[string]any)
	if !ok {
		t.Fatalf("params = %#v, want object", msg.Params)
	}
	meta, ok := params["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("params missing _meta: %#v", msg.Params)
	}
	id, _ := meta["io.modelcontextprotocol/subscriptionId"].(string)
	return id
}

func initialStateText(t *testing.T, msg JSONRPCNotification) string {
	params, ok := msg.Params.(map[string]any)
	if !ok {
		t.Fatalf("params = %#v, want object", msg.Params)
	}
	contents, ok := params["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents = %#v, want one entry", params["contents"])
	}
	first, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("content = %#v, want object", contents[0])
	}
	if uri, _ := first["uri"].(string); uri != "test://resource" {
		t.Fatalf("content uri = %q, want test://resource", uri)
	}
	if mimeType, _ := first["mimeType"].(string); mimeType != "application/json" {
		t.Fatalf("content mimeType = %q, want application/json", mimeType)
	}
	text, _ := first["text"].(string)
	return text
}

func resourceUpdateURI(t *testing.T, msg JSONRPCNotification) string {
	params, ok := msg.Params.(map[string]any)
	if !ok {
		t.Fatalf("params = %#v, want object", msg.Params)
	}
	uri, _ := params["uri"].(string)
	return uri
}

func TestContentBlockValidate(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		block   ContentBlock
		wantErr bool
	}{
		{name: "text", block: ContentBlock{Type: ContentTypeText, Text: "hello"}},
		{name: "image", block: ContentBlock{Type: ContentTypeImage, Data: "abc", MimeType: "image/png"}},
		{name: "audio", block: ContentBlock{Type: ContentTypeAudio, Data: "abc", MimeType: "audio/wav"}},
		{name: "resourceLink", block: ContentBlock{Type: ContentTypeResourceLink, Name: "log", URI: "file:///tmp/log"}},
		{name: "embeddedResource", block: ContentBlock{Type: ContentTypeResource, Resource: ResourceContent{URI: "file:///tmp/log", Text: "hello"}}},
		{name: "textMissingText", block: ContentBlock{Type: ContentTypeText}, wantErr: true},
		{name: "textWithImageField", block: ContentBlock{Type: ContentTypeText, Text: "hello", Data: "abc"}, wantErr: true},
		{name: "resourceLinkMissingURI", block: ContentBlock{Type: ContentTypeResourceLink, Name: "log"}, wantErr: true},
		{name: "embeddedResourceInvalid", block: ContentBlock{Type: ContentTypeResource, Resource: ResourceContent{URI: "file:///tmp/log"}}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			block := tt.block
			err := block.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

func TestResourceContentValidate(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		content ResourceContent
		wantErr bool
	}{
		{name: "text", content: ResourceContent{URI: "file:///tmp/log", Text: "hello"}},
		{name: "blob", content: ResourceContent{URI: "file:///tmp/log", Blob: "aGVsbG8="}},
		{name: "missingURI", content: ResourceContent{Text: "hello"}, wantErr: true},
		{name: "missingTextOrBlob", content: ResourceContent{URI: "file:///tmp/log"}, wantErr: true},
		{name: "bothTextAndBlob", content: ResourceContent{URI: "file:///tmp/log", Text: "hello", Blob: "aGVsbG8="}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content := tt.content
			err := content.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

func TestRequestMetaPreservesExtraFields(t *testing.T) {
	t.Parallel()

	const body = `{
		"io.modelcontextprotocol/protocolVersion":"2026-07-28",
		"io.modelcontextprotocol/clientInfo":{"name":"test-client","version":"1.0.0"},
		"io.modelcontextprotocol/clientCapabilities":{},
		"example.com/clientTrace":"trace-1",
		"example.com/nested":{"enabled":true}
	}`

	var meta RequestMeta
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := meta.Extra["example.com/clientTrace"]; got != "trace-1" {
		t.Fatalf("extra trace = %#v, want trace-1", got)
	}

	encoded, err := json.Marshal(RequestParams{Meta: meta})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal encoded: %v", err)
	}
	metaFields, ok := fields["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("encoded _meta = %#v, want object", fields["_meta"])
	}
	if got := metaFields["example.com/clientTrace"]; got != "trace-1" {
		t.Fatalf("encoded trace = %#v, want trace-1", got)
	}
	if _, ok := metaFields["example.com/nested"].(map[string]any); !ok {
		t.Fatalf("encoded nested = %#v, want object", metaFields["example.com/nested"])
	}
}
