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
		createSpec := c.specs()[1]
		if err := s.prefs.Update(userIDFromCtx(t.Context()), func(p *preferences.Preferences) {
			p.Harness = string(harness.Pi)
			p.Models = map[string]string{string(harness.Pi): "pi-default"}
			p.Efforts = preferences.EffortPreferences{string(harness.Pi): {"pi-default": "high"}}
		}); err != nil {
			t.Fatalf("Update preferences: %v", err)
		}

		raw, err := createSpec.Handler(t.Context(), json.RawMessage(`{"prompt":"do the task","repos":["myrepo"]}`))
		if err != nil {
			t.Fatalf("task_create handler returned error: %v", err)
		}
		if raw.IsError {
			t.Fatalf("task_create returned tool error: %+v", raw.Structured)
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
}
