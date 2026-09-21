// Benchmarks appending observations to the durable per-day metrics log.

package metricsdb

import (
	"log/slog"
	"testing"
	"time"

	"github.com/caic-xyz/caic/metrics"
)

func BenchmarkLogRecord(b *testing.B) {
	cases := []struct {
		name string
		attr []metrics.Attr
	}{
		{name: "no_attrs"},
		{name: "one_attr", attr: []metrics.Attr{{Key: "container.runtime", Value: "podman"}}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			log, err := NewLog(slog.New(slog.DiscardHandler), b.TempDir(), metrics.Resource{ServiceName: "caic"})
			if err != nil {
				b.Fatalf("NewLog: %v", err)
			}
			b.Cleanup(func() { _ = log.Close() })
			b.ReportAllocs()
			for b.Loop() {
				log.Record(b.Context(), "container.launch", metrics.OutcomeOK, 1500*time.Millisecond, tc.attr...)
			}
		})
	}
}
