// Tests for the background repo watcher.

package server

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCollectWatchDirs(t *testing.T) {
	t.Parallel()
	t.Run("skips dot-prefixed directories", func(t *testing.T) {
		t.Parallel()
		// Lay out a tree mirroring a workspace with checked-out repos. The
		// poller must not descend into the .git internals — gitutil treats
		// them as bare repos because they contain HEAD/objects/refs, which
		// would otherwise pollute the runner registry with default backends.
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

	t.Run("respects max depth", func(t *testing.T) {
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
