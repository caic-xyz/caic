// Tests for MCP tool and resource authorization policy.

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

type staticUsageFetcher struct {
	quota usage.ProviderQuota
}

func (f *staticUsageFetcher) Provider() agent.QuotaProvider { return f.quota.Provider }

func (f *staticUsageFetcher) Label() string { return f.quota.Label }

func (f *staticUsageFetcher) AuthKind() usage.AuthKind { return f.quota.AuthKind }

func (*staticUsageFetcher) UsageURL() string { return "" }

func (f *staticUsageFetcher) Get(context.Context) *usage.ProviderQuota { return &f.quota }

func TestCaicToolRegistryHandleGetUsage(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	s.usageHandlers.fetchers = []usage.ProviderFetcher{&staticUsageFetcher{quota: usage.ProviderQuota{
		Provider: agent.QuotaProviderAnthropic,
		Label:    "Anthropic",
		AuthKind: usage.AuthKindOAuth,
		RateLimits: []usage.QuotaRateLimit{
			{Window: "5h", UsedPct: 12},
		},
	}}}
	c := &mcpRegistry{usage: s.usageHandlers}
	result := c.handleGetUsage(t.Context(), struct{}{})
	output, ok := result.Structured.(mcp.TextOutput)
	if !ok {
		t.Fatalf("get_usage result type = %T, want mcp.TextOutput", result.Structured)
	}
	if !strings.Contains(output.Result, "Anthropic: 5h: 88% remaining") {
		t.Fatalf("get_usage result = %q, want remaining quota", output.Result)
	}
}

func TestVoiceTaskSummaryLineIncludesAgentConfiguration(t *testing.T) {
	t.Parallel()

	got := voiceTaskSummaryLine(1, &v1.Task{
		Title:   "Fix the parser",
		State:   v1.TaskStateRunning,
		Harness: v1.HarnessCodex,
		Model:   "gpt-5.4",
		Effort:  "high",
	})
	want := "- Task #1: Fix the parser (running, harness: codex, model: gpt-5.4, effort: high)"
	if got != want {
		t.Fatalf("voiceTaskSummaryLine() = %q, want %q", got, want)
	}
}

func TestVoiceTaskSummaryLineMarksUnspecifiedConfigurationAsDefault(t *testing.T) {
	t.Parallel()

	got := voiceTaskSummaryLine(1, &v1.Task{
		Title:   "Fix the parser",
		State:   v1.TaskStateRunning,
		Harness: v1.HarnessClaude,
	})
	want := "- Task #1: Fix the parser (running, harness: claude, model: default, effort: default)"
	if got != want {
		t.Fatalf("voiceTaskSummaryLine() = %q, want %q", got, want)
	}
}

func TestCaicToolRegistryHandleReposList(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	registerRouterCheckout(t, s.checkouts, "org/repo", newRouterTestCheckout(t.TempDir()))
	registerRouterCheckout(t, s.checkouts, "repo2", &repo.Checkout{BaseBranch: "develop", Dir: t.TempDir()})
	c := &mcpRegistry{serverConfig: s.serverHandlers}

	result := c.handleReposList(t.Context(), struct{}{})
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
}

func TestCaicToolRegistryHandleTaskCreate(t *testing.T) {
	t.Parallel()

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
		c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}

		result := c.handleTaskCreate(t.Context(), mcpTaskCreateArgs{
			Prompt:  "do the task",
			Repos:   []string{"myrepo"},
			Harness: string(harness.Pi),
		})
		if result.IsError {
			t.Fatalf("handleTaskCreate() returned tool error: %+v", result.Structured)
		}

		created := singleCreatedTask(t, s)
		if created.Harness != harness.Pi {
			t.Fatalf("created harness = %q, want %q", created.Harness, harness.Pi)
		}
		if got := created.GetModel(); got != "" {
			t.Fatalf("created model = %q, want empty", got)
		}
		if created.Effort != "" {
			t.Fatalf("created effort = %q, want empty", created.Effort)
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
		c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}

		result := c.handleTaskCreate(t.Context(), mcpTaskCreateArgs{
			Prompt: "do the task",
			Repos:  []string{"myrepo"},
		})
		if result.IsError {
			t.Fatalf("handleTaskCreate() returned tool error: %+v", result.Structured)
		}

		created := singleCreatedTask(t, s)
		if created.Harness != harness.Pi {
			t.Fatalf("created harness = %q, want %q", created.Harness, harness.Pi)
		}
		if got := created.GetModel(); got != "" {
			t.Fatalf("created model = %q, want empty", got)
		}
		if created.Effort != "" {
			t.Fatalf("created effort = %q, want empty", created.Effort)
		}
	})

	t.Run("tool handler reads preferences at call time", func(t *testing.T) {
		t.Parallel()

		s := newMCPTaskCreateTestRouter(t)
		c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
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
			raw, err := spec.Handler(t.Context(), json.RawMessage(`{"prompt":"do the task","repos":["myrepo"]}`))
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
		if got := created.GetModel(); got != "" {
			t.Fatalf("created model = %q, want empty", got)
		}
		if created.Effort != "" {
			t.Fatalf("created effort = %q, want empty", created.Effort)
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
		c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}

		result := c.handleTaskCreate(t.Context(), mcpTaskCreateArgs{
			Prompt:  "do the task",
			Repos:   []string{"myrepo"},
			Harness: string(harness.Pi),
		})
		if result.IsError {
			t.Fatalf("handleTaskCreate() returned tool error: %+v", result.Structured)
		}

		created := singleCreatedTask(t, s)
		if created.Harness != harness.Pi {
			t.Fatalf("created harness = %q, want %q", created.Harness, harness.Pi)
		}
		if got := created.GetModel(); got != "" {
			t.Fatalf("created model = %q, want empty", got)
		}
		if created.Effort != "" {
			t.Fatalf("created effort = %q, want empty", created.Effort)
		}
	})
}

func newMCPTaskCreateTestRouter(t *testing.T) *testRouter {
	s := newTestRouter(t, map[harness.Name]agent.Backend{
		harness.Claude: &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "claude-default"}}}, WireFactory: claudecode.New().NewWire},
		harness.Pi:     &agenttest.FakeBackend{Inventory: agent.ModelInventory{Models: []agent.Model{{ID: "pi-default"}}}, WireFactory: claudecode.New().NewWire},
	})
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

func TestCaicToolRegistryAuthorizeTool(t *testing.T) {
	t.Parallel()

	t.Run("remote forge tools require linked forge identity", func(t *testing.T) {
		t.Parallel()

		c := &mcpRegistry{}
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

		c := &mcpRegistry{}
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

		c := &mcpRegistry{}
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

		c := &mcpRegistry{}
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
}

func TestCaicToolRegistryTools(t *testing.T) {
	t.Parallel()

	registry := &mcpRegistry{}
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
