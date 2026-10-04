// Benchmarks Git status parsing and separate versus consolidated turn probes.

package mdruntime

import (
	"fmt"
	"os"
	"path/filepath"
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
			gitAheadCommitMarker,
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

// BenchmarkTurnGitCommands compares equivalent branch and turn measurements
// with separate commands versus one probe. It uses local Bash processes, so it
// excludes SSH latency and the additional reference sync removed in production.
func BenchmarkTurnGitCommands(b *testing.B) {
	dir := initStatusRepo(b)
	from := runTestGitOutput(b, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "turn.txt"), []byte("turn\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	runTestGit(b, dir, "add", ".")
	runTestGit(b, dir, "commit", "-m", "turn")
	to := runTestGitOutput(b, dir, "rev-parse", "HEAD")
	b.Run("separate", func(b *testing.B) {
		branch := compactGitStatusCommand(dir, "origin", "main")
		turn := "cd " + shellQuote(dir) + " && git diff --numstat --stat -z " + shellQuote(from) + " " + shellQuote(to) + " --"
		b.ReportAllocs()
		for b.Loop() {
			for _, cmd := range []string{branch, turn} {
				if _, err := newIsolatedGitCommand(b, "bash", "-c", cmd).Output(); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("combined", func(b *testing.B) {
		cmd, err := turnGitCommand(dir, "origin", "main", from, to)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			out, err := newIsolatedGitCommand(b, "bash", "-c", cmd).Output()
			if err != nil {
				b.Fatal(err)
			}
			if _, _, err := parseTurnGit(string(out)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
