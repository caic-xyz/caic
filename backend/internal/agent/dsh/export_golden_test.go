// Golden-file tests for exported DeepSeek discussions from native ACP recordings.

package dsh_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/dsh"
	"github.com/caic-xyz/caic/backend/internal/task/history"
)

var updateDiscussionGoldens = flag.Bool("update-discussion-goldens", false, "update DeepSeek discussion fixtures from retained native journals")

func TestExportDiscussionGolden(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("testdata/*.jsonl")
	if err != nil || len(files) == 0 {
		t.Fatalf("DeepSeek recordings: %v, %v", files, err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			b := dsh.New("", nil)
			got, err := history.ExportDiscussion(path, agent.Backends{b.Harness(): b})
			if err != nil {
				t.Fatal(err)
			}
			if *updateDiscussionGoldens {
				if err := os.WriteFile(strings.TrimSuffix(path, ".jsonl")+".md", []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".md")
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("discussion mismatch\n%s", got)
			}
		})
	}
}
