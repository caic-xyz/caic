// Tests for the background repo watcher's directory-collection helper.

package repomgr

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCollectWatchDirs(t *testing.T) {
	t.Parallel()
	t.Run("valid_skips_dot_prefixed_directories", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		makeDirs(t, root, []string{
			"clone/.git/objects",
			"clone/.git/refs/heads",
			"clone/src",
			".hidden",
		})

		dirs := collectWatchDirs(t.Context(), root, 3)

		want := []string{
			root,
			filepath.Join(root, "clone"),
			filepath.Join(root, "clone", "src"),
		}
		slices.Sort(dirs)
		slices.Sort(want)
		if !slices.Equal(dirs, want) {
			t.Fatalf("collectWatchDirs:\n got: %v\nwant: %v", dirs, want)
		}
	})

	t.Run("valid_respects_max_depth", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		makeDirs(t, root, []string{"a/b/c/d"})

		dirs := collectWatchDirs(t.Context(), root, 2)

		want := []string{
			root,
			filepath.Join(root, "a"),
			filepath.Join(root, "a", "b"),
		}
		slices.Sort(dirs)
		slices.Sort(want)
		if !slices.Equal(dirs, want) {
			t.Fatalf("collectWatchDirs:\n got: %v\nwant: %v", dirs, want)
		}
	})
}

func makeDirs(t *testing.T, root string, rels []string) {
	for _, rel := range rels {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
}
