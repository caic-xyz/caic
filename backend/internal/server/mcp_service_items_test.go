// Tests for caic's generic Go Mode service-item projection.

package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/maruel/gomode/mcp"
	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestServiceItems(t *testing.T) {
	t.Parallel()

	t.Run("renumbered running tasks update native voice references", func(t *testing.T) {
		t.Parallel()
		tasks := []v1.Task{
			{ID: ksid.NewID(), Title: "First work", State: v1.TaskStateRunning},
			{ID: ksid.NewID(), Title: "Second work", State: v1.TaskStateRunning},
			{ID: ksid.NewID(), Title: "Third work", State: v1.TaskStateRunning},
		}
		id := tasks[1].ID.String()
		for _, tc := range []struct {
			name      string
			tasks     []v1.Task
			reference string
			number    int
		}{
			{"baseline", tasks, "Task #2", 2},
			{"earlier task removed", tasks[1:], "Task #1", 1},
			{"earlier task inactive", []v1.Task{tasks[1], tasks[2], {ID: tasks[0].ID, Title: tasks[0].Title, State: v1.TaskStateStopped}}, "Task #1", 1},
			{"only remaining task", tasks[1:2], "", 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				out, err := boundedServiceItems(tc.tasks, mcpResourceJSONMaxBytes, id)
				if err != nil {
					t.Fatal(err)
				}
				if out.ActiveTaskID != id || out.ActiveTaskNumber != tc.number {
					t.Fatalf("active task = %q #%d, want %q #%d", out.ActiveTaskID, out.ActiveTaskNumber, id, tc.number)
				}
				i := slices.IndexFunc(out.Items, func(item serviceItem) bool { return item.ID == id })
				if i < 0 {
					t.Fatal("running task missing from native voice items")
				}
				item := out.Items[i]
				if item.Reference != tc.reference || item.State != "running" || item.Title != "Second work" || !item.Active {
					t.Fatalf("native voice item = %#v, want running Second work with reference %q", item, tc.reference)
				}
			})
		}
	})

	t.Run("prioritizes attention while preserving references", func(t *testing.T) {
		t.Parallel()
		tasks := []v1.Task{
			{ID: ksid.NewID(), Title: "Build feature", State: v1.TaskStateRunning},
			{ID: ksid.NewID(), Title: "Review plan", State: v1.TaskStateHasPlan},
			{ID: ksid.NewID(), Title: "Fix tests", State: v1.TaskStateStopped, Error: "lint failed"},
		}

		items := serviceItems(tasks, "")
		if len(items) != len(tasks) {
			t.Fatalf("item count = %d, want %d", len(items), len(tasks))
		}
		if !items[0].NeedsAttention || !items[1].NeedsAttention {
			t.Fatalf("attention items = %#v", items)
		}
		if items[2].NeedsAttention {
			t.Fatal("running item needs attention")
		}
		if items[1].State != string(v1.TaskStateHasPlan) {
			t.Fatalf("item state = %q, want %q", items[1].State, v1.TaskStateHasPlan)
		}
		if got := []string{items[0].Reference, items[1].Reference, items[2].Reference}; !slices.Equal(got, []string{"Task #3", "Task #2", "Task #1"}) {
			t.Fatalf("stable task references after priority ordering = %q", got)
		}
	})

	t.Run("falls back to the task ID title and omits a lone task reference", func(t *testing.T) {
		t.Parallel()
		task := v1.Task{ID: ksid.NewID(), State: v1.TaskStateRunning}

		items := serviceItems([]v1.Task{task}, "")

		if got := items[0].Title; got != task.ID.String() {
			t.Fatalf("title = %q, want task ID %q", got, task.ID)
		}
		if got := items[0].Reference; got != "" {
			t.Fatalf("reference = %q, want empty for a lone task", got)
		}
	})

	t.Run("marks the focused task and reports its snapshot number", func(t *testing.T) {
		t.Parallel()
		tasks := []v1.Task{
			{ID: ksid.NewID(), Title: "Build feature", State: v1.TaskStateRunning},
			{ID: ksid.NewID(), Title: "Review plan", State: v1.TaskStateHasPlan},
			{ID: ksid.NewID(), Title: "Fix tests", State: v1.TaskStateStopped},
		}
		activeID := tasks[1].ID.String()

		items := serviceItems(tasks, activeID)
		var active []string
		for _, item := range items {
			if item.Active {
				active = append(active, item.ID)
			}
		}
		if !slices.Equal(active, []string{activeID}) {
			t.Fatalf("active items = %q, want %q", active, activeID)
		}

		output, err := boundedServiceItems(tasks, mcpResourceJSONMaxBytes, activeID)
		if err != nil {
			t.Fatal(err)
		}
		if output.ActiveTaskID != activeID || output.ActiveTaskNumber != 2 {
			t.Fatalf("active task = %q #%d, want %q #2", output.ActiveTaskID, output.ActiveTaskNumber, activeID)
		}

		data, err := json.Marshal(output)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["activeTaskId"] != activeID || decoded["activeTaskNumber"] != float64(2) {
			t.Fatalf("active task JSON = %s", data)
		}
	})
}

func TestBoundedServiceItems(t *testing.T) {
	t.Parallel()

	t.Run("count limit preserves priority", func(t *testing.T) {
		t.Parallel()
		tasks := make([]v1.Task, mcpServiceItemMaxCount+1)
		for i := range tasks {
			tasks[i] = v1.Task{ID: ksid.NewID(), Title: strings.Repeat("x", maxMCPTaskTitle), State: v1.TaskStateRunning}
		}
		compareTaskID := func(a, b v1.Task) int { return strings.Compare(a.ID.String(), b.ID.String()) }
		newestID := slices.MaxFunc(tasks, compareTaskID).ID
		oldestID := slices.MinFunc(tasks, compareTaskID).ID
		for i := range tasks {
			if tasks[i].ID == oldestID {
				tasks[i].State = v1.TaskStateAsking
				break
			}
		}

		output, err := boundedServiceItems(tasks, mcpResourceJSONMaxBytes, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(output.Items) != mcpServiceItemMaxCount || output.OmittedCount != 1 {
			t.Fatalf("items = %d, omitted = %d, want %d and 1", len(output.Items), output.OmittedCount, mcpServiceItemMaxCount)
		}
		if output.Items[0].ID != newestID.String() || output.Items[0].NeedsAttention {
			t.Fatalf("first item = %#v, want newest running task", output.Items[0])
		}
		if output.Items[1].ID != oldestID.String() || !output.Items[1].NeedsAttention {
			t.Fatalf("second item = %#v, want older attention task", output.Items[1])
		}
		if !strings.Contains(output.MoreItemsHint, "tasks_list") || !strings.Contains(output.MoreItemsHint, "nextCursor") {
			t.Fatalf("more-items hint = %q, want paginated task guidance", output.MoreItemsHint)
		}
	})

	t.Run("byte limit preserves omitted items", func(t *testing.T) {
		t.Parallel()
		tasks := []v1.Task{
			{ID: ksid.NewID(), Title: "one", State: v1.TaskStateRunning},
			{ID: ksid.NewID(), Title: "two", State: v1.TaskStateRunning},
			{ID: ksid.NewID(), Title: "three", State: v1.TaskStateRunning},
		}
		full, err := boundedServiceItems(tasks, mcpResourceJSONMaxBytes, "")
		if err != nil {
			t.Fatal(err)
		}
		candidate := serviceItemsOutput{Items: full.Items[:2], MoreItemsHint: full.MoreItemsHint, OmittedCount: 1}
		data, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		output, err := boundedServiceItems(tasks, len(data), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(output.Items) != 2 || output.OmittedCount != 1 {
			t.Fatalf("items = %d, omitted = %d, want 2 and 1", len(output.Items), output.OmittedCount)
		}
	})
}

func TestServiceItemsResourceFocus(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	id := ksid.NewID()
	insertTestTask(s, id, mustNewTask(t, id, agent.Prompt{Text: "focused task"}, harness.Claude))
	svc := testTaskHandlers(s).taskSvc
	svc.focusTask(t.Context(), id)

	result, err := s.mcpHandlers.protocol.Registry.ReadResource(t.Context(), &mcp.ResourcesReadParams{URI: "gomode://items"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Contents) != 1 {
		t.Fatalf("contents = %d, want 1", len(result.Contents))
	}
	var output serviceItemsOutput
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &output); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	if output.ActiveTaskID != id.String() || output.ActiveTaskNumber != 1 {
		t.Fatalf("active task = %q #%d, want %q #1", output.ActiveTaskID, output.ActiveTaskNumber, id)
	}
	if len(output.Items) != 1 || !output.Items[0].Active {
		t.Fatalf("items = %#v, want the focused item marked active", output.Items)
	}
}

func TestServiceItemsResourceSessionFocus(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	first := ksid.NewID()
	insertTestTask(s, first, mustNewTask(t, first, agent.Prompt{Text: "first session task"}, harness.Claude))
	second := ksid.NewID()
	insertTestTask(s, second, mustNewTask(t, second, agent.Prompt{Text: "second session task"}, harness.Claude))
	svc := testTaskHandlers(s).taskSvc
	svc.focusTask(withMCPClientSession(t.Context(), "tab-a"), first)
	svc.focusTask(withMCPClientSession(t.Context(), "tab-b"), second)

	read := func(session string) serviceItemsOutput {
		result, err := s.mcpHandlers.protocol.Registry.ReadResource(t.Context(), &mcp.ResourcesReadParams{
			URI:  "gomode://items",
			Meta: mcp.RequestMeta{OpenAIRequestMeta: mcp.OpenAIRequestMeta{Session: session}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var output serviceItemsOutput
		if err := json.Unmarshal([]byte(result.Contents[0].Text), &output); err != nil {
			t.Fatalf("decode items: %v", err)
		}
		return output
	}

	if got := read("tab-a"); got.ActiveTaskID != first.String() {
		t.Fatalf("tab-a active task = %q, want %q", got.ActiveTaskID, first)
	}
	if got := read("tab-b"); got.ActiveTaskID != second.String() {
		t.Fatalf("tab-b active task = %q, want %q", got.ActiveTaskID, second)
	}
	if got := read(""); got.ActiveTaskID != "" {
		t.Fatalf("ambiguous session active task = %q, want none", got.ActiveTaskID)
	}
}
