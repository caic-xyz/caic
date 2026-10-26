// Tests MCP tool catalogs, authorization, task commands, schemas, and bounded task details.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/invopop/jsonschema"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	logv1 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v1"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/metrics"
	"github.com/maruel/gomode/mcp"
)

type staticUsageFetcher struct {
	quota usage.ProviderQuota
}

func (f *staticUsageFetcher) Provider() agent.QuotaProvider { return f.quota.Provider }

func (f *staticUsageFetcher) Label() string { return f.quota.Label }

func (f *staticUsageFetcher) AuthKind() usage.AuthKind { return f.quota.AuthKind }

func (*staticUsageFetcher) UsageURL() string { return "" }

func (f *staticUsageFetcher) Get(context.Context) *usage.ProviderQuota { return &f.quota }

// schemaHeaders maps each header-mirrored property name to its Mcp-Param-*
// header, failing when a header is attached below the top level.
func schemaHeaders(schema *jsonschema.Schema) (map[string]string, error) {
	params, err := mcp.HeaderParams(schema)
	if err != nil {
		return nil, err
	}
	headers := make(map[string]string, len(params))
	for _, param := range params {
		if len(param.Path) != 1 {
			return nil, fmt.Errorf("header %q path = %v, want one property", param.Header, param.Path)
		}
		headers[param.Path[0]] = param.Header
	}
	return headers, nil
}

func assertMCPToolErrorCode[T any](t *testing.T, result mcp.ToolResult[T], want api.ErrorCode) {
	if !result.IsError {
		t.Fatal("tool result is not an error")
	}
	if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(want) {
		t.Errorf("error code metadata = %q, want %q", got, want)
	}
}

func newMCPTaskCreateTestRouter(t *testing.T) *testRouter {
	s := newTestRouter(t, map[harness.Name]agent.Backend{
		harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "claude-default"}}}},
		harness.Pi:     &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "pi-default"}}}},
	})
	if err := s.taskMgr.Start(s.TaskMCPScoper); err != nil {
		t.Fatalf("Start task manager: %v", err)
	}
	if _, err := s.taskMgr.ImportInstances(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	registerRouterCheckout(t, s.taskMgr.Checkouts, "myrepo", newRouterTestCheckout(t.TempDir()))
	return s
}

func singleCreatedTask(t *testing.T, s *testRouter) *task.Task {
	entries := testEntries(s)
	if len(entries) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(entries))
	}
	return entries[0].Task()
}

func assertMCPToolVisibility(t *testing.T, tools []mcp.ToolDescriptor, name string, want bool) {
	for _, tool := range tools {
		if tool.Name == name {
			if !want {
				t.Fatalf("tool %q is visible, want hidden", name)
			}
			return
		}
	}
	if want {
		t.Fatalf("tool %q is hidden, want visible", name)
	}
}

// mcpToolInputSchema decodes the input contract exercised by tool schema tests.
type mcpToolInputSchema struct {
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

func newMCPDiffstatRouter(t *testing.T) (*testRouter, *runtimetest.RepositorySpy, ksid.ID) {
	s, rt, id := newTaskDiffTestRouter(t)
	spy := &runtimetest.RepositorySpy{FakeBackend: rt.FakeBackend}
	testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, spy)
	return s, spy, id
}

func taskDetailText(t *testing.T, v any) string {
	out, ok := v.(mcp.TextOutput)
	if !ok {
		t.Fatalf("result type=%T, want mcp.TextOutput", v)
	}
	return out.Result
}

func callTaskDetail(t *testing.T, reg mcp.Registry, a mcpTaskGetDetailArgs) mcp.RawToolResult {
	buf, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.CallTool(t.Context(), &mcp.ToolsCallParams{Name: "task_get_detail", Arguments: buf})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

type mcpTaskDetailWireResponse struct {
	Result mcpTaskDetailWireResult `json:"result"`
	Error  json.RawMessage         `json:"error"`
}

type mcpTaskDetailWireResult struct {
	Meta              mcp.MetaObject     `json:"_meta"`
	IsError           bool               `json:"isError"`
	Content           []mcp.ContentBlock `json:"content"`
	StructuredContent mcp.TextOutput     `json:"structuredContent"`
}

func taskDetailWire(t *testing.T, s *testRouter, a mcpTaskGetDetailArgs) mcpTaskDetailWireResponse {
	buf, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	txt := mcpRequestJSON("tools/call", fmt.Sprintf(`"name":"task_get_detail","arguments":%s`, buf))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(txt))
	r.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
	r.Header.Set("Mcp-Method", "tools/call")
	r.Header.Set("Mcp-Name", "task_get_detail")
	w := httptest.NewRecorder()
	s.mcpHandlers.handleMCP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP status=%d, want 200", w.Code)
	}
	var got mcpTaskDetailWireResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Error) != 0 {
		t.Fatalf("unexpected protocol error: %s", got.Error)
	}
	return got
}

func TestMCPTools(t *testing.T) {
	t.Parallel()
	t.Run("handleGetSystemStats", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			snapshot v1.SystemStatsResp
		}{
			{name: "warming_up", snapshot: v1.SystemStatsResp{Samples: []v1.SystemSample{}}},
			{name: "ready", snapshot: v1.SystemStatsResp{Samples: []v1.SystemSample{{Time: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 25}}, MemUsed: 512, MemTotal: 1024, MemPerc: 50, NetRx: 200, NetTx: 100, DiskUsed: 2048, DiskTotal: 4096}}}},
			{name: "sampling_warning", snapshot: v1.SystemStatsResp{Samples: []v1.SystemSample{{CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 25}}}}, Error: "memory unavailable"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				// Only the sampler is needed; no HTTP server, UI, runtime, or tasks.
				c := &mcpTools{serverConfig: &serverHandlers{systemStats: &systemStats{samples: tc.snapshot.Samples, err: tc.snapshot.Error}}, metrics: metrics.NewStore(metrics.Resource{ServiceName: "caic"})}
				for _, caller := range []struct {
					name string
					ctx  context.Context
				}{
					{"admin_scope", newMCPPrincipalContext(auth.NewContext(t.Context(), &auth.User{ID: "user-1"}), &mcpPrincipal{Remote: true, Scopes: []string{mcpScopeServerAdmin}})},
					{"browser", auth.NewContext(t.Context(), &auth.User{ID: "user-1"})},
				} {
					t.Run(caller.name, func(t *testing.T) {
						t.Parallel()
						result, err := c.CallTool(caller.ctx, &mcp.ToolsCallParams{Name: "get_system_stats", Arguments: json.RawMessage(`{}`)})
						if err != nil || result.IsError {
							t.Fatalf("CallTool: %+v, %v", result, err)
						}
						got, ok := result.Structured.(v1.SystemStatsResp)
						if !ok || !reflect.DeepEqual(got, tc.snapshot) {
							t.Fatalf("snapshot = %#v, want %#v", result.Structured, tc.snapshot)
						}
					})
				}
			})
		}
		t.Run("read_only_catalog", func(t *testing.T) {
			t.Parallel()
			c := &mcpTools{}
			tools, err := c.Tools(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range tools {
				if tool.Name != "get_system_stats" {
					continue
				}
				if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint || tool.Annotations.DestructiveHint || tool.Annotations.OpenWorldHint {
					t.Fatalf("annotations: %+v", tool.Annotations)
				}
				if tool.OutputSchema == nil || tool.InputSchema == nil || tool.InputSchema.Type != "object" {
					t.Fatalf("missing system stats schemas: %+v", tool)
				}
				return
			}
			t.Fatal("get_system_stats is not advertised")
		})
		t.Run("scope_denied", func(t *testing.T) {
			t.Parallel()
			c := &mcpTools{}
			for _, p := range []mcpPrincipal{
				{Remote: true, Scopes: []string{mcpScopeRead}},
				{Remote: true, Scopes: []string{mcpScopeTasksRead}},
				{Remote: true, TaskID: ksid.NewID(), Scopes: []string{mcpScopeServerAdmin}},
			} {
				ctx := newMCPPrincipalContext(auth.NewContext(t.Context(), &auth.User{ID: "user-1"}), &p)
				result, err := c.CallTool(ctx, &mcp.ToolsCallParams{Name: "get_system_stats", Arguments: json.RawMessage(`{}`)})
				if err != nil || !result.IsError {
					t.Fatalf("CallTool for %+v: %+v, %v", p, result, err)
				}
				if result.Meta[mcp.ToolErrorCodeMetaKey] != string(api.CodeUnauthorized) {
					t.Fatalf("error metadata: %+v", result.Meta)
				}
				tools, err := c.Tools(ctx)
				if err != nil {
					t.Fatal(err)
				}
				assertMCPToolVisibility(t, tools, "get_system_stats", false)
			}
		})
	})
	t.Run("handleGetUsage", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		s.usageHandlers.fetchers = []usage.ProviderFetcher{&staticUsageFetcher{quota: usage.ProviderQuota{
			Provider: agent.QuotaProviderAnthropic,
			Label:    "Anthropic",
			AuthKind: usage.AuthKindOAuth,
			RateLimits: []usage.QuotaRateLimit{
				{Window: "5h", Utilization: 0.12, ResetsAt: time.Date(2099, 10, 19, 15, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))},
				{Window: "7d", Utilization: 0.25, ResetsAt: time.Date(2099, 10, 25, 0, 0, 0, 0, time.UTC)},
			},
		}}, &staticUsageFetcher{quota: usage.ProviderQuota{
			Provider: agent.QuotaProviderCodex,
			Label:    "OpenAI",
			AuthKind: usage.AuthKindOAuth,
			RateLimits: []usage.QuotaRateLimit{
				{Window: "5h", Utilization: 1},
			},
		}}}
		c := &mcpTools{usage: s.usageHandlers}
		result := c.handleGetUsage(t.Context(), &mcp.ToolRequest[struct{}]{Arguments: struct{}{}})
		output, ok := result.Structured.(mcp.TextOutput)
		if !ok {
			t.Fatalf("get_usage result type = %T, want mcp.TextOutput", result.Structured)
		}
		for _, want := range []string{
			"Anthropic: 5h: 88% remaining (resets at 2099-10-19T13:30:00+00:00), 7d: 75% remaining (resets at 2099-10-25T00:00:00+00:00)",
			"OpenAI: 5h: 0% remaining",
		} {
			if !slices.Contains(strings.Split(output.Result, "\n"), want) {
				t.Errorf("get_usage result = %q, want %q", output.Result, want)
			}
		}
	})
	t.Run("handleGetUsage/timezone", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, location, winter, summer string
		}{
			{"Toronto", `{"timezone":"America/Toronto"}`, "2099-01-19T08:30:00-05:00", "2099-07-19T09:30:00-04:00"},
			{"Kolkata", `{"timezone":"Asia/Kolkata"}`, "2099-01-19T19:00:00+05:30", "2099-07-19T19:00:00+05:30"},
			{"UTC", `{"timezone":"UTC"}`, "", ""},
			{"missing", "", "", ""},
			{"null", `null`, "", ""},
			{"missing timezone", `{}`, "", ""},
			{"null timezone", `{"timezone":null}`, "", ""},
			{"empty timezone", `{"timezone":""}`, "", ""},
			{"invalid timezone", `{"timezone":"Invalid/Zone"}`, "", ""},
			{"server local", `{"timezone":"Local"}`, "", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				for _, native := range []bool{false, true} {
					t.Run(fmt.Sprintf("native=%t", native), func(t *testing.T) {
						t.Parallel()
						s := newTestRouter(t, nil)
						s.usageHandlers.fetchers = []usage.ProviderFetcher{&staticUsageFetcher{quota: usage.ProviderQuota{
							Provider: agent.QuotaProviderCodex,
							Label:    "OpenAI",
							AuthKind: usage.AuthKindOAuth,
							Balance:  usage.QuotaBalance{Currency: "USD", Total: 12.5},
							RateLimits: []usage.QuotaRateLimit{
								{Window: "winter", Utilization: 0.12, ResetsAt: time.Date(2099, 1, 19, 13, 30, 0, 0, time.UTC)},
								{Window: "summer", Utilization: 0.25, ResetsAt: time.Date(2099, 7, 19, 13, 30, 0, 0, time.UTC)},
								{Window: "unknown", Utilization: 1},
							},
						}}}
						body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_usage","arguments":{}`
						if tc.location != "" {
							body += `,"_meta":{"openai/userLocation":` + tc.location + `}`
						}
						body += `}}`
						if native {
							body = mcpRequestJSON("tools/call", `"name":"get_usage","arguments":{}`)
							if tc.location != "" {
								body = strings.Replace(body, `"_meta":{`, `"_meta":{"openai/userLocation":`+tc.location+`,`, 1)
							}
						}
						req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(body))
						if native {
							req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
							req.Header.Set("Mcp-Method", "tools/call")
							req.Header.Set("Mcp-Name", "get_usage")
						}
						w := httptest.NewRecorder()
						s.mcpHandlers.handleMCP(w, req)
						var response struct {
							Result struct {
								StructuredContent mcp.TextOutput `json:"structuredContent"`
							} `json:"result"`
						}
						if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
							t.Fatal(err)
						}
						winter, summer := tc.winter, tc.summer
						if winter == "" {
							winter, summer = "2099-01-19T13:30:00+00:00", "2099-07-19T13:30:00+00:00"
						}
						want := "OpenAI: $12.50, winter: 88% remaining (resets at " + winter + "), summer: 75% remaining (resets at " + summer + "), unknown: 0% remaining"
						if got := response.Result.StructuredContent.Result; !slices.Contains(strings.Split(got, "\n"), want) {
							t.Fatalf("response = %s; want line %q", w.Body.String(), want)
						}
					})
				}
			})
		}
	})

	t.Run("specs/taskNumberSchema", func(t *testing.T) {
		t.Parallel()

		registry := &mcpTools{}
		tools, err := registry.Tools(t.Context())
		if err != nil {
			t.Fatalf("Tools() error: %v", err)
		}
		for _, tool := range tools {
			if tool.Name != "task_stop" && tool.Name != "task_fork" {
				continue
			}
			t.Run(tool.Name, func(t *testing.T) {
				t.Parallel()

				data, err := json.Marshal(tool.InputSchema)
				if err != nil {
					t.Fatalf("marshal schema: %v", err)
				}
				var decoded struct {
					Properties map[string]struct {
						OneOf []struct {
							Type string `json:"type"`
						} `json:"oneOf"`
						Header string `json:"x-mcp-header"`
					} `json:"properties"`
				}
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatalf("decode schema: %v", err)
				}
				property, ok := decoded.Properties["task_number"]
				if !ok {
					t.Fatalf("task_number property missing: %s", data)
				}
				want := []string{"integer", "string"}
				got := make([]string, 0, len(property.OneOf))
				for _, branch := range property.OneOf {
					got = append(got, branch.Type)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("task_number schema = %s, want integer-or-string", data)
				}
				if property.Header != "Task-Number" {
					t.Fatalf("task_number header = %q, want Task-Number", property.Header)
				}
			})
		}
	})
	t.Run("specs/schemas", func(t *testing.T) {
		t.Parallel()

		registry := &mcpTools{}
		if err := registry.validateToolSchemas(); err != nil {
			t.Fatalf("validateToolSchemas() error: %v", err)
		}

		expectedHeaders := map[string]map[string]string{
			"task_stop":                  {"task_number": "Task-Number"},
			"task_purge":                 {"task_number": "Task-Number"},
			"task_revive":                {"task_number": "Task-Number"},
			"task_fork":                  {"task_number": "Task-Number"},
			"task_send_message":          {"task_number": "Task-Number"},
			"task_answer_question":       {"task_number": "Task-Number"},
			"task_push_branch_to_remote": {"task_number": "Task-Number"},
			"task_fix_pr":                {"task_number": "Task-Number"},
			"agent_last_message":         {"task_number": "Task-Number"},
			"bot_fix_ci":                 {"repo": "Repo"},
		}
		for _, spec := range registry.specs() {
			t.Run(spec.Name, func(t *testing.T) {
				t.Parallel()

				got, err := schemaHeaders(spec.InputSchema)
				if err != nil {
					t.Fatalf("HeaderParams() error: %v", err)
				}
				want := expectedHeaders[spec.Name]
				if len(got) != len(want) {
					t.Fatalf("headers = %v, want %v", got, want)
				}
				for property, header := range want {
					if got[property] != header {
						t.Fatalf("header for %q = %q, want %q", property, got[property], header)
					}
				}
				if spec.Name == "task_create" {
					if slices.Contains(spec.InputSchema.Required, "repos") {
						t.Fatal("repos must be optional")
					}
					repos, ok := spec.InputSchema.Properties.Get("repos")
					if !ok || repos.Type != "array" || repos.Items == nil || repos.Items.Type != "string" {
						t.Fatal("repos must be an array of repository paths")
					}
					if repos.MinItems != nil && *repos.MinItems > 0 {
						t.Fatal("repos must allow an empty array")
					}
				}
			})
		}

		t.Run("delegated schemas", func(t *testing.T) {
			t.Parallel()

			if err := mcp.ValidateToolSchema(buildDelegatedTaskCreateSchema()); err != nil {
				t.Fatalf("task_create delegated schema: %v", err)
			}
			if err := mcp.ValidateToolSchema(buildDelegatedTaskForkSchema()); err != nil {
				t.Fatalf("task_fork delegated schema: %v", err)
			}
			got, err := schemaHeaders(buildDelegatedTaskForkSchema())
			if err != nil {
				t.Fatalf("HeaderParams() error: %v", err)
			}
			if len(got) != 1 || got["task_number"] != "Task-Number" {
				t.Fatalf("delegated fork headers = %v, want task_number Task-Number", got)
			}
		})
	})
	t.Run("handleReposList", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		registerRouterCheckout(t, s.checkouts, "org/repo", newRouterTestCheckout(t.TempDir()))
		registerRouterCheckout(t, s.checkouts, "repo2", &repo.Checkout{BaseBranch: "develop", Dir: t.TempDir()})
		c := &mcpTools{serverConfig: s.serverHandlers}

		result := c.handleReposList(t.Context(), &mcp.ToolRequest[mcpRepoListArgs]{Arguments: mcpRepoListArgs{}})
		if result.IsError {
			t.Fatalf("handleReposList() returned tool error: %+v", result.Structured)
		}
		output, ok := result.Structured.(mcpRepoListOutput)
		if !ok {
			t.Fatalf("result type = %T, want mcpRepoListOutput", result.Structured)
		}
		got := output.Repositories
		if len(got) != 2 {
			t.Fatalf("repositories = %+v, want 2 repositories", got)
		}
		if got[0].Path != "org/repo" || got[0].BaseBranch.Name != "main" {
			t.Errorf("repositories[0] = %+v, want org/repo on main", got[0])
		}
		if got[1].Path != "repo2" || got[1].BaseBranch.Name != "develop" {
			t.Errorf("repositories[1] = %+v, want repo2 on develop", got[1])
		}
		invalid := c.handleReposList(t.Context(), &mcp.ToolRequest[mcpRepoListArgs]{Arguments: mcpRepoListArgs{Limit: mcpRepoPageSizeMax + 1}})
		if !invalid.IsError || invalid.Meta[mcp.ToolErrorCodeMetaKey] != string(api.CodeBadRequest) {
			t.Fatalf("invalid limit result = %#v, meta = %#v, want bad request", invalid.Structured, invalid.Meta)
		}
	})
	t.Run("handleHarnessesList", func(t *testing.T) {
		t.Parallel()

		s := newMCPTaskCreateTestRouter(t)
		c := &mcpTools{serverConfig: s.serverHandlers}

		result := c.handleHarnessesList(t.Context(), &mcp.ToolRequest[struct{}]{Arguments: struct{}{}})
		if result.IsError {
			t.Fatalf("handleHarnessesList() returned tool error: %+v", result.Structured)
		}
		output, ok := result.Structured.(mcpHarnessListOutput)
		if !ok {
			t.Fatalf("result type = %T, want mcpHarnessListOutput", result.Structured)
		}
		if len(output.Harnesses) != 2 {
			t.Fatalf("harnesses = %+v, want claude and pi", output.Harnesses)
		}
		if output.Harnesses[0].Name != v1.HarnessClaude || output.Harnesses[1].Name != v1.HarnessPi {
			t.Errorf("harness names = %q, %q, want claude, pi", output.Harnesses[0].Name, output.Harnesses[1].Name)
		}
		if models := output.Harnesses[0].Models; len(models) != 1 || models[0].ID != "claude-default" {
			t.Errorf("claude models = %+v, want claude-default", models)
		}
		if models := output.Harnesses[1].Models; len(models) != 1 || models[0].ID != "pi-default" {
			t.Errorf("pi models = %+v, want pi-default", models)
		}
	})
	t.Run("handleTasksList", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		stoppedID := ksid.NewID()
		stopped := mustNewTask(t, stoppedID, agent.Prompt{Text: "stopped"}, harness.Claude)
		stopped.SetState(taskslog.StateStopped)
		insertTestTask(s, stoppedID, stopped)
		runningID := ksid.NewID()
		running := mustNewTask(t, runningID, agent.Prompt{Text: "running"}, harness.Codex)
		running.SetState(taskslog.StateRunning)
		insertTestTask(s, runningID, running)
		registry := &mcpTools{taskSvc: testTaskHandlers(s).taskSvc}

		first := registry.handleTasksList(t.Context(), &mcp.ToolRequest[mcpTaskListArgs]{Arguments: mcpTaskListArgs{Limit: 1}})
		firstOutput, ok := first.Structured.(mcpTaskListOutput)
		if first.IsError || !ok {
			t.Fatalf("first page = %#v, want task-list output", first.Structured)
		}
		if len(firstOutput.Tasks) != 1 || firstOutput.Tasks[0].TaskNumber != 1 || firstOutput.Tasks[0].State != v1.TaskStateRunning || firstOutput.NextCursor == "" {
			t.Fatalf("first page = %#v, want running task and a next cursor", firstOutput)
		}

		second := registry.handleTasksList(t.Context(), &mcp.ToolRequest[mcpTaskListArgs]{Arguments: mcpTaskListArgs{Cursor: firstOutput.NextCursor, Limit: 1}})
		secondOutput, ok := second.Structured.(mcpTaskListOutput)
		if second.IsError || !ok {
			t.Fatalf("second page = %#v, want task-list output", second.Structured)
		}
		if len(secondOutput.Tasks) != 1 || secondOutput.Tasks[0].TaskNumber != 2 || secondOutput.Tasks[0].State != v1.TaskStateStopped || secondOutput.NextCursor != "" {
			t.Fatalf("second page = %#v, want stopped task without cursor", secondOutput)
		}
		stopped.SetState(taskslog.StateRunning)
		stale := registry.handleTasksList(t.Context(), &mcp.ToolRequest[mcpTaskListArgs]{Arguments: mcpTaskListArgs{Cursor: firstOutput.NextCursor, Limit: 1}})
		if !stale.IsError {
			t.Fatal("tasks_list accepted a cursor after another task changed ordering partition")
		}

		for name, args := range map[string]mcpTaskListArgs{
			"cursor": {Cursor: "invalid"},
			"limit":  {Limit: mcpTaskPageSizeMax + 1},
		} {
			t.Run("invalid "+name, func(t *testing.T) {
				t.Parallel()
				result := registry.handleTasksList(t.Context(), &mcp.ToolRequest[mcpTaskListArgs]{Arguments: args})
				if !result.IsError || result.Meta[mcp.ToolErrorCodeMetaKey] != string(api.CodeBadRequest) {
					t.Fatalf("result = %#v, meta = %#v, want bad-request tool error", result.Structured, result.Meta)
				}
			})
		}

		t.Run("task scoped list contains only direct children", func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			registry := &mcpTools{taskSvc: testTaskHandlers(s).taskSvc}
			parentID := ksid.NewID()
			firstChildID := ksid.NewID()
			firstChild := mustNewTask(t, firstChildID, agent.Prompt{Text: "first child"}, harness.Claude)
			firstChild.ParentTaskID = parentID
			insertTestTask(s, firstChildID, firstChild)
			secondChildID := ksid.NewID()
			secondChild := mustNewTask(t, secondChildID, agent.Prompt{Text: "second child"}, harness.Codex)
			secondChild.ParentTaskID = parentID
			insertTestTask(s, secondChildID, secondChild)
			unrelatedID := ksid.NewID()
			unrelated := mustNewTask(t, unrelatedID, agent.Prompt{Text: "unrelated"}, harness.Claude)
			insertTestTask(s, unrelatedID, unrelated)
			grandchildID := ksid.NewID()
			grandchild := mustNewTask(t, grandchildID, agent.Prompt{Text: "grandchild"}, harness.Codex)
			grandchild.ParentTaskID = firstChildID
			insertTestTask(s, grandchildID, grandchild)

			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{TaskID: parentID, Remote: true})
			result := registry.handleTasksList(ctx, &mcp.ToolRequest[mcpTaskListArgs]{Arguments: mcpTaskListArgs{}})
			output, ok := result.Structured.(mcpTaskListOutput)
			if result.IsError || !ok {
				t.Fatalf("task-scoped list = %#v, want task-list output", result.Structured)
			}
			if len(output.Tasks) != 2 {
				t.Fatalf("task-scoped tasks = %#v, want two direct children", output.Tasks)
			}
			got := map[string]struct{}{}
			for _, summary := range output.Tasks {
				got[summary.TaskID] = struct{}{}
			}
			want := map[string]struct{}{firstChildID.String(): {}, secondChildID.String(): {}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("task-scoped task IDs = %#v, want %#v", got, want)
			}
			detail := registry.handleTaskGetDetail(ctx, &mcp.ToolRequest[mcpTaskGetDetailArgs]{Arguments: mcpTaskGetDetailArgs{Task: taskID(strconv.Itoa(output.Tasks[0].TaskNumber))}})
			if detail.IsError {
				t.Fatalf("task detail = %#v, want listed child detail", detail.Structured)
			}
			detailOutput, ok := detail.Structured.(mcp.TextOutput)
			if !ok || !strings.Contains(detailOutput.Result, output.Tasks[0].Title) {
				t.Fatalf("task detail = %#v, want listed child %q", detail.Structured, output.Tasks[0].Title)
			}
			resources := &mcpResources{taskSvc: registry.taskSvc}
			sources, err := resources.subscriptionSources(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"caic://tasks/" + firstChildID.String()}})
			if err != nil {
				t.Fatalf("child subscription error: %v", err)
			}
			sources.taskResourceIDs = append(sources.taskResourceIDs, unrelatedID)
			update := sources.taskUpdate(ctx, resources)
			wantUpdateURIs := []string{"caic://tasks/" + firstChildID.String()}
			if !reflect.DeepEqual(update.ResourceURIs, wantUpdateURIs) {
				t.Fatalf("task subscription update URIs = %#v, want %#v", update.ResourceURIs, wantUpdateURIs)
			}
			if !update.ResourcesListChanged {
				t.Fatal("task subscription update did not report inaccessible task resource removal")
			}
			if _, err := resources.subscriptionSources(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"caic://tasks/" + unrelatedID.String()}}); err == nil {
				t.Fatal("unrelated task subscription succeeded")
			}
		})

		t.Run("task scoped controls resolve children and its own fork source", func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			registry := &mcpTools{taskSvc: testTaskHandlers(s).taskSvc}
			parentID := ksid.NewID()
			parent := mustNewTask(t, parentID, agent.Prompt{Text: "parent"}, harness.Claude)
			parent.SetState(taskslog.StateWaiting)
			insertTestTask(s, parentID, parent)
			childID := ksid.NewID()
			child := mustNewTask(t, childID, agent.Prompt{Text: "child"}, harness.Codex)
			child.ParentTaskID = parentID
			child.SetState(taskslog.StateWaiting)
			insertTestTask(s, childID, child)
			unrelatedID := ksid.NewID()
			unrelated := mustNewTask(t, unrelatedID, agent.Prompt{Text: "unrelated"}, harness.Claude)
			unrelated.SetState(taskslog.StateWaiting)
			insertTestTask(s, unrelatedID, unrelated)

			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{TaskID: parentID, Remote: true})
			_, self, err := registry.entryByTaskRef(ctx, taskID("0"), true)
			if err != nil || self.Task().ID != parentID {
				t.Fatalf("self fork source = %#v, %v; want parent", self, err)
			}
			_, childEntry, err := registry.entryByTaskRef(ctx, taskID("1"), false)
			if err != nil || childEntry.Task().ID != childID {
				t.Fatalf("child control target = %#v, %v; want child", childEntry, err)
			}
			_, hashtagEntry, err := registry.entryByTaskRef(ctx, taskID("#1"), false)
			if err != nil || hashtagEntry.Task().ID != childID {
				t.Fatalf("hashtag control target = %#v, %v; want child", hashtagEntry, err)
			}
			_, stableEntry, err := registry.entryByTaskRef(ctx, taskID(childID.String()), false)
			if err != nil || stableEntry.Task().ID != childID {
				t.Fatalf("stable control target = %#v, %v; want child", stableEntry, err)
			}
			if _, _, err := registry.entryByTaskRef(ctx, taskID("0"), false); err == nil {
				t.Fatal("task-scoped stop resolved the calling task")
			}
			if _, _, err := registry.entryByTaskRef(ctx, taskID("2"), false); err == nil {
				t.Fatal("task-scoped control resolved an unrelated task")
			}
		})

		t.Run("task scoped detail resolves an unrelated task from the global task list", func(t *testing.T) {
			t.Parallel()

			s := newTestRouter(t, nil)
			registry := &mcpTools{
				taskSvc: testTaskHandlers(s).taskSvc,
				metrics: metrics.NewStore(metrics.Resource{ServiceName: "caic"}),
			}
			callerID := ksid.NewID()
			caller := mustNewTask(t, callerID, agent.Prompt{Text: "caller"}, harness.Codex)
			insertTestTask(s, callerID, caller)
			targetID := ksid.NewID()
			target := mustNewTask(t, targetID, agent.Prompt{Text: "unrelated"}, harness.Claude)
			target.ParentTaskID = ksid.NewID()
			insertTestTask(s, targetID, target)

			result, err := (&mcpRegistry{mcpTools: registry}).ForTask(callerID).CallTool(t.Context(), &mcp.ToolsCallParams{Name: "task_get_detail", Arguments: json.RawMessage(fmt.Sprintf(`{"task":%q}`, targetID.String()))})
			if err != nil {
				t.Fatalf("CallTool() error: %v", err)
			}
			if result.IsError {
				t.Fatalf("CallTool() result = %#v, want unrelated task detail", result.Structured)
			}
			output, ok := result.Structured.(mcp.TextOutput)
			if !ok {
				t.Fatalf("CallTool() result type = %T, want mcp.TextOutput", result.Structured)
			}
			if !strings.Contains(output.Result, "unrelated") {
				t.Fatalf("task detail = %q, want unrelated task", output.Result)
			}
		})
	})
	t.Run("handleTaskCreate", func(t *testing.T) {
		t.Parallel()

		t.Run("repository lists", func(t *testing.T) {
			t.Parallel()

			for _, tc := range []struct {
				name string
				args string
				want []string
			}{
				{name: "omitted", args: `{"prompt":"do the task"}`},
				{name: "empty", args: `{"prompt":"do the task","repos":[]}`},
				{name: "one", args: `{"prompt":"do the task","repos":["myrepo"]}`, want: []string{"myrepo"}},
				{name: "many", args: `{"prompt":"do the task","repos":["otherrepo","myrepo"]}`, want: []string{"otherrepo", "myrepo"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()

					s := newMCPTaskCreateTestRouter(t)
					registerRouterCheckout(t, s.taskMgr.Checkouts, "otherrepo", newRouterTestCheckout(t.TempDir()))
					registry := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc, metrics: metrics.Nop{}}
					result, err := registry.CallTool(t.Context(), &mcp.ToolsCallParams{Name: "task_create", Arguments: json.RawMessage(tc.args)})
					if err != nil {
						t.Fatal(err)
					}
					if result.IsError {
						t.Fatalf("task_create returned tool error: %+v", result.Structured)
					}
					created := singleCreatedTask(t, s)
					got := make([]string, 0, len(created.Repos))
					for _, repo := range created.Repos {
						got = append(got, repo.Name)
					}
					if !slices.Equal(got, tc.want) {
						t.Fatalf("created repositories = %v, want %v", got, tc.want)
					}
				})
			}
		})

		t.Run("enables CAIC MCP by default", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			registry := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			result := registry.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{Prompt: "delegate the review", Repos: []string{"myrepo"}}})
			if result.IsError {
				t.Fatalf("handleTaskCreate() returned tool error: %+v", result.Structured)
			}
			if !singleCreatedTask(t, s).CaicMCP {
				t.Fatal("MCP-created task CaicMCP = false, want true")
			}
		})

		t.Run("bounds generated title", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			registry := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			result := registry.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{
				Prompt: strings.Repeat("€", maxMCPTaskTitle),
				Repos:  []string{"myrepo"},
			}})
			output, ok := result.Structured.(mcpTaskCreatedOutput)
			if result.IsError || !ok {
				t.Fatalf("handleTaskCreate() result = %#v", result)
			}
			const prefix = "Created task #1: "
			if !strings.HasPrefix(output.Result, prefix) || len(strings.TrimPrefix(output.Result, prefix)) > maxMCPTaskTitle {
				t.Fatalf("created task result length = %d", len(output.Result))
			}
			if !strings.HasSuffix(output.Result, "…") || !utf8.ValidString(output.Result) {
				t.Fatalf("created task result = %q, valid UTF-8 = %t", output.Result, utf8.ValidString(output.Result))
			}
		})

		t.Run("non default harness leaves omitted model and effort unset", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			if err := s.prefs.Update(userIDFromCtx(t.Context()), func(p *preferences.Preferences) {
				p.Harness = string(harness.Claude)
				p.Models = map[string]string{
					string(harness.Claude): "claude-default",
					string(harness.Pi):     "pi-default",
				}
				p.Efforts = preferences.EffortPreferences{
					string(harness.Claude): {"claude-default": "max"},
					string(harness.Pi):     {"pi-default": "high"},
				}
			}); err != nil {
				t.Fatalf("Update preferences: %v", err)
			}
			c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}

			result := c.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{
				Prompt:  "do the task",
				Repos:   []string{"myrepo"},
				Harness: string(harness.Pi),
			}})
			if result.IsError {
				t.Fatalf("handleTaskCreate() returned tool error: %+v", result.Structured)
			}

			created := singleCreatedTask(t, s)
			if created.Harness != harness.Pi {
				t.Fatalf("created harness = %q, want %q", created.Harness, harness.Pi)
			}
			if got := created.RequestedModel; got != "" {
				t.Fatalf("created model = %q, want empty", got)
			}
			if created.RequestedEffort != "" {
				t.Fatalf("created effort = %q, want empty", created.RequestedEffort)
			}
			prefs := s.prefs.Get(userIDFromCtx(t.Context()))
			if model, ok := prefs.Models[string(harness.Pi)]; !ok || model != "" {
				t.Fatalf("preferences model = %q, present = %v, want empty and present", model, ok)
			}
			if effort, ok := prefs.Efforts[string(harness.Pi)][""]; !ok || effort != "" {
				t.Fatalf("preferences effort = %q, present = %v, want empty and present", effort, ok)
			}
		})

		t.Run("omitted harness uses default preferences", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			if err := s.prefs.Update(userIDFromCtx(t.Context()), func(p *preferences.Preferences) {
				p.Harness = string(harness.Pi)
				p.Models = map[string]string{string(harness.Pi): "pi-default"}
				p.Efforts = preferences.EffortPreferences{string(harness.Pi): {"pi-default": "high"}}
			}); err != nil {
				t.Fatalf("Update preferences: %v", err)
			}
			c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}

			result := c.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{
				Prompt: "do the task",
				Repos:  []string{"myrepo"},
			}})
			if result.IsError {
				t.Fatalf("handleTaskCreate() returned tool error: %+v", result.Structured)
			}

			created := singleCreatedTask(t, s)
			if created.Harness != harness.Pi {
				t.Fatalf("created harness = %q, want %q", created.Harness, harness.Pi)
			}
			if got := created.RequestedModel; got != "" {
				t.Fatalf("created model = %q, want empty", got)
			}
			if created.RequestedEffort != "" {
				t.Fatalf("created effort = %q, want empty", created.RequestedEffort)
			}
		})

		t.Run("tool handler reads preferences at call time", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			if err := s.prefs.Update(userIDFromCtx(t.Context()), func(p *preferences.Preferences) {
				p.Harness = string(harness.Pi)
				p.Models = map[string]string{string(harness.Pi): "pi-default"}
				p.Efforts = preferences.EffortPreferences{string(harness.Pi): {"pi-default": "high"}}
			}); err != nil {
				t.Fatalf("Update preferences: %v", err)
			}

			var found bool
			for _, spec := range c.specs() {
				if spec.Name != "task_create" {
					continue
				}
				found = true
				raw, err := spec.Handler(t.Context(), &mcp.ToolsCallParams{Arguments: json.RawMessage(`{"prompt":"do the task","repos":["myrepo"]}`)})
				if err != nil {
					t.Fatalf("task_create handler returned error: %v", err)
				}
				if raw.IsError {
					t.Fatalf("task_create returned tool error: %+v", raw.Structured)
				}
				break
			}
			if !found {
				t.Fatal("task_create tool not found")
			}

			created := singleCreatedTask(t, s)
			if created.Harness != harness.Pi {
				t.Fatalf("created harness = %q, want %q", created.Harness, harness.Pi)
			}
			if got := created.RequestedModel; got != "" {
				t.Fatalf("created model = %q, want empty", got)
			}
			if created.RequestedEffort != "" {
				t.Fatalf("created effort = %q, want empty", created.RequestedEffort)
			}
		})

		t.Run("non default harness without preference omits default harness model and effort", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			if err := s.prefs.Update(userIDFromCtx(t.Context()), func(p *preferences.Preferences) {
				p.Harness = string(harness.Claude)
				p.Models = map[string]string{string(harness.Claude): "claude-default"}
				p.Efforts = preferences.EffortPreferences{string(harness.Claude): {"claude-default": "max"}}
			}); err != nil {
				t.Fatalf("Update preferences: %v", err)
			}
			c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}

			result := c.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{
				Prompt:  "do the task",
				Repos:   []string{"myrepo"},
				Harness: string(harness.Pi),
			}})
			if result.IsError {
				t.Fatalf("handleTaskCreate() returned tool error: %+v", result.Structured)
			}

			created := singleCreatedTask(t, s)
			if created.Harness != harness.Pi {
				t.Fatalf("created harness = %q, want %q", created.Harness, harness.Pi)
			}
			if got := created.RequestedModel; got != "" {
				t.Fatalf("created model = %q, want empty", got)
			}
			if created.RequestedEffort != "" {
				t.Fatalf("created effort = %q, want empty", created.RequestedEffort)
			}
		})
	})
	t.Run("handleTaskCreate/unknownRepository", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			scopes []string
			want   string
		}{
			{
				name:   "with repository read access",
				scopes: []string{mcpScopeRead, mcpScopeTasksCreate},
				want:   "unknown repo: mistyped. Call repos_list, use an exact returned path, then retry task_create.",
			},
			{
				name:   "without repository read access",
				scopes: []string{mcpScopeTasksCreate},
				want:   "unknown repo: mistyped. The path must exactly match a configured repository.",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				s := newMCPTaskCreateTestRouter(t)
				c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
				ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: tt.scopes, Remote: true})
				ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})

				result := c.handleTaskCreate(ctx, &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{Prompt: "do the task", Repos: []string{"mistyped"}}})
				if !result.IsError {
					t.Fatal("handleTaskCreate() did not return a tool error")
				}
				output, ok := result.Structured.(mcp.ErrorOutput)
				if !ok {
					t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
				}
				if output.Error != tt.want {
					t.Errorf("error = %q, want %q", output.Error, tt.want)
				}
				if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeUnknownRepository) {
					t.Errorf("error code metadata = %q, want %q", got, api.CodeUnknownRepository)
				}
			})
		}
	})
	t.Run("handleBotFixCI/unknownRepository", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			scopes []string
			want   string
		}{
			{
				name:   "with repository read access",
				scopes: []string{mcpScopeRead, mcpScopeReposWrite},
				want:   "unknown repository: mistyped. Call repos_list, use an exact returned path, then retry bot_fix_ci.",
			},
			{
				name:   "without repository read access",
				scopes: []string{mcpScopeReposWrite},
				want:   "unknown repository: mistyped. The path must exactly match a configured repository.",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				s := newTestRouter(t, nil)
				c := &mcpTools{ci: s.ciHandlers}
				ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: tt.scopes, Remote: true})
				ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})

				result := c.handleBotFixCI(ctx, &mcp.ToolRequest[mcpBotFixCIArgs]{Arguments: mcpBotFixCIArgs{Repo: "mistyped"}})
				if !result.IsError {
					t.Fatal("handleBotFixCI() did not return a tool error")
				}
				output, ok := result.Structured.(mcp.ErrorOutput)
				if !ok {
					t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
				}
				if output.Error != tt.want {
					t.Errorf("error = %q, want %q", output.Error, tt.want)
				}
				if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeUnknownRepository) {
					t.Errorf("error code metadata = %q, want %q", got, api.CodeUnknownRepository)
				}
			})
		}
	})
	t.Run("handleTaskCreate/error", func(t *testing.T) {
		t.Parallel()

		t.Run("selection", func(t *testing.T) {
			t.Parallel()

			tests := []struct {
				name     string
				args     mcpTaskCreateArgs
				want     string
				wantCode api.ErrorCode
			}{
				{
					name:     "unknown harness",
					args:     mcpTaskCreateArgs{Prompt: "do the task", Repos: []string{"myrepo"}, Harness: "mistyped"},
					want:     `unsupported harness "mistyped". Omit harness to use caic's default harness, then retry task_create.`,
					wantCode: api.CodeUnknownHarness,
				},
				{
					name:     "unsupported model",
					args:     mcpTaskCreateArgs{Prompt: "do the task", Repos: []string{"myrepo"}, Harness: "claude", Model: "mistyped"},
					want:     "unsupported model for claude: mistyped. Supported models: claude-default. Omit model to use the selected harness's default model, then retry task_create.",
					wantCode: api.CodeUnsupportedModel,
				},
				{
					name:     "unknown runtime",
					args:     mcpTaskCreateArgs{Prompt: "do the task", Repos: []string{"myrepo"}, Harness: "claude", RuntimeName: "mistyped"},
					want:     "unknown runtime: mistyped. Omit runtimeName to use caic's default runtime, then retry task_create.",
					wantCode: api.CodeUnknownRuntime,
				},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()

					s := newMCPTaskCreateTestRouter(t)
					c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
					result := c.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: tt.args})
					if !result.IsError {
						t.Fatal("handleTaskCreate() did not return a tool error")
					}
					output, ok := result.Structured.(mcp.ErrorOutput)
					if !ok {
						t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
					}
					if output.Error != tt.want {
						t.Errorf("error = %q, want %q", output.Error, tt.want)
					}
					if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(tt.wantCode) {
						t.Errorf("error code metadata = %q, want %q", got, tt.wantCode)
					}
				})
			}
		})

		t.Run("unsafe recovery", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			result := c.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{
				Prompt:  "do the task",
				Repos:   []string{"myrepo"},
				Harness: "mistyped",
				Model:   "pi-default",
			}})
			if !result.IsError {
				t.Fatal("handleTaskCreate() did not return a tool error")
			}
			output, ok := result.Structured.(mcp.ErrorOutput)
			if !ok {
				t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
			}
			if output.Error != `unsupported harness "mistyped"` {
				t.Errorf("error = %q, want unadvised unknown-harness error", output.Error)
			}
			if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeUnknownHarness) {
				t.Errorf("error code metadata = %q, want %q", got, api.CodeUnknownHarness)
			}
		})

		t.Run("runtime recovery without default", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			s.taskMgr.Runtimes = &runtime.Router{}
			c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			result := c.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{
				Prompt:      "do the task",
				Repos:       []string{"myrepo"},
				Harness:     "claude",
				RuntimeName: "mistyped",
			}})
			if !result.IsError {
				t.Fatal("handleTaskCreate() did not return a tool error")
			}
			output, ok := result.Structured.(mcp.ErrorOutput)
			if !ok {
				t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
			}
			if output.Error != "unknown runtime: mistyped" {
				t.Errorf("error = %q, want unadvised unknown-runtime error", output.Error)
			}
			if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeUnknownRuntime) {
				t.Errorf("error code metadata = %q, want %q", got, api.CodeUnknownRuntime)
			}
		})

		t.Run("invalid default harness", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			if err := s.prefs.Update("default", func(p *preferences.Preferences) {
				p.Harness = "mistyped"
			}); err != nil {
				t.Fatal(err)
			}
			c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			result := c.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{Prompt: "do the task", Repos: []string{"myrepo"}}})
			if !result.IsError {
				t.Fatal("handleTaskCreate() did not return a tool error")
			}
			output, ok := result.Structured.(mcp.ErrorOutput)
			if !ok {
				t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
			}
			if output.Error != `unsupported harness "mistyped"` {
				t.Errorf("error = %q, want default-resolution error", output.Error)
			}
			if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeConflict) {
				t.Errorf("error code metadata = %q, want %q", got, api.CodeConflict)
			}
		})

		t.Run("unavailable default harness", func(t *testing.T) {
			t.Parallel()

			s := newMCPTaskCreateTestRouter(t)
			if err := s.prefs.Update("default", func(p *preferences.Preferences) {
				p.Harness = string(harness.Codex)
			}); err != nil {
				t.Fatal(err)
			}
			c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			result := c.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{Prompt: "do the task", Repos: []string{"myrepo"}}})
			if !result.IsError {
				t.Fatal("handleTaskCreate() did not return a tool error")
			}
			output, ok := result.Structured.(mcp.ErrorOutput)
			if !ok {
				t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
			}
			if output.Error != "unknown harness: codex" {
				t.Errorf("error = %q, want unavailable-default error", output.Error)
			}
			if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeConflict) {
				t.Errorf("error code metadata = %q, want %q", got, api.CodeConflict)
			}
		})
	})
	t.Run("CallTool/domainErrors", func(t *testing.T) {
		t.Parallel()

		s := newMCPTaskCreateTestRouter(t)
		c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		t.Run("malformed task request", func(t *testing.T) {
			t.Parallel()

			result := c.handleTaskCreate(t.Context(), &mcp.ToolRequest[mcpTaskCreateArgs]{Arguments: mcpTaskCreateArgs{Repos: []string{"myrepo"}}})
			assertMCPToolErrorCode(t, result, api.CodeBadRequest)
		})
		t.Run("invalid task number", func(t *testing.T) {
			t.Parallel()

			result := c.handleTaskGetDetail(t.Context(), &mcp.ToolRequest[mcpTaskGetDetailArgs]{Arguments: mcpTaskGetDetailArgs{Task: taskID("-1")}})
			assertMCPToolErrorCode(t, result, api.CodeBadRequest)
		})
		t.Run("invalid task reference", func(t *testing.T) {
			t.Parallel()

			result := c.handleTaskGetDetail(t.Context(), &mcp.ToolRequest[mcpTaskGetDetailArgs]{Arguments: mcpTaskGetDetailArgs{Task: taskID("!")}})
			assertMCPToolErrorCode(t, result, api.CodeBadRequest)
		})
		t.Run("missing task", func(t *testing.T) {
			t.Parallel()

			result := c.handleTaskGetDetail(t.Context(), &mcp.ToolRequest[mcpTaskGetDetailArgs]{Arguments: mcpTaskGetDetailArgs{Task: taskID(ksid.NewID().String())}})
			assertMCPToolErrorCode(t, result, api.CodeNotFound)
		})
		t.Run("unclassified failure", func(t *testing.T) {
			t.Parallel()

			result := domainToolError[mcp.TextOutput](errors.New("backend failure"))
			assertMCPToolErrorCode(t, result, api.CodeInternalError)
		})
	})
	t.Run("handleTaskFork/selectionErrors", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			args     mcpTaskForkArgs
			want     string
			wantCode api.ErrorCode
		}{
			{
				name:     "unknown override harness",
				args:     mcpTaskForkArgs{TaskNumber: taskID("1"), Prompt: "fork", Harness: "mistyped"},
				want:     `unsupported harness "mistyped". Omit harness to inherit the source task's harness, then retry task_fork.`,
				wantCode: api.CodeUnknownHarness,
			},
			{
				name:     "unsupported override model",
				args:     mcpTaskForkArgs{TaskNumber: taskID("1"), Prompt: "fork", Harness: "claude", Model: "mistyped"},
				want:     "unsupported model for claude: mistyped. Supported models: claude-default. Omit model to inherit the source task's model, then retry task_fork.",
				wantCode: api.CodeUnsupportedModel,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				s := newMCPTaskCreateTestRouter(t)
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "source"}, harness.Claude)
				tk.Repos = []taskslog.RepoMount{{Name: "myrepo", Branch: "main"}}
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "source"), runtime.ConnectionTarget{}, "", "", 0)
				tk.SetState(taskslog.StateWaiting)
				insertTestTask(s, tk.ID, tk)
				c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}

				result := c.handleTaskFork(t.Context(), &mcp.ToolRequest[mcpTaskForkArgs]{Arguments: tt.args})
				if !result.IsError {
					t.Fatal("handleTaskFork() did not return a tool error")
				}
				output, ok := result.Structured.(mcp.ErrorOutput)
				if !ok {
					t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
				}
				if output.Error != tt.want {
					t.Errorf("error = %q, want %q", output.Error, tt.want)
				}
				if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(tt.wantCode) {
					t.Errorf("error code metadata = %q, want %q", got, tt.wantCode)
				}
			})
		}
	})
	t.Run("handleTaskFork/unsafeRecovery", func(t *testing.T) {
		t.Parallel()

		s := newMCPTaskCreateTestRouter(t)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "source"}, harness.Claude)
		tk.Repos = []taskslog.RepoMount{{Name: "myrepo", Branch: "main"}}
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "source"), runtime.ConnectionTarget{}, "", "", 0)
		tk.SetState(taskslog.StateWaiting)
		insertTestTask(s, tk.ID, tk)
		c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := c.handleTaskFork(t.Context(), &mcp.ToolRequest[mcpTaskForkArgs]{Arguments: mcpTaskForkArgs{TaskNumber: taskID("1"), Prompt: "fork", Harness: "mistyped", Model: "pi-default"}})
		if !result.IsError {
			t.Fatal("handleTaskFork() did not return a tool error")
		}
		output, ok := result.Structured.(mcp.ErrorOutput)
		if !ok {
			t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
		}
		if output.Error != `unsupported harness "mistyped"` {
			t.Errorf("error = %q, want unadvised unknown-harness error", output.Error)
		}
		if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeUnknownHarness) {
			t.Errorf("error code metadata = %q, want %q", got, api.CodeUnknownHarness)
		}
	})
	t.Run("handleTaskFork/retiredSourceModel", func(t *testing.T) {
		t.Parallel()

		s := newMCPTaskCreateTestRouter(t)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "source"}, harness.Claude)
		tk.RequestedModel = "retired-model"
		tk.Repos = []taskslog.RepoMount{{Name: "myrepo", Branch: "main"}}
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "source"), runtime.ConnectionTarget{}, "", "", 0)
		tk.SetState(taskslog.StateWaiting)
		insertTestTask(s, tk.ID, tk)
		c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := c.handleTaskFork(t.Context(), &mcp.ToolRequest[mcpTaskForkArgs]{Arguments: mcpTaskForkArgs{TaskNumber: taskID("1"), Prompt: "fork", Harness: "mistyped"}})
		if !result.IsError {
			t.Fatal("handleTaskFork() did not return a tool error")
		}
		output, ok := result.Structured.(mcp.ErrorOutput)
		if !ok {
			t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
		}
		if output.Error != `unsupported harness "mistyped"` {
			t.Errorf("error = %q, want unadvised unknown-harness error", output.Error)
		}
		if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeUnknownHarness) {
			t.Errorf("error code metadata = %q, want %q", got, api.CodeUnknownHarness)
		}
	})
	t.Run("handleTaskFork/unsafeModelRecovery", func(t *testing.T) {
		t.Parallel()

		s := newMCPTaskCreateTestRouter(t)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "source"}, harness.Claude)
		tk.RequestedModel = "claude-default"
		tk.Repos = []taskslog.RepoMount{{Name: "myrepo", Branch: "main"}}
		tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "source"), runtime.ConnectionTarget{}, "", "", 0)
		tk.SetState(taskslog.StateWaiting)
		insertTestTask(s, tk.ID, tk)
		c := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := c.handleTaskFork(t.Context(), &mcp.ToolRequest[mcpTaskForkArgs]{Arguments: mcpTaskForkArgs{TaskNumber: taskID("1"), Prompt: "fork", Harness: "pi", Model: "mistyped"}})
		if !result.IsError {
			t.Fatal("handleTaskFork() did not return a tool error")
		}
		output, ok := result.Structured.(mcp.ErrorOutput)
		if !ok {
			t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
		}
		if output.Error != "unsupported model for pi: mistyped" {
			t.Errorf("error = %q, want unadvised unsupported-model error", output.Error)
		}
		if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeUnsupportedModel) {
			t.Errorf("error code metadata = %q, want %q", got, api.CodeUnsupportedModel)
		}
	})
	t.Run("handleCloneRepo/pathConflict", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "taken"), 0o750); err != nil {
			t.Fatalf("make existing clone directory: %v", err)
		}
		s := newCheckoutConstructionTestServer(t, root).server
		registry := &mcpTools{serverConfig: s.serverHandlers}

		result := registry.handleCloneRepo(t.Context(), &mcp.ToolRequest[mcpCloneRepoArgs]{Arguments: mcpCloneRepoArgs{URL: "https://example.com/repo.git", Path: "taken"}})
		if !result.IsError {
			t.Fatal("handleCloneRepo() did not return a tool error")
		}
		output, ok := result.Structured.(mcp.ErrorOutput)
		if !ok {
			t.Fatalf("result type = %T, want mcp.ErrorOutput", result.Structured)
		}
		if want := "directory already exists: taken. Choose a different path and retry clone_repo."; output.Error != want {
			t.Errorf("error = %q, want %q", output.Error, want)
		}
		if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeRepositoryPathConflict) {
			t.Errorf("error code metadata = %q, want %q", got, api.CodeRepositoryPathConflict)
		}
	})
	t.Run("CallTool/authorization", func(t *testing.T) {
		t.Parallel()

		t.Run("scope denial supplies an authentication challenge and code", func(t *testing.T) {
			t.Parallel()

			c := &mcpTools{metrics: metrics.NewStore(metrics.Resource{ServiceName: "caic"})}
			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Remote: true})
			ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})
			result, err := c.CallTool(ctx, &mcp.ToolsCallParams{Name: "task_create", Arguments: nil})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError {
				t.Fatal("CallTool() did not return a tool error")
			}
			if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(api.CodeUnauthorized) {
				t.Errorf("error code metadata = %q, want %q", got, api.CodeUnauthorized)
			}
			wantChallenge := mcpScopeChallenge(mcpScopeTasksCreate)
			if got := result.Meta["mcp/www_authenticate"]; !reflect.DeepEqual(got, []string{wantChallenge}) {
				t.Errorf("authentication challenge = %#v, want %#v", got, []string{wantChallenge})
			}
		})

		t.Run("remote forge tools require linked forge identity", func(t *testing.T) {
			t.Parallel()

			c := &mcpTools{}
			tests := []struct {
				name string
				user *auth.User
			}{
				{name: "no user"},
				{name: "GitLab without token", user: &auth.User{Provider: auth.ProviderGitLab, Username: "alice"}},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()

					for name := range mcpForgeTools {
						ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{
							Scopes: []string{mcpScopeReposWrite},
							Remote: true,
						})
						if tt.user != nil {
							ctx = auth.NewContext(ctx, tt.user)
						}

						reason, ok := c.authorizeTool(ctx, name)
						if ok {
							t.Fatalf("authorizeTool(%q) ok = true, want false", name)
						}
						if reason != "linked GitHub identity or GitLab token is required for forge MCP tools" {
							t.Fatalf("authorizeTool(%q) reason = %q", name, reason)
						}
					}
				})
			}
		})

		t.Run("remote forge tools allow linked GitHub server authority", func(t *testing.T) {
			t.Parallel()

			c := &mcpTools{}
			for name := range mcpForgeTools {
				ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{
					Scopes: []string{mcpScopeReposWrite},
					Remote: true,
				})
				ctx = auth.NewContext(ctx, &auth.User{Provider: auth.ProviderGitHub, Username: "alice"})

				reason, ok := c.authorizeTool(ctx, name)
				if !ok {
					t.Fatalf("authorizeTool(%q) ok = false, reason = %q", name, reason)
				}
				if reason != "allow" {
					t.Fatalf("authorizeTool(%q) reason = %q", name, reason)
				}
			}
		})

		t.Run("remote forge tools allow linked user authority", func(t *testing.T) {
			t.Parallel()

			c := &mcpTools{}
			for name := range mcpForgeTools {
				ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{
					Scopes: []string{mcpScopeReposWrite},
					Remote: true,
				})
				ctx = auth.NewContext(ctx, &auth.User{Provider: auth.ProviderGitLab, Username: "alice", AccessToken: "forge-token"})

				reason, ok := c.authorizeTool(ctx, name)
				if !ok {
					t.Fatalf("authorizeTool(%q) ok = false, reason = %q", name, reason)
				}
				if reason != "allow" {
					t.Fatalf("authorizeTool(%q) reason = %q", name, reason)
				}
			}
		})

		t.Run("task creation requires its independent scope", func(t *testing.T) {
			t.Parallel()

			c := &mcpTools{}
			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: []string{mcpScopeTasksWrite}, Remote: true})
			ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})
			reason, ok := c.authorizeTool(ctx, "task_create")
			if ok {
				t.Fatal("authorizeTool(task_create) ok = true, want false")
			}
			if reason != "missing required MCP scope: "+mcpScopeTasksCreate {
				t.Fatalf("authorizeTool(task_create) reason = %q", reason)
			}

			ctx = newMCPPrincipalContext(ctx, &mcpPrincipal{Scopes: []string{mcpScopeTasksCreate}, Remote: true})
			reason, ok = c.authorizeTool(ctx, "task_create")
			if !ok || reason != "allow" {
				t.Fatalf("authorizeTool(task_create) = (%q, %t), want (allow, true)", reason, ok)
			}
		})
	})
	t.Run("Tools", func(t *testing.T) {
		t.Parallel()

		registry := &mcpTools{}
		user := &auth.User{ID: "user-1", Provider: auth.ProviderGitHub, Username: "alice"}

		t.Run("read grant hides task creation", func(t *testing.T) {
			t.Parallel()

			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: []string{mcpScopeRead}, Remote: true})
			ctx = auth.NewContext(ctx, user)
			tools, err := registry.Tools(ctx)
			if err != nil {
				t.Fatalf("Tools() error: %v", err)
			}
			assertMCPToolVisibility(t, tools, "repos_list", true)
			assertMCPToolVisibility(t, tools, "harnesses_list", true)
			assertMCPToolVisibility(t, tools, "get_system_stats", false)
			assertMCPToolVisibility(t, tools, "task_create", false)
			assertMCPToolVisibility(t, tools, "tasks_list", false)
		})

		t.Run("task creation grant exposes only task creation", func(t *testing.T) {
			t.Parallel()

			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: []string{mcpScopeTasksCreate}, Remote: true})
			ctx = auth.NewContext(ctx, user)
			tools, err := registry.Tools(ctx)
			if err != nil {
				t.Fatalf("Tools() error: %v", err)
			}
			assertMCPToolVisibility(t, tools, "repos_list", false)
			assertMCPToolVisibility(t, tools, "get_system_stats", false)
			assertMCPToolVisibility(t, tools, "task_create", true)
			assertMCPToolVisibility(t, tools, "task_fork", false)
		})

		t.Run("local clients retain the complete catalog", func(t *testing.T) {
			t.Parallel()

			tools, err := registry.Tools(t.Context())
			if err != nil {
				t.Fatalf("Tools() error: %v", err)
			}
			if len(tools) != len(registry.specs()) {
				t.Fatalf("Tools() returned %d tools, want %d", len(tools), len(registry.specs()))
			}
		})

		t.Run("scope visibility does not depend on forge linkage", func(t *testing.T) {
			t.Parallel()

			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: []string{mcpScopeReposWrite}, Remote: true})
			ctx = auth.NewContext(ctx, &auth.User{Provider: auth.ProviderGitLab, Username: "alice"})
			tools, err := registry.Tools(ctx)
			if err != nil {
				t.Fatalf("Tools() error: %v", err)
			}
			assertMCPToolVisibility(t, tools, "task_push_branch_to_remote", true)
		})

		t.Run("task scoped client can manage its children and inspect all tasks", func(t *testing.T) {
			t.Parallel()
			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{TaskID: ksid.NewID(), Remote: true})
			registry := &mcpTools{}
			tools, err := registry.Tools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertMCPToolVisibility(t, tools, "task_create", true)
			assertMCPToolVisibility(t, tools, "tasks_list", true)
			assertMCPToolVisibility(t, tools, "task_fork", true)
			assertMCPToolVisibility(t, tools, "task_stop", true)
			assertMCPToolVisibility(t, tools, "task_purge", true)
			assertMCPToolVisibility(t, tools, "get_system_stats", false)
			assertMCPToolVisibility(t, tools, "task_get_detail", true)
			assertMCPToolVisibility(t, tools, "repos_list", false)
		})
	})
	t.Run("handleTaskGetDetail/diffstat", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			t.Run("repository and file summaries", func(t *testing.T) {
				t.Parallel()
				s, rt, id := newMCPDiffstatRouter(t)
				rt.RepositoryStatusValue.Uncommitted = append(rt.RepositoryStatusValue.Uncommitted, runtime.GitFileStatus{Path: "image.png", Binary: true, OldSize: 12, NewSize: 24})
				ent, _ := s.taskMgr.GetEntry(id)
				ent.Task().Repos = append(ent.Task().Repos, taskslog.RepoMount{Name: "other", Branch: "other-work", ContainerPath: "/workspace/other"})
				res := callTaskDetail(t, s.mcpHandlers.protocol.Registry, mcpTaskGetDetailArgs{Task: taskID(id.String()), IncludeDiffstat: true})
				if res.IsError {
					t.Fatal(res)
				}
				txt := taskDetailText(t, res.Structured)
				for _, exp := range []string{`Repository "repo": branch "caic-1", upstream "origin/main", ahead 1, behind 1`, `Repository "other"`, "Local commit", "Missing upstream commit", "Add API", "Missing upstream change", `"committed.go": +4 -1`, `"upstream.go": +1 -0`, `"old.go" -> "renamed.go": +2 -0`, `"image.png": binary (12 -> 24 bytes)`} {
					if !strings.Contains(txt, exp) {
						t.Errorf("missing %q in %s", exp, txt)
					}
				}
				if rt.StatusReads.Load() != 2 || rt.PatchReads.Load() != 0 {
					t.Fatalf("reads: %d status, %d patches", rt.StatusReads.Load(), rt.PatchReads.Load())
				}
				if !strings.HasSuffix(txt, "Total (all compared commits and working trees): 8 file changes, 14 insertions(+), 2 deletions(-), 2 binary file changes.\n") {
					t.Fatalf("missing complete totals: %s", txt)
				}
			})
			t.Run("default avoids repository reads", func(t *testing.T) {
				t.Parallel()
				s, rt, id := newMCPDiffstatRouter(t)
				res := callTaskDetail(t, s.mcpHandlers.protocol.Registry, mcpTaskGetDetailArgs{Task: taskID(id.String())})
				if res.IsError || rt.StatusReads.Load() != 0 || rt.PatchReads.Load() != 0 {
					t.Fatalf("default result=%+v, status=%d, patches=%d", res, rt.StatusReads.Load(), rt.PatchReads.Load())
				}
			})
			t.Run("truncation retains complete rows", func(t *testing.T) {
				t.Parallel()
				s, rt, id := newMCPDiffstatRouter(t)
				rt.RepositoryStatusValue.Commits = nil
				rt.RepositoryStatusValue.Uncommitted = nil
				for i := range 80 {
					rt.RepositoryStatusValue.Uncommitted = append(rt.RepositoryStatusValue.Uncommitted, runtime.GitFileStatus{Path: fmt.Sprintf("%03d-", i) + strings.Repeat("\u754c", 700), LinesAdded: 1})
				}
				got := taskDetailWire(t, s, mcpTaskGetDetailArgs{Task: taskID(id.String()), IncludeDiffstat: true})
				txt := got.Result.StructuredContent.Result
				if got.Result.IsError || len(txt) > 32768 || !utf8.ValidString(txt) {
					t.Fatalf("invalid diffstat: error=%v, bytes=%d", got.Result.IsError, len(txt))
				}
				if !strings.Contains(txt, `  "000-`+strings.Repeat("\u754c", 700)+`": +1 -0`+"\n") {
					t.Fatal("first file row was shortened")
				}
				before, after, ok := strings.Cut(txt, "[Diffstat output truncated]\n")
				if !ok || !strings.HasSuffix(before, "\": +1 -0\n") || after != "\nTotal (all compared commits and working trees): 80 file changes, 80 insertions(+), 0 deletions(-), 0 binary file changes.\n" {
					t.Fatal("truncation marker must replace complete rows before the totals")
				}
			})
			t.Run("serialized output remains bounded", func(t *testing.T) {
				t.Parallel()
				s, rt, id := newMCPDiffstatRouter(t)
				rt.RepositoryStatusValue.Commits[0].Subject = strings.Repeat("huge\xffsubject", 10000)
				for i := range 500 {
					p := fmt.Sprintf("%03d/", i) + strings.Repeat("\xff\u754c/", 500)
					rt.RepositoryStatusValue.Uncommitted = append(rt.RepositoryStatusValue.Uncommitted, runtime.GitFileStatus{Path: p, LinesAdded: 1})
					rt.RepositoryStatusValue.DiffStat = append(rt.RepositoryStatusValue.DiffStat, runtime.GitFileStat{Path: p, LinesAdded: 1})
				}
				got := taskDetailWire(t, s, mcpTaskGetDetailArgs{Task: taskID(id.String()), IncludeDiffstat: true})
				if got.Result.IsError {
					t.Fatal(got)
				}
				txt := got.Result.StructuredContent.Result
				if len(txt) > 32768 || !utf8.ValidString(txt) || !strings.Contains(txt, "[Diffstat output truncated]") || got.Result.Meta[mcpTruncatedMetaKey] != true {
					t.Fatalf("bad bounded output: %d bytes", len(txt))
				}
				if len(got.Result.Content) != 1 || got.Result.Content[0].Text != txt {
					t.Fatal("text and structured output disagree")
				}
				if !strings.Contains(txt, `Repository "repo"`) || !strings.HasSuffix(txt, "[Diffstat output truncated]\n\nTotal (all compared commits and working trees): 503 file changes, 507 insertions(+), 1 deletions(-), 0 binary file changes.\n") {
					t.Fatal("middle truncation must preserve the beginning and complete totals")
				}
				// The live read updates cached paths, whose invalid bytes must also
				// stay bounded when later serialized without a diffstat request.
				got = taskDetailWire(t, s, mcpTaskGetDetailArgs{Task: taskID(id.String())})
				if got.Result.IsError || len(got.Result.StructuredContent.Result) > 32768 {
					t.Fatalf("cached output: error=%v, bytes=%d", got.Result.IsError, len(got.Result.StructuredContent.Result))
				}
			})
			for _, name := range []string{"owner", "self"} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					s, rt, id := newMCPDiffstatRouter(t)
					ent, _ := s.taskMgr.GetEntry(id)
					ent.Task().OwnerID = "owner"
					ctx := auth.NewContext(t.Context(), &auth.User{ID: "owner"})
					reg := s.mcpHandlers.protocol.Registry
					if name == "self" {
						ctx = t.Context()
						reg = s.TaskMCPScoper.ForTask(id)
					}
					res, err := reg.CallTool(ctx, &mcp.ToolsCallParams{Name: "task_get_detail", Arguments: json.RawMessage(fmt.Sprintf(`{"task":%q,"include_diffstat":true}`, id.String()))})
					if err != nil || res.IsError || rt.StatusReads.Load() != 1 {
						t.Fatalf("result=%+v, err=%v, reads=%d", res, err, rt.StatusReads.Load())
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("oversized repository diagnostics", func(t *testing.T) {
				t.Parallel()
				s, rt, id := newMCPDiffstatRouter(t)
				rt.StatusErr = errors.New(strings.Repeat("bad\xffrecord", 20000))
				got := taskDetailWire(t, s, mcpTaskGetDetailArgs{Task: taskID(id.String()), IncludeDiffstat: true})
				if !got.Result.IsError || got.Result.Meta[mcp.ToolErrorCodeMetaKey] != string(api.CodeInternalError) || got.Result.Meta[mcpTruncatedMetaKey] != true {
					t.Fatalf("missing bounded error metadata: %+v", got.Result.Meta)
				}
				if len(got.Result.Content) != 1 {
					t.Fatalf("error content blocks: %d", len(got.Result.Content))
				}
				txt := got.Result.Content[0].Text
				if len(txt) > 32768 || !utf8.ValidString(txt) || !strings.Contains(txt, "Output truncated") {
					t.Fatalf("bad bounded error: %d bytes", len(txt))
				}
			})
			t.Run("repository failure", func(t *testing.T) {
				t.Parallel()
				s, rt, id := newMCPDiffstatRouter(t)
				rt.StatusErr = errors.New("status unavailable")
				res := callTaskDetail(t, s.mcpHandlers.protocol.Registry, mcpTaskGetDetailArgs{Task: taskID(id.String()), IncludeDiffstat: true})
				if !res.IsError || res.Meta[mcp.ToolErrorCodeMetaKey] != string(api.CodeInternalError) {
					t.Fatalf("missing repository error: %+v", res)
				}
			})
			t.Run("another owner", func(t *testing.T) {
				t.Parallel()
				s, rt, id := newMCPDiffstatRouter(t)
				ent, _ := s.taskMgr.GetEntry(id)
				ent.Task().OwnerID = "owner"
				ctx := auth.NewContext(t.Context(), &auth.User{ID: "other"})
				res, err := s.mcpHandlers.protocol.Registry.CallTool(ctx, &mcp.ToolsCallParams{Name: "task_get_detail", Arguments: json.RawMessage(fmt.Sprintf(`{"task":%q,"include_diffstat":true}`, id.String()))})
				if err != nil {
					t.Fatal(err)
				}
				if !res.IsError || rt.StatusReads.Load() != 0 || rt.PatchReads.Load() != 0 {
					t.Fatalf("unauthorized result=%+v, reads=%d/%d", res, rt.StatusReads.Load(), rt.PatchReads.Load())
				}
			})
		})
	})
	t.Run("specs/taskFocus", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"agent_last_message", "task_answer_question", "task_fix_pr", "task_fork", "task_get_detail", "task_purge", "task_push_branch_to_remote", "task_revive", "task_send_message", "task_stop"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				for _, ref := range []string{"1", `"#1"`, "id", `"missing"`} {
					t.Run(ref, func(t *testing.T) {
						t.Parallel()
						s := newTestRouter(t, nil)
						svc := testTaskHandlers(s).taskSvc
						id := ksid.NewID()
						target := mustNewTask(t, id, agent.Prompt{Text: "focus target"}, harness.Claude)
						target.SetState(taskslog.StateStopped)
						insertTestTask(s, id, target)
						ch, cancel := svc.subscribeTaskFocus(t.Context())
						t.Cleanup(cancel)
						registry := &mcpTools{taskSvc: svc, ci: s.ciHandlers}
						value := ref
						if ref == "id" {
							value = fmt.Sprintf("%q", id.String())
						}
						field := "task_number"
						if name == "task_get_detail" {
							field = "task"
						}
						args := json.RawMessage(fmt.Sprintf(`{%q:%s}`, field, value))
						for _, spec := range registry.specs() {
							if spec.Name == name {
								if _, err := spec.Handler(t.Context(), &mcp.ToolsCallParams{Name: name, Arguments: args}); err != nil {
									t.Fatal(err)
								}
								break
							}
						}
						select {
						case got := <-ch:
							if ref == `"missing"` || got != id.String() {
								t.Fatalf("focus = %q for reference %s", got, ref)
							}
						default:
							if ref != `"missing"` {
								t.Fatal("tool did not focus the target task")
							}
						}
					})
				}
			})
		}
	})
	t.Run("handleAgentLastMessage/historyReaders", func(t *testing.T) {
		t.Run("scoped_tools_and_root_last_message", func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex)
			root := agent.MessageSource{ThreadID: "root", TurnID: "turn", RootThreadID: "root"}
			child := agent.MessageSource{ThreadID: "child", TurnID: "turn", RootThreadID: "root"}
			tk.SeedTimeline([]agent.Message{
				&agent.ToolUseMessage{Source: root, ToolUseID: "local", Input: json.RawMessage(`{"owner":"root"}`)},
				&agent.TextMessage{Source: root, Text: "root answer"},
				&agent.ToolUseMessage{Source: child, ToolUseID: "local", Input: json.RawMessage(`{"owner":"child"}`)},
				&agent.TextMessage{Source: child, Text: "child answer"},
				&agent.UserInputMessage{Source: child, Text: "child prompt"},
			})
			entry := s.taskMgr.NewEntry(tk, nil)
			s.taskMgr.Insert(tk.ID, entry)
			svc := testTaskHandlers(s).taskSvc
			if _, err := svc.taskToolInput(t.Context(), entry, "local", v1.TaskToolInputQuery{}); err == nil {
				t.Fatal("ambiguous unscoped tool lookup succeeded")
			}
			for _, tc := range []struct{ thread, want string }{{"root", `{"owner":"root"}`}, {"child", `{"owner":"child"}`}} {
				q := url.Values{"threadID": {tc.thread}, "turnID": {"turn"}}
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+tk.ID.String()+"/tool/local?"+q.Encode(), http.NoBody)
				req.SetPathValue("id", tk.ID.String())
				req.SetPathValue("toolUseID", "local")
				w := httptest.NewRecorder()
				testTaskHandlers(s).handleTaskToolInput(w, req)
				var got v1.TaskToolInputResp
				if w.Code != http.StatusOK {
					t.Fatalf("scoped input HTTP %d: %s", w.Code, w.Body.String())
				}
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if string(got.Input) != tc.want {
					t.Errorf("%s input = %s", tc.thread, got.Input)
				}
			}
			if _, err := svc.taskToolInput(t.Context(), entry, "local", v1.TaskToolInputQuery{ThreadID: "missing", TurnID: "turn"}); err == nil {
				t.Fatal("missing owner fell back to another tool")
			}
			registry := &mcpTools{serverConfig: s.serverHandlers, taskSvc: svc}
			result := registry.handleAgentLastMessage(t.Context(), &mcp.ToolRequest[mcpTaskNumberArgs]{Arguments: mcpTaskNumberArgs{TaskNumber: taskID("1")}})
			out, ok := result.Structured.(mcp.TextOutput)
			if result.IsError || !ok || out.Result != "Last message from task #1: root answer" {
				t.Fatalf("last message = %#v", result.Structured)
			}
		})
		t.Parallel()

		t.Run("valid_read_from_log_without_hydrating_task", func(t *testing.T) {
			t.Parallel()
			logDir := t.TempDir()
			restoredID := ksid.NewID()
			meta := mustJSON(t, logv1.MetaMessage{
				MessageType: "caic_meta", Version: 1, Prompt: "inspect history", Harness: string(harness.Claude),
				StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			})
			toolUse := mustJSON(t, map[string]any{
				"type": "assistant",
				"message": map[string]any{"content": []any{map[string]any{
					"type": "tool_use", "id": "tool-1", "name": "Bash", "input": map[string]any{"command": "make check"},
				}}},
			})
			lastMessage := mustJSON(t, map[string]any{
				"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "history result"}}},
			})
			trailer := mustJSON(t, logv1.MetaResultMessage{MessageType: "caic_result", State: "purged"})
			writeLogFile(t, logDir, restoredID.String()+".jsonl", meta, toolUse, lastMessage, trailer)

			s := newTestRouter(t, map[harness.Name]agent.Backend{
				harness.Claude: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire},
			})
			if err := loadPurgedTasksForTest(s, logDir); err != nil {
				t.Fatal(err)
			}
			entry, ok := s.taskMgr.GetEntry(restoredID)
			if !ok {
				t.Fatal("loaded task entry not found")
			}

			resp, err := testTaskHandlers(s).taskSvc.taskToolInput(t.Context(), entry, "tool-1", v1.TaskToolInputQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if string(resp.Input) != `{"command":"make check"}` {
				t.Fatalf("tool input = %s", resp.Input)
			}

			registry := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			result := registry.handleAgentLastMessage(t.Context(), &mcp.ToolRequest[mcpTaskNumberArgs]{Arguments: mcpTaskNumberArgs{TaskNumber: taskID("1")}})
			if result.IsError {
				t.Fatalf("agent_last_message error = %#v", result.Structured)
			}
			output, ok := result.Structured.(mcp.TextOutput)
			if !ok || output.Result != "Last message from task #1: history result" {
				t.Fatalf("agent_last_message = %#v", result.Structured)
			}

			history, _, unsubscribe := entry.Task().Subscribe(t.Context())
			unsubscribe()
			if len(history) != 0 {
				t.Fatalf("restored task retained %d history messages", len(history))
			}
		})

		t.Run("live_read_without_log", func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "no log"}, harness.Claude)
			tk.SeedTimeline([]agent.Message{
				&agent.ToolUseMessage{ToolUseID: "tool-1", Input: json.RawMessage(`{"command":"make check"}`)},
				&agent.TextMessage{Text: "live result"},
			})
			entry := s.taskMgr.NewEntry(tk, nil)
			s.taskMgr.Insert(tk.ID, entry)

			resp, err := testTaskHandlers(s).taskSvc.taskToolInput(t.Context(), entry, "tool-1", v1.TaskToolInputQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if string(resp.Input) != `{"command":"make check"}` {
				t.Fatalf("tool input = %s", resp.Input)
			}

			registry := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			result := registry.handleAgentLastMessage(t.Context(), &mcp.ToolRequest[mcpTaskNumberArgs]{Arguments: mcpTaskNumberArgs{TaskNumber: taskID("1")}})
			output, ok := result.Structured.(mcp.TextOutput)
			if result.IsError || !ok || output.Result != "Last message from task #1: live result" {
				t.Fatalf("agent_last_message = %#v", result.Structured)
			}
		})

		t.Run("restored_error_no_log", func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "no log"}, harness.Claude)
			loaded := &taskslog.LoadedTask{State: taskslog.StatePurged, LastTrailer: &taskslog.Result{State: taskslog.StatePurged}}
			entry := s.taskMgr.NewEntry(tk, loaded)
			s.taskMgr.Insert(tk.ID, entry)

			_, err := testTaskHandlers(s).taskSvc.taskToolInput(t.Context(), entry, "tool-1", v1.TaskToolInputQuery{})
			apiErr, ok := errors.AsType[*api.Error](err)
			if !ok || apiErr.Code != api.CodeInternalError || !strings.Contains(apiErr.Error(), "history unavailable") {
				t.Fatalf("taskToolInput error = %v, want explicit history-unavailable error", err)
			}

			registry := &mcpTools{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			result := registry.handleAgentLastMessage(t.Context(), &mcp.ToolRequest[mcpTaskNumberArgs]{Arguments: mcpTaskNumberArgs{TaskNumber: taskID("1")}})
			if !result.IsError || !strings.Contains(fmt.Sprint(result.Structured), "history is unavailable") {
				t.Fatalf("agent_last_message = %#v, want explicit history-unavailable error", result.Structured)
			}
		})
	})
}

func TestTaskID(t *testing.T) {
	t.Parallel()
	t.Run("Decode", func(t *testing.T) {
		t.Parallel()

		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			stableID := ksid.NewID()
			for _, tc := range []struct {
				name       string
				ref        taskID
				max        int
				wantID     ksid.ID
				wantNumber int
			}{
				{name: "task number", ref: "2", max: 2, wantNumber: 2},
				{name: "hashtag task number", ref: taskID("#2"), max: 2, wantNumber: 2},
				{name: "stable ID", ref: taskID(stableID.String()), max: 2, wantID: stableID},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					gotID, gotNumber, err := tc.ref.Decode(tc.max)
					if err != nil {
						t.Fatalf("Decode() error: %v", err)
					}
					if gotID != tc.wantID || gotNumber != tc.wantNumber {
						t.Fatalf("Decode() = (%q, %d), want (%q, %d)", gotID, gotNumber, tc.wantID, tc.wantNumber)
					}
				})
			}
		})

		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name string
				ref  taskID
				max  int
			}{
				{name: "out of range number", ref: "3", max: 2},
				{name: "out of range hashtag number", ref: taskID("#3"), max: 2},
				{name: "invalid reference", ref: "not a task", max: 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					if _, _, err := tc.ref.Decode(tc.max); err == nil {
						t.Fatal("Decode() error = nil, want error")
					}
				})
			}
		})
	})
	t.Run("UnmarshalJSON", func(t *testing.T) {
		t.Parallel()

		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name string
				raw  string
				want taskID
			}{
				{name: "integer", raw: "3", want: "3"},
				{name: "number string", raw: `"3"`, want: "3"},
				{name: "hashtag string", raw: `"#3"`, want: "#3"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					var got taskID
					if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
						t.Fatalf("Unmarshal() error: %v", err)
					}
					if got != tc.want {
						t.Fatalf("Unmarshal() = %q, want %q", got, tc.want)
					}
				})
			}
		})

		t.Run("error", func(t *testing.T) {
			t.Parallel()
			var got taskID
			if err := json.Unmarshal([]byte(`{}`), &got); err == nil {
				t.Fatal("Unmarshal() error = nil, want error")
			}
		})
	})
}
