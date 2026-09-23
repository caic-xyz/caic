// Benchmarks generating the Go Mode notification feed from a task snapshot.

package server

import (
	"testing"

	"github.com/maruel/ksid"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func BenchmarkNotificationFeed(b *testing.B) {
	tasks := make([]v1.Task, 100)
	for i := range tasks {
		tasks[i] = v1.Task{ID: ksid.NewID(), Title: "Build feature", State: v1.TaskStateRunning}
	}
	feed := newNotificationFeed()
	feed.notifications(b.Context(), tasks, v1.UsageResp{})
	b.ResetTimer()
	for b.Loop() {
		feed.notifications(b.Context(), tasks, v1.UsageResp{})
	}
}
