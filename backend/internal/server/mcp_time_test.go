// Tests caller-local MCP timestamps, UTC fallback, and isolation from shared snapshots.

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/metrics"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/ksid"
)

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
	c := &mcpRegistry{
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
