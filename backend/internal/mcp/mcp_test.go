// MCP protocol DTO behavior tests.

package mcp

import (
	"encoding/json"
	"testing"
)

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
