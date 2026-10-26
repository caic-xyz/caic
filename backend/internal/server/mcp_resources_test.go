// Tests MCP resource visibility, limits, subscriptions, and system health.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/repo"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/maruel/gomode/mcp"
)

func TestMCPResources(t *testing.T) {
	t.Parallel()
	t.Run("collection resources are represented by tools and individual resources", func(t *testing.T) {
		t.Parallel()

		for _, resource := range mcpStaticResources() {
			if resource.URI == "caic://repos" || resource.URI == "caic://tasks" {
				t.Fatalf("aggregate resource %q remains in the static catalog", resource.URI)
			}
		}
	})
	t.Run("resource overflow audit", func(t *testing.T) {
		t.Parallel()

		store := &auditStore{log: testLogger()}
		registry := &mcpResources{audit: store}
		_, err := registry.resourceJSON(t.Context(), "caic://large", strings.Repeat("a", mcpResourceJSONMaxBytes))
		if err == nil {
			t.Fatal("resourceJSON() accepted an oversized resource")
		}
		events := store.snapshot()
		if len(events) != 1 || events[0].Status != "error" {
			t.Fatalf("audit events = %#v, want one error outcome", events)
		}
	})
	t.Run("resource task title", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		id := ksid.NewID()
		tk := mustNewTask(t, id, agent.Prompt{Text: "task"}, harness.Claude)
		tk.SetTitle(strings.Repeat("€", maxMCPTaskTitle))
		insertTestTask(s, id, tk)
		registry := &mcpResources{taskSvc: testTaskHandlers(s).taskSvc}
		keys := []mcpResourceKey{{URI: "caic://tasks/" + id.String(), Kind: mcpResourceTask, TaskID: id}}
		var yielded bool
		for resource, err := range registry.resourceDescriptors(t.Context(), keys) {
			yielded = true
			if err != nil {
				t.Fatal(err)
			}
			if len(resource.Title) > maxMCPTaskTitle || !utf8.ValidString(resource.Title) {
				t.Fatalf("resource title length = %d, valid UTF-8 = %t", len(resource.Title), utf8.ValidString(resource.Title))
			}
			if !strings.HasSuffix(resource.Title, "…") {
				t.Fatalf("resource title = %q, want bounded task title with ellipsis", resource.Title)
			}
		}
		if !yielded {
			t.Fatal("resourceDescriptors() yielded no task resource")
		}
	})
	t.Run("resource repository path", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		checkout := &repo.Checkout{Dir: t.TempDir()}
		registerRouterCheckout(t, s.checkouts, "repo", checkout)
		checkout.RelPath = strings.Repeat("x", maxMCPRepoField+1)
		registry := &mcpResources{serverConfig: s.serverHandlers}

		ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: []string{mcpScopeRead}, Remote: true})
		if _, err := registry.ListResources(ctx, ""); !errors.Is(err, errMCPRepositoryPathTooLong) {
			t.Fatalf("ListResources() error = %v, want %v", err, errMCPRepositoryPathTooLong)
		}

		checkout.RelPath = string([]byte{0xff})
		if _, err := registry.ListResources(ctx, ""); !errors.Is(err, errMCPRepositoryPathInvalidUTF8) || strings.Contains(err.Error(), "512") {
			t.Fatalf("ListResources() UTF-8 error = %v, want %v without a size diagnosis", err, errMCPRepositoryPathInvalidUTF8)
		}
	})
	t.Run("SubscribeResourceUpdates/readScope", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			registry := &mcpResources{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: []string{mcpScopeRead}, Remote: true})
			ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})
			if _, err := registry.subscriptionSources(ctx, mcp.SubscriptionFilter{ResourcesListChanged: true}); err != nil {
				t.Fatalf("subscriptionSources() error: %v", err)
			}
		})
	})
	t.Run("ReadResource/systemHealth", func(t *testing.T) {
		t.Parallel()
		const uri = "caic://system/health"
		t.Run("read_subscribe_recovery_and_keepalive", func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := newTestRouter(t, nil)
				registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
				if !ok {
					t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
				}
				stats := &systemStats{samples: make([]v1.SystemSample, 0, 60), changed: make(chan struct{})}
				registry.mcpResources.serverConfig.systemStats = stats
				ctx, cancel := context.WithCancel(newMCPPrincipalContext(t.Context(), &mcpPrincipal{Remote: true, Scopes: []string{mcpScopeServerAdmin}}))
				t.Cleanup(cancel)
				resources, err := registry.ListResources(ctx, "")
				if err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(resources.Resources, func(r mcp.ResourceDescriptor) bool { return r.URI == uri }) {
					t.Fatal("health resource is not advertised")
				}
				read := func() v1.ServerHealth {
					result, err := registry.ReadResource(ctx, &mcp.ResourcesReadParams{URI: uri})
					if err != nil {
						t.Fatal(err)
					}
					if result.CacheScope != mcp.CacheScopePrivate || len(result.Contents) != 1 {
						t.Fatalf("result = %+v", result)
					}
					var health v1.ServerHealth
					if err := json.Unmarshal([]byte(result.Contents[0].Text), &health); err != nil {
						t.Fatal(err)
					}
					return health
				}
				if h := read(); !h.Time.IsZero() || h.Error == "" {
					t.Fatalf("warmup=%+v", h)
				}
				changes, err := registry.SubscribeResourceUpdates(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{uri}})
				if err != nil {
					t.Fatal(err)
				}
				sample := v1.SystemSample{Time: time.Now(), CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 85}}, MemPerc: 85, MemUsed: 850, MemTotal: 1000}
				stats.record(&sample, nil)
				count := 0
				for update, err := range changes {
					if err != nil {
						t.Fatal(err)
					}
					count++
					if count == 4 {
						if !update.KeepAlive || len(update.ResourceURIs) != 0 {
							t.Fatalf("keepalive=%+v", update)
						}
						cancel()
						continue
					}
					if !slices.Equal(update.ResourceURIs, []string{uri}) {
						t.Fatalf("update=%+v", update)
					}
					h := read()
					switch count {
					case 1:
						if !h.CPUHigh || !h.MemHigh || h.CPUCount != 1 || h.CPUPerc != 85 {
							t.Fatalf("first sample=%+v", h)
						}
						// Update while delivering an earlier notification, proving rearming.
						stats.record(&v1.SystemSample{}, errors.New("sampler unavailable"))
					case 2:
						if !h.CPUHigh || !h.MemHigh || h.Error != "sampler unavailable" {
							t.Fatalf("failure=%+v", h)
						}
						sample.Time = time.Now()
						sample.CPUs[0].UsagePerc, sample.MemPerc = 70, 70
						stats.record(&sample, nil)
					case 3:
						if h.CPUHigh || h.MemHigh || h.Error != "" {
							t.Fatalf("recovery=%+v", h)
						}
					default:
						t.Fatalf("unexpected update=%+v", update)
					}
				}
				if count != 4 {
					t.Fatalf("updates=%d", count)
				}
			})
		})
		t.Run("scope_denied", func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
			if !ok {
				t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
			}
			for _, p := range []mcpPrincipal{
				{Remote: true, Scopes: []string{mcpScopeRead}},
				{Remote: true, Scopes: []string{mcpScopeTasksRead}},
				{Remote: true, TaskID: ksid.NewID(), Scopes: []string{mcpScopeServerAdmin}},
			} {
				ctx := newMCPPrincipalContext(t.Context(), &p)
				if _, err := registry.ReadResource(ctx, &mcp.ResourcesReadParams{URI: uri}); err == nil {
					t.Fatalf("read allowed for %+v", p)
				}
				if _, err := registry.SubscribeResourceUpdates(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{uri}}); err == nil {
					t.Fatalf("subscribe allowed for %+v", p)
				}
				resources, err := registry.ListResources(ctx, "")
				if err != nil {
					t.Fatal(err)
				}
				if slices.ContainsFunc(resources.Resources, func(r mcp.ResourceDescriptor) bool { return r.URI == uri }) {
					t.Fatalf("resource advertised for %+v", p)
				}
			}
		})
	})
	t.Run("SubscribeResourceUpdates/taskScopeDenied", func(t *testing.T) {
		t.Parallel()
		ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{TaskID: ksid.NewID(), Remote: true})
		if _, err := (&mcpResources{}).subscriptionSources(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"gomode://items"}}); err == nil {
			t.Fatal("task-scoped subscription succeeded")
		}
	})
}
