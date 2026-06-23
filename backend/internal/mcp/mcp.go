// Package mcp implements the Model Context Protocol HTTP endpoint and SDK DTOs.
package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/invopop/jsonschema"
)

const (
	jsonRPCVersion = "2.0"

	// ProtocolVersion is caic's native MCP protocol version.
	//
	// Native support owns server/discover, required per-request _meta,
	// Mcp-Method/Mcp-Name headers, resultType, ttlMs, and cacheScope. Released
	// initialize-based versions 2025-06-18 and 2025-11-25 are isolated in
	// compat.go so their DTOs can be deleted when those clients are no longer
	// supported.
	ProtocolVersion = "2026-07-28"

	// DefaultTTLMS is the default cache lifetime returned by MCP list/read methods.
	DefaultTTLMS = 10_000
)

// ErrorCode is a JSON-RPC error code used by MCP responses.
type ErrorCode int

// JSON-RPC error codes returned by the MCP handler.
const (
	ParseErrorCode                      ErrorCode = -32700
	InvalidRequestCode                  ErrorCode = -32600
	MethodNotFoundCode                  ErrorCode = -32601
	InvalidParamsCode                   ErrorCode = -32602
	InternalErrorCode                   ErrorCode = -32603
	MissingRequiredClientCapabilityCode ErrorCode = -32003
	UnsupportedProtocolVersionCode      ErrorCode = -32004
)

// Method is an MCP JSON-RPC method name.
type Method string

// MCP method names supported by the handler.
const (
	MethodServerDiscover        Method = "server/discover"
	MethodToolsList             Method = "tools/list"
	MethodToolsCall             Method = "tools/call"
	MethodResourcesList         Method = "resources/list"
	MethodResourcesRead         Method = "resources/read"
	MethodResourceTemplatesList Method = "resources/templates/list"
	MethodSubscriptionsListen   Method = "subscriptions/listen"
)

// ResultType describes whether an MCP result is complete or partial.
type ResultType string

// Result type values used by MCP responses.
const (
	ResultTypeComplete ResultType = "complete"
)

// CacheScope describes who may cache an MCP result.
type CacheScope string

// Cache scope values used by MCP responses.
const (
	CacheScopePublic  CacheScope = "public"
	CacheScopePrivate CacheScope = "private"
)

// ContentType identifies the kind of content block in a tool result.
type ContentType string

// Content type values used by MCP tool results.
const (
	ContentTypeAudio        ContentType = "audio"
	ContentTypeImage        ContentType = "image"
	ContentTypeResource     ContentType = "resource"
	ContentTypeResourceLink ContentType = "resource_link"
	ContentTypeText         ContentType = "text"
)

// Handler serves the MCP JSON-RPC HTTP endpoint.
type Handler struct {
	Registry   Registry
	ServerInfo Implementation
}

// Registry supplies MCP tools, resources, instructions, and subscription invalidations to Handler.
type Registry interface {
	Instructions(ctx context.Context) (string, error)
	Tools(ctx context.Context) ([]ToolDescriptor, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (RawToolResult, error)
	ListResources(ctx context.Context) ResourcesListResult
	ReadResource(ctx context.Context, uri string) (ResourcesReadResult, error)
	SubscribeResourceUpdates(ctx context.Context, filter SubscriptionFilter) (iter.Seq2[ResourceUpdate, error], error)
}

// RawToolResult is the transport-neutral output from a tool handler.
type RawToolResult struct {
	Meta       MetaObject
	Structured any
	IsError    bool
}

// ResourceUpdate describes a signal-backed resource update candidate.
type ResourceUpdate struct {
	// ResourcesListChanged indicates the resource list may have changed.
	ResourcesListChanged bool
	// ResourceURIs are subscribed resource URIs whose contents may have changed.
	ResourceURIs []string
}

// JSONRPCRequest is a JSON-RPC request that expects a response.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitzero"`
	Method  Method          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse is a JSON-RPC response containing either a result or an error.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitzero"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// JSONRPCError is a JSON-RPC error object.
type JSONRPCError struct {
	// Code identifies the error type.
	Code ErrorCode `json:"code"`
	// Message is a concise single-sentence description of the error.
	Message string `json:"message"`
	// Data carries sender-defined additional error information.
	Data any `json:"data,omitempty"`
}

type unsupportedProtocolVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

// MetaObject is metadata attached to MCP interactions.
//
// MCP reserves protocol-level key names. Implementations should use prefixed
// reverse-DNS keys for their own metadata.
type MetaObject map[string]any

// RequestMeta is the metadata object required in MCP request params.
type RequestMeta struct {
	// ProtocolVersion is the MCP protocol version used for this request.
	//
	// For HTTP, it must match the MCP-Protocol-Version header.
	ProtocolVersion string `json:"io.modelcontextprotocol/protocolVersion"`
	// ClientInfo identifies the client software making the request.
	ClientInfo Implementation `json:"io.modelcontextprotocol/clientInfo"`
	// ClientCapabilities declares client capabilities for this request.
	ClientCapabilities ClientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
	// ProgressToken requests out-of-band progress notifications for this request.
	ProgressToken any `json:"progressToken,omitempty"`
	// DeprecatedLogLevel requests server log message notifications for this request.
	//
	// Deprecated as of protocol version 2026-07-28, but still present in that
	// schema. This is native 2026-07-28 compatibility, not released-version
	// compat.go support; remove it only after the upstream 2026+ schema drops it.
	DeprecatedLogLevel string `json:"io.modelcontextprotocol/logLevel,omitempty"`
	// Extra preserves forward-compatible metadata fields.
	Extra MetaObject `json:"-"`
}

// UnmarshalJSON decodes RequestMeta while preserving forward-compatible _meta fields.
func (m *RequestMeta) UnmarshalJSON(data []byte) error {
	type requestMeta RequestMeta
	var meta requestMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range requestMetaKnownKeys() {
		delete(raw, key)
	}
	if len(raw) > 0 {
		meta.Extra = make(MetaObject, len(raw))
		for key, value := range raw {
			var decoded any
			if err := json.Unmarshal(value, &decoded); err != nil {
				return err
			}
			meta.Extra[key] = decoded
		}
	}
	*m = RequestMeta(meta)
	return nil
}

// MarshalJSON encodes RequestMeta with preserved forward-compatible _meta fields.
//
//nolint:gocritic // Value receiver ensures json.Marshaler runs for RequestMeta value fields.
func (m RequestMeta) MarshalJSON() ([]byte, error) {
	fields := make(MetaObject, len(m.Extra)+5)
	maps.Copy(fields, m.Extra)
	fields["io.modelcontextprotocol/protocolVersion"] = m.ProtocolVersion
	fields["io.modelcontextprotocol/clientInfo"] = m.ClientInfo
	fields["io.modelcontextprotocol/clientCapabilities"] = m.ClientCapabilities
	if m.ProgressToken != nil {
		fields["progressToken"] = m.ProgressToken
	}
	if m.DeprecatedLogLevel != "" {
		fields["io.modelcontextprotocol/logLevel"] = m.DeprecatedLogLevel
	}
	return json.Marshal(fields)
}

func requestMetaKnownKeys() []string {
	return []string{
		"io.modelcontextprotocol/clientCapabilities",
		"io.modelcontextprotocol/clientInfo",
		"io.modelcontextprotocol/logLevel",
		"io.modelcontextprotocol/protocolVersion",
		"progressToken",
	}
}

// ClientCapabilities describes capabilities the client supports for a request.
type ClientCapabilities struct {
	// Experimental contains non-standard capabilities supported by the client.
	Experimental Extensions `json:"experimental,omitempty"`
	// DeprecatedRoots is present if the client supports listing roots.
	//
	// Deprecated as of protocol version 2026-07-28, but still present in that
	// schema. This is native 2026-07-28 compatibility, not released-version
	// compat.go support; remove it only after the upstream 2026+ schema drops it.
	DeprecatedRoots MetaObject `json:"roots,omitempty"`
	// DeprecatedSampling is present if the client supports server-initiated LLM sampling.
	//
	// Deprecated as of protocol version 2026-07-28, but still present in that
	// schema. This is native 2026-07-28 compatibility, not released-version
	// compat.go support; remove it only after the upstream 2026+ schema drops it.
	DeprecatedSampling *SamplingCapability `json:"sampling,omitempty"`
	// Elicitation is present if the client supports server-initiated user elicitation.
	Elicitation *ElicitationCapability `json:"elicitation,omitempty"`
	// Extensions contains optional MCP extensions supported by the client.
	Extensions Extensions `json:"extensions,omitempty"`
}

// SamplingCapability describes deprecated client sampling support.
type SamplingCapability struct {
	// Context declares context inclusion support.
	Context MetaObject `json:"context,omitempty"`
	// Tools declares tool-use support.
	Tools MetaObject `json:"tools,omitempty"`
}

// ElicitationCapability describes client elicitation support.
type ElicitationCapability struct {
	// Form declares support for form-mode elicitation.
	Form MetaObject `json:"form,omitempty"`
	// URL declares support for URL-mode elicitation.
	URL MetaObject `json:"url,omitempty"`
}

// Extensions stores namespaced MCP extension payloads.
//
// The draft schema constrains values to JSON objects. RawMessage keeps the DTO
// forward-compatible; validate extension values before depending on object shape.
type Extensions map[string]json.RawMessage

// RequestParams contains common MCP request metadata.
type RequestParams struct {
	Meta RequestMeta `json:"_meta"`
}

// ServerDiscoverResult is the result returned for a server/discover request.
type ServerDiscoverResult struct {
	ResultType ResultType `json:"resultType"`
	// SupportedVersions lists MCP protocol versions supported by this server.
	SupportedVersions []string `json:"supportedVersions"`
	// Capabilities advertises server features.
	Capabilities Capabilities `json:"capabilities"`
	// ServerInfo describes the server software implementation.
	ServerInfo Implementation `json:"serverInfo"`
	// Instructions gives natural-language guidance for using the server effectively.
	//
	// Clients may include it in an LLM system prompt. It should not duplicate tool
	// descriptions.
	Instructions string `json:"instructions,omitempty"`
	// TTLMS hints how long clients may cache this response in milliseconds.
	TTLMS int `json:"ttlMs"`
	// CacheScope indicates whether the response may be cached publicly or privately.
	CacheScope CacheScope `json:"cacheScope"`
}

// Capabilities describes server-supported MCP features.
type Capabilities struct {
	// Experimental contains non-standard capabilities supported by the server.
	Experimental Extensions `json:"experimental,omitempty"`
	// DeprecatedLogging is present if the server supports sending log messages to the client.
	//
	// Deprecated as of protocol version 2026-07-28, but still present in that
	// schema. This is native 2026-07-28 compatibility, not released-version
	// compat.go support; remove it only after the upstream 2026+ schema drops it.
	DeprecatedLogging MetaObject `json:"logging,omitempty"`
	// Completions is present if the server supports argument completion suggestions.
	Completions MetaObject `json:"completions,omitempty"`
	// Prompts is present if the server offers prompt templates.
	Prompts *PromptsCapability `json:"prompts,omitempty"`
	// Resources is present if the server offers resources to read.
	Resources ResourcesCapability `json:"resources,omitzero"`
	// Tools is present if the server offers tools to call.
	Tools ToolsCapability `json:"tools"`
	// Extensions contains optional MCP extensions supported by the server.
	Extensions Extensions `json:"extensions,omitempty"`
}

// PromptsCapability describes prompt support advertised by the server.
type PromptsCapability struct {
	// ListChanged indicates support for prompt list change notifications.
	ListChanged bool `json:"listChanged,omitempty"`
}

// ToolsCapability describes tool support advertised by the server.
type ToolsCapability struct {
	// ListChanged indicates support for tool list change notifications.
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability describes resource support advertised by the server.
type ResourcesCapability struct {
	// Subscribe indicates support for subscribing to individual resource updates.
	Subscribe bool `json:"subscribe,omitempty"`
	// ListChanged indicates support for resource list change notifications.
	ListChanged bool `json:"listChanged,omitempty"`
}

// Implementation describes an MCP client or server implementation.
type Implementation struct {
	// Icons contains optional sized icons for UI display.
	Icons []Icon `json:"icons,omitempty"`
	// Name is the programmatic implementation identifier.
	Name string `json:"name"`
	// Title is a human-readable display name.
	Title string `json:"title,omitempty"`
	// Version is the implementation version.
	Version string `json:"version"`
	// Description explains what this implementation does.
	Description string `json:"description,omitempty"`
	// WebsiteURL is an optional website for this implementation.
	WebsiteURL string `json:"websiteUrl,omitempty"`
}

// Icon describes an optionally-sized icon for UI display.
type Icon struct {
	// Src is an icon URI, such as an HTTPS URL or data URI.
	Src string `json:"src"`
	// MimeType overrides a missing or generic source MIME type.
	MimeType string `json:"mimeType,omitempty"`
	// Sizes lists supported dimensions, such as "48x48" or "any".
	Sizes []string `json:"sizes,omitempty"`
	// Theme indicates whether the icon targets a light or dark background.
	Theme string `json:"theme,omitempty"`
}

// PaginatedRequestParams contains common request metadata and an optional cursor.
type PaginatedRequestParams struct {
	Meta RequestMeta `json:"_meta"`
	// Cursor is an opaque pagination token.
	Cursor string `json:"cursor,omitempty"`
}

// ToolsListResult is the response payload for tools/list.
type ToolsListResult struct {
	Meta       MetaObject       `json:"_meta,omitempty"`
	ResultType ResultType       `json:"resultType"`
	NextCursor string           `json:"nextCursor,omitempty"`
	Tools      []ToolDescriptor `json:"tools"`
	TTLMS      int              `json:"ttlMs"`
	CacheScope CacheScope       `json:"cacheScope"`
}

// ToolDescriptor describes a tool the client can call.
type ToolDescriptor struct {
	Meta  MetaObject `json:"_meta,omitempty"`
	Icons []Icon     `json:"icons,omitempty"`
	// Name is the programmatic tool identifier.
	Name string `json:"name"`
	// Title is a human-readable display name.
	Title string `json:"title,omitempty"`
	// Description helps clients and LLMs understand the tool.
	Description string `json:"description,omitempty"`
	// InputSchema defines the expected JSON object arguments for the tool.
	InputSchema *jsonschema.Schema `json:"inputSchema"`
	// OutputSchema defines the structuredContent shape for successful results.
	OutputSchema *jsonschema.Schema `json:"outputSchema,omitempty"`
	// Annotations contains optional tool behavior hints.
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations describe MCP tool behavior hints.
type ToolAnnotations struct {
	// Title is a human-readable title for the tool.
	Title string `json:"title,omitempty"`
	// ReadOnlyHint indicates the tool does not modify its environment.
	ReadOnlyHint bool `json:"readOnlyHint,omitempty"`
	// DestructiveHint indicates the tool may perform destructive updates.
	DestructiveHint bool `json:"destructiveHint,omitempty"`
	// IdempotentHint indicates repeated calls with the same arguments have no additional effect.
	IdempotentHint bool `json:"idempotentHint,omitempty"`
	// OpenWorldHint indicates the tool may interact with external entities.
	OpenWorldHint bool `json:"openWorldHint,omitempty"`
}

// ToolsCallParams is the request params payload for tools/call.
type ToolsCallParams struct {
	Meta RequestMeta `json:"_meta"`
	// InputResponses carries responses to server-initiated requests from a prior input_required result.
	InputResponses json.RawMessage `json:"inputResponses,omitempty"`
	// RequestState carries opaque state from a prior input_required result.
	RequestState string `json:"requestState,omitempty"`
	// Name identifies the tool to invoke.
	Name string `json:"name"`
	// Arguments contains tool arguments as a JSON object.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolCallResult is the response payload for a tool call.
type ToolCallResult struct {
	Meta       MetaObject `json:"_meta,omitempty"`
	ResultType ResultType `json:"resultType"`
	// Content is the unstructured result of the tool call.
	Content []ContentBlock `json:"content"`
	// StructuredContent is optional JSON matching the tool output schema on success.
	StructuredContent any `json:"structuredContent,omitempty"`
	// IsError indicates the tool call ended in an error visible to the model.
	IsError bool `json:"isError,omitempty"`
}

// ContentBlock is an MCP content item.
//
// The draft schema models this as a union. Validate checks that only fields for
// the selected content type are present.
type ContentBlock struct {
	Meta  MetaObject `json:"_meta,omitempty"`
	Icons []Icon     `json:"icons,omitempty"`
	// Type identifies the content variant.
	Type ContentType `json:"type"`
	// Name is used by resource_link content.
	Name string `json:"name,omitempty"`
	// Title is a human-readable display name.
	Title string `json:"title,omitempty"`
	// Text is the text content for text blocks.
	Text string `json:"text,omitempty"`
	// Data is base64-encoded data for image and audio blocks.
	Data string `json:"data,omitempty"`
	// URI identifies resource_link content.
	URI string `json:"uri,omitempty"`
	// Description helps clients and LLMs understand linked resources.
	Description string `json:"description,omitempty"`
	// MimeType is required for image and audio content and optional for resources.
	MimeType string `json:"mimeType,omitempty"`
	// Size is the raw resource size in bytes, if known.
	Size int64 `json:"size,omitzero"`
	// Resource contains embedded resource contents for resource blocks.
	Resource ResourceContent `json:"resource,omitzero"`
	// Annotations provide optional client metadata.
	Annotations Annotations `json:"annotations,omitzero"`
}

// Validate checks that the content block matches the draft schema variant for its type.
func (c *ContentBlock) Validate() error {
	if c == nil {
		return errors.New("content block is nil")
	}
	switch c.Type {
	case ContentTypeText:
		if c.Text == "" {
			return errors.New("text content requires text")
		}
		if c.hasMediaFields() || c.hasResourceLinkFields() || !c.Resource.IsZero() {
			return errors.New("text content contains fields from another content type")
		}
	case ContentTypeImage:
		if c.Data == "" || c.MimeType == "" {
			return errors.New("image content requires data and mimeType")
		}
		if c.Text != "" || c.hasResourceLinkFields() || !c.Resource.IsZero() {
			return errors.New("image content contains fields from another content type")
		}
	case ContentTypeAudio:
		if c.Data == "" || c.MimeType == "" {
			return errors.New("audio content requires data and mimeType")
		}
		if c.Text != "" || c.hasResourceLinkFields() || !c.Resource.IsZero() {
			return errors.New("audio content contains fields from another content type")
		}
	case ContentTypeResourceLink:
		if c.Name == "" || c.URI == "" {
			return errors.New("resource_link content requires name and uri")
		}
		if c.Text != "" || c.Data != "" || !c.Resource.IsZero() {
			return errors.New("resource_link content contains fields from another content type")
		}
	case ContentTypeResource:
		if err := c.Resource.Validate(); err != nil {
			return fmt.Errorf("resource content requires valid embedded resource: %w", err)
		}
		if c.Text != "" || c.Data != "" || c.hasResourceLinkFields() || c.MimeType != "" {
			return errors.New("resource content contains fields from another content type")
		}
	default:
		return fmt.Errorf("unknown content type %q", c.Type)
	}
	return nil
}

func (c *ContentBlock) hasMediaFields() bool {
	return c.Data != "" || c.MimeType != ""
}

func (c *ContentBlock) hasResourceLinkFields() bool {
	return len(c.Icons) > 0 || c.Name != "" || c.Title != "" || c.URI != "" || c.Description != "" || c.Size != 0
}

// Annotations provide optional metadata for MCP resources and content.
type Annotations struct {
	// Audience describes who the data is intended for.
	Audience []Role `json:"audience,omitempty"`
	// Priority describes importance from 0 to 1, with 1 most important.
	Priority *int `json:"priority,omitempty"`
	// LastModified is an ISO 8601 timestamp for the last modification time.
	LastModified string `json:"lastModified,omitempty"`
}

// IsZero reports whether annotations are absent for json omitzero.
func (a Annotations) IsZero() bool {
	return len(a.Audience) == 0 && a.Priority == nil && a.LastModified == ""
}

// Role identifies an MCP audience role.
type Role string

// MCP annotation audience roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ResourceLink identifies an MCP resource referenced from content.
type ResourceLink struct {
	Type  ContentType `json:"type"`
	Icons []Icon      `json:"icons,omitempty"`
	// Name is the programmatic resource identifier.
	Name string `json:"name"`
	// Title is a human-readable display name.
	Title string `json:"title,omitempty"`
	// URI identifies the resource.
	URI string `json:"uri"`
	// Description helps clients and LLMs understand the resource.
	Description string `json:"description,omitempty"`
	// MimeType is the resource MIME type, if known.
	MimeType string `json:"mimeType,omitempty"`
	// Size is the raw resource size in bytes, if known.
	Size int64 `json:"size,omitzero"`
}

// MarshalJSON encodes resource links with the schema-required resource_link type.
//
//nolint:gocritic // Value receiver ensures json.Marshaler runs for ResourceLink values.
func (r ResourceLink) MarshalJSON() ([]byte, error) {
	type resourceLink ResourceLink
	if r.Type == "" {
		r.Type = ContentTypeResourceLink
	}
	return json.Marshal(resourceLink(r))
}

// ResourcesListResult is the response payload for resources/list.
type ResourcesListResult struct {
	Meta       MetaObject           `json:"_meta,omitempty"`
	ResultType ResultType           `json:"resultType"`
	NextCursor string               `json:"nextCursor,omitempty"`
	Resources  []ResourceDescriptor `json:"resources"`
	TTLMS      int                  `json:"ttlMs"`
	CacheScope CacheScope           `json:"cacheScope"`
}

// ResourceDescriptor describes a resource the server can read.
type ResourceDescriptor struct {
	Meta  MetaObject `json:"_meta,omitempty"`
	Icons []Icon     `json:"icons,omitempty"`
	// URI identifies this resource.
	URI string `json:"uri"`
	// Name is the programmatic resource identifier.
	Name string `json:"name"`
	// Title is a human-readable display name.
	Title string `json:"title,omitempty"`
	// Description helps clients and LLMs understand the resource.
	Description string `json:"description,omitempty"`
	// MimeType is the resource MIME type, if known.
	MimeType string `json:"mimeType,omitempty"`
	// Annotations provide optional client metadata.
	Annotations *Annotations `json:"annotations,omitempty"`
	// Size is the raw resource size in bytes, if known.
	Size int64 `json:"size,omitzero"`
}

// ResourceTemplatesListResult is the response payload for resources/templates/list.
type ResourceTemplatesListResult struct {
	Meta              MetaObject                   `json:"_meta,omitempty"`
	ResultType        ResultType                   `json:"resultType"`
	NextCursor        string                       `json:"nextCursor,omitempty"`
	ResourceTemplates []ResourceTemplateDescriptor `json:"resourceTemplates"`
	TTLMS             int                          `json:"ttlMs"`
	CacheScope        CacheScope                   `json:"cacheScope"`
}

// ResourceTemplateDescriptor describes a parameterized MCP resource.
type ResourceTemplateDescriptor struct {
	Meta  MetaObject `json:"_meta,omitempty"`
	Icons []Icon     `json:"icons,omitempty"`
	// Name is the programmatic template identifier.
	Name string `json:"name"`
	// Title is a human-readable display name.
	Title string `json:"title,omitempty"`
	// URITemplate is an RFC 6570 URI template for constructing resource URIs.
	URITemplate string `json:"uriTemplate"`
	// Description helps clients and LLMs understand the template.
	Description string `json:"description,omitempty"`
	// MimeType is the MIME type for matching resources, if uniform.
	MimeType string `json:"mimeType,omitempty"`
	// Annotations provide optional client metadata.
	Annotations *Annotations `json:"annotations,omitempty"`
}

// ResourcesReadParams is the request params payload for resources/read.
type ResourcesReadParams struct {
	Meta RequestMeta `json:"_meta"`
	// InputResponses carries responses to server-initiated requests from a prior input_required result.
	InputResponses json.RawMessage `json:"inputResponses,omitempty"`
	// RequestState carries opaque state from a prior input_required result.
	RequestState string `json:"requestState,omitempty"`
	// URI identifies the resource to read.
	URI string `json:"uri"`
}

// ResourcesReadResult is the response payload for resources/read.
type ResourcesReadResult struct {
	Meta       MetaObject `json:"_meta,omitempty"`
	ResultType ResultType `json:"resultType"`
	// Contents contains text or blob resource contents.
	Contents []ResourceContent `json:"contents"`
	// TTLMS hints how long clients may cache this response in milliseconds.
	TTLMS int `json:"ttlMs"`
	// CacheScope indicates whether the response may be cached publicly or privately.
	CacheScope CacheScope `json:"cacheScope"`
}

// ResourceContent contains resource data returned by resources/read.
type ResourceContent struct {
	Meta MetaObject `json:"_meta,omitempty"`
	// URI identifies this resource.
	URI string `json:"uri"`
	// MimeType is the resource MIME type, if known.
	MimeType string `json:"mimeType,omitempty"`
	// Text contains textual resource contents.
	Text string `json:"text,omitempty"`
	// Blob contains base64-encoded binary resource contents.
	Blob string `json:"blob,omitempty"`
}

// IsZero reports whether resource content is absent for json omitzero.
func (r ResourceContent) IsZero() bool {
	return len(r.Meta) == 0 && r.URI == "" && r.MimeType == "" && r.Text == "" && r.Blob == ""
}

// Validate checks that resource content matches exactly one draft schema variant.
func (r ResourceContent) Validate() error {
	if r.URI == "" {
		return errors.New("resource content requires uri")
	}
	hasText := r.Text != ""
	hasBlob := r.Blob != ""
	if hasText == hasBlob {
		return errors.New("resource content requires exactly one of text or blob")
	}
	return nil
}

// SubscriptionsListenParams is the request params payload for subscriptions/listen.
type SubscriptionsListenParams struct {
	Meta RequestMeta `json:"_meta"`
	// Notifications declares notification types the client opts in to.
	Notifications SubscriptionFilter `json:"notifications"`
}

// SubscriptionFilter describes MCP subscription notifications requested by a client.
type SubscriptionFilter struct {
	// ResourcesListChanged requests resource list change notifications.
	ResourcesListChanged bool `json:"resourcesListChanged,omitempty"`
	// ResourceSubscriptions requests updates for individual resource URIs.
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
}

// JSONRPCNotification is a JSON-RPC notification that does not expect a response.
type JSONRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// SubscriptionNotificationParams is the payload for subscription notifications.
type SubscriptionNotificationParams struct {
	Meta MetaObject `json:"_meta,omitempty"`
	// Notifications is the subset of requested notification types the server accepted.
	Notifications *SubscriptionFilter `json:"notifications,omitempty"`
	// URI identifies an updated resource.
	URI string `json:"uri,omitempty"`
}

// HandleMCP handles one MCP HTTP request, dispatching to caic's native
// 2026-07-28 protocol or the released Streamable HTTP compatibility handler
// based on the request shape.
func (h *Handler) HandleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// The native 2026-07-28 transport is POST-only; a GET is a released
		// client opening an SSE stream, which the released handler answers
		// with 405.
		h.handleCompat(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeResponse(w, http.StatusBadRequest, JSONRPCResponse{JSONRPC: jsonRPCVersion, Error: rpcError(ParseErrorCode, "Parse error")})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if isNativeMCPRequest(r, body) {
		h.handleNative(w, r)
		return
	}
	h.handleCompat(w, r)
}

// isNativeMCPRequest reports whether a POST uses caic's native 2026-07-28
// revision. That transport requires an Mcp-Method header on every request,
// which every caic-native client (frontend, Android, generated SDKs) always
// sets. server/discover is exclusive to the 2026-07-28 revision, so it is
// treated as native even without the header to preserve protocol diagnostics.
// Released clients send neither the header nor server/discover.
func isNativeMCPRequest(r *http.Request, body []byte) bool {
	if r.Header.Get("Mcp-Method") != "" {
		return true
	}
	var probe struct {
		Method Method `json:"method"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Method == MethodServerDiscover
}

// handleNative handles one MCP HTTP request using caic's native 2026-07-28
// protocol revision.
func (h *Handler) handleNative(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		rpcErr := rpcError(InvalidRequestCode, "Method not allowed")
		logMCPFailure(r, http.StatusMethodNotAllowed, nil, rpcErr, nil)
		h.writeResponse(w, http.StatusMethodNotAllowed, JSONRPCResponse{JSONRPC: jsonRPCVersion, Error: rpcErr})
		return
	}
	var req JSONRPCRequest
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		rpcErr := rpcError(ParseErrorCode, "Parse error")
		logMCPFailure(r, http.StatusBadRequest, nil, rpcErr, err)
		h.writeResponse(w, http.StatusBadRequest, JSONRPCResponse{JSONRPC: jsonRPCVersion, Error: rpcErr})
		return
	}
	if req.JSONRPC != jsonRPCVersion || req.Method == "" || !validJSONRPCRequestID(req.ID) {
		rpcErr := rpcError(InvalidRequestCode, "Invalid Request")
		logMCPFailure(r, http.StatusBadRequest, &req, rpcErr, nil)
		h.writeResponse(w, http.StatusBadRequest, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	// Transport-layer rejections (bad HTTP method, unparseable body, failed
	// _meta/header validation) carry a non-200 status. Once a request is valid
	// MCP it reaches dispatch, whose protocol errors use the transport status
	// mandated by the 2026-07-28 Streamable HTTP binding.
	if status, rpcErr := validateMCPRequest(r, &req); rpcErr != nil {
		logMCPFailure(r, status, &req, rpcErr, nil)
		h.writeResponse(w, status, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	if req.Method == MethodSubscriptionsListen {
		if rpcErr := h.handleSubscription(r.Context(), w, req.ID, req.Params); rpcErr != nil {
			status := rpcHTTPStatus(rpcErr)
			logMCPFailure(r, status, &req, rpcErr, nil)
			h.writeResponse(w, status, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		}
		return
	}
	result, rpcErr := h.dispatch(r.Context(), req.Method, req.Params, r.Header)
	if rpcErr != nil {
		status := rpcHTTPStatus(rpcErr)
		logMCPFailure(r, status, &req, rpcErr, nil)
		h.writeResponse(w, status, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Error: rpcErr})
		return
	}
	h.writeResponse(w, http.StatusOK, JSONRPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result})
}

// dispatch routes a validated MCP request to its handler. Returned rpcErr values
// are JSON-RPC errors; handleMCP maps them to the transport status required by
// the 2026-07-28 Streamable HTTP binding.
func (h *Handler) dispatch(ctx context.Context, method Method, params json.RawMessage, header http.Header) (result any, rpcErr *JSONRPCError) {
	switch method {
	case MethodServerDiscover:
		instructions, err := h.Registry.Instructions(ctx)
		if err != nil {
			return nil, rpcError(InternalErrorCode, err.Error())
		}
		t := ServerDiscoverResult{
			ResultType:        ResultTypeComplete,
			SupportedVersions: []string{ProtocolVersion},
			Capabilities:      Capabilities{Tools: ToolsCapability{}, Resources: ResourcesCapability{Subscribe: true, ListChanged: true}},
			ServerInfo:        h.serverInfo(),
			Instructions:      instructions,
			TTLMS:             DefaultTTLMS,
			CacheScope:        CacheScopePrivate,
		}
		return t, nil
	case MethodToolsList:
		var p PaginatedRequestParams
		if err := decodeParams(params, &p); err != nil {
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
		t := ToolsListResult{
			ResultType: ResultTypeComplete,
			NextCursor: next,
			Tools:      page,
			TTLMS:      DefaultTTLMS,
			CacheScope: CacheScopePrivate,
		}
		return t, nil
	case MethodToolsCall:
		var p ToolsCallParams
		if err := decodeParams(params, &p); err != nil || p.Name == "" {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		if err := h.validateToolParamHeaders(ctx, header, p.Name, p.Arguments); err != nil {
			return nil, rpcError(InvalidRequestCode, err.Error())
		}
		res, err := h.Registry.CallTool(ctx, p.Name, p.Arguments)
		if err != nil {
			return nil, registryError(err)
		}
		t := ToolCallResult{
			Meta:       res.Meta,
			ResultType: ResultTypeComplete,
			Content:    []ContentBlock{{Type: ContentTypeText, Text: toolResultText(res.Structured)}},
			IsError:    res.IsError,
		}
		if !res.IsError {
			t.StructuredContent = res.Structured
		}
		for i := range t.Content {
			if err := t.Content[i].Validate(); err != nil {
				return nil, rpcError(InternalErrorCode, fmt.Sprintf("invalid tool content %d: %v", i, err))
			}
		}
		return t, nil
	case MethodResourcesList:
		var p PaginatedRequestParams
		if err := decodeParams(params, &p); err != nil {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		res := h.Registry.ListResources(ctx)
		page, next, err := paginate(res.Resources, p.Cursor)
		if err != nil {
			return nil, rpcError(InvalidParamsCode, err.Error())
		}
		res.Resources = page
		res.NextCursor = next
		return res, nil
	case MethodResourceTemplatesList:
		var p PaginatedRequestParams
		if err := decodeParams(params, &p); err != nil {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		templates := h.resourceTemplates()
		page, next, err := paginate(templates, p.Cursor)
		if err != nil {
			return nil, rpcError(InvalidParamsCode, err.Error())
		}
		return ResourceTemplatesListResult{ResultType: ResultTypeComplete, NextCursor: next, ResourceTemplates: page, TTLMS: DefaultTTLMS, CacheScope: CacheScopePrivate}, nil
	case MethodResourcesRead:
		var p ResourcesReadParams
		if err := decodeParams(params, &p); err != nil || p.URI == "" {
			return nil, rpcError(InvalidParamsCode, "Invalid params")
		}
		res, err := h.Registry.ReadResource(ctx, p.URI)
		if err != nil {
			return nil, registryError(err)
		}
		for i := range res.Contents {
			if err := res.Contents[i].Validate(); err != nil {
				return nil, rpcError(InternalErrorCode, fmt.Sprintf("invalid resource content %d: %v", i, err))
			}
		}
		return res, nil
	default:
		return nil, rpcError(MethodNotFoundCode, "Method not found")
	}
}

func (h *Handler) writeResponse(w http.ResponseWriter, status int, resp JSONRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Warn("write mcp response", "err", err)
	}
}

func (h *Handler) serverInfo() Implementation {
	info := h.ServerInfo
	if info.Version == "" {
		info.Version = "unknown"
	}
	return info
}

func (h *Handler) resourceTemplates() []ResourceTemplateDescriptor {
	return []ResourceTemplateDescriptor{
		{Name: "repo", Title: "Repository", URITemplate: "caic://repos/{path}", Description: "Managed repository detail by path", MimeType: "application/json"},
		{Name: "task", Title: "Task", URITemplate: "caic://tasks/{id}", Description: "Coding task detail by task ID", MimeType: "application/json"},
	}
}

func (h *Handler) handleSubscription(ctx context.Context, w http.ResponseWriter, id, params json.RawMessage) *JSONRPCError {
	var p SubscriptionsListenParams
	if err := decodeParams(params, &p); err != nil {
		return rpcError(InvalidParamsCode, "Invalid params")
	}
	stream, ok := w.(subscriptionStreamWriter)
	if !ok {
		return rpcError(InternalErrorCode, "streaming unavailable")
	}
	subID := mcpSubscriptionID(id)
	changes, err := h.Registry.SubscribeResourceUpdates(ctx, p.Notifications)
	if err != nil {
		return registryError(err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := writeMCPNotification(stream, JSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/subscriptions/acknowledged", Params: SubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID), Notifications: &p.Notifications}}); err != nil {
		slog.WarnContext(ctx, "write mcp subscription acknowledgment", "err", err)
		return nil
	}
	h.streamSubscriptionNotifications(ctx, stream, subID, p.Notifications, changes)
	return nil
}

type subscriptionStreamWriter interface {
	http.ResponseWriter
	http.Flusher
}

func (h *Handler) streamSubscriptionNotifications(ctx context.Context, w subscriptionStreamWriter, subID string, filter SubscriptionFilter, changes iter.Seq2[ResourceUpdate, error]) {
	lastResources, lastResourceContents := h.subscriptionSnapshot(ctx, filter)
	for update, err := range changes {
		if err != nil {
			slog.WarnContext(ctx, "mcp resource update stream stopped", "err", err)
			return
		}
		if update.ResourcesListChanged && filter.ResourcesListChanged {
			resources := h.subscriptionResourcesHash(ctx)
			if resources != lastResources {
				if err := writeMCPNotification(w, JSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/resources/list_changed", Params: SubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID)}}); err != nil {
					slog.WarnContext(ctx, "write mcp resources notification", "err", err)
					return
				}
				lastResources = resources
			}
		}
		for _, uri := range update.ResourceURIs {
			if !slices.Contains(filter.ResourceSubscriptions, uri) {
				continue
			}
			content := h.subscriptionResourceContentHash(ctx, uri)
			if content == lastResourceContents[uri] {
				continue
			}
			if err := writeMCPNotification(w, JSONRPCNotification{JSONRPC: jsonRPCVersion, Method: "notifications/resources/updated", Params: SubscriptionNotificationParams{Meta: mcpSubscriptionMeta(subID), URI: uri}}); err != nil {
				slog.WarnContext(ctx, "write mcp resource update notification", "err", err)
				return
			}
			lastResourceContents[uri] = content
		}
	}
}

func (h *Handler) subscriptionSnapshot(ctx context.Context, filter SubscriptionFilter) (resourcesHash string, contents map[string]string) {
	if filter.ResourcesListChanged {
		resourcesHash = h.subscriptionResourcesHash(ctx)
	}
	contents = make(map[string]string, len(filter.ResourceSubscriptions))
	for _, uri := range filter.ResourceSubscriptions {
		contents[uri] = h.subscriptionResourceContentHash(ctx, uri)
	}
	return resourcesHash, contents
}

func (h *Handler) subscriptionResourcesHash(ctx context.Context) string {
	return stableJSON(h.Registry.ListResources(ctx).Resources)
}

func (h *Handler) subscriptionResourceContentHash(ctx context.Context, uri string) string {
	res, err := h.Registry.ReadResource(ctx, uri)
	if err != nil {
		return err.Error()
	}
	return stableJSON(res.Contents)
}

func mcpSubscriptionID(id json.RawMessage) string {
	var s string
	if err := json.Unmarshal(id, &s); err == nil {
		return s
	}
	return string(id)
}

func mcpSubscriptionMeta(id string) MetaObject {
	return MetaObject{"io.modelcontextprotocol/subscriptionId": id}
}

func logMCPFailure(r *http.Request, status int, req *JSONRPCRequest, rpcErr *JSONRPCError, err any) {
	attrs := []any{
		"status", status,
		"http_method", r.Method,
		"path", r.URL.Path,
	}
	if req != nil {
		if req.Method != "" {
			attrs = append(attrs, "mcp_method", req.Method)
		}
		if len(req.ID) != 0 {
			attrs = append(attrs, "id", string(req.ID))
		}
	}
	if rpcErr != nil {
		attrs = append(attrs, "rpc_code", rpcErr.Code)
		if err == nil {
			err = rpcErr.Message
		}
	}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	slog.ErrorContext(r.Context(), "mcp request failed", attrs...)
}

func writeMCPNotification(w subscriptionStreamWriter, msg JSONRPCNotification) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	w.Flush()
	return nil
}

func stableJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(data)
}

func (h *Handler) validateToolParamHeaders(ctx context.Context, header http.Header, name string, args json.RawMessage) error {
	tools, err := h.Registry.Tools(ctx)
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

// HeaderParam maps an MCP input-schema property to a required HTTP header.
type HeaderParam struct {
	Header string
	Path   []string
}

func mcpHeaderParams(schema *jsonschema.Schema) ([]HeaderParam, error) {
	var params []HeaderParam
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
			params = append(params, HeaderParam{Header: header, Path: append([]string(nil), path...)})
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

func rpcHTTPStatus(err *JSONRPCError) int {
	switch err.Code {
	case MethodNotFoundCode:
		return http.StatusNotFound
	case InvalidRequestCode:
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

func validateMCPRequest(r *http.Request, req *JSONRPCRequest) (int, *JSONRPCError) {
	var p RequestParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return http.StatusBadRequest, rpcError(InvalidParamsCode, "Invalid params")
	}
	meta := p.Meta
	if meta.ProtocolVersion == "" || meta.ClientInfo.Name == "" || meta.ClientInfo.Version == "" {
		return http.StatusBadRequest, rpcError(InvalidParamsCode, "Invalid params: required _meta fields missing")
	}
	headerVersion := r.Header.Get("Mcp-Protocol-Version")
	if headerVersion == "" {
		return http.StatusBadRequest, rpcError(InvalidRequestCode, "Header mismatch: MCP-Protocol-Version header is required")
	}
	if headerVersion != meta.ProtocolVersion {
		return http.StatusBadRequest, rpcError(InvalidRequestCode, "Header mismatch: MCP-Protocol-Version header does not match request _meta")
	}
	if meta.ProtocolVersion != ProtocolVersion {
		return http.StatusBadRequest, &JSONRPCError{
			Code:    UnsupportedProtocolVersionCode,
			Message: "Unsupported protocol version",
			Data: unsupportedProtocolVersionData{
				Supported: []string{ProtocolVersion},
				Requested: meta.ProtocolVersion,
			},
		}
	}
	if got := r.Header.Get("Mcp-Method"); got == "" {
		return http.StatusBadRequest, rpcError(InvalidRequestCode, "Header mismatch: Mcp-Method header is required")
	} else if got != string(req.Method) {
		return http.StatusBadRequest, rpcError(InvalidRequestCode, "Header mismatch: Mcp-Method header does not match request method")
	}
	name, required, err := mcpRequestName(req.Method, req.Params)
	if err != nil {
		return http.StatusBadRequest, rpcError(InvalidParamsCode, "Invalid params")
	}
	if !required {
		return http.StatusOK, nil
	}
	if got := r.Header.Get("Mcp-Name"); got == "" {
		return http.StatusBadRequest, rpcError(InvalidRequestCode, "Header mismatch: Mcp-Name header is required")
	} else if got != name {
		return http.StatusBadRequest, rpcError(InvalidRequestCode, "Header mismatch: Mcp-Name header does not match request params")
	}
	return http.StatusOK, nil
}

func mcpRequestName(method Method, params json.RawMessage) (name string, required bool, err error) {
	switch method {
	case MethodToolsCall:
		var p ToolsCallParams
		if err := decodeParams(params, &p); err != nil {
			return "", true, err
		}
		if p.Name == "" {
			return "", true, errors.New("name is required")
		}
		return p.Name, true, nil
	case MethodResourcesRead:
		var p ResourcesReadParams
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

func rpcError(code ErrorCode, message string) *JSONRPCError {
	return &JSONRPCError{Code: code, Message: message}
}

// invalidParamsError marks a registry failure caused by bad client input — an
// unknown tool or resource, or undecodable arguments — so the dispatcher reports
// it as a JSON-RPC invalid-params error. Unmarked registry errors are treated as
// internal faults (e.g. a backend lookup failing while building the tool catalog).
type invalidParamsError struct{ err error }

func (e invalidParamsError) Error() string { return e.err.Error() }

func (e invalidParamsError) Unwrap() error { return e.err }

// ErrInvalidParams marks a registry error as client-caused invalid params.
func ErrInvalidParams(format string, args ...any) error {
	return invalidParamsError{err: fmt.Errorf(format, args...)}
}

// registryError maps a registry error to a JSON-RPC error: invalid params for
// client-input faults, internal for everything else.
func registryError(err error) *JSONRPCError {
	if _, ok := errors.AsType[invalidParamsError](err); ok {
		return rpcError(InvalidParamsCode, err.Error())
	}
	return rpcError(InternalErrorCode, err.Error())
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

// ToolHandler executes a tool with raw JSON arguments.
type ToolHandler func(context.Context, json.RawMessage) (RawToolResult, error)

// ToolSpec describes a server-side tool implementation.
type ToolSpec struct {
	Name         string
	Title        string
	Description  string
	InputSchema  *jsonschema.Schema
	OutputSchema *jsonschema.Schema
	Annotations  *ToolAnnotations
	Handler      ToolHandler
}

// ToolResult carries a tool handler's output. The T type parameter exists only
// to drive output-schema generation in NewToolSpec (via the [0]T phantom field);
// it does NOT constrain Structured, which stays any so error paths can substitute
// a different payload. Concretely, ToolError[T] puts an ErrorOutput in
// Structured regardless of T, so the emitted structuredContent is not guaranteed
// to match the advertised outputSchema — IsError signals when it diverges. Treat
// outputSchema as a hint for success results, not a wire contract.
type ToolResult[T any] struct {
	Meta       MetaObject
	Structured any
	IsError    bool
	_          [0]T
}

func (r ToolResult[T]) toRawToolResult() RawToolResult {
	return RawToolResult{Meta: r.Meta, Structured: r.Structured, IsError: r.IsError}
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

// NewToolSpec builds a ToolSpec with reflected input and output schemas.
func NewToolSpec[In, Out any](name, title, description string, handler func(context.Context, In) ToolResult[Out]) ToolSpec {
	inputSchema := SchemaFor[In]()
	AddHeaderToProperty(inputSchema, "task_number", "Task-Number")
	return ToolSpec{
		Name:         name,
		Title:        title,
		Description:  description,
		InputSchema:  inputSchema,
		OutputSchema: SchemaFor[Out](),
		Handler: func(ctx context.Context, argsJSON json.RawMessage) (RawToolResult, error) {
			args, err := DecodeToolArgument[In](argsJSON)
			if err != nil {
				return RawToolResult{}, ErrInvalidParams("invalid arguments: %w", err)
			}
			return handler(ctx, args).toRawToolResult(), nil
		},
	}
}

// AddHeaderToProperty marks a primitive schema property as mirrored in an MCP parameter header.
func AddHeaderToProperty(schema *jsonschema.Schema, property, header string) {
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

// DecodeToolArgument decodes strict JSON tool arguments into T.
func DecodeToolArgument[T any](data json.RawMessage) (T, error) {
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

// ResourceJSON encodes value as a JSON MCP resource read result.
func ResourceJSON(uri string, value any) (ResourcesReadResult, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ResourcesReadResult{}, err
	}
	return ResourcesReadResult{ResultType: ResultTypeComplete, Contents: []ResourceContent{{URI: uri, MimeType: "application/json", Text: string(data)}}, TTLMS: DefaultTTLMS, CacheScope: CacheScopePrivate}, nil
}

// SchemaFor reflects T into a JSON schema suitable for MCP descriptors.
func SchemaFor[T any]() *jsonschema.Schema {
	r := jsonschema.Reflector{Anonymous: true, DoNotReference: true}
	return r.ReflectFromType(reflect.TypeFor[T]())
}

// TypedToolResult returns a successful structured tool result.
func TypedToolResult[T any](structured T) ToolResult[T] {
	return ToolResult[T]{Structured: structured}
}

// TextToolResult returns a successful text output result.
func TextToolResult(message string) ToolResult[TextOutput] {
	return TypedToolResult(TextOutput{Result: message})
}

// ToolError returns an MCP tool error result.
func ToolError[T any](message string) ToolResult[T] {
	return ToolResult[T]{Structured: ErrorOutput{Error: message}, IsError: true}
}

// ToolAuthError returns an MCP tool error result with a WWW-Authenticate hint.
func ToolAuthError[T any](message, challenge string) ToolResult[T] {
	return ToolResult[T]{Meta: MetaObject{"mcp/www_authenticate": []string{challenge}}, Structured: ErrorOutput{Error: message}, IsError: true}
}

// TextOutput is the standard human-readable successful tool payload.
type TextOutput struct {
	Result string `json:"result" jsonschema_description:"Human-readable tool result"`
}

// ErrorOutput is the standard human-readable tool error payload.
type ErrorOutput struct {
	Error string `json:"error" jsonschema_description:"Human-readable error message"`
}
