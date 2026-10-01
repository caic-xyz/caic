// Benchmarks runtime-scoped image refresh status lookups.

package server

import (
	"testing"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func BenchmarkImageRefreshStatus(b *testing.B) {
	r := &ImageRefresh{
		Clients: map[string]ImageWarmer{"docker": &imageWarmerFake{}},
		statuses: map[imageRefreshKey]v1.ImageRefreshStatus{
			{userID: "alice", runtime: "docker"}: {State: v1.ImageRefreshRunning},
		},
	}
	b.ReportAllocs()
	for b.Loop() {
		status, err := r.Status("alice", "docker")
		if err != nil || status.State != v1.ImageRefreshRunning {
			b.Fatalf("Status = %+v, %v", status, err)
		}
	}
}
