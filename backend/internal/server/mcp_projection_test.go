// Tests shared MCP projections, caller-local timestamp isolation, pagination, and bounded output.

package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/metrics"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/ksid"
)

func TestMCPProjection(t *testing.T) {
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
		registry := &mcpTools{serverConfig: s.serverHandlers}
		result := registry.handleReposList(t.Context(), &mcp.ToolRequest[mcpRepoListArgs]{Arguments: mcpRepoListArgs{}})
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
		if _, _, err := repositorySummary(registry.serverConfig, checkout); !errors.Is(err, errMCPRepositoryPathTooLong) {
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
}

func TestMCPResourceTimezone(t *testing.T) {
	t.Parallel()
	for _, version := range []string{mcp.ProtocolVersion, "2025-06-18", "2025-11-25"} {
		for _, tc := range []struct{ zone, winter, summer string }{
			{"America/Toronto", "2099-01-19T08:30:00-05:00", "2099-07-19T09:30:00-04:00"},
			{"Asia/Kolkata", "2099-01-19T19:00:00+05:30", "2099-07-19T19:00:00+05:30"},
			{"", "2099-01-19T13:30:00Z", "2099-07-19T13:30:00Z"},
			{"Invalid/Zone", "2099-01-19T13:30:00Z", "2099-07-19T13:30:00Z"},
			{"Local", "2099-01-19T13:30:00Z", "2099-07-19T13:30:00Z"},
		} {
			t.Run(version+"/"+tc.zone, func(t *testing.T) {
				t.Parallel()
				s := newTestRouter(t, nil)
				s.usageHandlers.fetchers = []usage.ProviderFetcher{&staticUsageFetcher{quota: usage.ProviderQuota{
					Provider: agent.QuotaProviderCodex, Label: "OpenAI", AuthKind: usage.AuthKindOAuth,
					Balance: usage.QuotaBalance{Currency: "USD", Total: 12.5},
					RateLimits: []usage.QuotaRateLimit{
						{Window: "winter", Utilization: 0.12, ResetsAt: time.Date(2099, 1, 19, 13, 30, 0, 0, time.UTC)},
						{Window: "summer", Utilization: 0.25, ResetsAt: time.Date(2099, 7, 19, 13, 30, 0, 0, time.UTC)},
						{Window: "unknown", Utilization: 1},
					},
				}}}
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"caic://usage","_meta":{"openai/userLocation":{"timezone":%q}}}}`, tc.zone)
				if version == mcp.ProtocolVersion {
					body = strings.Replace(body, `"_meta":{`, `"_meta":{"io.modelcontextprotocol/protocolVersion":"`+version+`","io.modelcontextprotocol/clientInfo":{"name":"test","version":"1"},"io.modelcontextprotocol/clientCapabilities":{},`, 1)
				}
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(body))
				req.Header.Set("Mcp-Protocol-Version", version)
				if version == mcp.ProtocolVersion {
					req.Header.Set("Mcp-Method", "resources/read")
					req.Header.Set("Mcp-Name", "caic://usage")
				}
				w := httptest.NewRecorder()
				s.mcpHandlers.protocol.HandleMCP(w, req)
				var response struct {
					Result mcp.ResourcesReadResult `json:"result"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Result.Contents) != 1 {
					t.Fatalf("resource response: %s", w.Body)
				}
				var got v1.UsageResp
				text := response.Result.Contents[0].Text
				if err := json.Unmarshal([]byte(text), &got); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(text, tc.winter) || !strings.Contains(text, tc.summer) || strings.Contains(text, "0001-") {
					t.Fatalf("incorrect reset timestamps or zero timestamp emitted: %s", text)
				}
				if len(got.Providers) != 1 || got.Providers[0].Balance.Total != 12.5 || got.Providers[0].RateLimits[0].Utilization != 0.12 {
					t.Fatalf("quota or balance changed: %+v", got)
				}
			})
		}
	}
}

func TestMCPTaskInLocation(t *testing.T) {
	t.Parallel()
	winter := time.Date(2099, 1, 19, 13, 30, 0, 123456789, time.UTC)
	summer := time.Date(2099, 7, 19, 13, 30, 0, 0, time.UTC)
	task := v1.Task{
		InitialPrompt: "2099-01-19T13:30:00Z", StateUpdatedAt: winter, StartedAt: winter,
		CacheExpiresAt: summer, TurnStartedAt: summer, RateLimit: v1.TaskRateLimit{ResetsAt: summer},
		CIChecks: []v1.ForgeCheck{{QueuedAt: winter, StartedAt: summer}},
	}
	before, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	loc := mcpRequestLocation(&mcp.RequestMeta{UserLocation: mcp.OpenAIUserLocation{Timezone: "America/Toronto"}})
	got := mcpTaskInLocation(&task, loc)
	for _, stamp := range []time.Time{got.StateUpdatedAt, got.StartedAt, got.CIChecks[0].QueuedAt} {
		if !stamp.Equal(winter) || stamp.Format(time.RFC3339Nano) != "2099-01-19T08:30:00.123456789-05:00" {
			t.Fatalf("winter timestamp changed instant or lost precision: %s", stamp)
		}
	}
	for _, stamp := range []time.Time{got.CacheExpiresAt, got.TurnStartedAt, got.RateLimit.ResetsAt, got.CIChecks[0].StartedAt} {
		if !stamp.Equal(summer) || stamp.Format(time.RFC3339) != "2099-07-19T09:30:00-04:00" {
			t.Fatalf("summer timestamp changed instant: %s", stamp)
		}
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "completedAt") || got.InitialPrompt != task.InitialPrompt {
		t.Fatalf("zero time or arbitrary text altered: %s", encoded)
	}
	after, err := json.Marshal(task)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("source task mutated: %s, %v", after, err)
	}
}

func TestMCPTaskResourceTimezone(t *testing.T) {
	t.Parallel()
	s, _, id := newTaskDiffTestRouter(t)
	child := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "child"}, harness.Claude)
	child.ParentTaskID = id
	child.StartedAt = time.Date(2099, 1, 19, 13, 30, 0, 0, time.UTC)
	insertTestTask(s, child.ID, child)
	reg := s.TaskMCPScoper.ForTask(id)
	for _, tc := range []struct{ zone, expected string }{
		{"America/Toronto", "2099-01-19T08:30:00-05:00"},
		{"Asia/Kolkata", "2099-01-19T19:00:00+05:30"},
		{"", "2099-01-19T13:30:00Z"},
	} {
		params := mcp.ResourcesReadParams{
			URI: "caic://tasks/" + child.ID.String(),
			Meta: mcp.RequestMeta{OpenAIRequestMeta: mcp.OpenAIRequestMeta{
				UserLocation: mcp.OpenAIUserLocation{Timezone: tc.zone},
			}},
		}
		result, err := reg.ReadResource(t.Context(), &params)
		if err != nil {
			t.Fatal(err)
		}
		var got v1.Task
		if len(result.Contents) != 1 {
			t.Fatalf("missing task: %+v", result)
		}
		if err := json.Unmarshal([]byte(result.Contents[0].Text), &got); err != nil {
			t.Fatal(err)
		}
		if got.StartedAt.Format(time.RFC3339) != tc.expected {
			t.Fatalf("scoped resource with zone %q: %s", tc.zone, got.StartedAt)
		}
	}
}

func TestMCPUsageInLocation(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2099, 7, 19, 13, 30, 0, 0, time.UTC)
	input := v1.UsageResp{Providers: []v1.ProviderQuota{{PricingTransitionAt: stamp, RateLimits: []v1.QuotaRateLimit{{ResetsAt: stamp}, {}}}}}
	loc := mcpRequestLocation(&mcp.RequestMeta{UserLocation: mcp.OpenAIUserLocation{Timezone: "Asia/Kolkata"}})
	got := mcpUsageInLocation(input, loc)
	if got.Providers[0].PricingTransitionAt.Format(time.RFC3339) != "2099-07-19T19:00:00+05:30" || got.Providers[0].RateLimits[0].ResetsAt.Format(time.RFC3339) != "2099-07-19T19:00:00+05:30" {
		t.Fatalf("pricing and reset times use different zones: %+v", got)
	}
	if input.Providers[0].RateLimits[0].ResetsAt.Location() != time.UTC || input.Providers[0].PricingTransitionAt.Location() != time.UTC || !got.Providers[0].RateLimits[1].ResetsAt.IsZero() {
		t.Fatal("shared usage snapshot mutated or zero reset changed")
	}
}

func TestMCPNotificationsInLocation(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2099, 1, 19, 13, 30, 0, 0, time.UTC)
	input := []serviceNotification{{ID: "ready-2099-01-19T13:30:00Z", OccurredAt: stamp, ExpiresAt: stamp.Add(time.Hour)}}
	loc := mcpRequestLocation(&mcp.RequestMeta{UserLocation: mcp.OpenAIUserLocation{Timezone: "America/Toronto"}})
	got := mcpNotificationsInLocation(input, loc)
	if got[0].ID != input[0].ID || got[0].OccurredAt.Format(time.RFC3339) != "2099-01-19T08:30:00-05:00" || got[0].ExpiresAt.Format(time.RFC3339) != "2099-01-19T09:30:00-05:00" || input[0].OccurredAt.Location() != time.UTC {
		t.Fatalf("notification timestamps or source changed: %+v", got)
	}
}

func TestMCPSystemStatsTimezone(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2099, 7, 19, 13, 30, 0, 0, time.UTC)
	c := &mcpTools{
		serverConfig: &serverHandlers{systemStats: &systemStats{samples: []v1.SystemSample{{Time: stamp, MemUsed: 512}}}},
		metrics:      metrics.NewStore(metrics.Resource{ServiceName: "caic"}),
	}
	params := mcp.ToolsCallParams{
		Name: "get_system_stats", Arguments: json.RawMessage(`{}`),
		Meta: mcp.RequestMeta{OpenAIRequestMeta: mcp.OpenAIRequestMeta{
			UserLocation: mcp.OpenAIUserLocation{Timezone: "America/Toronto"},
		}},
	}
	result, err := c.CallTool(t.Context(), &params)
	if err != nil || result.IsError {
		t.Fatalf("system stats: %+v, %v", result, err)
	}
	got, ok := result.Structured.(v1.SystemStatsResp)
	if !ok || len(got.Samples) != 1 || got.Samples[0].Time.Format(time.RFC3339) != "2099-07-19T09:30:00-04:00" || got.Samples[0].MemUsed != 512 {
		t.Fatalf("system sample changed or timezone lost: %+v", result.Structured)
	}
	if c.serverConfig.systemStats.snapshot().Samples[0].Time.Location() != time.UTC {
		t.Fatal("shared system stats snapshot mutated")
	}
}
