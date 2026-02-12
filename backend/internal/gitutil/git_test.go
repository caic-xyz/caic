package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestDiscoverRepos(t *testing.T) {
	root := t.TempDir()

	// Create repos at various depths.
	mkGit := func(parts ...string) {
		t.Helper()
		p := append(append([]string{root}, parts...), ".git")
		if err := os.MkdirAll(filepath.Join(p...), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	mkGit("repoA")
	mkGit("org", "repoB")
	mkGit("org", "repoC")
	mkGit("deep", "nested", "repoD")
	mkGit("deep", "nested", "too", "repoE") // depth 4 — excluded at maxDepth=3

	// Hidden directory should be skipped.
	mkGit(".hidden", "repoF")

	// Nested repo inside a repo — recursion should stop at repoA.
	mkGit("repoA", "sub", ".git")

	repos, err := DiscoverRepos(root, 3)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		filepath.Join(root, "deep", "nested", "repoD"),
		filepath.Join(root, "org", "repoB"),
		filepath.Join(root, "org", "repoC"),
		filepath.Join(root, "repoA"),
	}
	slices.Sort(repos)
	slices.Sort(want)

	if !slices.Equal(repos, want) {
		t.Errorf("repos = %v\n want %v", repos, want)
	}
}

func TestDiscoverReposDepthZero(t *testing.T) {
	root := t.TempDir()

	// Root itself is a repo.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}

	repos, err := DiscoverRepos(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0] != root {
		t.Errorf("repos = %v, want [%s]", repos, root)
	}
}

func TestMaxBranchSeqNum(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	// Initialize a real git repo.
	for _, args := range [][]string{
		{"init"},
		{"-c", "user.name=Test", "-c", "user.email=test@test", "commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // test helper, args are constant.
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// No caic branches → -1.
	n, err := MaxBranchSeqNum(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != -1 {
		t.Fatalf("got %d, want -1", n)
	}

	// Create some branches.
	for _, b := range []string{"caic/w0", "caic/w3", "caic/w7", "other/branch"} {
		cmd := exec.CommandContext(ctx, "git", "branch", b) //nolint:gosec // test helper, args are constant.
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git branch %s: %v\n%s", b, err, out)
		}
	}

	n, err = MaxBranchSeqNum(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("got %d, want 7", n)
	}
}

func TestDefaultBranch(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	// Create a bare repo with "main" as the default branch, then clone it.
	type gitCmd struct {
		dir  string
		args []string
	}
	for _, c := range []gitCmd{
		{"", []string{"init", "--bare", "--initial-branch=main", bare}},
		{"", []string{"clone", bare, clone}},
		{clone, []string{"-c", "user.name=Test", "-c", "user.email=test@test", "commit", "--allow-empty", "-m", "init"}},
		{clone, []string{"push", "origin", "main"}},
	} {
		cmd := exec.CommandContext(ctx, "git", c.args...) //nolint:gosec // test helper, args are constant.
		if c.dir != "" {
			cmd.Dir = c.dir
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", c.args, err, out)
		}
	}

	// DefaultBranch should return "main" via the symbolic ref.
	got, err := DefaultBranch(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Fatalf("got %q, want %q", got, "main")
	}

	// Switch to a different branch and verify DefaultBranch still returns "main".
	cmd := exec.CommandContext(ctx, "git", "checkout", "-b", "feature")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b feature: %v\n%s", err, out)
	}
	got, err = DefaultBranch(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Fatalf("got %q after checkout, want %q", got, "main")
	}

	// Remove the symbolic ref to exercise the fallback probe path.
	cmd = exec.CommandContext(ctx, "git", "remote", "set-head", "origin", "--delete")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote set-head origin --delete: %v\n%s", err, out)
	}
	got, err = DefaultBranch(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Fatalf("got %q after deleting symbolic ref, want %q", got, "main")
	}
}

func TestRemoteToHTTPS(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"git@github.com:owner/repo.git", "https://github.com/owner/repo"},
		{"git@github.com:owner/repo", "https://github.com/owner/repo"},
		{"ssh://git@github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"ssh://git@gitlab.com/owner/repo.git", "https://gitlab.com/owner/repo"},
		{"https://github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"http://github.com/owner/repo.git", "http://github.com/owner/repo"},
		{"", ""},
		{"  git@github.com:o/r.git  ", "https://github.com/o/r"},
	}
	for _, tt := range tests {
		got := RemoteToHTTPS(tt.in)
		if got != tt.want {
			t.Errorf("RemoteToHTTPS(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDiscoverReposEmpty(t *testing.T) {
	root := t.TempDir()
	repos, err := DiscoverRepos(root, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Errorf("repos = %v, want empty", repos)
	}
}
