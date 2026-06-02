// Unit tests for DTO types: JSON marshaling and validation contracts.

package v1

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/forge"
)

func TestTaskListEvent(t *testing.T) {
	t.Parallel()

	t.Run("emptySnapshot", func(t *testing.T) {
		t.Parallel()
		// Regression: when there are no tasks, Snapshot is make([]Task, 0)
		// (non-nil empty slice). omitempty omits empty slices, but
		// omitzero (Go 1.24+) only omits the zero value (nil for slices),
		// so the empty array is always marshaled.
		ev := TaskListEvent{
			Kind:     "snapshot",
			Snapshot: make([]Task, 0),
		}
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["snapshot"]; !ok {
			t.Error(`"snapshot" field is missing from JSON (zero-value nil was not used)`)
		}
		arr, ok := raw["snapshot"].([]any)
		if !ok {
			t.Fatalf(`"snapshot" is not an array, got %T`, raw["snapshot"])
		}
		if len(arr) != 0 {
			t.Errorf("snapshot array length = %d, want 0", len(arr))
		}
	})

	t.Run("emptyRepos", func(t *testing.T) {
		t.Parallel()
		// Same regression check for the Repos field with omitzero.
		ev := TaskListEvent{
			Kind:  "repos",
			Repos: make([]Repo, 0),
		}
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["repos"]; !ok {
			t.Error(`"repos" field is missing from JSON (zero-value nil was not used)`)
		}
	})

	t.Run("nilOmitted", func(t *testing.T) {
		t.Parallel()
		// Non-snapshot events have zero-valued (nil) snapshot/repos, which
		// omitzero correctly omits.
		ev := TaskListEvent{
			Kind:   "delete",
			Delete: "abc123",
		}
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"snapshot", "upsert", "patch", "repos", "warning"} {
			if _, ok := raw[f]; ok {
				t.Errorf("%q field should be omitted but is present", f)
			}
		}
		if raw["delete"] != "abc123" {
			t.Errorf(`delete = %v, want "abc123"`, raw["delete"])
		}
	})
}

func TestUserSettings(t *testing.T) {
	t.Parallel()

	t.Run("falseSettingsRoundTrip", func(t *testing.T) {
		t.Parallel()
		data, err := json.Marshal(UserSettings{})
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"autoFixOnCIFailure", "autoFixOnPROpen", "useDefaultCaches"} {
			got, ok := raw[field]
			if !ok {
				t.Fatalf("%q missing from JSON", field)
			}
			if got != false {
				t.Fatalf("%q = %v, want false", field, got)
			}
		}
	})
}

func TestHarness(t *testing.T) {
	t.Parallel()

	want := map[Harness]agent.Harness{
		HarnessClaude:   agent.Claude,
		HarnessCodex:    agent.Codex,
		HarnessGemini:   agent.Gemini,
		HarnessKilo:     agent.Kilo,
		HarnessOpenCode: agent.OpenCode,
		HarnessPi:       agent.Pi,
	}
	for dtoHarness, agentHarness := range want {
		if string(dtoHarness) != string(agentHarness) {
			t.Errorf("%s = %q, want agent harness %q", dtoHarness, dtoHarness, agentHarness)
		}
	}

	got := map[agent.Harness]struct{}{}
	for dtoHarness, agentHarness := range want {
		if _, ok := got[agentHarness]; ok {
			t.Fatalf("duplicate agent harness mapping for DTO harness %q", dtoHarness)
		}
		got[agentHarness] = struct{}{}
	}
	for _, agentHarness := range []agent.Harness{
		agent.Claude,
		agent.Codex,
		agent.Gemini,
		agent.Kilo,
		agent.OpenCode,
		agent.Pi,
	} {
		if _, ok := got[agentHarness]; !ok {
			t.Errorf("agent harness %q is missing from DTO harness constants", agentHarness)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("agent harness mapping count = %d, want %d", len(got), len(want))
	}
}

func TestForge(t *testing.T) {
	t.Parallel()

	want := map[Forge]forge.Kind{
		ForgeGitHub: forge.KindGitHub,
		ForgeGitLab: forge.KindGitLab,
	}
	for dtoForge, forgeKind := range want {
		if string(dtoForge) != string(forgeKind) {
			t.Errorf("%s = %q, want forge kind %q", dtoForge, dtoForge, forgeKind)
		}
	}

	got := map[forge.Kind]struct{}{}
	for dtoForge, forgeKind := range want {
		if _, ok := got[forgeKind]; ok {
			t.Fatalf("duplicate forge kind mapping for DTO forge %q", dtoForge)
		}
		got[forgeKind] = struct{}{}
	}
	for _, forgeKind := range []forge.Kind{
		forge.KindGitHub,
		forge.KindGitLab,
	} {
		if _, ok := got[forgeKind]; !ok {
			t.Errorf("forge kind %q is missing from DTO forge constants", forgeKind)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("forge kind mapping count = %d, want %d", len(got), len(want))
	}
}

func TestRoutes(t *testing.T) {
	t.Parallel()

	t.Run("getTaskDiffDeclaresPathQuery", func(t *testing.T) {
		t.Parallel()
		r := routeByName(t, "getTaskDiff")
		if r.Method != http.MethodGet {
			t.Fatalf("method = %q, want GET", r.Method)
		}
		if r.Path != "/api/v1/tasks/{id}/diff" {
			t.Fatalf("path = %q, want diff path", r.Path)
		}
		if len(r.QueryParams) != 1 || r.QueryParams[0] != "path" {
			t.Fatalf("QueryParams = %v, want [path]", r.QueryParams)
		}
	})

	t.Run("closeVoiceRTCDeclared", func(t *testing.T) {
		t.Parallel()
		r := routeByName(t, "closeVoiceRTC")
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		if r.Path != "/api/v1/voice/rtc/{sessionID}" {
			t.Fatalf("path = %q, want voice RTC close path", r.Path)
		}
		if r.RespName() != "StatusResp" {
			t.Fatalf("response = %q, want StatusResp", r.RespName())
		}
	})
}

func routeByName(t *testing.T, name string) Route {
	for i := range Routes {
		if Routes[i].Name == name {
			return Routes[i]
		}
	}
	t.Fatalf("route %q not found", name)
	return Route{}
}
