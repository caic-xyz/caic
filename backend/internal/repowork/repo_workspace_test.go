// Tests for RepoWorkspace: branch allocation, git sync, and diff operations.

package repowork

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// fakeTaskView is a minimal TaskView implementation for tests. It cannot be
// replaced with *task.Task: internal/task imports internal/repowork, so a
// test in package repowork importing internal/task would create an import
// cycle.
type fakeTaskView struct {
	instanceID runtime.InstanceID
	repos      []runtime.Repo
	baseBranch string
}

func (f *fakeTaskView) RuntimeInstanceID() runtime.InstanceID { return f.instanceID }
func (f *fakeTaskView) RuntimeRepos() []runtime.Repo          { return f.repos }
func (f *fakeTaskView) SetRepoBranch(i int, branch string)    { f.repos[i].Branch = branch }
func (f *fakeTaskView) PrimaryBaseBranch() string             { return f.baseBranch }

func newTestRepoWorkspace(baseBranch, dir string, backend runtime.Backend) *RepoWorkspace {
	workspace, err := NewRepoWorkspace(baseBranch, dir, filepath.Base(dir), time.Minute, backend, slog.With("repo", "test"))
	if err != nil {
		panic(err)
	}
	return workspace
}

func TestRepoWorkspace(t *testing.T) {
	t.Parallel()
	t.Run("Init", func(t *testing.T) {
		t.Parallel()
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t, "main")
			r := newTestRepoWorkspace("main", clone, nil)
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
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

			r := newTestRepoWorkspace("main", clone, nil)
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
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

			r := newTestRepoWorkspace("main", clone, nil)
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
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

			r := newTestRepoWorkspace("main", clone, nil)
			if err := r.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r.nextID != 3 {
				t.Errorf("nextID = %d, want 3", r.nextID)
			}
		})
	})

	t.Run("BranchDiffStat", func(t *testing.T) {
		t.Parallel()
		sc := &stubContainer{}
		r := newTestRepoWorkspace("", "/repo", sc)
		tv := &fakeTaskView{instanceID: "ctr-1", repos: []runtime.Repo{{HostPath: "/repo", Branch: "feature"}}}
		ds := r.BranchDiffStat(t.Context(), tv)
		if !sc.fetched {
			t.Error("BranchDiffStat did not call Fetch")
		}
		if len(sc.fetchIDs) != 1 || sc.fetchIDs[0] != "ctr-1" {
			t.Errorf("fetch IDs = %v, want [ctr-1]", sc.fetchIDs)
		}
		if len(ds) != 1 || ds[0].Path != "main.go" || ds[0].Added != 5 || ds[0].Deleted != 1 {
			t.Errorf("BranchDiffStat = %+v, want [{main.go +5 -1}]", ds)
		}
	})
	t.Run("BranchDiffStatMultiRepoUsesInstanceID", func(t *testing.T) {
		t.Parallel()
		sc := &stubContainer{}
		r := newTestRepoWorkspace("", "/home/user/src/caic", sc)
		tv := &fakeTaskView{instanceID: "ctr-2", repos: []runtime.Repo{
			{HostPath: "/home/user/src/caic", Branch: "caic-7", MountPath: "/home/user/src/caic"},
			{HostPath: "/home/user/src/genai", Branch: "caic-0", MountPath: "/home/user/src/genai"},
		}}

		ds := r.BranchDiffStat(t.Context(), tv)

		if len(ds) != 2 {
			t.Fatalf("BranchDiffStat len = %d, want 2", len(ds))
		}
		if len(sc.diffIDs) != 2 {
			t.Fatalf("diff calls = %d, want 2", len(sc.diffIDs))
		}
		for i, id := range sc.diffIDs {
			if id != "ctr-2" {
				t.Errorf("diff call %d id = %q, want ctr-2", i, id)
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
		r := newTestRepoWorkspace("", "", nil)
		if ds := r.BranchDiffStat(t.Context(), &fakeTaskView{}); ds != nil {
			t.Errorf("BranchDiffStat with no instance = %+v, want nil", ds)
		}
	})
	t.Run("BranchDiffStatNoDir", func(t *testing.T) {
		t.Parallel()
		r := newTestRepoWorkspace("", "", &stubContainer{})
		if ds := r.BranchDiffStat(t.Context(), &fakeTaskView{}); ds != nil {
			t.Errorf("BranchDiffStat with no dir = %+v, want nil", ds)
		}
	})
}

func TestTaskRuntime(t *testing.T) {
	t.Parallel()
	t.Run("valid_preserves_mounted_path", func(t *testing.T) {
		t.Parallel()
		r := newTestRepoWorkspace("", "/home/user/src/caic-xyz/caic", nil)
		tv := &fakeTaskView{instanceID: "ctr-1", repos: []runtime.Repo{
			{Branch: "caic-7", MountPath: "/home/user/src/caic-xyz/caic"},
			{Branch: "caic-0", HostPath: "/home/user/src/caic-xyz/md", MountPath: "/home/user/src/caic-xyz/md"},
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
		if repos[0].HostPath != "/home/user/src/caic-xyz/caic" {
			t.Errorf("primary HostPath = %q, want workspace dir", repos[0].HostPath)
		}
		if repos[0].MountPath != "/home/user/src/caic-xyz/caic" {
			t.Errorf("primary MountPath = %q, want qualified mount", repos[0].MountPath)
		}
		if repos[1].MountPath != "/home/user/src/caic-xyz/md" {
			t.Errorf("extra MountPath = %q, want qualified mount", repos[1].MountPath)
		}
	})
	t.Run("valid_no_repos", func(t *testing.T) {
		t.Parallel()
		r := newTestRepoWorkspace("", "/repo", nil)
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
		if _, _, err := newTestRepoWorkspace("", "", nil).taskRuntime(&fakeTaskView{}); err == nil {
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
			repo:  runtime.Repo{MountPath: "~/src/caic"},
			multi: true,
			want:  []string{"--src-prefix=a/caic/", "--dst-prefix=b/caic/"},
		},
		"multi repo path": {
			path:  "b/main.go",
			repo:  runtime.Repo{MountPath: "~/src/caic-xyz/caic"},
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
			repo: runtime.Repo{MountPath: "~/src/caic"},
			want: "caic",
		},
		"tilde collision mount": {
			repo: runtime.Repo{MountPath: "~/src/caic-xyz/caic"},
			want: "caic-xyz/caic",
		},
		"home source mount": {
			repo: runtime.Repo{MountPath: "/home/user/src/caic-xyz/caic"},
			want: "caic-xyz/caic",
		},
		"host fallback": {
			repo: runtime.Repo{HostPath: "/home/user/src/caic"},
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

// stubContainer implements runtime.Backend for testing. Diff returns a fixed
// numstat line; Fetch records that it was called.
type stubContainer struct {
	fetched   bool
	launchErr error // If set, Launch returns this error.
	fetchErr  error // If set, Fetch returns this error.
	stopped   bool
	diffIDs   []runtime.InstanceID
	fetchIDs  []runtime.InstanceID
	diffIdxs  []int
}

func (s *stubContainer) Launch(_ context.Context, _ []runtime.Repo, _ *runtime.StartOptions) (runtime.InstanceID, error) {
	if s.launchErr != nil {
		return "", s.launchErr
	}
	return "stub", nil
}

func (s *stubContainer) Connect(_ context.Context, id runtime.InstanceID, _ *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	return runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: string(id)}}, nil
}

func (s *stubContainer) Diff(_ context.Context, id runtime.InstanceID, repoIdx int, _ ...string) (string, error) {
	s.diffIDs = append(s.diffIDs, id)
	s.diffIdxs = append(s.diffIdxs, repoIdx)
	return "5\t1\tmain.go\n", nil
}

func (s *stubContainer) Fetch(_ context.Context, id runtime.InstanceID) error {
	s.fetched = true
	s.fetchIDs = append(s.fetchIDs, id)
	if s.fetchErr != nil {
		return s.fetchErr
	}
	return nil
}

func (s *stubContainer) Stop(_ context.Context, _ runtime.InstanceID) error {
	s.stopped = true
	return nil
}

func (s *stubContainer) Purge(_ context.Context, _ runtime.InstanceID) error {
	return nil
}

func (s *stubContainer) Revive(_ context.Context, _ runtime.InstanceID) error {
	return nil
}

func (s *stubContainer) Fork(_ context.Context, _ runtime.InstanceID, _ []runtime.Repo, _ *runtime.ForkOptions) (runtime.InstanceID, runtime.ConnectionInfo, []runtime.Repo, error) {
	return "stub-fork", runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: "stub-fork"}}, nil, nil
}
func (s *stubContainer) VNCPort(_ context.Context, _ runtime.InstanceID) int { return 0 }

func (s *stubContainer) Processes(_ context.Context, _ runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	return nil, nil
}

func (s *stubContainer) Signal(_ context.Context, _ runtime.InstanceID, _ int, _ string) error {
	return nil
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
