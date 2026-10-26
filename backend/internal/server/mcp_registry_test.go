// Tests task-scoped MCP registry catalogs and delegated call validation.

package server

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/invopop/jsonschema"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/metrics"
	"github.com/maruel/gomode/mcp"
)

func TestMCPRegistry(t *testing.T) {
	t.Parallel()
	t.Run("ForTask/catalog", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_schema", func(t *testing.T) {
			t.Parallel()
			registry := &mcpTools{}
			scopedRegistry := (&mcpRegistry{mcpTools: registry}).ForTask(ksid.NewID())
			tools, err := scopedRegistry.Tools(t.Context())
			if err != nil {
				t.Fatalf("Tools() error: %v", err)
			}
			if len(tools) != 6 {
				t.Fatalf("Tools() = %#v, want six task-scoped tools", tools)
			}
			assertMCPToolVisibility(t, tools, "task_create", true)
			assertMCPToolVisibility(t, tools, "tasks_list", true)
			assertMCPToolVisibility(t, tools, "task_fork", true)
			assertMCPToolVisibility(t, tools, "task_stop", true)
			assertMCPToolVisibility(t, tools, "task_purge", true)
			assertMCPToolVisibility(t, tools, "get_system_stats", false)
			assertMCPToolVisibility(t, tools, "task_get_detail", true)
			var create, detail mcp.ToolDescriptor
			for _, tool := range tools {
				if tool.Name == "task_create" {
					create = tool
				}
				if tool.Name == "task_get_detail" {
					detail = tool
				}
			}
			schema, err := json.Marshal(create.InputSchema)
			if err != nil {
				t.Fatalf("marshal delegated schema: %v", err)
			}
			var decoded struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(schema, &decoded); err != nil {
				t.Fatalf("decode delegated schema: %v", err)
			}
			if len(decoded.Properties) != 1 || decoded.Properties["prompt"] == nil {
				t.Fatalf("delegated input schema = %s, want prompt only", schema)
			}
			detailSchema, err := json.Marshal(detail.InputSchema)
			if err != nil {
				t.Fatalf("marshal detail schema: %v", err)
			}
			var detailDecoded mcpToolInputSchema
			if err := json.Unmarshal(detailSchema, &detailDecoded); err != nil {
				t.Fatalf("decode detail schema: %v", err)
			}
			var taskReference jsonschema.Schema
			if err := json.Unmarshal(detailDecoded.Properties["task"], &taskReference); err != nil {
				t.Fatalf("decode task reference schema: %v", err)
			}
			if len(taskReference.OneOf) != 2 || taskReference.OneOf[0].Type != "integer" || taskReference.OneOf[1].Type != "string" {
				t.Fatalf("task detail schema = %s, want a task integer-or-string reference", detailSchema)
			}
			if len(detailDecoded.Properties) != 2 || detailDecoded.Properties["include_diffstat"] == nil {
				t.Fatalf("task detail schema = %s, want task and include_diffstat", detailSchema)
			}

			if !slices.Equal(detailDecoded.Required, []string{"task"}) {
				t.Fatalf("detail required arguments=%v, want only task", detailDecoded.Required)
			}
		})
		t.Run("error_extra_task_properties", func(t *testing.T) {
			t.Parallel()
			registry := &mcpTools{metrics: metrics.NewStore(metrics.Resource{ServiceName: "caic"})}
			scopedRegistry := (&mcpRegistry{mcpTools: registry}).ForTask(ksid.NewID())
			result, err := scopedRegistry.CallTool(t.Context(), &mcp.ToolsCallParams{Name: "task_create", Arguments: json.RawMessage(`{"prompt":"child","repos":["repo"]}`)})
			if err != nil {
				t.Fatalf("CallTool() error: %v", err)
			}
			if !result.IsError {
				t.Fatal("task-scoped create with repos succeeded, want rejection")
			}
		})
	})
}
