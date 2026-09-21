// Benchmarks recording measurements into the in-memory store.

package metrics_test

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/metrics"
)

func BenchmarkStoreRecord(b *testing.B) {
	cases := []struct {
		name  string
		attrs []metrics.Attr
	}{
		{name: "no_attrs"},
		{name: "one_attr", attrs: []metrics.Attr{{Key: "container.runtime", Value: "podman"}}},
		{name: "two_attrs", attrs: []metrics.Attr{
			{Key: "container.runtime", Value: "podman"},
			{Key: "forge.name", Value: "github"},
		}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			store := metrics.NewStore(metrics.Resource{ServiceName: "caic"})
			b.ReportAllocs()
			for b.Loop() {
				store.Record(b.Context(), "container.launch", metrics.OutcomeOK, metrics.Duration(1500*time.Millisecond), tc.attrs...)
			}
		})
	}
}
