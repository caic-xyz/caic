// MCP protocol DTO behavior tests.

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

	t.Run("subscription resource update uses notifier", func(t *testing.T) {
		registry := newSubscriptionTestRegistry()
		h := &Handler{Registry: registry, ServerInfo: Implementation{Name: "test", Version: "1.0.0"}}
		server := httptest.NewServer(http.HandlerFunc(h.HandleMCP))
		t.Cleanup(server.Close)

		ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(nativeMCPRequestJSON("subscriptions/listen", `"notifications":{"resourceSubscriptions":["test://resource"]}`)))
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
		r := bufio.NewReader(resp.Body)
		ack := readSSEMessage(t, r)
		if ack.Method != NotificationMethodSubscriptionsAcknowledged {
			t.Fatalf("first notification = %q, want acknowledgment", ack.Method)
		}

		// An initial resource update arrives before any mutation, forcing the
		// client to re-read and closing the read-then-subscribe staleness gap.
		initial := readSSEMessage(t, r)
		if initial.Method != NotificationMethodResourcesUpdated {
			t.Fatalf("initial notification = %q, want resource update", initial.Method)
		}
		if uri := resourceUpdateURI(t, initial); uri != "test://resource" {
			t.Fatalf("initial uri = %q, want test://resource", uri)
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

		// A subsequent change still produces exactly one update; the initial
		// burst left the dedup baseline untouched, so the change is not
		// swallowed and not duplicated.
		registry.setResource("changed")
		got := readSSEMessage(t, r)
		if got.Method != NotificationMethodResourcesUpdated {
			t.Fatalf("notification = %q, want resource update", got.Method)
		}
		if uri := resourceUpdateURI(t, got); uri != "test://resource" {
			t.Fatalf("uri = %q, want test://resource", uri)
		}
	})
}

type subscriptionTestRegistry struct {
	mu         sync.Mutex
	resource   string
	changes    chan struct{}
	heartbeats chan struct{}
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
	return RawToolResult{}, nil
}

func (r *subscriptionTestRegistry) ListResources(context.Context) ResourcesListResult {
	return ResourcesListResult{ResultType: ResultTypeComplete, Resources: []ResourceDescriptor{{URI: "test://resource", Name: "resource", MimeType: "application/json"}}}
}

func (r *subscriptionTestRegistry) ReadResource(context.Context, string) (ResourcesReadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func readSSEMessage(t *testing.T, r *bufio.Reader) JSONRPCNotification {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var msg JSONRPCNotification
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			t.Fatalf("decode SSE data: %v", err)
		}
		return msg
	}
}

func resourceUpdateURI(t *testing.T, msg JSONRPCNotification) string {
	t.Helper()
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
