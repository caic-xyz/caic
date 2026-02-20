package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	// Set up bare remote + clone with initial commit.
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

	// No caic branches → -1.
	n, err := MaxBranchSeqNum(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	if n != -1 {
		t.Fatalf("got %d, want -1", n)
	}

	// Add a second remote simulating an md container.
	mdBare := filepath.Join(dir, "md-container.git")
	cmd := exec.CommandContext(ctx, "git", "init", "--bare", mdBare) //nolint:gosec // test helper, args are constant.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare md: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(ctx, "git", "remote", "add", "md-abc123", mdBare) //nolint:gosec // test helper, args are constant.
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}

	// Create branches: push some to origin, some to the md remote.
	for _, b := range []string{"caic/w0", "caic/w3", "other/branch"} {
		cmd := exec.CommandContext(ctx, "git", "branch", b) //nolint:gosec // test helper, args are constant.
		cmd.Dir = clone
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git branch %s: %v\n%s", b, err, out)
		}
		cmd = exec.CommandContext(ctx, "git", "push", "origin", b) //nolint:gosec // test helper, args are constant.
		cmd.Dir = clone
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git push origin %s: %v\n%s", b, err, out)
		}
	}
	// Push the highest branch only to the md remote.
	cmd = exec.CommandContext(ctx, "git", "branch", "caic/w7")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch caic/w7: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(ctx, "git", "push", "md-abc123", "caic/w7")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push md-abc123 caic/w7: %v\n%s", err, out)
	}

	// Delete all local branches so only remote refs remain.
	for _, b := range []string{"caic/w0", "caic/w3", "caic/w7", "other/branch"} {
		cmd := exec.CommandContext(ctx, "git", "branch", "-d", b) //nolint:gosec // test helper, args are constant.
		cmd.Dir = clone
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git branch -d %s: %v\n%s", b, err, out)
		}
	}

	// Must find caic/w7 even though it's on the md remote, not origin.
	n, err = MaxBranchSeqNum(ctx, clone)
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

func TestPushRef(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	// Set up bare remote + clone with initial commit.
	type gitCmd struct {
		dir  string
		args []string
	}
	for _, c := range []gitCmd{
		{"", []string{"init", "--bare", "--initial-branch=main", bare}},
		{"", []string{"clone", bare, clone}},
		{clone, []string{"-c", "user.name=Test", "-c", "user.email=test@test", "commit", "--allow-empty", "-m", "init"}},
		{clone, []string{"push", "origin", "main"}},
		{clone, []string{"checkout", "-b", "caic/w0"}},
	} {
		cmd := exec.CommandContext(ctx, "git", c.args...) //nolint:gosec // test helper, args are constant.
		if c.dir != "" {
			cmd.Dir = c.dir
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", c.args, err, out)
		}
	}

	// Add a commit on the branch.
	if err := os.WriteFile(filepath.Join(clone, "new.txt"), []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "git", "add", ".")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(ctx, "git", "-c", "user.name=Test", "-c", "user.email=test@test", "commit", "-m", "add file")
	cmd.Dir = clone
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// Push the local branch ref to origin as caic/w0.
	if err := PushRef(ctx, clone, "caic/w0", "caic/w0", false); err != nil {
		t.Fatal(err)
	}

	// Verify the branch exists on the remote.
	cmd = exec.CommandContext(ctx, "git", "branch", "--list", "caic/w0")
	cmd.Dir = bare
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "caic/w0") {
		t.Errorf("branch caic/w0 not found on remote, got: %q", string(out))
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

func TestSquashOnto(t *testing.T) {
	// Helper: set up a bare remote + clone with an initial commit on main.
	setup := func(t *testing.T) (bare, clone string) {
		t.Helper()
		ctx := t.Context()
		dir := t.TempDir()
		bare = filepath.Join(dir, "remote.git")
		clone = filepath.Join(dir, "clone")
		type gitCmd struct {
			dir  string
			args []string
		}
		for _, c := range []gitCmd{
			{"", []string{"init", "--bare", "--initial-branch=main", bare}},
			{"", []string{"clone", bare, clone}},
			{clone, []string{"config", "user.name", "Test"}},
			{clone, []string{"config", "user.email", "test@test"}},
			{clone, []string{"commit", "--allow-empty", "-m", "init"}},
			{clone, []string{"push", "origin", "main"}},
		} {
			cmd := exec.CommandContext(ctx, "git", c.args...) //nolint:gosec // test helper
			if c.dir != "" {
				cmd.Dir = c.dir
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", c.args, err, out)
			}
		}
		return bare, clone
	}

	// Helper: run a git command in dir.
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test helper
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	t.Run("Basic", func(t *testing.T) {
		bare, clone := setup(t)
		ctx := t.Context()

		// Make two commits on a feature branch.
		run(t, clone, "checkout", "-b", "feature")
		if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("a\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, clone, "add", ".")
		run(t, clone, "commit", "-m", "add a")
		if err := os.WriteFile(filepath.Join(clone, "b.txt"), []byte("b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, clone, "add", ".")
		run(t, clone, "commit", "-m", "add b")

		// Squash onto main.
		if err := SquashOnto(ctx, clone, "feature", "main", "squash: add a + b"); err != nil {
			t.Fatal(err)
		}

		// Verify: origin/main should have exactly 2 commits (init + squash).
		log := run(t, bare, "log", "--oneline", "main")
		lines := strings.Split(log, "\n")
		if len(lines) != 2 {
			t.Fatalf("expected 2 commits on main, got %d:\n%s", len(lines), log)
		}
		if !strings.Contains(lines[0], "squash: add a + b") {
			t.Errorf("expected squash commit message, got: %s", lines[0])
		}

		// Both files should be present.
		for _, name := range []string{"a.txt", "b.txt"} {
			cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", "main:"+name) //nolint:gosec // test
			cmd.Dir = bare
			if err := cmd.Run(); err != nil {
				t.Errorf("file %s missing on main after squash", name)
			}
		}
	})

	t.Run("NonFastForward", func(t *testing.T) {
		_, clone := setup(t)
		ctx := t.Context()

		// Make a commit on a feature branch.
		run(t, clone, "checkout", "-b", "feature2")
		if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("f\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, clone, "add", ".")
		run(t, clone, "commit", "-m", "add f")

		// Advance origin/main by pushing a new commit directly.
		run(t, clone, "checkout", "main")
		if err := os.WriteFile(filepath.Join(clone, "other.txt"), []byte("other\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, clone, "add", ".")
		run(t, clone, "commit", "-m", "advance main")
		run(t, clone, "push", "origin", "main")

		// SquashOnto will fetch (getting latest main), create a squash commit
		// parented on the fetched main, and push. This should succeed because
		// origin/main was refreshed by the Fetch inside SquashOnto.
		if err := SquashOnto(ctx, clone, "feature2", "main", "squash f"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIsReachable(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	run := func(t *testing.T, d string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // test helper
		cmd.Dir = d
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Set up bare remote + clone with initial commit on main.
	run(t, "", "init", "--bare", "--initial-branch=main", bare)
	run(t, "", "clone", bare, clone)
	run(t, clone, "config", "user.name", "Test")
	run(t, clone, "config", "user.email", "test@test")
	run(t, clone, "commit", "--allow-empty", "-m", "init")
	run(t, clone, "push", "origin", "main")

	// Add a container remote with a new commit unreachable from origin.
	containerBare := filepath.Join(dir, "container.git")
	run(t, "", "init", "--bare", "--initial-branch=main", containerBare)
	run(t, clone, "remote", "add", "md-caic-w0", containerBare)

	// Create a commit on a feature branch and push only to the container remote.
	run(t, clone, "checkout", "-b", "caic/w0")
	if err := os.WriteFile(filepath.Join(clone, "work.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, clone, "add", ".")
	run(t, clone, "commit", "-m", "container work")
	containerCommit := run(t, clone, "rev-parse", "HEAD")
	run(t, clone, "push", "md-caic-w0", "caic/w0")
	run(t, clone, "checkout", "main")
	run(t, clone, "branch", "-D", "caic/w0")

	// The initial commit is on origin/main — reachable.
	initCommit := run(t, clone, "rev-parse", "origin/main")

	t.Run("Reachable", func(t *testing.T) {
		ok, err := IsReachable(ctx, clone, initCommit)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("expected commit on origin/main to be reachable")
		}
	})

	t.Run("Unreachable", func(t *testing.T) {
		ok, err := IsReachable(ctx, clone, containerCommit)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Error("expected commit only on container remote to be unreachable")
		}
	})

	t.Run("ReachableViaLocalBranch", func(t *testing.T) {
		// Create a local branch pointing at the container commit.
		run(t, clone, "branch", "local-backup", containerCommit)
		defer func() {
			run(t, clone, "branch", "-D", "local-backup")
		}()
		ok, err := IsReachable(ctx, clone, containerCommit)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Error("expected commit on local branch to be reachable")
		}
	})
}

func TestCreateBranchAt(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	run := func(t *testing.T, d string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // test helper
		cmd.Dir = d
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run(t, "", "init", "--initial-branch=main", dir)
	run(t, dir, "config", "user.name", "Test")
	run(t, dir, "config", "user.email", "test@test")
	run(t, dir, "commit", "--allow-empty", "-m", "init")
	commit := run(t, dir, "rev-parse", "HEAD")

	t.Run("Basic", func(t *testing.T) {
		if err := CreateBranchAt(ctx, dir, "caic-backup/caic/w0", commit); err != nil {
			t.Fatal(err)
		}
		got := run(t, dir, "rev-parse", "caic-backup/caic/w0")
		if got != commit {
			t.Errorf("branch points at %s, want %s", got, commit)
		}
	})

	t.Run("AlreadyExists", func(t *testing.T) {
		if err := CreateBranchAt(ctx, dir, "caic-backup/caic/w0", commit); err == nil {
			t.Error("expected error for duplicate branch")
		}
	})
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
