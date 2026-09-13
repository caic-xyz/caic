// Benchmarks parsing batched writable-layer sizes returned by container runtimes.

package mdruntime

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkParseDiskUsage(b *testing.B) {
	lines := make([]string, 0, 20)
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("/md-caic-task-%d\t%d", i, 100_000_000+i))
	}
	out := strings.Join(lines, "\n")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parseDiskUsage(out, "docker"); err != nil {
			b.Fatal(err)
		}
	}
}
