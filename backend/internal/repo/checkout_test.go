// Tests for Checkout: branch allocation, git sync, and diff operations.

package repo

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
)

// fakeTaskView is a minimal TaskView implementation for tests. It cannot be
// replaced with *task.Task: internal/task imports internal/repo, so a
// test in package repo importing internal/task would create an import
// cycle.
type fakeTaskView struct {
	instanceID runtime.ID
	repo       []runtime.Repo
	baseBranch string
}

type testRuntimeSystem struct {
	testRuntimeBackend
	runtimetest.FakeInfo
}

type testRuntimeBackend interface {
	runtime.Lifecycle
	runtime.Repository
}

func (*testRuntimeSystem) Name() runtime.Name { return "test-runtime" }

func (f *fakeTaskView) RuntimeInstanceID() runtime.ID      { return f.instanceID }
func (f *fakeTaskView) RuntimeRepos() []runtime.Repo       { return f.repo }
func (f *fakeTaskView) SetRepoBranch(i int, branch string) { f.repo[i].Branch = branch }
func (f *fakeTaskView) PrimaryBaseBranch() string          { return f.baseBranch }

func TestLiveBranchesByRoot(t *testing.T) {
	t.Parallel()
	instances := []runtime.Instance{
		{Repos: []runtime.Repo{
			{GitRoot: "/home/user/src/genai", Branch: "caic-5"},
			{GitRoot: "/home/user/src/caic", Branch: "caic-2"},
		}},
		{Repos: []runtime.Repo{
			{GitRoot: "/home/user/src/genai", Branch: "caic-4"},
			{GitRoot: "", Branch: "caic-9"},               // no repo: ignored.
			{GitRoot: "/home/user/src/other", Branch: ""}, // unset branch: ignored.
		}},
	}
	got := LiveBranchesByRoot(instances)
	want := map[string][]string{
		"/home/user/src/genai": {"caic-5", "caic-4"},
		"/home/user/src/caic":  {"caic-2"},
	}
	if len(got) != len(want) {
		t.Fatalf("LiveBranchesByRoot() = %+v, want %+v", got, want)
	}
	for root, branches := range want {
		if !slices.Equal(got[root], branches) {
			t.Errorf("LiveBranchesByRoot()[%q] = %v, want %v", root, got[root], branches)
		}
	}
}

func TestRuntimeRemoteRef(t *testing.T) {
	t.Parallel()
	got := runtimeRemoteRef(runtime.NewID("docker", "md-agent-1"), "caic-1")
	want := "refs/remotes/md-agent-1/caic-1"
	if got != want {
		t.Fatalf("runtimeRemoteRef = %q, want %q", got, want)
	}
}

func newTestCheckout(dir string) *Checkout {
	return &Checkout{
		Dir:        dir,
		RelPath:    filepath.Base(dir),
		GitTimeout: time.Minute,
	}
}

func newTestRuntime(t *testing.T, backend testRuntimeBackend) *runtime.Router {
	runtimes, err := runtime.NewRouter(logtest.Logger(t), []runtime.System{&testRuntimeSystem{testRuntimeBackend: backend}})
	if err != nil {
		t.Fatal(err)
	}
	return runtimes
}

func newInitializedTestCheckout(t *testing.T, dir string) *Checkout {
	return newInitializedTestCheckoutWithLiveBranches(t, dir, nil)
}

func newInitializedTestCheckoutWithLiveBranches(t *testing.T, dir string, liveBranches []string) *Checkout {
	checkout, err := NewCheckout(t.Context(), logtest.Logger(t), dir, "main", liveBranches)
	if err != nil {
		t.Fatal(err)
	}
	return checkout
}

func TestCheckout(t *testing.T) {
	t.Parallel()
	t.Run("NewCheckout", func(t *testing.T) {
		t.Parallel()
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			r := newInitializedTestCheckout(t, clone)
			if r.nextID != 0 {
				t.Errorf("nextID = %d, want 0", r.nextID)
			}
		})
		t.Run("SkipsExisting", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			// Pre-create branches and push to remote.
			runGit(t, clone, "branch", "caic-0")
			runGit(t, clone, "push", "origin", "caic-0")
			runGit(t, clone, "branch", "caic-3")
			runGit(t, clone, "push", "origin", "caic-3")

			r := newInitializedTestCheckout(t, clone)
			if r.nextID != 4 {
				t.Errorf("nextID = %d, want 4", r.nextID)
			}
		})
		t.Run("SkipsLocalOnly", func(t *testing.T) {
			t.Parallel()
			// Local-only branches (e.g. from stopped tasks that were never
			// pushed) must also be accounted for.
			clone := initTestRepo(t, "main")
			runGit(t, clone, "branch", "caic-5")
			// Do NOT push — simulates a stopped task whose branch was
			// never synced to origin.

			r := newInitializedTestCheckout(t, clone)
			if r.nextID != 6 {
				t.Errorf("nextID = %d, want 6", r.nextID)
			}
		})
		t.Run("IgnoresNonCaicPrefix", func(t *testing.T) {
			t.Parallel()
			// Branches like "foo-caic-9" must not be matched.
			clone := initTestRepo(t, "main")
			runGit(t, clone, "branch", "foo-caic-9")
			runGit(t, clone, "branch", "caic-2")

			r := newInitializedTestCheckout(t, clone)
			if r.nextID != 3 {
				t.Errorf("nextID = %d, want 3", r.nextID)
			}
		})
		t.Run("AccountsForLiveContainerBranches", func(t *testing.T) {
			t.Parallel()
			// A branch a running container already holds may never have made
			// it into git (e.g. its "git branch" step failed after the name
			// was decided). It must still reserve its sequence number, or the
			// next allocation would reissue the same name to a second
			// container. See checkRepoOverlap in the md package.
			clone := initTestRepo(t, "main")
			runGit(t, clone, "branch", "caic-1")

			r := newInitializedTestCheckoutWithLiveBranches(t, clone, []string{"caic-5"})
			if r.nextID != 6 {
				t.Errorf("nextID = %d, want 6", r.nextID)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			if _, err := NewCheckout(t.Context(), logtest.Logger(t), "", "main", nil); err == nil {
				t.Fatal("want directory error")
			}
			if _, err := NewCheckout(t.Context(), nil, t.TempDir(), "main", nil); err == nil {
				t.Fatal("want logger error")
			}
		})
	})

	t.Run("BranchDiffStat", func(t *testing.T) {
		t.Parallel()
		sc := newRecordingContainer()
		r := newTestCheckout("/repo")
		tv := &fakeTaskView{instanceID: runtime.NewID("test-runtime", "ctr-1"), repo: []runtime.Repo{{GitRoot: "/repo", Branch: "feature"}}}
		ds := r.BranchDiffStat(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), tv)
		if len(sc.fetchIDs) != 0 {
			t.Errorf("BranchDiffStat called Fetch %d times, want 0", len(sc.fetchIDs))
		}
		if len(ds) != 1 || ds[0].Path != "main.go" || ds[0].Added != 5 || ds[0].Deleted != 1 {
			t.Errorf("BranchDiffStat = %+v, want [{main.go +5 -1}]", ds)
		}
	})
	t.Run("BranchDiffStatMultiRepoUsesInstanceID", func(t *testing.T) {
		t.Parallel()
		sc := newRecordingContainer()
		r := newTestCheckout("/home/user/src/caic")
		tv := &fakeTaskView{instanceID: runtime.NewID("test-runtime", "ctr-2"), repo: []runtime.Repo{
			{GitRoot: "/home/user/src/caic", Branch: "caic-7", ContainerPath: "/home/user/src/caic"},
			{GitRoot: "/home/user/src/genai", Branch: "caic-0", ContainerPath: "/home/user/src/genai"},
		}}

		ds := r.BranchDiffStat(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), tv)

		if len(ds) != 2 {
			t.Fatalf("BranchDiffStat len = %d, want 2", len(ds))
		}
		if len(sc.diffIDs) != 2 {
			t.Fatalf("diff calls = %d, want 2", len(sc.diffIDs))
		}
		for i, id := range sc.diffIDs {
			if id != "test-runtime:ctr-2" {
				t.Errorf("diff call %d id = %q, want test-runtime:ctr-2", i, id)
			}
		}
		if sc.diffIdxs[0] != 0 || sc.diffIdxs[1] != 1 {
			t.Errorf("diff indexes = %v, want [0 1]", sc.diffIdxs)
		}
		if ds[1].Path != "genai/main.go" {
			t.Errorf("second path = %q, want genai/main.go", ds[1].Path)
		}
	})
	t.Run("BranchDiffStatNoContainer", func(t *testing.T) {
		t.Parallel()
		r := newTestCheckout(t.TempDir())
		if ds := r.BranchDiffStat(t.Context(), logtest.Logger(t), nil, &fakeTaskView{}); ds != nil {
			t.Errorf("BranchDiffStat with no instance = %+v, want nil", ds)
		}
	})
}

func TestTaskRuntime(t *testing.T) {
	t.Parallel()
	t.Run("valid_preserves_mounted_path", func(t *testing.T) {
		t.Parallel()
		r := newTestCheckout("/home/user/src/caic-xyz/caic")
		tv := &fakeTaskView{instanceID: "ctr-1", repo: []runtime.Repo{
			{Branch: "caic-7", ContainerPath: "/home/user/src/caic-xyz/caic"},
			{Branch: "caic-0", GitRoot: "/home/user/src/caic-xyz/md", ContainerPath: "/home/user/src/caic-xyz/md"},
		}}

		id, repos, err := r.taskRuntime(tv)
		if err != nil {
			t.Fatalf("taskRuntime: %v", err)
		}
		if id != "ctr-1" {
			t.Errorf("id = %q, want ctr-1", id)
		}
		if len(repos) != 2 {
			t.Fatalf("repos len = %d, want 2", len(repos))
		}
		if repos[0].GitRoot != "/home/user/src/caic-xyz/caic" {
			t.Errorf("primary HostPath = %q, want checkout dir", repos[0].GitRoot)
		}
		if repos[0].ContainerPath != "/home/user/src/caic-xyz/caic" {
			t.Errorf("primary ContainerPath = %q, want qualified path", repos[0].ContainerPath)
		}
		if repos[1].ContainerPath != "/home/user/src/caic-xyz/md" {
			t.Errorf("extra ContainerPath = %q, want qualified path", repos[1].ContainerPath)
		}
	})
	t.Run("valid_no_repos", func(t *testing.T) {
		t.Parallel()
		r := newTestCheckout("/repo")
		tv := &fakeTaskView{instanceID: "ctr-1"}
		id, repos, err := r.taskRuntime(tv)
		if err != nil {
			t.Fatalf("taskRuntime: %v", err)
		}
		if id != "ctr-1" {
			t.Errorf("id = %q, want ctr-1", id)
		}
		if repos != nil {
			t.Fatalf("repos = %+v, want nil", repos)
		}
	})
	t.Run("error_no_instance", func(t *testing.T) {
		t.Parallel()
		if _, _, err := newTestCheckout(t.TempDir()).taskRuntime(&fakeTaskView{}); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestExtractRepoDS(t *testing.T) {
	t.Parallel()
	ds := agent.DiffStat{
		{Path: "a/b/main.go", Added: 10, Deleted: 3},
		{Path: "a/b/util.go", Added: 5, Deleted: 0},
	}
	t.Run("Multi", func(t *testing.T) {
		t.Parallel()
		got := extractRepoDS(ds, "a/b", true)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
		if got[0].Path != "main.go" || got[1].Path != "util.go" {
			t.Errorf("got paths = %q, %q", got[0].Path, got[1].Path)
		}
	})
	t.Run("Single", func(t *testing.T) {
		t.Parallel()
		got := extractRepoDS(ds, "a/b", false)
		if len(got) != 2 || got[0].Path != "a/b/main.go" {
			t.Errorf("single repo should return unchanged, got %+v", got)
		}
	})
}

func TestDiffContentArgs(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		path  string
		repo  runtime.Repo
		multi bool
		want  []string
	}{
		"single repo full diff": {
			want: []string{"--src-prefix=", "--dst-prefix="},
		},
		"single repo path": {
			path: "main.go",
			want: []string{"--src-prefix=", "--dst-prefix=", "--", "main.go"},
		},
		"multi repo full diff": {
			repo:  runtime.Repo{ContainerPath: "~/src/caic"},
			multi: true,
			want:  []string{"--src-prefix=a/caic/", "--dst-prefix=b/caic/"},
		},
		"multi repo path": {
			path:  "b/main.go",
			repo:  runtime.Repo{ContainerPath: "~/src/caic-xyz/caic"},
			multi: true,
			want:  []string{"--src-prefix=a/caic-xyz/caic/", "--dst-prefix=b/caic-xyz/caic/", "--", "b/main.go"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := diffContentArgs(tc.path, &tc.repo, tc.multi); !slices.Equal(got, tc.want) {
				t.Errorf("diffContentArgs() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDiffRepoPrefix(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		repo runtime.Repo
		want string
	}{
		"tilde source mount": {
			repo: runtime.Repo{ContainerPath: "~/src/caic"},
			want: "caic",
		},
		"tilde collision mount": {
			repo: runtime.Repo{ContainerPath: "~/src/caic-xyz/caic"},
			want: "caic-xyz/caic",
		},
		"home source mount": {
			repo: runtime.Repo{ContainerPath: "/home/user/src/caic-xyz/caic"},
			want: "caic-xyz/caic",
		},
		"host fallback": {
			repo: runtime.Repo{GitRoot: "/home/user/src/caic"},
			want: "caic",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := diffRepoPrefix(&tc.repo); got != tc.want {
				t.Errorf("diffRepoPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}

// recordingContainer is a fake runtime whose Diff reports a fixed one-file
// numstat and which records Fetch/Diff calls, so tests can assert per-repo
// diffing by instance id and repo index, and that BranchDiffStat does not
// fetch.
type recordingContainer struct {
	*runtimetest.FakeBackend

	fetchIDs []runtime.ID
	diffIDs  []runtime.ID
	diffIdxs []int
}

func (c *recordingContainer) Fetch(ctx context.Context, id runtime.ID) error {
	c.fetchIDs = append(c.fetchIDs, id)
	return c.FakeBackend.Fetch(ctx, id)
}

func (c *recordingContainer) Diff(ctx context.Context, id runtime.ID, repoIdx int, args ...string) (string, error) {
	c.diffIDs = append(c.diffIDs, id)
	c.diffIdxs = append(c.diffIdxs, repoIdx)
	return c.FakeBackend.Diff(ctx, id, repoIdx, args...)
}

// newRecordingContainer builds a recordingContainer with the fixed diff output.
func newRecordingContainer() *recordingContainer {
	return &recordingContainer{FakeBackend: &runtimetest.FakeBackend{DiffOutput: "5\t1\tmain.go\n"}}
}

// initTestRepo creates a bare "remote" and a local clone with one commit on
// baseBranch. Returns the clone directory. origin points to the bare repo so
// git fetch/push work locally.
func initTestRepo(t *testing.T, baseBranch string) string { //nolint:unparam // baseBranch is parameterized for clarity.
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	clone := filepath.Join(dir, "clone")

	runGit(t, "", "init", "--bare", bare)
	runGit(t, "", "init", clone)
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "config", "user.email", "test@test.com")
	runGit(t, clone, "checkout", "-b", baseBranch)

	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-m", "init")
	runGit(t, clone, "remote", "add", "origin", bare)
	runGit(t, clone, "push", "-u", "origin", baseBranch)
	return clone
}

func runGit(t *testing.T, dir string, args ...string) {
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test helper with controlled args
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestParseDiffNumstat(t *testing.T) {
	t.Parallel()
	t.Run("Normal", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
		if ds := ParseDiffNumstat(""); len(ds) != 0 {
			t.Errorf("expected zero DiffStat, got %+v", ds)
		}
		if ds := ParseDiffNumstat("  \n  \n"); len(ds) != 0 {
			t.Errorf("expected zero DiffStat for whitespace, got %+v", ds)
		}
	})

	t.Run("Mixed", func(t *testing.T) {
		t.Parallel()
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

func TestDeleteLocalBranchIfUnmodified(t *testing.T) {
	t.Parallel()

	newCheckout := func(dir string) *git.Checkout {
		return &git.Checkout{Root: dir, Logger: slog.Default()}
	}
	branchExists := func(t *testing.T, dir, branch string) bool {
		cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) //nolint:gosec // test helper with controlled args
		return cmd.Run() == nil
	}

	t.Run("valid_deletes_unmodified", func(t *testing.T) {
		t.Parallel()
		clone := initTestRepo(t, "main")
		runGit(t, clone, "branch", "caic-1", "main")
		deleted, err := deleteLocalBranchIfUnmodified(t.Context(), newCheckout(clone), "caic-1", "main")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !deleted {
			t.Error("deleted = false, want true")
		}
		if branchExists(t, clone, "caic-1") {
			t.Error("branch caic-1 still exists after deletion")
		}
	})

	t.Run("valid_keeps_modified", func(t *testing.T) {
		t.Parallel()
		clone := initTestRepo(t, "main")
		runGit(t, clone, "checkout", "-b", "caic-2", "main")
		if err := os.WriteFile(filepath.Join(clone, "extra.txt"), []byte("work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, clone, "add", ".")
		runGit(t, clone, "commit", "-m", "work")
		runGit(t, clone, "checkout", "main")
		deleted, err := deleteLocalBranchIfUnmodified(t.Context(), newCheckout(clone), "caic-2", "main")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if deleted {
			t.Error("deleted = true, want false for a modified branch")
		}
		if !branchExists(t, clone, "caic-2") {
			t.Error("branch caic-2 was deleted despite unique commits")
		}
	})

	t.Run("error_checked_out", func(t *testing.T) {
		t.Parallel()
		clone := initTestRepo(t, "main")
		runGit(t, clone, "checkout", "-b", "caic-3", "main")
		deleted, err := deleteLocalBranchIfUnmodified(t.Context(), newCheckout(clone), "caic-3", "main")
		if !errors.Is(err, errBranchCheckedOut) {
			t.Fatalf("err = %v, want errBranchCheckedOut", err)
		}
		if deleted {
			t.Error("deleted = true, want false for a checked-out branch")
		}
		if !branchExists(t, clone, "caic-3") {
			t.Error("checked-out branch caic-3 was deleted")
		}
	})
}
