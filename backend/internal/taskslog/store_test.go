// Tests for Store's settled-task retention window and per-repo cap.

package taskslog

import (
	"strconv"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	t.Parallel()
	t.Run("Settled", func(t *testing.T) {
		t.Parallel()
		t.Run("filters_beyond_retention", func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			logs := []*LoadedTask{
				{TaskID: "old", LastStateUpdateAt: now.Add(-SettledRetention - time.Second)},
				{TaskID: "recent", LastStateUpdateAt: now.Add(-SettledRetention + time.Second)},
			}
			got := (&Store{}).Settled(logs, now)
			if len(got) != 1 || got[0].TaskID != "recent" {
				t.Fatalf("Settled() = %v, want only %q", taskIDs(got), "recent")
			}
		})
		t.Run("caps_per_repo_most_recent_first", func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			const n = MaxSettledPerRepo + 2
			logs := make([]*LoadedTask, 0, n)
			for i := range n {
				logs = append(logs, &LoadedTask{
					TaskID:            strconv.Itoa(i),
					Repos:             []RepoMount{{Name: "repo/a"}},
					LastStateUpdateAt: now.Add(time.Duration(i) * time.Second),
				})
			}
			got := (&Store{}).Settled(logs, now)
			if len(got) != MaxSettledPerRepo {
				t.Fatalf("len(Settled()) = %d, want %d", len(got), MaxSettledPerRepo)
			}
			for i, lt := range got {
				want := strconv.Itoa(n - 1 - i)
				if lt.TaskID != want {
					t.Errorf("Settled()[%d].TaskID = %q, want %q (most recent first)", i, lt.TaskID, want)
				}
			}
		})
		t.Run("caps_independently_per_repo", func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			logs := []*LoadedTask{
				{TaskID: "a1", Repos: []RepoMount{{Name: "repo/a"}}, LastStateUpdateAt: now},
				{TaskID: "b1", Repos: []RepoMount{{Name: "repo/b"}}, LastStateUpdateAt: now},
			}
			got := (&Store{}).Settled(logs, now)
			if len(got) != 2 {
				t.Fatalf("len(Settled()) = %d, want 2", len(got))
			}
		})
	})
}

func taskIDs(logs []*LoadedTask) []string {
	ids := make([]string, len(logs))
	for i, lt := range logs {
		ids[i] = lt.TaskID
	}
	return ids
}
