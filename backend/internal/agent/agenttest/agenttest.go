// Package agenttest provides shared test helpers for agent harness golden-file tests.
package agenttest

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// Parser parses one harness log line into normalized agent messages.
type Parser func([]byte) ([]agent.Message, error)

// ParseJSONL parses every non-empty line in a JSONL fixture with parser.
func ParseJSONL(t testing.TB, path string, parser Parser) []agent.Message {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})

	var out []agent.Message
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		data := scanner.Bytes()
		if strings.TrimSpace(string(data)) == "" {
			continue
		}
		msgs, err := parser(data)
		if err != nil {
			t.Fatalf("parse %s:%d: %v", path, line, err)
		}
		out = append(out, msgs...)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

// NativeParserResolver resolves a harness-native parser after task-log header
// validation. Its shape lets external harness tests inject task.ExportDiscussion
// without importing task into this shared test-helper package.
type NativeParserResolver func(harness.Name) (func([]byte) ([]agent.Message, error), error)

// DiscussionExporter loads and renders one physical task log.
type DiscussionExporter func(string, NativeParserResolver) (string, error)

// RunExportDiscussionGolden runs golden-file tests for task-owned physical
// export loading against all .jsonl files in testdata/. newParser typically
// returns b.NewWire().ParseMessage from the harness backend.
func RunExportDiscussionGolden(t *testing.T, export DiscussionExporter, newParser func() Parser) {
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
			got, err := export(f, func(h harness.Name) (func([]byte) ([]agent.Message, error), error) {
				return newParser(), nil
			})
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
