// Package mcptest provides shared test doubles for the mcp package's interfaces.
package mcptest

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/invopop/jsonschema"

	"github.com/caic-xyz/caic/backend/internal/mcp"
)

// FakeRegistry is a minimal mcp.Registry: it advertises one "echo" tool and one
// "caic://tasks" resource, enough to exercise protocol envelopes. Set CallErr,
// CallResult, ListErr, or ReadErr to customize a tool or resource response.
type FakeRegistry struct {
	CallErr    error
	CallResult *mcp.RawToolResult
	ListErr    error
	ReadErr    error
}

// Instructions implements mcp.Registry.
func (FakeRegistry) Instructions(context.Context) (string, error) { return "be helpful", nil }

// Tools implements mcp.Registry.
func (FakeRegistry) Tools(context.Context) ([]mcp.ToolDescriptor, error) {
	return []mcp.ToolDescriptor{{
		Name:        "echo",
		Description: "Echo the input back",
		InputSchema: &jsonschema.Schema{Type: "object"},
	}}, nil
}

// CallTool implements mcp.Registry.
func (f FakeRegistry) CallTool(_ context.Context, name string, _ json.RawMessage) (mcp.RawToolResult, error) {
	if f.CallErr != nil {
		return mcp.RawToolResult{}, f.CallErr
	}
	if name != "echo" {
		return mcp.RawToolResult{}, mcp.ErrInvalidParams("unknown tool: %s", name)
	}
	if f.CallResult != nil {
		return *f.CallResult, nil
	}
	return mcp.RawToolResult{Structured: mcp.TextOutput{Result: "ok"}}, nil
}

// ListResources implements mcp.Registry.
func (f FakeRegistry) ListResources(context.Context, string) (mcp.ResourcesListResult, error) {
	if f.ListErr != nil {
		return mcp.ResourcesListResult{}, f.ListErr
	}
	return mcp.ResourcesListResult{
		ResultType: mcp.ResultTypeComplete,
		Resources:  []mcp.ResourceDescriptor{{URI: "caic://tasks", Name: "tasks", MimeType: "application/json"}},
	}, nil
}

// Resources implements mcp.Registry.
func (FakeRegistry) Resources(context.Context) iter.Seq2[mcp.ResourceDescriptor, error] {
	return func(yield func(mcp.ResourceDescriptor, error) bool) {
		yield(mcp.ResourceDescriptor{URI: "caic://tasks", Name: "tasks", MimeType: "application/json"}, nil)
	}
}

// ReadResource implements mcp.Registry.
func (f FakeRegistry) ReadResource(_ context.Context, uri string) (mcp.ResourcesReadResult, error) {
	if f.ReadErr != nil {
		return mcp.ResourcesReadResult{}, f.ReadErr
	}
	return mcp.ResourceJSON(uri, map[string]any{"ok": true})
}

// SubscribeResourceUpdates implements mcp.Registry.
func (FakeRegistry) SubscribeResourceUpdates(context.Context, mcp.SubscriptionFilter) (iter.Seq2[mcp.ResourceUpdate, error], error) {
	return func(func(mcp.ResourceUpdate, error) bool) {}, nil
}

// Ensure the fake satisfies the interface at compile time.
var _ mcp.Registry = FakeRegistry{}
