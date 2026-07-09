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
// "caic://tasks" resource, enough to exercise protocol envelopes. Set CallErr
// or ReadErr to make CallTool or ReadResource return a canned failure.
type FakeRegistry struct {
	CallErr error
	ReadErr error
}

// Ensure the fake satisfies the interface at compile time.
var _ mcp.Registry = FakeRegistry{}

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
	return mcp.RawToolResult{Structured: mcp.TextOutput{Result: "ok"}}, nil
}

// ListResources implements mcp.Registry.
func (FakeRegistry) ListResources(context.Context) mcp.ResourcesListResult {
	return mcp.ResourcesListResult{
		ResultType: mcp.ResultTypeComplete,
		Resources:  []mcp.ResourceDescriptor{{URI: "caic://tasks", Name: "tasks", MimeType: "application/json"}},
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
