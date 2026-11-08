// MCP resource catalogs, scoped reads, authoritative task invalidations, and subscription lifetimes.

package server

import (
	"context"
	"errors"
	"iter"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/server/data/audit"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	providerusage "github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/ksid"
)

type mcpResources struct {
	serverConfig  *serverHandlers
	taskSvc       *taskService
	usage         *usageHandlers
	notifications *notificationFeed
	audit         *auditStore
	skills        []mcp.Skill
}

// ListSkills returns the complete static MCP Skills catalog in one page.
func (m *mcpResources) ListSkills(_ context.Context, cursor string) (mcp.SkillsListResult, error) {
	if cursor != "" {
		return mcp.SkillsListResult{}, mcp.ErrInvalidParams("invalid cursor")
	}
	return mcp.SkillsListResult{
		ResultType: mcp.ResultTypeComplete,
		Skills:     m.skills,
		TTLMS:      mcp.DefaultTTLMS,
		CacheScope: mcp.CacheScopePublic,
	}, nil
}

// GetSkill returns a static MCP Skill by its SKILL.md URI.
func (m *mcpResources) GetSkill(_ context.Context, uri string) (mcp.SkillsGetResult, error) {
	for _, skill := range m.skills {
		if skill.URI == uri {
			return mcp.SkillsGetResult{
				ResultType: mcp.ResultTypeComplete,
				Skill:      skill,
				TTLMS:      mcp.DefaultTTLMS,
				CacheScope: mcp.CacheScopePublic,
			}, nil
		}
	}
	return mcp.SkillsGetResult{}, mcp.ErrInvalidParams("unknown skill: %s", uri)
}

func (m *mcpResources) ListResources(ctx context.Context, cursor string) (mcp.ResourcesListResult, error) {
	keys, err := m.resourceKeys(ctx)
	if err != nil {
		return mcp.ResourcesListResult{}, err
	}
	page, next, err := paginateMCPResourceKeys(keys, cursor)
	if err != nil {
		return mcp.ResourcesListResult{}, mcp.ErrInvalidParams("invalid cursor: %w", err)
	}
	resources := make([]mcp.ResourceDescriptor, 0, len(page))
	for resource, projectionErr := range m.resourceDescriptors(ctx, page) {
		if projectionErr != nil {
			return mcp.ResourcesListResult{}, projectionErr
		}
		resources = append(resources, resource)
	}
	return mcp.ResourcesListResult{ResultType: mcp.ResultTypeComplete, NextCursor: next, Resources: resources, TTLMS: mcp.DefaultTTLMS, CacheScope: mcp.CacheScopePrivate}, nil
}

func (m *mcpResources) Resources(ctx context.Context) iter.Seq2[mcp.ResourceDescriptor, error] {
	keys, err := m.resourceKeys(ctx)
	if err != nil {
		return func(yield func(mcp.ResourceDescriptor, error) bool) {
			yield(mcp.ResourceDescriptor{}, err)
		}
	}
	return m.resourceDescriptors(ctx, keys)
}

// ResourceTemplates returns caic's parameterized repository and task resources.
func (m *mcpResources) ResourceTemplates(context.Context) ([]mcp.ResourceTemplateDescriptor, error) {
	return []mcp.ResourceTemplateDescriptor{
		{Name: "repo", Title: "Repository", URITemplate: "caic://repos/{path}", Description: "Managed repository detail by path", MimeType: "application/json"},
		{Name: "task", Title: "Task", URITemplate: "caic://tasks/{id}", Description: "Coding task detail by task ID", MimeType: "application/json"},
	}, nil
}

func (m *mcpResources) ReadResource(ctx context.Context, params *mcp.ResourcesReadParams) (mcp.ResourcesReadResult, error) {
	ctx, err := mcpRequestSession(ctx, params.Meta.Session)
	if err != nil {
		return mcp.ResourcesReadResult{}, err
	}
	uri := params.URI
	if authResult, ok := m.authorizeResource(ctx, uri); !ok {
		m.audit.record(ctx, &audit.Event{Operation: "resources/read", Name: uri, Decision: authResult})
		return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("%s", authResult)
	}
	switch {
	case m.hasSkill(uri):
		return m.skillResource(uri), nil
	case uri == "caic://system/health":
		health, _ := m.serverConfig.systemStats.health()
		health.Time = mcpTimeInLocation(health.Time, mcpRequestLocation(&params.Meta))
		result, err := m.resourceJSON(ctx, uri, health)
		result.TTLMS = 5000
		return result, err
	case uri == "caic://usage":
		usage := m.usage.buildResp(ctx)
		return m.resourceJSON(ctx, uri, mcpUsageInLocation(usage, mcpRequestLocation(&params.Meta)))
	case uri == "gomode://items":
		taskList := m.taskSvc.taskListSnapshot(ctx)
		items, err := boundedServiceItems(taskList, mcpResourceJSONMaxBytes, m.taskSvc.focusedTaskIDString(ctx))
		if err != nil {
			return mcp.ResourcesReadResult{}, err
		}
		return m.resourceJSON(ctx, uri, items)
	case uri == "gomode://notifications":
		taskList, replays, _ := m.taskSvc.taskListSnapshotWithReplay(ctx, map[string]uint64{}, false)
		notifications := m.notifications.notifications(ctx, taskList, replays, m.usage.buildResp(ctx), m.taskSvc.focusedTaskIDString(ctx))
		return m.resourceJSON(ctx, uri, mcpNotificationsInLocation(notifications, mcpRequestLocation(&params.Meta)))
	case strings.HasPrefix(uri, "caic://repos/"):
		name, err := url.PathUnescape(strings.TrimPrefix(uri, "caic://repos/"))
		if err != nil {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("invalid repo uri: %w", err)
		}
		checkout, ok := m.serverConfig.checkouts.Checkout(name)
		if !ok {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("repo not found: %s", name)
		}
		repository, truncated, err := repositorySummary(m.serverConfig, checkout)
		if err != nil {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("%s", err)
		}
		result, err := m.resourceJSON(ctx, uri, repository)
		if truncated {
			result.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
		}
		return result, err
	case strings.HasPrefix(uri, "caic://tasks/"):
		rawID := strings.TrimPrefix(uri, "caic://tasks/")
		id, err := mcpTaskResourceID(uri)
		if err != nil {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("task not found: %s", rawID)
		}
		entry, ok := visibleTaskEntry(ctx, m.taskSvc, id)
		if !ok {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("task not found: %s", rawID)
		}
		task, err := taskDTO(ctx, entry, m.taskSvc.taskMgr, m.taskSvc.checkouts, m.taskSvc.authStore)
		if err != nil {
			return mcp.ResourcesReadResult{}, err
		}
		return m.resourceJSON(ctx, uri, mcpTaskInLocation(&task, mcpRequestLocation(&params.Meta)))
	default:
		return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("unknown resource: %s", uri)
	}
}

func (m *mcpResources) SubscribeResourceUpdates(ctx context.Context, params *mcp.SubscriptionsListenParams) (iter.Seq2[mcp.ResourceUpdate, error], error) {
	ctx, err := mcpRequestSession(ctx, params.Meta.Session)
	if err != nil {
		return nil, err
	}
	sources, err := m.subscriptionSources(ctx, params.Notifications)
	if err != nil {
		return nil, err
	}
	var focusC <-chan string
	var stopFocus func() bool
	var unsubscribeFocus func()
	if len(sources.focusResourceURIs) > 0 {
		focusC, unsubscribeFocus = m.taskSvc.subscribeTaskFocus(ctx)
		stopFocus = context.AfterFunc(ctx, unsubscribeFocus)
	}
	taskC := sources.taskC
	repoC := sources.repoC
	repoStatusC := sources.repoStatusC
	healthC := sources.healthC
	return func(yield func(mcp.ResourceUpdate, error) bool) {
		if unsubscribeFocus != nil {
			defer func() {
				stopFocus()
				unsubscribeFocus()
			}()
		}
		heartbeatTicker := time.NewTicker(subscriptionHeartbeatInterval)
		defer heartbeatTicker.Stop()
		var usageC <-chan time.Time
		if sources.usagePolling {
			usageTicker := time.NewTicker(providerusage.CacheTTL)
			defer usageTicker.Stop()
			usageC = usageTicker.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-taskC:
				// Re-arm before yielding. yield may block while another task mutation
				// closes the replacement channel; retaining the versioned snapshot
				// makes that later mutation observable after the callback returns.
				var changes []taskmgr.TaskChange
				var gap bool
				sources.taskVersion, taskC, changes, gap = m.taskSvc.taskMgr.TaskChanges(sources.taskVersion)
				if gap {
					if !yield(sources.taskUpdate(ctx, m, taskmgr.TaskChange{Catalog: true, Summary: true, Resync: true}), nil) {
						return
					}
				} else {
					// Do not coalesce distinct alert occurrences. Each committed task
					// event retains its own notification, including events during yield.
					for _, change := range changes {
						if !yield(sources.taskUpdate(ctx, m, change), nil) {
							return
						}
					}
				}
			case <-repoC:
				if !yield(sources.repoUpdate(), nil) {
					return
				}
				repoC = m.serverConfig.checkouts.Changed()
			case <-repoStatusC:
				if !yield(mcp.ResourceUpdate{ResourceURIs: sources.repoResourceURIs}, nil) {
					return
				}
				repoStatusC = m.serverConfig.repoStatus.Changed()
			case <-focusC:
				if !yield(mcp.ResourceUpdate{ResourceURIs: slices.Clone(sources.focusResourceURIs)}, nil) {
					return
				}
			case <-usageC:
				if !yield(sources.usageUpdate(), nil) {
					return
				}
			case <-healthC:
				_, healthC = m.serverConfig.systemStats.health()
				if !yield(mcp.ResourceUpdate{ResourceURIs: []string{"caic://system/health"}}, nil) {
					return
				}
			case <-heartbeatTicker.C:
				if !yield(mcp.ResourceUpdate{KeepAlive: true}, nil) {
					return
				}
			}
		}
	}, nil
}

func (m *mcpResources) subscriptionSources(ctx context.Context, filter mcp.SubscriptionFilter) (subscriptionSources, error) {
	var sources subscriptionSources
	for _, uri := range filter.ResourceSubscriptions {
		if decision, ok := m.authorizeResource(ctx, uri); !ok {
			return subscriptionSources{}, mcp.ErrInvalidParams("%s", decision)
		}
		switch {
		case strings.HasPrefix(uri, "caic://tasks/"):
			id, err := mcpTaskResourceID(uri)
			if err != nil {
				return subscriptionSources{}, mcp.ErrInvalidParams("task not found")
			}
			sources.taskVersion, sources.taskC = m.taskSvc.taskMgr.ChangeSnapshot()
			sources.taskResourceIDs = append(sources.taskResourceIDs, id)
		case strings.HasPrefix(uri, "caic://repos/"):
			sources.repoC = m.serverConfig.checkouts.Changed()
			sources.repoStatusC = m.serverConfig.repoStatus.Changed()
			sources.repoResourceURIs = append(sources.repoResourceURIs, uri)
		case uri == "caic://system/health":
			_, sources.healthC = m.serverConfig.systemStats.health()
		case uri == "gomode://items":
			sources.taskVersion, sources.taskC = m.taskSvc.taskMgr.ChangeSnapshot()
			sources.taskUpdateURIs = append(sources.taskUpdateURIs, uri)
			sources.focusResourceURIs = append(sources.focusResourceURIs, uri)
		case uri == "gomode://notifications":
			sources.taskVersion, sources.taskC = m.taskSvc.taskMgr.ChangeSnapshot()
			sources.taskUpdateURIs = append(sources.taskUpdateURIs, uri)
			sources.focusResourceURIs = append(sources.focusResourceURIs, uri)
			sources.usagePolling = true
			sources.usageResourceURIs = append(sources.usageResourceURIs, uri)
		default:
			return subscriptionSources{}, mcp.ErrInvalidParams("unsupported resource subscription: %s", uri)
		}
	}
	if filter.ResourcesListChanged {
		if !mcpHasScope(ctx, mcpScopeRead) {
			return subscriptionSources{}, mcp.ErrInvalidParams("missing required MCP scope: %s", mcpScopeRead)
		}
		sources.taskVersion, sources.taskC = m.taskSvc.taskMgr.ChangeSnapshot()
		sources.repoC = m.serverConfig.checkouts.Changed()
		sources.repoStatusC = m.serverConfig.repoStatus.Changed()
		sources.resourcesListChanged = true
	}
	return sources, nil
}

func (m *mcpResources) resourceKeys(ctx context.Context) ([]mcpResourceKey, error) {
	staticResources := mcpStaticResources()
	keys := make([]mcpResourceKey, 0, len(staticResources))
	for i := range staticResources {
		resource := &staticResources[i]
		if _, ok := m.authorizeResource(ctx, resource.URI); ok {
			keys = append(keys, mcpResourceKey{URI: resource.URI, Kind: mcpResourceStatic, Value: resource.Name})
		}
	}
	if mcpHasScope(ctx, mcpScopeRead) {
		for checkout := range m.serverConfig.checkouts.Checkouts() {
			if err := validateMCPRepositoryPath(checkout.RelPath); err != nil {
				return nil, err
			}
			keys = append(keys, mcpResourceKey{URI: "caic://repos/" + url.PathEscape(checkout.RelPath), Kind: mcpResourceRepo, Value: checkout.RelPath})
		}
	}
	_, taskScoped := taskMCPTaskID(ctx)
	if mcpHasScope(ctx, mcpScopeTasksRead) || taskScoped {
		taskKeys, _ := taskKeys(ctx, m.taskSvc)
		for _, taskKey := range taskKeys {
			keys = append(keys, mcpResourceKey{URI: "caic://tasks/" + taskKey.ID.String(), Kind: mcpResourceTask, TaskID: taskKey.ID})
		}
	}
	slices.SortFunc(keys, func(a, b mcpResourceKey) int { return strings.Compare(a.URI, b.URI) })
	return keys, nil
}

func (m *mcpResources) resourceDescriptors(ctx context.Context, keys []mcpResourceKey) iter.Seq2[mcp.ResourceDescriptor, error] {
	return func(yield func(mcp.ResourceDescriptor, error) bool) {
		staticResources := mcpStaticResources()
		for _, key := range keys {
			var resource mcp.ResourceDescriptor
			switch key.Kind {
			case mcpResourceStatic:
				for i := range staticResources {
					if staticResources[i].Name == key.Value {
						resource = staticResources[i]
						break
					}
				}
			case mcpResourceRepo:
				resource = mcp.ResourceDescriptor{URI: key.URI, Name: "repo " + key.Value, Title: key.Value, MimeType: "application/json"}
			case mcpResourceTask:
				entry, ok := visibleTaskEntry(ctx, m.taskSvc, key.TaskID)
				if !ok {
					yield(mcp.ResourceDescriptor{}, errors.New("resource list changed while paging; restart without a cursor"))
					return
				}
				title, truncated := truncateMCPTaskTitle(entry.Task().Title())
				resource = mcp.ResourceDescriptor{URI: key.URI, Name: "task " + key.Value, Title: title, MimeType: "application/json"}
				if truncated {
					resource.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
				}
			}
			if !yield(resource, nil) {
				return
			}
		}
	}
}

func (m *mcpResources) hasSkill(uri string) bool {
	for _, skill := range m.skills {
		if skill.URI == uri {
			return true
		}
	}
	return false
}

func (m *mcpResources) skillResource(uri string) mcp.ResourcesReadResult {
	return mcp.ResourcesReadResult{
		ResultType: mcp.ResultTypeComplete,
		Contents: []mcp.ResourceContent{{
			URI:      uri,
			MimeType: "text/markdown",
			Text:     mcpTasksSkillMarkdown,
		}},
		TTLMS:      mcp.DefaultTTLMS,
		CacheScope: mcp.CacheScopePublic,
	}
}

func (m *mcpResources) resourceJSON(ctx context.Context, uri string, value any) (mcp.ResourcesReadResult, error) {
	result, err := boundedResourceJSON(uri, value)
	status := "ok"
	if err != nil {
		status = "error"
	}
	m.audit.record(ctx, &audit.Event{Operation: "resources/read", Name: uri, Decision: "allow", Status: status})
	return result, err
}

func (m *mcpResources) authorizeResource(ctx context.Context, uri string) (string, bool) {
	if m.hasSkill(uri) {
		return "allow", true
	}
	if _, ok := strings.CutPrefix(uri, "caic://tasks/"); ok {
		id, err := mcpTaskResourceID(uri)
		if err != nil {
			return "task not found", false
		}
		if _, taskScoped := taskMCPTaskID(ctx); taskScoped {
			if _, ok := visibleTaskEntry(ctx, m.taskSvc, id); !ok {
				return "task not found", false
			}
			return "allow", true
		}
		if !mcpHasScope(ctx, mcpScopeTasksRead) {
			return "missing required MCP scope: " + mcpScopeTasksRead, false
		}
		if _, ok := visibleTaskEntry(ctx, m.taskSvc, id); !ok {
			return "task not found", false
		}
		return "allow", true
	}
	required := mcpScopeRead
	if uri == "caic://system/health" {
		required = mcpScopeServerAdmin
	}
	if uri == "gomode://items" || uri == "gomode://notifications" {
		required = mcpScopeTasksRead
	}
	if !mcpHasScope(ctx, required) {
		return "missing required MCP scope: " + required, false
	}
	return "allow", true
}

const subscriptionHeartbeatInterval = 15 * time.Second

type subscriptionSources struct {
	healthC      <-chan struct{}
	taskC        <-chan struct{}
	taskVersion  uint64
	repoC        <-chan struct{}
	repoStatusC  <-chan struct{}
	usagePolling bool

	resourcesListChanged bool
	taskResourceIDs      []ksid.ID
	taskUpdateURIs       []string
	focusResourceURIs    []string
	repoResourceURIs     []string
	usageResourceURIs    []string
}

func (s *subscriptionSources) taskUpdate(ctx context.Context, registry *mcpResources, change taskmgr.TaskChange) mcp.ResourceUpdate {
	global := change.TaskID == 0
	if !global {
		if change.Removed {
			if !taskAccessFromContext(ctx).canAccessOwner(change.OwnerID, change.ParentTaskID) {
				return mcp.ResourceUpdate{}
			}
		} else if _, visible := visibleTaskEntry(ctx, registry.taskSvc, change.TaskID); !visible {
			return mcp.ResourceUpdate{}
		}
	}
	var uris []string
	for _, uri := range s.taskUpdateURIs {
		if change.Resync || (uri == "gomode://items" && change.Summary) || (uri == "gomode://notifications" && change.Notifications) {
			uris = append(uris, uri)
		}
	}
	resourcesListChanged := s.resourcesListChanged && change.Catalog
	for _, id := range s.taskResourceIDs {
		if global && !change.Resync {
			continue
		}
		if !global && id != change.TaskID {
			continue
		}
		if change.Removed {
			uris = append(uris, "caic://tasks/"+id.String())
			continue
		}
		if _, ok := visibleTaskEntry(ctx, registry.taskSvc, id); ok {
			uris = append(uris, "caic://tasks/"+id.String())
		} else {
			resourcesListChanged = true
		}
	}
	return mcp.ResourceUpdate{ResourcesListChanged: resourcesListChanged, ResourceURIs: uris}
}

func (s *subscriptionSources) repoUpdate() mcp.ResourceUpdate {
	return mcp.ResourceUpdate{ResourcesListChanged: s.resourcesListChanged, ResourceURIs: s.repoResourceURIs}
}

func (s *subscriptionSources) usageUpdate() mcp.ResourceUpdate {
	return mcp.ResourceUpdate{ResourceURIs: s.usageResourceURIs}
}
