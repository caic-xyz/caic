// Tests for MCP tool and resource authorization policy.

package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/metrics"
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

func TestCaicToolRegistryHandleReposList(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	registerRouterCheckout(t, s.checkouts, "org/repo", newRouterTestCheckout(t.TempDir()))
	registerRouterCheckout(t, s.checkouts, "repo2", &repo.Checkout{BaseBranch: "develop", Dir: t.TempDir()})
	c := &mcpRegistry{serverConfig: s.serverHandlers}

	result := c.handleReposList(t.Context(), mcpRepoListArgs{})
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
	invalid := c.handleReposList(t.Context(), mcpRepoListArgs{Limit: mcpRepoPageSizeMax + 1})
	if !invalid.IsError || invalid.Meta[mcp.ToolErrorCodeMetaKey] != string(api.CodeBadRequest) {
		t.Fatalf("invalid limit result = %#v, meta = %#v, want bad request", invalid.Structured, invalid.Meta)
	}
}

func TestCaicToolRegistryHandleTasksList(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	stoppedID := ksid.NewID()
	stopped := mustNewTask(t, stoppedID, agent.Prompt{Text: "stopped"}, harness.Claude)
	stopped.SetState(taskslog.StateStopped)
	insertTestTask(s, stoppedID.String(), stopped)
	runningID := ksid.NewID()
	running := mustNewTask(t, runningID, agent.Prompt{Text: "running"}, harness.Codex)
	running.SetState(taskslog.StateRunning)
	insertTestTask(s, runningID.String(), running)
	registry := &mcpRegistry{taskSvc: testTaskHandlers(s).taskSvc}

	first := registry.handleTasksList(t.Context(), mcpTaskListArgs{Limit: 1})
	firstOutput, ok := first.Structured.(mcpTaskListOutput)
	if first.IsError || !ok {
		t.Fatalf("first page = %#v, want task-list output", first.Structured)
	}
	if len(firstOutput.Tasks) != 1 || firstOutput.Tasks[0].TaskNumber != 1 || firstOutput.Tasks[0].State != v1.TaskStateRunning || firstOutput.NextCursor == "" {
		t.Fatalf("first page = %#v, want running task and a next cursor", firstOutput)
	}

	second := registry.handleTasksList(t.Context(), mcpTaskListArgs{Cursor: firstOutput.NextCursor, Limit: 1})
	secondOutput, ok := second.Structured.(mcpTaskListOutput)
	if second.IsError || !ok {
		t.Fatalf("second page = %#v, want task-list output", second.Structured)
	}
	if len(secondOutput.Tasks) != 1 || secondOutput.Tasks[0].TaskNumber != 2 || secondOutput.Tasks[0].State != v1.TaskStateStopped || secondOutput.NextCursor != "" {
		t.Fatalf("second page = %#v, want stopped task without cursor", secondOutput)
	}
	stopped.SetState(taskslog.StateRunning)
	stale := registry.handleTasksList(t.Context(), mcpTaskListArgs{Cursor: firstOutput.NextCursor, Limit: 1})
	if !stale.IsError {
		t.Fatal("tasks_list accepted a cursor after another task changed ordering partition")
	}

	for name, args := range map[string]mcpTaskListArgs{
		"cursor": {Cursor: "invalid"},
		"limit":  {Limit: mcpTaskPageSizeMax + 1},
	} {
		t.Run("invalid "+name, func(t *testing.T) {
			t.Parallel()
			result := registry.handleTasksList(t.Context(), args)
			if !result.IsError || result.Meta[mcp.ToolErrorCodeMetaKey] != string(api.CodeBadRequest) {
				t.Fatalf("result = %#v, meta = %#v, want bad-request tool error", result.Structured, result.Meta)
			}
		})
	}
}

func TestMCPResultBounds(t *testing.T) {
	t.Parallel()

	t.Run("repository pagination survives earlier insertion", func(t *testing.T) {
		t.Parallel()

		repositories := []string{"b", "c"}
		first, next, err := paginateMCPRepositories(repositories, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(first) != 1 || first[0] != "b" || next == "" {
			t.Fatalf("first page = %#v, cursor = %q", first, next)
		}
		repositories = []string{"a", "b", "c"}
		second, next, err := paginateMCPRepositories(repositories, next, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(second) != 1 || second[0] != "c" || next != "" {
			t.Fatalf("second page = %#v, cursor = %q", second, next)
		}
	})

	t.Run("repository default page size", func(t *testing.T) {
		t.Parallel()

		paths := make([]string, mcpRepoPageSizeDefault+1)
		for i := range paths {
			paths[i] = fmt.Sprintf("repo-%03d", i)
		}
		page, next, err := paginateMCPRepositories(paths, "", mcpRepoPageSizeDefault)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != mcpRepoPageSizeDefault || next == "" {
			t.Fatalf("page length = %d, cursor = %q, want %d repositories and a cursor", len(page), next, mcpRepoPageSizeDefault)
		}
	})

	t.Run("JSON size limit shortens page without losing cursor position", func(t *testing.T) {
		t.Parallel()

		paths := []string{"a", "b", "c", "d"}
		repositories := make([]mcpRepoSummary, len(paths))
		for i, path := range paths {
			repositories[i] = mcpRepoSummary{Path: path, RemoteURL: strings.Repeat(path, 40)}
		}
		cursorForCount := func(count int) (string, error) { return mcpKeyCursor(paths[count-1]), nil }
		outputForPage := func(page []mcpRepoSummary, cursor string) any {
			return mcpRepoListOutput{Repositories: page, NextCursor: cursor}
		}
		twoItems, err := json.Marshal(outputForPage(repositories[:2], mcpKeyCursor(paths[1])))
		if err != nil {
			t.Fatal(err)
		}
		page, next, err := fitMCPJSONPage(repositories, false, len(twoItems), cursorForCount, outputForPage)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 2 || next != mcpKeyCursor("b") {
			t.Fatalf("bounded page length = %d, cursor = %q, want two items anchored at b", len(page), next)
		}
		remaining, next, err := paginateMCPRepositories(paths, next, mcpRepoPageSizeDefault)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(remaining, []string{"c", "d"}) || next != "" {
			t.Fatalf("remaining page = %#v, cursor = %q, want c and d", remaining, next)
		}
	})

	t.Run("collection resources are represented by tools and individual resources", func(t *testing.T) {
		t.Parallel()

		for _, resource := range mcpStaticResources() {
			if resource.URI == "caic://repos" || resource.URI == "caic://tasks" {
				t.Fatalf("aggregate resource %q remains in the static catalog", resource.URI)
			}
		}
	})

	t.Run("repository cursor bounds long paths", func(t *testing.T) {
		t.Parallel()

		paths := []string{strings.Repeat("a", mcpTaskCursorMaxBytes*2), "z"}
		first, cursor, err := paginateMCPRepositories(paths, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(first) != 1 || len(cursor) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
			t.Fatalf("first page = %#v, cursor length = %d", first, len(cursor))
		}
		second, next, err := paginateMCPRepositories(paths, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(second) != 1 || second[0] != "z" || next != "" {
			t.Fatalf("second page = %#v, cursor = %q", second, next)
		}
	})

	t.Run("repository cursor rejects removed anchor", func(t *testing.T) {
		t.Parallel()

		_, cursor, err := paginateMCPRepositories([]string{"a", "b"}, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := paginateMCPRepositories([]string{"b"}, cursor, 1); err == nil {
			t.Fatal("paginateMCPRepositories() accepted a removed cursor anchor")
		}
	})

	t.Run("repository fields", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		longValue := strings.Repeat("€", maxMCPRepoField)
		checkout := &repo.Checkout{
			BaseBranch:       longValue,
			BaseBranchRemote: longValue,
			Dir:              t.TempDir(),
			Repository:       &repo.Repository{Remote: "https://example.com/" + longValue},
		}
		registerRouterCheckout(t, s.checkouts, "bounded-repo", checkout)
		registry := &mcpRegistry{serverConfig: s.serverHandlers}
		result := registry.handleReposList(t.Context(), mcpRepoListArgs{})
		output, ok := result.Structured.(mcpRepoListOutput)
		if result.IsError || !ok || len(output.Repositories) != 1 {
			t.Fatalf("handleReposList() result = %#v", result)
		}
		repository := output.Repositories[0]
		if len(repository.BaseBranch.Name) > maxMCPRepoField || len(repository.BaseBranch.Remote) > maxMCPRepoField || len(repository.RemoteURL) > maxMCPRepoField {
			t.Fatalf("repository fields exceed limit: %#v", repository)
		}
		if !output.FieldsTruncated || result.Meta[mcpTruncatedMetaKey] != true {
			t.Fatalf("truncation output = %#v, metadata = %#v", output, result.Meta)
		}

		checkout.RelPath = strings.Repeat("x", maxMCPRepoField+1)
		if _, _, err := registry.repositorySummary(checkout); !errors.Is(err, errMCPRepositoryPathTooLong) {
			t.Fatalf("repositorySummary() path error = %v", err)
		}
	})

	t.Run("resource pagination survives earlier insertion", func(t *testing.T) {
		t.Parallel()

		keys := make([]mcpResourceKey, mcpResourcePageSize+1)
		for i := range keys {
			keys[i] = mcpResourceKey{URI: fmt.Sprintf("test://resource/%03d/%s", i, strings.Repeat("x", mcpTaskCursorMaxBytes*2))}
		}
		first, cursor, err := paginateMCPResourceKeys(keys, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(first) != mcpResourcePageSize || len(cursor) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
			t.Fatalf("first page length = %d, cursor = %q", len(first), cursor)
		}
		keys = append([]mcpResourceKey{{URI: "test://resource/-1"}}, keys...)
		second, next, err := paginateMCPResourceKeys(keys, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(second) != 1 || !strings.HasPrefix(second[0].URI, "test://resource/100/") || next != "" {
			t.Fatalf("second page = %#v, cursor = %q", second, next)
		}
	})

	t.Run("text boundary", func(t *testing.T) {
		t.Parallel()

		exact := strings.Repeat("a", mcpTextOutputMaxBytes)
		result := boundedTextToolResult(exact)
		output, ok := result.Structured.(mcp.TextOutput)
		if !ok {
			t.Fatalf("exact boundary result type = %T, want mcp.TextOutput", result.Structured)
		}
		if output.Result != exact || result.Meta != nil {
			t.Fatalf("exact boundary was changed: length = %d, meta = %#v", len(output.Result), result.Meta)
		}

		overflow := strings.Repeat("a", mcpTextOutputMaxBytes-1) + "€"
		result = boundedTextToolResult(overflow)
		output, ok = result.Structured.(mcp.TextOutput)
		if !ok {
			t.Fatalf("overflow result type = %T, want mcp.TextOutput", result.Structured)
		}
		if len(output.Result) > mcpTextOutputMaxBytes || !strings.Contains(output.Result, "[Output truncated.") {
			t.Fatalf("overflow result length = %d, value suffix = %q", len(output.Result), output.Result[len(output.Result)-100:])
		}
		if got := result.Meta[mcpTruncatedMetaKey]; got != true {
			t.Fatalf("truncated metadata = %#v, want true", got)
		}
		if !utf8.ValidString(output.Result) {
			t.Fatal("overflow result is not valid UTF-8")
		}
	})

	t.Run("resource overflow", func(t *testing.T) {
		t.Parallel()

		if _, err := boundedResourceJSON("caic://large", strings.Repeat("a", mcpResourceJSONMaxBytes-2)); err != nil {
			t.Fatalf("boundedResourceJSON() rejected exact boundary: %v", err)
		}
		_, err := boundedResourceJSON("caic://large", strings.Repeat("a", mcpResourceJSONMaxBytes))
		if err == nil || !strings.Contains(err.Error(), "256 KiB") {
			t.Fatalf("boundedResourceJSON() error = %v", err)
		}
	})

	t.Run("resource overflow audit", func(t *testing.T) {
		t.Parallel()

		store := &auditStore{log: testLogger()}
		registry := &mcpRegistry{audit: store}
		_, err := registry.resourceJSON(t.Context(), "caic://large", strings.Repeat("a", mcpResourceJSONMaxBytes))
		if err == nil {
			t.Fatal("resourceJSON() accepted an oversized resource")
		}
		events := store.snapshot()
		if len(events) != 1 || events[0].Status != "error" {
			t.Fatalf("audit events = %#v, want one error outcome", events)
		}
	})

	t.Run("task summary strings", func(t *testing.T) {
		t.Parallel()

		tk := v1.Task{Title: strings.Repeat("€", maxMCPTaskTitle), RequestedModel: strings.Repeat("m", maxTaskSummaryModel+1)}
		summary := taskMCPSummary(1, &tk)
		if len(summary.Title) > maxMCPTaskTitle || len(summary.Model) > maxTaskSummaryModel {
			t.Fatalf("summary string lengths = title %d, model %d", len(summary.Title), len(summary.Model))
		}
		if !utf8.ValidString(summary.Title) || !strings.HasSuffix(summary.Title, "…") || !strings.HasSuffix(summary.Model, "…") {
			t.Fatalf("summary strings were not visibly UTF-8 truncated: %#v", summary)
		}

		resolved := taskMCPSummary(1, &v1.Task{
			RequestedModel:  "requested-model",
			RequestedEffort: "high",
			ReportedModel:   "reported-model",
			ReportedEffort:  "medium",
		})
		if resolved.Model != "reported-model" || resolved.Effort != "medium" {
			t.Fatalf("resolved settings = %q/%q, want reported-model/medium", resolved.Model, resolved.Effort)
		}
	})

	t.Run("resource task title", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		id := ksid.NewID()
		tk := mustNewTask(t, id, agent.Prompt{Text: "task"}, harness.Claude)
		tk.SetTitle(strings.Repeat("€", maxMCPTaskTitle))
		insertTestTask(s, id.String(), tk)
		registry := &mcpRegistry{taskSvc: testTaskHandlers(s).taskSvc}
		keys := []mcpResourceKey{{URI: "caic://tasks/" + id.String(), Kind: mcpResourceTask, Value: id.String()}}
		var yielded bool
		for resource, err := range registry.resourceDescriptors(t.Context(), keys) {
			yielded = true
			if err != nil {
				t.Fatal(err)
			}
			if len(resource.Title) > maxMCPTaskTitle || !utf8.ValidString(resource.Title) {
				t.Fatalf("resource title length = %d, valid UTF-8 = %t", len(resource.Title), utf8.ValidString(resource.Title))
			}
			if resource.Meta[mcpTruncatedMetaKey] != true {
				t.Fatalf("resource metadata = %#v, want truncation marker", resource.Meta)
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
		registry := &mcpRegistry{serverConfig: s.serverHandlers}

		ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: []string{mcpScopeRead}, Remote: true})
		if _, err := registry.ListResources(ctx, ""); !errors.Is(err, errMCPRepositoryPathTooLong) {
			t.Fatalf("ListResources() error = %v, want %v", err, errMCPRepositoryPathTooLong)
		}

		checkout.RelPath = string([]byte{0xff})
		if _, err := registry.ListResources(ctx, ""); !errors.Is(err, errMCPRepositoryPathInvalidUTF8) || strings.Contains(err.Error(), "512") {
			t.Fatalf("ListResources() UTF-8 error = %v, want %v without a size diagnosis", err, errMCPRepositoryPathInvalidUTF8)
		}
	})
}

func TestCaicToolRegistryHandleTaskCreate(t *testing.T) {
	t.Parallel()

	t.Run("bounds generated title", func(t *testing.T) {
		t.Parallel()

		s := newMCPTaskCreateTestRouter(t)
		registry := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := registry.handleTaskCreate(t.Context(), mcpTaskCreateArgs{
			Prompt: strings.Repeat("€", maxMCPTaskTitle),
			Repos:  []string{"myrepo"},
		})
		output, ok := result.Structured.(mcpTaskCreatedOutput)
		if result.IsError || !ok {
			t.Fatalf("handleTaskCreate() result = %#v", result)
		}
		const prefix = "Created task #1: "
		if !strings.HasPrefix(output.Result, prefix) || len(strings.TrimPrefix(output.Result, prefix)) > maxMCPTaskTitle {
			t.Fatalf("created task result length = %d", len(output.Result))
		}
		if result.Meta[mcpTruncatedMetaKey] != true || !utf8.ValidString(output.Result) {
			t.Fatalf("created task metadata = %#v, valid UTF-8 = %t", result.Meta, utf8.ValidString(output.Result))
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
		if got := created.RequestedModel; got != "" {
			t.Fatalf("created model = %q, want empty", got)
		}
		if created.RequestedEffort != "" {
			t.Fatalf("created effort = %q, want empty", created.RequestedEffort)
		}
	})
}

func TestCaicToolRegistryHandleTaskCreateUnknownRepository(t *testing.T) {
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
			c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: tt.scopes, Remote: true})
			ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})

			result := c.handleTaskCreate(ctx, mcpTaskCreateArgs{Prompt: "do the task", Repos: []string{"mistyped"}})
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
}

func TestCaicToolRegistryHandleBotFixCIUnknownRepository(t *testing.T) {
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
			c := &mcpRegistry{ci: s.ciHandlers}
			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: tt.scopes, Remote: true})
			ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})

			result := c.handleBotFixCI(ctx, mcpBotFixCIArgs{Repo: "mistyped"})
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
}

func TestCaicToolRegistryHandleTaskCreateErrors(t *testing.T) {
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
				want:     "unsupported model for claude: mistyped. Omit model to use the selected harness's default model, then retry task_create.",
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
				c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
				result := c.handleTaskCreate(t.Context(), tt.args)
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
		c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := c.handleTaskCreate(t.Context(), mcpTaskCreateArgs{
			Prompt:  "do the task",
			Repos:   []string{"myrepo"},
			Harness: "mistyped",
			Model:   "pi-default",
		})
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
		c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := c.handleTaskCreate(t.Context(), mcpTaskCreateArgs{
			Prompt:      "do the task",
			Repos:       []string{"myrepo"},
			Harness:     "claude",
			RuntimeName: "mistyped",
		})
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
		c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := c.handleTaskCreate(t.Context(), mcpTaskCreateArgs{Prompt: "do the task", Repos: []string{"myrepo"}})
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
		c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
		result := c.handleTaskCreate(t.Context(), mcpTaskCreateArgs{Prompt: "do the task", Repos: []string{"myrepo"}})
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
}

func TestCaicToolRegistryToolErrorCodes(t *testing.T) {
	t.Parallel()

	s := newMCPTaskCreateTestRouter(t)
	c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
	t.Run("malformed task request", func(t *testing.T) {
		t.Parallel()

		result := c.handleTaskCreate(t.Context(), mcpTaskCreateArgs{Repos: []string{"myrepo"}})
		assertMCPToolErrorCode(t, result, api.CodeBadRequest)
	})
	t.Run("invalid task number", func(t *testing.T) {
		t.Parallel()

		result := c.handleTaskGetDetail(t.Context(), mcpTaskNumberArgs{TaskNumber: -1})
		assertMCPToolErrorCode(t, result, api.CodeBadRequest)
	})
	t.Run("missing task", func(t *testing.T) {
		t.Parallel()

		result := c.handleTaskGetDetail(t.Context(), mcpTaskNumberArgs{TaskNumber: 1})
		assertMCPToolErrorCode(t, result, api.CodeNotFound)
	})
	t.Run("unclassified failure", func(t *testing.T) {
		t.Parallel()

		result := domainToolError[mcp.TextOutput](errors.New("backend failure"))
		assertMCPToolErrorCode(t, result, api.CodeInternalError)
	})
}

func assertMCPToolErrorCode[T any](t *testing.T, result mcp.ToolResult[T], want api.ErrorCode) {
	if !result.IsError {
		t.Fatal("tool result is not an error")
	}
	if got := result.Meta[mcp.ToolErrorCodeMetaKey]; got != string(want) {
		t.Errorf("error code metadata = %q, want %q", got, want)
	}
}

func TestCaicToolRegistryHandleTaskForkSelectionErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     mcpTaskForkArgs
		want     string
		wantCode api.ErrorCode
	}{
		{
			name:     "unknown override harness",
			args:     mcpTaskForkArgs{TaskNumber: 1, Prompt: "fork", Harness: "mistyped"},
			want:     `unsupported harness "mistyped". Omit harness to inherit the source task's harness, then retry task_fork.`,
			wantCode: api.CodeUnknownHarness,
		},
		{
			name:     "unsupported override model",
			args:     mcpTaskForkArgs{TaskNumber: 1, Prompt: "fork", Harness: "claude", Model: "mistyped"},
			want:     "unsupported model for claude: mistyped. Omit model to inherit the source task's model, then retry task_fork.",
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
			insertTestTask(s, tk.ID.String(), tk)
			c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}

			result := c.handleTaskFork(t.Context(), tt.args)
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
}

func TestCaicToolRegistryHandleTaskForkDoesNotSuggestUnsafeRecovery(t *testing.T) {
	t.Parallel()

	s := newMCPTaskCreateTestRouter(t)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "source"}, harness.Claude)
	tk.Repos = []taskslog.RepoMount{{Name: "myrepo", Branch: "main"}}
	tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "source"), runtime.ConnectionTarget{}, "", "", 0)
	tk.SetState(taskslog.StateWaiting)
	insertTestTask(s, tk.ID.String(), tk)
	c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
	result := c.handleTaskFork(t.Context(), mcpTaskForkArgs{TaskNumber: 1, Prompt: "fork", Harness: "mistyped", Model: "pi-default"})
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
}

func TestCaicToolRegistryHandleTaskForkDoesNotSuggestRecoveryForRetiredSourceModel(t *testing.T) {
	t.Parallel()

	s := newMCPTaskCreateTestRouter(t)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "source"}, harness.Claude)
	tk.RequestedModel = "retired-model"
	tk.Repos = []taskslog.RepoMount{{Name: "myrepo", Branch: "main"}}
	tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "source"), runtime.ConnectionTarget{}, "", "", 0)
	tk.SetState(taskslog.StateWaiting)
	insertTestTask(s, tk.ID.String(), tk)
	c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
	result := c.handleTaskFork(t.Context(), mcpTaskForkArgs{TaskNumber: 1, Prompt: "fork", Harness: "mistyped"})
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
}

func TestCaicToolRegistryHandleTaskForkDoesNotSuggestUnsafeModelRecovery(t *testing.T) {
	t.Parallel()

	s := newMCPTaskCreateTestRouter(t)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "source"}, harness.Claude)
	tk.RequestedModel = "claude-default"
	tk.Repos = []taskslog.RepoMount{{Name: "myrepo", Branch: "main"}}
	tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "source"), runtime.ConnectionTarget{}, "", "", 0)
	tk.SetState(taskslog.StateWaiting)
	insertTestTask(s, tk.ID.String(), tk)
	c := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
	result := c.handleTaskFork(t.Context(), mcpTaskForkArgs{TaskNumber: 1, Prompt: "fork", Harness: "pi", Model: "mistyped"})
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
}

func TestCaicToolRegistryHandleCloneRepoPathConflict(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "taken"), 0o750); err != nil {
		t.Fatalf("make existing clone directory: %v", err)
	}
	s := newCheckoutConstructionTestServer(t, root).server
	registry := &mcpRegistry{serverConfig: s.serverHandlers}

	result := registry.handleCloneRepo(t.Context(), mcpCloneRepoArgs{URL: "https://example.com/repo.git", Path: "taken"})
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

	t.Run("scope denial supplies an authentication challenge and code", func(t *testing.T) {
		t.Parallel()

		c := &mcpRegistry{metrics: metrics.NewStore(metrics.Resource{ServiceName: "caic"})}
		ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Remote: true})
		ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})
		result, err := c.CallTool(ctx, "task_create", nil)
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

	t.Run("task scoped client exposes only prompt-only creation", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_schema", func(t *testing.T) {
			t.Parallel()
			registry := &mcpRegistry{}
			scopedRegistry := registry.ForTask(ksid.NewID())
			tools, err := scopedRegistry.Tools(t.Context())
			if err != nil {
				t.Fatalf("Tools() error: %v", err)
			}
			if len(tools) != 1 || tools[0].Name != "task_create" {
				t.Fatalf("Tools() = %#v, want only task_create", tools)
			}
			schema, err := json.Marshal(tools[0].InputSchema)
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
		})
		t.Run("error_extra_task_properties", func(t *testing.T) {
			t.Parallel()
			registry := &mcpRegistry{metrics: metrics.NewStore(metrics.Resource{ServiceName: "caic"})}
			scopedRegistry := registry.ForTask(ksid.NewID())
			result, err := scopedRegistry.CallTool(t.Context(), "task_create", json.RawMessage(`{"prompt":"child","repos":["repo"]}`))
			if err != nil {
				t.Fatalf("CallTool() error: %v", err)
			}
			if !result.IsError {
				t.Fatal("task-scoped create with repos succeeded, want rejection")
			}
		})
	})

	t.Run("task scoped client cannot read or subscribe", func(t *testing.T) {
		t.Parallel()
		ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{TaskID: ksid.NewID(), Remote: true})
		registry := &mcpRegistry{}
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("voice_session_defaults", func(t *testing.T) {
				t.Parallel()
				if got := registry.voiceSessionDefaults(ctx); got != "" {
					t.Fatalf("voiceSessionDefaults() = %q, want no task-scoped preference data", got)
				}
				readOnlyCtx := newMCPPrincipalContext(ctx, &mcpPrincipal{Scopes: []string{mcpScopeRead}, Remote: true})
				readOnlyCtx = auth.NewContext(readOnlyCtx, &auth.User{ID: "user-1"})
				if got := registry.voiceSessionDefaults(readOnlyCtx); got != "" {
					t.Fatalf("voiceSessionDefaults() = %q, want no read-only preference data", got)
				}
			})
			t.Run("resource_subscription", func(t *testing.T) {
				t.Parallel()
				if _, err := registry.subscriptionSources(ctx, mcp.SubscriptionFilter{ResourceSubscriptions: []string{"gomode://items"}}); err == nil {
					t.Fatal("task-scoped subscription succeeded")
				}
			})
		})
	})

	t.Run("read scope subscribes to resource-list changes", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			s := newTestRouter(t, nil)
			registry := &mcpRegistry{serverConfig: s.serverHandlers, taskSvc: testTaskHandlers(s).taskSvc}
			ctx := newMCPPrincipalContext(t.Context(), &mcpPrincipal{Scopes: []string{mcpScopeRead}, Remote: true})
			ctx = auth.NewContext(ctx, &auth.User{ID: "user-1"})
			if _, err := registry.subscriptionSources(ctx, mcp.SubscriptionFilter{ResourcesListChanged: true}); err != nil {
				t.Fatalf("subscriptionSources() error: %v", err)
			}
		})
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
