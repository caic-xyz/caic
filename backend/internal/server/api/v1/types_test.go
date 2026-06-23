// Unit tests for DTO types: JSON marshaling and validation contracts.

package v1

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/harness"
)

func TestTaskListEvent(t *testing.T) {
	t.Parallel()

	assertEmptyArray := func(t *testing.T, ev TaskListEvent, field string) {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		arr, ok := raw[field].([]any)
		if !ok {
			t.Fatalf("%q is not an array, got %T", field, raw[field])
		}
		if len(arr) != 0 {
			t.Errorf("%s array length = %d, want 0", field, len(arr))
		}
	}

	t.Run("emptySnapshot", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			ev   TaskListEvent
		}{
			{name: "nil", ev: TaskListEvent{Kind: "snapshot"}},
			{name: "non_nil", ev: TaskListEvent{Kind: "snapshot", Snapshot: make([]Task, 0)}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				assertEmptyArray(t, tc.ev, "snapshot")
			})
		}
	})

	t.Run("emptyRepos", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			ev   TaskListEvent
		}{
			{name: "nil", ev: TaskListEvent{Kind: "repos"}},
			{name: "non_nil", ev: TaskListEvent{Kind: "repos", Repos: make([]Repo, 0)}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				assertEmptyArray(t, tc.ev, "repos")
			})
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

func TestEventToolUse(t *testing.T) {
	t.Parallel()

	t.Run("zero input view omitted", func(t *testing.T) {
		t.Parallel()
		data, err := json.Marshal(EventToolUse{ToolUseID: "t1", Name: "Read"})
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["inputView"]; ok {
			t.Fatalf("inputView should be omitted: %s", data)
		}
	})

	t.Run("zero files omitted", func(t *testing.T) {
		t.Parallel()
		data, err := json.Marshal(EventToolInputView{
			Kind:      EventToolInputSubagents,
			Subagents: []EventSubagentSpawn{{Agent: "reviewer", Task: "Review"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["files"]; ok {
			t.Fatalf("files should be omitted: %s", data)
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
		for _, field := range []string{"autoFixOnCIFailure", "autoFixOnPROpen"} {
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

	want := map[Harness]harness.Name{
		HarnessClaude:   harness.Claude,
		HarnessCodex:    harness.Codex,
		HarnessOpenCode: harness.OpenCode,
		HarnessPi:       harness.Pi,
	}
	for dtoHarness, agentHarness := range want {
		if string(dtoHarness) != string(agentHarness) {
			t.Errorf("%s = %q, want agent harness %q", dtoHarness, dtoHarness, agentHarness)
		}
	}

	got := map[harness.Name]struct{}{}
	for dtoHarness, agentHarness := range want {
		if _, ok := got[agentHarness]; ok {
			t.Fatalf("duplicate agent harness mapping for DTO harness %q", dtoHarness)
		}
		got[agentHarness] = struct{}{}
	}
	for _, agentHarness := range []harness.Name{
		harness.Claude,
		harness.Codex,
		harness.OpenCode,
		harness.Pi,
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
		if r.Path != "/api/caic/v1/tasks/{id}/diff" {
			t.Fatalf("path = %q, want diff path", r.Path)
		}
		if len(r.QueryParams) != 1 || r.QueryParams[0] != "path" {
			t.Fatalf("QueryParams = %v, want [path]", r.QueryParams)
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
