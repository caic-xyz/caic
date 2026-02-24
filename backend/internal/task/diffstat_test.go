package task

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestParseDiffNumstat(t *testing.T) {
	t.Run("Normal", func(t *testing.T) {
		input := "10\t3\tsrc/main.go\n5\t0\tsrc/util.go\n"
		ds := ParseDiffNumstat(input)
		if len(ds) != 2 {
			t.Fatalf("files = %d, want 2", len(ds))
		}
		want := []agent.DiffFileStat{
			{Path: "src/main.go", Added: 10, Deleted: 3},
			{Path: "src/util.go", Added: 5, Deleted: 0},
		}
		for i, f := range ds {
			if f != want[i] {
				t.Errorf("files[%d] = %+v, want %+v", i, f, want[i])
			}
		}
	})

	t.Run("Binary", func(t *testing.T) {
		input := "-\t-\timage.png\n"
		ds := ParseDiffNumstat(input)
		if len(ds) != 1 {
			t.Fatalf("files = %d, want 1", len(ds))
		}
		f := ds[0]
		if f.Path != "image.png" {
			t.Errorf("path = %q, want %q", f.Path, "image.png")
		}
		if !f.Binary {
			t.Error("expected binary = true")
		}
	})

	t.Run("Empty", func(t *testing.T) {
		if ds := ParseDiffNumstat(""); len(ds) != 0 {
			t.Errorf("expected zero DiffStat, got %+v", ds)
		}
		if ds := ParseDiffNumstat("  \n  \n"); len(ds) != 0 {
			t.Errorf("expected zero DiffStat for whitespace, got %+v", ds)
		}
	})

	t.Run("Mixed", func(t *testing.T) {
		input := "10\t3\tsrc/main.go\n-\t-\tdata.bin\n2\t1\tREADME.md\n"
		ds := ParseDiffNumstat(input)
		if len(ds) != 3 {
			t.Fatalf("files = %d, want 3", len(ds))
		}
		if ds[1].Binary != true {
			t.Error("files[1] should be binary")
		}
		if ds[2].Path != "README.md" {
			t.Errorf("files[2].path = %q, want %q", ds[2].Path, "README.md")
		}
	})
}
