// Tests MCP resource visibility, limits, subscriptions, and system health.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
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
	"github.com/caic-xyz/caic/backend/internal/task"
	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"
	"github.com/maruel/gomode/mcp"
)

func TestMCPResources(t *testing.T) {
	t.Parallel()
	t.Run("BackgroundLoadedCatalog", func(t *testing.T) {
		t.Parallel()
		owned, foreign := ksid.NewID(), ksid.NewID()
		synctest.Test(t, func(t *testing.T) {
			s := newTestRouter(t, nil)
			reg, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
			if !ok {
				t.Fatalf("registry=%T", s.mcpHandlers.protocol.Registry)
			}
			ctx, cancel := context.WithCancel(newMCPPrincipalContext(auth.NewContext(t.Context(), &auth.User{ID: "owner"}), &mcpPrincipal{Remote: true, Scopes: []string{mcpScopeRead, mcpScopeTasksRead}}))
			var workers sync.WaitGroup
			t.Cleanup(func() { cancel(); workers.Wait() })
			seq, err := reg.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ResourcesListChanged: true}})
			if err != nil {
				t.Fatal(err)
			}
			updates := make(chan mcp.ResourceUpdate, 1)
			workers.Go(func() {
				for update, err := range seq {
					if err != nil {
						t.Error(err)
						return
					}
					updates <- update
					return
				}
			})
			synctest.Wait()
			if err := s.taskMgr.LoadPurgedTasks([]*taskhistory.LoadedTask{
				{TaskID: owned.String(), Prompt: "owned", Title: "Restored owned", OwnerID: "owner", State: task.StatePurged, Harness: harness.Claude},
				{TaskID: foreign.String(), Prompt: "foreign", Title: "Restored foreign", OwnerID: "foreign", State: task.StatePurged, Harness: harness.Claude},
			}); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			select {
			case update := <-updates:
				if !update.ResourcesListChanged || len(update.ResourceURIs) != 0 {
					t.Fatalf("loaded catalog=%+v", update)
				}
			default:
				t.Fatal("connected subscriber missed loaded membership")
			}
			listed, err := reg.ListResources(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			uris := make([]string, 0, len(listed.Resources))
			for _, resource := range listed.Resources {
				uris = append(uris, resource.URI)
			}
			if !slices.Contains(uris, "caic://tasks/"+owned.String()) || slices.Contains(uris, "caic://tasks/"+foreign.String()) {
				t.Fatalf("loaded catalog visibility=%v", uris)
			}
		})
	})
	t.Run("CatalogAuthorization", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct {
			name                          string
			scopes                        []string
			delegated, wantTask, wantRepo bool
		}{
			{name: "reposOnly", scopes: []string{mcpScopeRead}, wantRepo: true},
			{name: "tasksOnly", scopes: []string{mcpScopeTasksRead}, wantTask: true},
			{name: "both", scopes: []string{mcpScopeRead, mcpScopeTasksRead}, wantTask: true, wantRepo: true},
			{name: "delegated", delegated: true, wantTask: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				ownedID, foreignID, parentID, createdID := ksid.NewID(), ksid.NewID(), ksid.NewID(), ksid.NewID()
				synctest.Test(t, func(t *testing.T) {
					s := newTestRouter(t, nil)
					reg, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
					if !ok {
						t.Fatalf("registry=%T", s.mcpHandlers.protocol.Registry)
					}
					principal := &mcpPrincipal{Remote: true, Scopes: test.scopes}
					ctx := t.Context()
					if test.delegated {
						principal.TaskID = parentID
					} else {
						ctx = auth.NewContext(ctx, &auth.User{ID: "owner"})
					}
					ctx, cancel := context.WithCancel(newMCPPrincipalContext(ctx, principal))
					var workers sync.WaitGroup
					t.Cleanup(func() { cancel(); workers.Wait() })
					seq, err := reg.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ResourcesListChanged: true}})
					if err != nil {
						t.Fatal(err)
					}
					updates := make(chan mcp.ResourceUpdate, 16)
					workers.Go(func() {
						for update, err := range seq {
							if err != nil {
								t.Error(err)
								return
							}
							if update.ResourcesListChanged || len(update.ResourceURIs) > 0 {
								updates <- update
							}
						}
					})
					synctest.Wait()
					if err := s.taskMgr.LoadPurgedTasks([]*taskhistory.LoadedTask{{TaskID: foreignID.String(), OwnerID: "foreign", Prompt: "foreign", Title: "Foreign", State: task.StatePurged, Harness: harness.Claude}}); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
					select {
					case update := <-updates:
						t.Errorf("foreign restoration delivered %+v", update)
					default:
					}
					if err := s.taskMgr.LoadPurgedTasks([]*taskhistory.LoadedTask{{TaskID: ownedID.String(), OwnerID: "owner", ParentTaskID: parentID.String(), Prompt: "owned", Title: "Owned", State: task.StatePurged, Harness: harness.Claude}}); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
					select {
					case update := <-updates:
						if !test.wantTask || !update.ResourcesListChanged {
							t.Errorf("owned restoration delivered %+v", update)
						}
					default:
						if test.wantTask {
							t.Error("missing owned restoration")
						}
					}
					entry, ok := s.taskMgr.GetEntry(ownedID)
					if !ok {
						t.Fatal("missing restored task")
					}
					entry.Task().SetTitle("Renamed")
					s.taskMgr.NotifyTaskUpdate(ownedID)
					synctest.Wait()
					select {
					case update := <-updates:
						if !test.wantTask || !update.ResourcesListChanged {
							t.Errorf("rename delivered %+v", update)
						}
					default:
						if test.wantTask {
							t.Error("missing owned rename")
						}
					}
					created := mustNewTask(t, createdID, agent.Prompt{Text: "newly created"}, harness.Claude)
					created.OwnerID = "owner"
					created.ParentTaskID = parentID
					insertTestTask(s, createdID, created)
					synctest.Wait()
					select {
					case update := <-updates:
						if !test.wantTask || !update.ResourcesListChanged {
							t.Errorf("create delivered %+v", update)
						}
					default:
						if test.wantTask {
							t.Error("missing owned create")
						}
					}
					listed, err := reg.ListResources(ctx, "")
					if err != nil {
						t.Fatal(err)
					}
					var foundOwned bool
					for _, descriptor := range listed.Resources {
						if descriptor.URI == "caic://tasks/"+foreignID.String() {
							t.Error("foreign task was discoverable")
						}
						if descriptor.URI == "caic://tasks/"+createdID.String() {
							foundOwned = true
						}
					}
					if foundOwned != test.wantTask {
						t.Errorf("discovery disagrees with catalog authorization: owned=%v", foundOwned)
					}
					// A detail-only update after restoration must remain quiet.
					s.taskMgr.NotifyTaskUpdate(ownedID)
					synctest.Wait()
					select {
					case update := <-updates:
						t.Errorf("detail delivered %+v", update)
					default:
					}
					checkout := newRouterTestCheckout(t.TempDir())
					checkout.RelPath = "independent-repository"
					if err := s.serverHandlers.checkouts.RegisterCheckout(checkout); err != nil {
						t.Fatal(err)
					}
					synctest.Wait()
					select {
					case update := <-updates:
						if !test.wantRepo || !update.ResourcesListChanged {
							t.Errorf("repository catalog=%+v", update)
						}
					default:
						if test.wantRepo {
							t.Error("missing repository catalog")
						}
					}
				})
			})
		}
	})
	t.Run("SubscribeResourceUpdates/conversationFocus", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			s := newTestRouter(t, nil)
			registry, ok := s.mcpHandlers.protocol.Registry.(mcp.SubscriptionRegistry)
			if !ok {
				t.Fatalf("registry type=%T", s.mcpHandlers.protocol.Registry)
			}
			ctx, cancel := context.WithCancel(t.Context())
			var workers sync.WaitGroup
			updates := make(map[string]chan mcp.ResourceUpdate)
			for _, session := range []string{"tab-a", "tab-b"} {
				changes, err := registry.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{
					Meta:          mcp.RequestMeta{OpenAIRequestMeta: mcp.OpenAIRequestMeta{Session: session}},
					Notifications: mcp.SubscriptionFilter{ResourceSubscriptions: []string{"caic://conversation/context", "gomode://notifications"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				ch := make(chan mcp.ResourceUpdate, 1)
				updates[session] = ch
				workers.Go(func() {
					for update, err := range changes {
						if err != nil {
							t.Error(err)
							return
						}
						ch <- update
						return
					}
				})
			}
			t.Cleanup(func() { cancel(); workers.Wait() })
			synctest.Wait()
			testTaskHandlers(s).taskSvc.focusTask(withMCPClientSession(ctx, "tab-a"), ksid.NewID())
			synctest.Wait()
			select {
			case update := <-updates["tab-a"]:
				if !slices.Equal(update.ResourceURIs, []string{"caic://conversation/context", "gomode://notifications"}) {
					t.Fatalf("conversation focus update=%+v", update)
				}
			default:
				t.Fatal("tab-a did not receive its focus update")
			}
			select {
			case update := <-updates["tab-b"]:
				t.Fatalf("tab-b received another conversation's focus update: %+v", update)
			default:
			}
			cancel()
			workers.Wait()
			synctest.Wait()
			svc := testTaskHandlers(s).taskSvc
			svc.focusMu.Lock()
			defer svc.focusMu.Unlock()
			if len(svc.focusSubscribers) != 0 {
				t.Fatalf("focus subscribers after cancellation=%d", len(svc.focusSubscribers))
			}
		})
	})
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
	t.Run("SubscribeResourceUpdates/cancelsFocusWithoutIteration", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			s := newTestRouter(t, nil)
			registry, ok := s.mcpHandlers.protocol.Registry.(*mcpRegistry)
			if !ok {
				t.Fatalf("registry type = %T", s.mcpHandlers.protocol.Registry)
			}
			ctx, cancel := context.WithCancel(t.Context())
			_, err := registry.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ResourceSubscriptions: []string{"caic://conversation/context"}}})
			if err != nil {
				t.Fatal(err)
			}
			svc := testTaskHandlers(s).taskSvc
			svc.focusMu.Lock()
			registered := len(svc.focusSubscribers)
			svc.focusMu.Unlock()
			if registered != 1 {
				t.Fatalf("focus subscribers = %d, want 1", registered)
			}
			cancel()
			time.Sleep(time.Millisecond)
			svc.focusMu.Lock()
			registered = len(svc.focusSubscribers)
			svc.focusMu.Unlock()
			if registered != 0 {
				t.Fatalf("focus subscribers after cancellation = %d, want 0", registered)
			}
		})
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
				changes, err := registry.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ResourceSubscriptions: []string{uri}}})
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
				if _, err := registry.SubscribeResourceUpdates(ctx, &mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ResourceSubscriptions: []string{uri}}}); err == nil {
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
		if _, err := (&mcpResources{}).subscriptionSources(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"caic://catalog"}}); err == nil {
			t.Fatal("task-scoped subscription succeeded")
		}
	})
}

// BenchmarkMCPResources measures notification reads including authorized capture,
// quota projection, owner reconciliation and wire JSON.
func BenchmarkMCPResources(b *testing.B) {
	s := newTestRouter(b, nil)
	for range 20 {
		tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark"}, harness.Claude)
		tk.SetState(task.StateWaiting)
		insertTestTask(s, tk.ID, tk)
	}
	reg := s.mcpHandlers.protocol.Registry
	request := &mcp.ResourcesReadParams{URI: "gomode://notifications"}
	if _, err := reg.ReadResource(b.Context(), request); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := reg.ReadResource(b.Context(), request); err != nil {
			b.Fatal(err)
		}
	}
}
