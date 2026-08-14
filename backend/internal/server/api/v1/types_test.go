// Unit tests for DTO types: JSON marshaling and validation contracts.

package v1

import (
	"encoding/json"
	"net/http"
	"testing"
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
