// Tests MCP task-reference decoding and exact JSON field and omission contracts.

package mcpschema

import (
	"encoding/json"
	"testing"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestWireContracts(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2099, time.January, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		value any
		json  string
	}{
		{"empty service items", ServiceItemsOutput{Items: []ServiceItem{}}, `{"items":[],"moreItemsHint":""}`},
		{"empty task list", TaskListOutput{Tasks: []TaskSummary{}}, `{"tasks":[]}`},
		{"empty alerts", []ServiceNotification{}, `[]`},
		{
			"service items with attention",
			ServiceItemsOutput{
				Items:         []ServiceItem{{ID: "task-id", Reference: "Task #2", Title: "Title", State: "waiting", NeedsAttention: true}},
				MoreItemsHint: "more", OmittedCount: 1,
			},
			`{"items":[{"id":"task-id","reference":"Task #2","title":"Title","state":"waiting","needsAttention":true}],"moreItemsHint":"more","omittedCount":1}`,
		},
		{
			"focused task list",
			TaskListOutput{Tasks: []TaskSummary{{
				Active: true, HasPendingInput: true, TaskNumber: 2, TaskID: "task-id", Title: "Title", State: v1.TaskStateWaiting, Harness: v1.HarnessClaude,
				ChangedFiles: 1, Additions: 2, Deletions: 3, DurationSeconds: 4, CostUSD: 0.5, CostKnown: true, CostComplete: true,
			}}},
			`{"tasks":[{"active":true,"hasPendingInput":true,"taskNumber":2,"taskID":"task-id","title":"Title","state":"waiting","harness":"claude","changedFiles":1,"additions":2,"deletions":3,"durationSeconds":4,"costUSD":0.5,"costKnown":true,"costComplete":true}]}`,
		},
		{
			"focused alert array",
			[]ServiceNotification{{
				ActiveTaskID: "task-id", ActiveTaskNumber: 2,
				ID: "alert-id", ItemID: "task-id", URL: "/task/@task-id", Title: "Title", Body: "Body", ExpiresAt: stamp.Add(time.Hour),
			}},
			`[{"activeTaskId":"task-id","activeTaskNumber":2,"id":"alert-id","itemId":"task-id","url":"/task/@task-id","title":"Title","body":"Body","expiresAt":"2099-01-01T13:00:00Z"}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.json {
				t.Fatalf("JSON contract changed:\n got %s\nwant %s", data, tc.json)
			}
		})
	}
}

func TestTaskReference(t *testing.T) {
	t.Parallel()
	t.Run("UnmarshalJSON", func(t *testing.T) {
		t.Parallel()

		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name string
				raw  string
				want TaskReference
			}{
				{name: "integer", raw: "3", want: "3"},
				{name: "number string", raw: `"3"`, want: "3"},
				{name: "hashtag string", raw: `"#3"`, want: "#3"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					var got TaskReference
					if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
						t.Fatalf("Unmarshal() error: %v", err)
					}
					if got != tc.want {
						t.Fatalf("Unmarshal() = %q, want %q", got, tc.want)
					}
				})
			}
		})

		t.Run("error", func(t *testing.T) {
			t.Parallel()
			var got TaskReference
			if err := json.Unmarshal([]byte(`{}`), &got); err == nil {
				t.Fatal("Unmarshal() error = nil, want error")
			}
		})
	})
}
