// Benchmarks parsing representative container git status and per-commit stat output.

package mdruntime

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkParseGitStatus(b *testing.B) {
	records := make([]string, 0, 88)
	records = append(records,
		"# branch.oid 0123456789abcdef",
		"# branch.head caic-42",
		"# branch.upstream origin/main",
		"# branch.ab +10 -0",
	)
	for i := range 20 {
		records = append(records, fmt.Sprintf("1 .M N... 100644 100644 100644 abc def src/file-%02d.go", i))
	}
	records = append(records, gitWorktreeStatMarker)
	for i := range 20 {
		records = append(records, fmt.Sprintf("5\t5\tsrc/file-%02d.go", i))
	}
	records = append(records, gitLogMarker, "")
	for i := range 10 {
		records = append(records,
			gitCommitMarker,
			fmt.Sprintf("%040x", i),
			"2026-09-01",
			"",
			fmt.Sprintf("Commit description %d", i),
			"",
			fmt.Sprintf("\n5\t5\tsrc/file-%02d.go", i),
		)
	}
	out := strings.Join(records, "\x00")
	b.ReportAllocs()
	for b.Loop() {
		status, err := parseGitStatus(out)
		if err != nil {
			b.Fatal(err)
		}
		if len(status.Commits) != 10 || len(status.Uncommitted) != 20 {
			b.Fatalf("unexpected status size: %+v", status)
		}
	}
}
