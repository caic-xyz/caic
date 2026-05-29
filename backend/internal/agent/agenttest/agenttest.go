// Package agenttest provides shared test helpers for agent harness golden-file tests.
package agenttest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// RunExportDiscussionGolden runs golden-file tests for ExportDiscussion
// against all .jsonl files in testdata/. The parser is typically
// b.NewWire().ParseMessage from the harness backend.
func RunExportDiscussionGolden(t *testing.T, parser func([]byte) ([]agent.Message, error)) {
	files, err := filepath.Glob("testdata/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no .jsonl files in testdata")
	}

	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			got, err := agent.ExportDiscussion(f, parser)
			if err != nil {
				t.Fatal(err)
			}
			golden := strings.TrimSuffix(f, ".jsonl") + ".md"
			want, err := os.ReadFile(filepath.Clean(golden))
			if err != nil {
				t.Fatalf("golden file %s not found; run go generate to create", golden)
			}
			if got != string(want) {
				t.Errorf("mismatch; run go generate to regenerate\n--- got ---\n%s", got)
			}
		})
	}
}
