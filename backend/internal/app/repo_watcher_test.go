// Tests repository watcher boundaries and removal of stale nested checkouts.

package app

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/metrics"
)

func TestCollectRepoWatchDirs(t *testing.T) {
	t.Parallel()
	t.Run("valid_skips_dot_prefixed_directories", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		makeDirs(t, root, []string{
			"clone/.hidden/objects",
			"clone/src",
			".hidden",
		})

		dirs := collectRepoWatchDirs(t.Context(), slog.New(slog.DiscardHandler).With("cmp", "test"), root, 3)

		want := []string{
			root,
			filepath.Join(root, "clone"),
			filepath.Join(root, "clone", "src"),
		}
		slices.Sort(dirs)
		slices.Sort(want)
		if !slices.Equal(dirs, want) {
			t.Fatalf("collectRepoWatchDirs:\n got: %v\nwant: %v", dirs, want)
		}
	})

	t.Run("valid_stops_at_repositories", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		makeDirs(t, root, []string{
			"group/ordinary/.git",
			"group/ordinary/nested/.git",
			"group/ordinary/submodule",
			"group/worktree/nested/.git",
			"group/bare/objects",
			"group/bare/refs",
			"group/bare/nested/.git",
			"group/plain/child",
		})
		for _, rel := range []string{"group/ordinary/submodule/.git", "group/worktree/.git", "group/bare/HEAD"} {
			if err := os.WriteFile(filepath.Join(root, rel), []byte("gitdir: external\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		dirs := collectRepoWatchDirs(t.Context(), slog.New(slog.DiscardHandler), root, 5)
		want := []string{root, filepath.Join(root, "group"), filepath.Join(root, "group", "plain"), filepath.Join(root, "group", "plain", "child")}
		slices.Sort(dirs)
		slices.Sort(want)
		if !slices.Equal(dirs, want) {
			t.Fatalf("collectRepoWatchDirs:\n got: %v\nwant: %v", dirs, want)
		}
	})

	t.Run("valid_root_is_repository", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		makeDirs(t, root, []string{".git", "nested/.git", "plain/child"})
		got := collectRepoWatchDirs(t.Context(), slog.New(slog.DiscardHandler), root, 5)
		if !slices.Equal(got, []string{root}) {
			t.Fatalf("collectRepoWatchDirs = %v, want only %s", got, root)
		}
	})

	t.Run("valid_respects_max_depth", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		makeDirs(t, root, []string{"a/b/c/d"})

		dirs := collectRepoWatchDirs(t.Context(), slog.New(slog.DiscardHandler).With("cmp", "test"), root, 2)

		want := []string{
			root,
			filepath.Join(root, "a"),
			filepath.Join(root, "a", "b"),
		}
		slices.Sort(dirs)
		slices.Sort(want)
		if !slices.Equal(dirs, want) {
			t.Fatalf("collectRepoWatchDirs:\n got: %v\nwant: %v", dirs, want)
		}
	})
}

func TestRepoWatcher(t *testing.T) {
	t.Parallel()
	t.Run("poll_discovers_new_repository_boundary", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		outer := filepath.Join(root, "group", "outer")
		nested := filepath.Join(outer, "nested")
		initRepo := func(dir string) {
			for _, args := range [][]string{
				{"init", dir},
				{"-C", dir, "remote", "add", "origin", dir},
				{"-C", dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main"},
			} {
				if out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil { //nolint:gosec // Fixed Git fixture commands use private test-directory paths.
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
		}
		initRepo(nested)
		log := slog.New(slog.DiscardHandler)
		registry := repo.NewRegistry()
		router, err := runtime.NewRouter([]runtime.System{&runtimetest.FakeSystem{}}, metrics.Nop{})
		if err != nil {
			t.Fatal(err)
		}
		w := newRepoWatcher(t.Context(), log, root, registry, nil, router)
		mtimes := make(map[string]time.Time)
		w.poll(t.Context(), mtimes)
		if _, ok := registry.Checkout("group/outer/nested"); !ok {
			t.Fatal("initial nested checkout was not discovered")
		}
		parentInfo, err := os.Stat(filepath.Dir(outer))
		if err != nil {
			t.Fatal(err)
		}
		initRepo(outer)
		// Creating .git inside outer leaves its parent's mtime unchanged.
		if err := os.Chtimes(filepath.Dir(outer), parentInfo.ModTime(), parentInfo.ModTime()); err != nil {
			t.Fatal(err)
		}
		w.poll(t.Context(), mtimes)
		if _, ok := registry.Checkout("group/outer"); !ok {
			t.Fatal("new topmost checkout was not discovered")
		}
		if got := slices.Collect(registry.Checkouts()); len(got) != 1 {
			t.Fatalf("registered %d checkouts, want only topmost checkout", len(got))
		}
	})

	t.Run("poll", func(t *testing.T) {
		for _, rootIsRepo := range []bool{false, true} {
			name := "repository_below_root"
			if rootIsRepo {
				name = "repository_at_root"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				outer := root
				if !rootIsRepo {
					outer = filepath.Join(root, "group", "outer")
				}
				log := slog.New(slog.DiscardHandler)
				registry := repo.NewRegistry()
				for _, dir := range []string{outer, filepath.Join(outer, "nested"), filepath.Join(outer, "src", "nested"), t.TempDir()} {
					cmd := exec.CommandContext(t.Context(), "git", "init", dir) //nolint:gosec // Initializes a Git fixture in a private test directory.
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git init: %v: %s", err, out)
					}
					checkout, err := repo.NewCheckout(t.Context(), log, dir, "main", nil)
					if err != nil {
						t.Fatal(err)
					}
					checkout.RelPath = dir
					if err := registry.RegisterCheckout(checkout); err != nil {
						t.Fatal(err)
					}
				}
				w := newRepoWatcher(t.Context(), log, root, registry, nil, nil)
				mtimes := make(map[string]time.Time)
				w.poll(t.Context(), mtimes)
				for checkout := range registry.Checkouts() {
					if checkout.Dir != outer && strings.HasPrefix(checkout.Dir, root+string(filepath.Separator)) {
						t.Fatalf("nested checkout remains registered: %s", checkout.Dir)
					}
				}
				if got := slices.Collect(registry.Checkouts()); len(got) != 2 {
					t.Fatalf("registered %d checkouts, want outer and external checkout", len(got))
				}
				if _, ok := registry.Checkout(outer); !ok {
					t.Fatal("topmost repository was unregistered")
				}
				// Repeated polls must retain the same repository boundary.
				w.poll(t.Context(), mtimes)
				if got := slices.Collect(registry.Checkouts()); len(got) != 2 {
					t.Fatalf("registered %d checkouts after second poll, want 2", len(got))
				}
			})
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
