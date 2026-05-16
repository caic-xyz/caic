// Unit tests for DTO types: JSON marshaling and validation contracts.

package v1

import (
	"encoding/json"
	"testing"
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
