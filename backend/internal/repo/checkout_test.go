// Tests Checkout branch preparation and rollback, pushes, diffs, and summaries.

package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/gittest"
	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/metrics"
)

// fakeTaskView is a minimal TaskView implementation for tests. It cannot be
// replaced with *taskrun.Task: internal/task/taskrun imports internal/repo, so a
// test in package repo importing internal/task/taskrun would create an import
// cycle.
type fakeTaskView struct {
	instanceID runtime.ID
	repo       []runtime.Repo
	baseBranch string
}

func (f *fakeTaskView) GitTarget() GitTarget {
	return GitTarget{InstanceID: f.instanceID, Repos: f.repo}
}
func (f *fakeTaskView) SetRepoBranch(i int, branch string) { f.repo[i].Branch = branch }
func (f *fakeTaskView) PrimaryBaseBranch() string          { return f.baseBranch }

type testRuntimeSystem struct {
	testRuntimeBackend
	runtimetest.FakeInfo
}

func (*testRuntimeSystem) Name() runtime.Name { return "test-runtime" }

type testRuntimeBackend interface {
	runtime.Files
	runtime.Lifecycle
	runtime.Repository
}

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

func runtimeRemoteRef(id runtime.ID, branch string) string {
	return "refs/remotes/" + string(id.InstanceID()) + "/" + branch
}

func newTestCheckout(dir string) *Checkout {
	return &Checkout{
		Dir:         dir,
		RelPath:     filepath.Base(dir),
		GitTimeout:  time.Minute,
		PushTimeout: time.Minute,
	}
}

func newTestRuntime(t *testing.T, backend testRuntimeBackend) *runtime.Router {
	runtimes, err := runtime.NewRouter([]runtime.System{&testRuntimeSystem{testRuntimeBackend: backend}}, metrics.Nop{})
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
	t.Run("AllocateBranch", runAllocateBranchTests)
	t.Run("pushCommit", func(t *testing.T) {
		t.Parallel()
		// The host repository's own hook rejects everything: the container already
		// ran it, so the host must push without running any hook.
		root := initTestRepo(t)
		writePushFile(t, root, ".git/hooks/pre-push", "#!/bin/sh\nexit 1\n", 0o700)
		runGit(t, root, "checkout", "-b", "task")
		writePushFile(t, root, "task.txt", "task\n", 0o600)
		runGit(t, root, "add", "task.txt")
		runGit(t, root, "commit", "-m", "task")
		tip := runPushGit(t, root, "rev-parse", "HEAD")
		runGit(t, root, "checkout", "main")
		writePushFile(t, root, "README.md", "pending edit\n", 0o600)
		before := runPushGit(t, root, "status", "--porcelain=v2")
		g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
		d := PushDestination{Remote: "origin", Branch: "pushed"}
		if _, err := pushCommit(t.Context(), logtest.Logger(t), g, tip, &d, false); err != nil {
			t.Fatal(err)
		}
		if got := runPushGit(t, root, "ls-remote", "origin", "refs/heads/pushed"); !strings.HasPrefix(got, tip+"\t") {
			t.Fatal(got)
		}
		if got := runPushGit(t, root, "status", "--porcelain=v2"); got != before || runPushGit(t, root, "branch", "--show-current") != "main" {
			t.Fatal("host checkout changed")
		}
		// Rewriting the pushed branch needs force.
		runGit(t, root, "checkout", "-q", "--detach", "main")
		runGit(t, root, "commit", "-q", "--allow-empty", "-m", "other")
		other := runPushGit(t, root, "rev-parse", "HEAD")
		if _, err := pushCommit(t.Context(), logtest.Logger(t), g, other, &d, false); err == nil {
			t.Fatal("non-fast-forward push accepted")
		}
		if _, err := pushCommit(t.Context(), logtest.Logger(t), g, other, &d, true); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("Push", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			t.Run("PreservesCommits", func(t *testing.T) {
				t.Parallel()
				clone := initTestRepo(t)
				g := &git.Checkout{Root: clone, Logger: logtest.Logger(t)}
				runGit(t, clone, "checkout", "-b", "caic-1")
				for _, content := range []string{"first change\n", "second change\n"} {
					if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, clone, "commit", "-am", content)
				}
				tip, err := g.RevParse(t.Context(), "HEAD")
				if err != nil {
					t.Fatal(err)
				}
				id := runtime.NewID("test-runtime", "ctr-1")
				runGit(t, clone, "update-ref", runtimeRemoteRef(id, "caic-1"), tip)
				runGit(t, clone, "checkout", "main")
				before, err := g.RevParse(t.Context(), "main")
				if err != nil {
					t.Fatal(err)
				}
				w := newInitializedTestCheckout(t, clone)
				tv := &fakeTaskView{instanceID: id, repo: []runtime.Repo{{GitRoot: clone, Branch: "caic-1", ContainerPath: "/repo"}}}
				sc := newRecordingContainer()
				sc.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: tip}}
				res, err := w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), tv.GitTarget(), []PushDestination{{Remote: "origin", Branch: "main", BaseRef: "refs/remotes/origin/main"}}, PushOptions{})
				issues := res.SafetyIssues
				if err != nil {
					t.Fatal(err)
				}
				if fetches := sc.Fetches(); len(fetches) != 1 || fetches[0].Commit {
					t.Fatalf("fetch must leave task history and pending edits untouched: %+v", fetches)
				}
				if len(issues) != 0 {
					t.Fatalf("unexpected safety issues: %+v", issues)
				}
				if err := g.Fetch(t.Context()); err != nil {
					t.Fatal(err)
				}
				got, err := g.RevParse(t.Context(), "origin/main")
				if err != nil || got != tip {
					t.Fatalf("origin/main = %q, %v; want %q", got, err, tip)
				}
				got, err = g.RevParse(t.Context(), "HEAD")
				if err != nil || got != before {
					t.Fatalf("host HEAD = %q, %v; want %q", got, err, before)
				}
			})
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("RejectsNonFastForward", func(t *testing.T) {
				t.Parallel()
				clone := initTestRepo(t)
				g := &git.Checkout{Root: clone, Logger: logtest.Logger(t)}
				runGit(t, clone, "checkout", "-b", "caic-1")
				for _, content := range []string{"first change\n", "second change\n"} {
					if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, clone, "commit", "-am", content)
				}
				tip, err := g.RevParse(t.Context(), "HEAD")
				if err != nil {
					t.Fatal(err)
				}
				id := runtime.NewID("test-runtime", "ctr-1")
				runGit(t, clone, "update-ref", runtimeRemoteRef(id, "caic-1"), tip)
				runGit(t, clone, "checkout", "main")
				if err := os.WriteFile(filepath.Join(clone, "other.txt"), []byte("remote change\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, clone, "add", "other.txt")
				runGit(t, clone, "commit", "-m", "Remote advanced")
				runGit(t, clone, "push", "origin", "main")
				before, err := g.RevParse(t.Context(), "main")
				if err != nil {
					t.Fatal(err)
				}
				w := newInitializedTestCheckout(t, clone)
				tv := &fakeTaskView{instanceID: id, repo: []runtime.Repo{{GitRoot: clone, Branch: "caic-1", ContainerPath: "/repo"}}}
				sc := newRecordingContainer()
				sc.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: tip}}
				res, err := w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), tv.GitTarget(), []PushDestination{{Remote: "origin", Branch: "main", BaseRef: "refs/remotes/origin/main"}}, PushOptions{})
				issues := res.SafetyIssues
				if err == nil {
					t.Fatal("diverged main push succeeded")
				}
				if fetches := sc.Fetches(); len(fetches) != 1 || fetches[0].Commit {
					t.Fatalf("fetch must leave task history and pending edits untouched: %+v", fetches)
				}
				if len(issues) != 0 {
					t.Fatalf("unexpected safety issues: %+v", issues)
				}
				if err := g.Fetch(t.Context()); err != nil {
					t.Fatal(err)
				}
				got, err := g.RevParse(t.Context(), "origin/main")
				if err != nil || got != before {
					t.Fatalf("origin/main = %q, %v; want %q", got, err, before)
				}
				got, err = g.RevParse(t.Context(), "HEAD")
				if err != nil || got != before {
					t.Fatalf("host HEAD = %q, %v; want %q", got, err, before)
				}
			})
		})
	})
	t.Run("PushDestinations", func(t *testing.T) {
		t.Parallel()
		id := runtime.NewID("test-runtime", "ctr-1")
		roots := []string{initTestRepo(t), initTestRepo(t)}
		runGit(t, roots[1], "remote", "rename", "origin", "upstream")
		runGit(t, roots[1], "branch", "-m", "main", "trunk")
		runGit(t, roots[1], "push", "upstream", "trunk")
		runGit(t, roots[1], "symbolic-ref", "refs/remotes/upstream/HEAD", "refs/remotes/upstream/trunk")
		var target GitTarget
		target.InstanceID = id
		sc := newRecordingContainer()
		for i, root := range roots {
			g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
			runGit(t, root, "checkout", "-b", "caic-1")
			if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("task change\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, root, "commit", "-am", "task change")
			tip, err := g.RevParse(t.Context(), "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("/home/user/src/repo%d", i)
			taskPath, fetchedPath := path, path
			if i == 0 {
				// md resolves home-relative task paths in fetched results.
				taskPath = fmt.Sprintf("~/src/repo%d", i)
			} else {
				// Other runtimes may return home-relative fetched paths.
				fetchedPath = fmt.Sprintf("~/src/repo%d", i)
			}
			target.Repos = append(target.Repos, runtime.Repo{GitRoot: root, ContainerPath: taskPath, Branch: "caic-1"})
			sc.FetchedBranches = append(sc.FetchedBranches, runtime.FetchedBranch{RepositoryPath: fetchedPath, BranchName: "caic-1", CommitHash: tip})
			// Move the runtime tracking ref to a later commit carrying a secret.
			// The immutable Fetch result remains the task commit above.
			if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("sk-"+strings.Repeat("a", 24)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, root, "add", "secret.txt")
			runGit(t, root, "commit", "-m", "later secret")
			runGit(t, root, "update-ref", runtimeRemoteRef(id, "caic-1"), "HEAD")
		}
		w := newInitializedTestCheckout(t, roots[0])
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		target.Repos[0].GitRoot = ""
		target, destinations, err := w.ResolvePushDestinations(ctx, logtest.Logger(t), target, true)
		if err != nil {
			t.Fatal(err)
		}
		if target.Repos[0].GitRoot != roots[0] {
			t.Fatalf("primary host root not resolved: %+v", target.Repos[0])
		}
		res, err := w.Push(context.WithoutCancel(ctx), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
		ds, issues := res.DiffStat, res.SafetyIssues
		if err != nil || len(issues) != 0 || len(ds) != 2 {
			t.Fatalf("push pinned commits: stats=%+v issues=%+v err=%v", ds, issues, err)
		}
		if !slices.Equal(sc.refreshIDs, []runtime.ID{id}) {
			t.Fatalf("successful push must refresh container upstream refs: %v", sc.refreshIDs)
		}
		if destinations[0].Remote != "origin" || destinations[0].Branch != "main" || destinations[1].Remote != "upstream" || destinations[1].Branch != "trunk" {
			t.Fatalf("independent destinations: %+v", destinations)
		}
		for i, root := range roots {
			g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
			d := destinations[i]
			out, err := g.RunGit(t.Context(), "ls-remote", d.Remote, "refs/heads/"+d.Branch)
			if err != nil || !strings.HasPrefix(out, sc.FetchedBranches[i].CommitHash+"\t") {
				t.Fatalf("pushed commit %d = %q, %v", i, out, err)
			}
			if strings.Contains(ds[i].Path, "secret") {
				t.Fatalf("diff used moved ref: %+v", ds)
			}
		}
		// A safety issue in the second repository must prevent the first
		// repository from publishing its otherwise safe branch.
		g := &git.Checkout{Root: roots[1], Logger: logtest.Logger(t)}
		secretTip, err := g.RevParse(t.Context(), "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		sc.FetchedBranches[1].CommitHash = secretTip
		for i := range destinations {
			destinations[i].Branch = "blocked-task"
		}
		res, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{Force: true})
		issues = res.SafetyIssues
		if err != nil || len(issues) == 0 {
			t.Fatalf("second-repo safety check: %+v, %v", issues, err)
		}
		if len(sc.refreshIDs) != 1 {
			t.Fatal("blocked push refreshed container refs")
		}
		for i, root := range roots {
			g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
			out, err := g.RunGit(t.Context(), "ls-remote", destinations[i].Remote, "refs/heads/blocked-task")
			if err != nil || out != "" {
				t.Fatalf("blocked multi-repo push: %q, %v", out, err)
			}
		}
		// Incomplete fetch results cannot fall back to a stale tracking ref.
		sc.FetchedBranches = nil
		_, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
		if err == nil || !strings.Contains(err.Error(), "fetch did not return commit") {
			t.Fatalf("incomplete fetch: %v", err)
		}
		sc.FetchErr = errors.New("fetch unavailable")
		_, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
		if !errors.Is(err, sc.FetchErr) {
			t.Fatalf("failed fetch: %v", err)
		}
	})
	t.Run("PushPreHook", func(t *testing.T) {
		t.Parallel()
		root := initTestRepo(t)
		g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
		runGit(t, root, "branch", "caic-1")
		tip, err := g.RevParse(t.Context(), "caic-1")
		if err != nil {
			t.Fatal(err)
		}
		w := newInitializedTestCheckout(t, root)
		target := GitTarget{InstanceID: runtime.NewID("test-runtime", "ctr-1"), Repos: []runtime.Repo{{GitRoot: root, ContainerPath: "/repo", Branch: "caic-1", BaseBranch: "main"}}}
		target, destinations, err := w.ResolvePushDestinations(t.Context(), logtest.Logger(t), target, false)
		if err != nil {
			t.Fatal(err)
		}
		sc := newRecordingContainer()
		sc.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: tip}}
		sc.prePushErr = errors.New("verify failed")
		_, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
		if !errors.Is(err, sc.prePushErr) {
			t.Fatalf("hook rejection: %v", err)
		}
		if got := runPushGit(t, root, "ls-remote", "origin", "refs/heads/caic-1"); got != "" {
			t.Fatalf("rejected commit was pushed: %q", got)
		}
		sc.prePushErr = nil
		if _, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{}); err != nil {
			t.Fatal(err)
		}
		// A branch that does not exist yet reports the all-zero object ID, as Git does.
		want := runtime.PushRefs{Remote: destinations[0].Remote, Branch: "caic-1", RemoteCommit: strings.Repeat("0", len(tip))}
		if last := sc.prePushes[len(sc.prePushes)-1]; last.commit != tip || last.dest != want {
			t.Fatalf("hook ran for %+v, want %s %+v", last, tip, want)
		}
		if got := runPushGit(t, root, "ls-remote", "origin", "refs/heads/caic-1"); !strings.HasPrefix(got, tip+"\t") {
			t.Fatalf("verified commit not pushed: %q", got)
		}
		// An existing branch reports its own tip, not the default branch's.
		runGit(t, root, "checkout", "-q", "caic-1")
		runGit(t, root, "commit", "-q", "--allow-empty", "-m", "next")
		next, err := g.RevParse(t.Context(), "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, root, "checkout", "-q", "main")
		sc.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: next}}
		var hookOut strings.Builder
		if _, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{HookOutput: &hookOut}); err != nil {
			t.Fatal(err)
		}
		if got := sc.prePushOuts[len(sc.prePushOuts)-1]; got != io.Writer(&hookOut) {
			t.Fatalf("RunPrePush sink = %v, want the option's writer", got)
		}
		want = runtime.PushRefs{Remote: destinations[0].Remote, Branch: "caic-1", RemoteCommit: tip}
		if last := sc.prePushes[len(sc.prePushes)-1]; last.commit != next || last.dest != want {
			t.Fatalf("second hook ran for %+v, want %s %+v", last, next, want)
		}
	})
	t.Run("remoteBranchTip", func(t *testing.T) {
		t.Parallel()
		root := initTestRepo(t)
		g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
		tip := runPushGit(t, root, "rev-parse", "HEAD")
		got, err := remoteBranchTip(t.Context(), g, &PushDestination{Remote: "origin", Branch: "main"}, len(tip))
		if err != nil || got != tip {
			t.Fatalf("existing branch: %q, %v", got, err)
		}
		got, err = remoteBranchTip(t.Context(), g, &PushDestination{Remote: "origin", Branch: "absent"}, len(tip))
		if err != nil || got != strings.Repeat("0", len(tip)) {
			t.Fatalf("absent branch: %q, %v", got, err)
		}
		if _, err = remoteBranchTip(t.Context(), g, &PushDestination{Remote: "nowhere", Branch: "main"}, len(tip)); err == nil {
			t.Fatal("unreachable remote accepted")
		}
	})
	t.Run("PushTaskDiffReport", func(t *testing.T) {
		t.Parallel()
		for _, remoteBase := range []bool{false, true} {
			t.Run(fmt.Sprintf("RemoteBase=%v", remoteBase), func(t *testing.T) {
				t.Parallel()
				root := initTestRepo(t)
				g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
				runGit(t, root, "checkout", "-b", "feature")
				if err := os.WriteFile(filepath.Join(root, "baseline.txt"), []byte("existing feature baseline\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, root, "add", "baseline.txt")
				runGit(t, root, "commit", "-m", "existing baseline")
				if remoteBase {
					runGit(t, root, "push", "origin", "feature")
				}
				runGit(t, root, "branch", "caic-1")
				tip, err := g.RevParse(t.Context(), "caic-1")
				if err != nil {
					t.Fatal(err)
				}
				w := newInitializedTestCheckout(t, root)
				target := GitTarget{InstanceID: runtime.NewID("test-runtime", "ctr-1"), Repos: []runtime.Repo{{GitRoot: root, ContainerPath: "/repo", Branch: "caic-1", BaseBranch: "feature"}}}
				sc := newRecordingContainer()
				// md owns task-diff baselines, including local upstream overrides
				// and recorded integration refs. An unchanged task reports no diff
				// even though its non-default base differs from origin/main.
				sc.RepositoryStatusValue.DiffStat = nil
				sc.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: tip}}
				target, destinations, err := w.ResolvePushDestinations(t.Context(), logtest.Logger(t), target, false)
				if err != nil {
					t.Fatal(err)
				}
				res, err := w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
				ds, issues := res.DiffStat, res.SafetyIssues
				if err != nil || len(ds) != 0 || len(issues) != 0 {
					t.Fatalf("unchanged task report: stats=%+v issues=%+v err=%v", ds, issues, err)
				}
				if !res.Updated {
					t.Fatal("publishing a new task branch reported no update")
				}
				// Pending runtime edits belong in the public default-sync report
				// while the exact existing task commit is pushed without them.
				if err := os.WriteFile(filepath.Join(root, "pending.txt"), []byte("sk-"+strings.Repeat("a", 24)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				sc.RepositoryStatusValue.DiffStat = []runtime.GitFileStat{{Path: "pending.txt", LinesAdded: 1}}
				target, destinations, err = w.ResolvePushDestinations(t.Context(), logtest.Logger(t), target, true)
				if err != nil {
					t.Fatal(err)
				}
				res, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
				ds, issues = res.DiffStat, res.SafetyIssues
				if err != nil || len(ds) != 1 || ds[0].Path != "pending.txt" || len(issues) != 0 || destinations[0].Branch != "main" {
					t.Fatalf("pending report is separate from committed safety: stats=%+v issues=%+v destination=%+v err=%v", ds, issues, destinations[0], err)
				}
				if !res.Updated {
					t.Fatal("moving the default branch reported no update")
				}
				// Republishing the same commit is a no-op the caller can detect.
				res, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
				if err != nil || res.Updated || len(res.SafetyIssues) != 0 {
					t.Fatalf("republishing the same commit must report no update: %+v, %v", res, err)
				}
				out, err := g.RunGit(t.Context(), "ls-remote", "origin", "refs/heads/main")
				if err != nil || !strings.HasPrefix(out, tip+"\t") {
					t.Fatalf("default must push unchanged commit: %q, %v", out, err)
				}
				for _, opts := range sc.Fetches() {
					if opts.Commit {
						t.Fatal("push committed pending edits without authorization")
					}
				}
				sc.refreshErr = errors.New("container refs unavailable")
				_, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
				if !errors.Is(err, sc.refreshErr) || !strings.Contains(err.Error(), "push completed") {
					t.Fatalf("post-push refresh failure must report that publication completed: %v", err)
				}
				if _, ok := errors.AsType[*PushRefreshError](err); !ok {
					t.Fatalf("CI must distinguish a successful push with failed refresh: %v", err)
				}
			})
		}
	})
	t.Run("PushTimeout", func(t *testing.T) {
		t.Parallel()
		root := initTestRepo(t)
		w := newInitializedTestCheckout(t, root)
		w.GitTimeout = 10 * time.Millisecond
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		target := GitTarget{InstanceID: runtime.NewID("test-runtime", "ctr-1"), Repos: []runtime.Repo{{GitRoot: root, ContainerPath: "/repo", Branch: "caic-1"}}}
		sc := &timeoutFetchBackend{FakeBackend: &runtimetest.FakeBackend{}}
		_, err := w.Push(context.WithoutCancel(ctx), logtest.Logger(t), newTestRuntime(t, sc), target, []PushDestination{{Remote: "origin", Branch: "caic-1", BaseRef: "refs/remotes/origin/main"}}, PushOptions{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("detached fetch must remain bounded: %v", err)
		}
		w.GitTimeout = time.Nanosecond
		_, _, err = w.ResolvePushDestinations(ctx, logtest.Logger(t), target, false)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("detached destination resolution must remain bounded: %v", err)
		}
	})
	t.Run("PushPolicies", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name     string
				opts     PushOptions
				secret   bool
				diverged bool
				blocked  bool
			}{
				{name: "CommitPending", opts: PushOptions{CommitPending: true}},
				{name: "SafetyBlocksForce", opts: PushOptions{Force: true}, secret: true, blocked: true},
				{name: "BypassSafety", opts: PushOptions{BypassSafety: true}, secret: true},
				{name: "Force", opts: PushOptions{Force: true}, diverged: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					root := initTestRepo(t)
					g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
					runGit(t, root, "checkout", "-b", "caic-1")
					content := "safe task change\n"
					if tc.secret {
						content = "sk-" + strings.Repeat("a", 24) + "\n"
					}
					if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, root, "commit", "-am", "task change")
					tip, err := g.RevParse(t.Context(), "HEAD")
					if err != nil {
						t.Fatal(err)
					}
					if tc.diverged {
						runGit(t, root, "checkout", "main")
						runGit(t, root, "commit", "--allow-empty", "-m", "divergent destination")
						runGit(t, root, "push", "origin", "HEAD:caic-1")
					}
					sc := newRecordingContainer()
					sc.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: tip}}
					w := newInitializedTestCheckout(t, root)
					target := GitTarget{InstanceID: runtime.NewID("test-runtime", "ctr-1"), Repos: []runtime.Repo{{GitRoot: root, ContainerPath: "/repo", Branch: "caic-1"}}}
					res, err := w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, []PushDestination{{Remote: "origin", Branch: "caic-1", BaseRef: "refs/remotes/origin/main"}}, tc.opts)
					issues := res.SafetyIssues
					if err != nil || (len(issues) != 0) != tc.secret {
						t.Fatalf("issues=%+v err=%v", issues, err)
					}
					if got := sc.Fetches(); len(got) != 1 || got[0].Commit != tc.opts.CommitPending {
						t.Fatalf("commit policy: %+v", got)
					}
					out, err := g.RunGit(t.Context(), "ls-remote", "origin", "refs/heads/caic-1")
					if err != nil {
						t.Fatal(err)
					}
					if tc.blocked && out != "" {
						t.Fatalf("blocked push published branch: %q", out)
					}
					if !tc.blocked && !strings.HasPrefix(out, tip+"\t") {
						t.Fatalf("push did not preserve task commit: %q", out)
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("BypassDoesNotForce", func(t *testing.T) {
				t.Parallel()
				root := initTestRepo(t)
				g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
				runGit(t, root, "checkout", "-b", "caic-1")
				if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("safe task change\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, root, "commit", "-am", "task change")
				tip, err := g.RevParse(t.Context(), "HEAD")
				if err != nil {
					t.Fatal(err)
				}
				runGit(t, root, "checkout", "main")
				runGit(t, root, "commit", "--allow-empty", "-m", "divergent destination")
				runGit(t, root, "push", "origin", "HEAD:caic-1")

				sc := newRecordingContainer()
				sc.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: tip}}
				w := newInitializedTestCheckout(t, root)
				target := GitTarget{InstanceID: runtime.NewID("test-runtime", "ctr-1"), Repos: []runtime.Repo{{GitRoot: root, ContainerPath: "/repo", Branch: "caic-1"}}}
				res, err := w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, []PushDestination{{Remote: "origin", Branch: "caic-1", BaseRef: "refs/remotes/origin/main"}}, PushOptions{BypassSafety: true})
				issues := res.SafetyIssues
				if err == nil || len(issues) != 0 {
					t.Fatalf("issues=%+v err=%v, want push error", issues, err)
				}
				if got := sc.Fetches(); len(got) != 1 || got[0].Commit {
					t.Fatalf("commit policy: %+v", got)
				}
				out, err := g.RunGit(t.Context(), "ls-remote", "origin", "refs/heads/caic-1")
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(out, tip+"\t") {
					t.Fatalf("failed push updated remote branch: %q", out)
				}
			})
		})
	})
	t.Run("NewCheckout", func(t *testing.T) {
		t.Parallel()
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t)
			r := newInitializedTestCheckout(t, clone)
			if r.nextID != 0 {
				t.Errorf("nextID = %d, want 0", r.nextID)
			}
		})
		t.Run("SkipsExisting", func(t *testing.T) {
			t.Parallel()
			clone := initTestRepo(t)
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
			clone := initTestRepo(t)
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
			clone := initTestRepo(t)
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
			clone := initTestRepo(t)
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

	t.Run("AdoptLocalBranch", func(t *testing.T) {
		t.Parallel()
		clone := initTestRepo(t)
		runGit(t, clone, "branch", "local-work")
		runGit(t, clone, "branch", "--set-upstream-to", "origin/main", "local-work")
		r := newInitializedTestCheckout(t, clone)
		branches, err := r.AdoptableBranches(t.Context(), logtest.Logger(t))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(branches, "local-work") {
			t.Fatalf("AdoptableBranches = %v, want local-work", branches)
		}
		tv := &fakeTaskView{
			baseBranch: "local-work",
			repo:       []runtime.Repo{{GitRoot: clone, BaseBranch: "local-work", Branch: "local-work"}},
		}
		if err := r.FetchAndCreateBranch(t.Context(), logtest.Logger(t), tv, "local-work"); err != nil {
			t.Fatal(err)
		}
		r.DeleteUnmodifiedTaskBranches(t.Context(), logtest.Logger(t), tv)
		if _, err := (&git.Checkout{Root: clone, Logger: logtest.Logger(t)}).RevParse(t.Context(), "refs/heads/local-work"); err != nil {
			t.Fatalf("adopted branch was deleted: %v", err)
		}
	})
	t.Run("BranchDiffStatNoContainer", func(t *testing.T) {
		t.Parallel()
		r := newTestCheckout(t.TempDir())
		if _, err := r.DiffStat(t.Context(), logtest.Logger(t), nil, GitTarget{}); err == nil {
			t.Error("BranchDiffStat with no instance succeeded")
		}
	})

	// DiffStat covers the compact card probe: the combined diff
	// stat, the per-repo state summary, and how partial probe failures degrade so
	// one failing repository cannot blank the whole task card.
	t.Run("DiffStat", func(t *testing.T) {
		t.Parallel()
		status := runtime.RepositoryStatus{
			Branch:   "caic-3",
			Upstream: "origin/main",
			Ahead:    2,
			Behind:   1,
			DiffStat: []runtime.GitFileStat{
				{Path: "main.go", LinesAdded: 5, LinesDeleted: 1},
				{Path: "logo.png", Binary: true, OldSize: 10, NewSize: 20},
			},
			Uncommitted: []runtime.GitFileStatus{
				{Path: "main.go", WorktreeStatus: "M"},
				{Path: "conflict.txt", IndexStatus: "U", WorktreeStatus: "U"},
			},
		}

		t.Run("single repo", func(t *testing.T) {
			t.Parallel()
			sc := newRecordingContainer()
			sc.RepositoryStatusValue = status
			r := newTestCheckout("/repo")
			tv := &fakeTaskView{instanceID: runtime.NewID("test-runtime", "ctr-1"), repo: []runtime.Repo{{GitRoot: "/repo", Branch: "feature", ContainerPath: "/repo"}}}

			snapshot, err := r.DiffStat(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), tv.GitTarget())
			ds, states := snapshot.DiffStat, snapshot.RepoStates
			if snapshot.Read.InstanceID != tv.instanceID {
				t.Fatalf("snapshot instance = %q", snapshot.Read.InstanceID)
			}
			if err != nil {
				t.Fatal(err)
			}
			wantDS := v3.DiffStat{
				{Path: "main.go", LinesAdded: 5, LinesDeleted: 1},
				{Path: "logo.png", Binary: true, OldSize: 10, NewSize: 20},
			}
			if !slices.Equal(ds, wantDS) {
				t.Errorf("DiffStat = %+v, want %+v", ds, wantDS)
			}
			wantStates := []v3.RepoState{{
				RepoIndex:        0,
				Branch:           "caic-3",
				Ahead:            2,
				Behind:           1,
				ChangedFiles:     2,
				LinesAdded:       5,
				LinesDeleted:     1,
				UncommittedFiles: 2,
				Conflicts:        1,
			}}
			if !slices.Equal(states, wantStates) {
				t.Errorf("RepoStates = %+v, want %+v", states, wantStates)
			}
		})

		t.Run("multi repo prefixes paths and aligns indexes", func(t *testing.T) {
			t.Parallel()
			sc := newRecordingContainer()
			sc.RepositoryStatusValue = status
			r := newTestCheckout("/home/user/src/caic")
			repos := []runtime.Repo{
				{GitRoot: "/home/user/src/caic", Branch: "caic-7", ContainerPath: "/home/user/src/caic"},
				{GitRoot: "/home/user/src/genai", Branch: "caic-0", ContainerPath: "/home/user/src/genai"},
			}

			snapshot, err := r.DiffStat(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), GitTarget{InstanceID: "test-runtime:ctr-2", Repos: repos})
			ds, states := snapshot.DiffStat, snapshot.RepoStates
			if err != nil {
				t.Fatal(err)
			}
			if len(ds) != 4 {
				t.Fatalf("DiffStat len = %d, want 4", len(ds))
			}
			if ds[2].Path != "genai/main.go" {
				t.Errorf("third path = %q, want genai/main.go", ds[2].Path)
			}
			for i, state := range states {
				if state.RepoIndex != i {
					t.Errorf("state %d RepoIndex = %d", i, state.RepoIndex)
				}
			}
		})

		t.Run("partial probe failure keeps the other repositories", func(t *testing.T) {
			t.Parallel()
			sc := newRecordingContainer()
			sc.RepositoryStatusValue = status
			r := newTestCheckout("/home/user/src/caic")
			repos := []runtime.Repo{
				{GitRoot: "/home/user/src/caic", Branch: "caic-7", ContainerPath: "/home/user/src/caic"},
				{GitRoot: "/home/user/src/genai", Branch: "caic-0", ContainerPath: "/home/user/src/genai"},
			}
			runtimes := newTestRuntime(t, &compactFailingBackend{FakeBackend: sc.FakeBackend, failIdx: 1})

			snapshot, err := r.DiffStat(t.Context(), logtest.Logger(t), runtimes, GitTarget{InstanceID: "test-runtime:ctr-2", Repos: repos})
			ds, states := snapshot.DiffStat, snapshot.RepoStates
			if err == nil {
				t.Fatal("want the probe error")
			}
			// The successful repository must still reach the card instead of the
			// whole update being dropped.
			if len(ds) != 2 || slices.ContainsFunc(ds, func(f v3.DiffFileStat) bool { return f.Path == "genai/main.go" }) {
				t.Errorf("DiffStat = %+v, want only the first repository", ds)
			}
			if len(states) != 1 || states[0].RepoIndex != 0 {
				t.Errorf("RepoStates = %+v, want only repo 0", states)
			}
		})

		t.Run("all probes fail", func(t *testing.T) {
			t.Parallel()
			sc := newRecordingContainer()
			r := newTestCheckout("/repo")
			repos := []runtime.Repo{{GitRoot: "/repo", Branch: "feature", ContainerPath: "/repo"}}
			runtimes := newTestRuntime(t, &compactFailingBackend{FakeBackend: sc.FakeBackend, failIdx: 0})

			snapshot, err := r.DiffStat(t.Context(), logtest.Logger(t), runtimes, GitTarget{InstanceID: "test-runtime:ctr-1", Repos: repos})
			ds, states := snapshot.DiffStat, snapshot.RepoStates
			if err == nil {
				t.Fatal("want the probe error")
			}
			// With nothing to report the caller must not emit an update, so a
			// transient failure keeps the previously pushed card stats.
			if len(ds) != 0 || len(states) != 0 {
				t.Errorf("DiffStat = %+v, RepoStates = %+v, want both empty", ds, states)
			}
		})
	})

	t.Run("queryRuntime", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_preserves_mounted_path", func(t *testing.T) {
			t.Parallel()
			r := newTestCheckout("/home/user/src/caic-xyz/caic")
			tv := &fakeTaskView{instanceID: "ctr-1", repo: []runtime.Repo{
				{Branch: "caic-7", ContainerPath: "/home/user/src/caic-xyz/caic"},
				{Branch: "caic-0", GitRoot: "/home/user/src/caic-xyz/md", ContainerPath: "/home/user/src/caic-xyz/md"},
			}}

			id, repos, err := r.queryRuntime(tv.GitTarget())
			if err != nil {
				t.Fatalf("queryRuntime: %v", err)
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
			id, repos, err := r.queryRuntime(tv.GitTarget())
			if err != nil {
				t.Fatalf("queryRuntime: %v", err)
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
			if _, _, err := newTestCheckout(t.TempDir()).queryRuntime(GitTarget{}); err == nil {
				t.Fatal("want error")
			}
		})
	})
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

// recordingContainer supplies fixed repository statistics and records task
// fetching and ref refreshes for push and snapshot behavior tests.
type recordingContainer struct {
	*runtimetest.FakeBackend

	fetchIDs    []runtime.ID
	refreshIDs  []runtime.ID
	refreshErr  error
	prePushes   []prePushCall
	prePushOuts []io.Writer
	prePushErr  error
	diffIDs     []runtime.ID
	diffIdxs    []int
}

// newRecordingContainer supplies a one-file repository summary.
func newRecordingContainer() *recordingContainer {
	return &recordingContainer{FakeBackend: &runtimetest.FakeBackend{RepositoryStatusValue: runtime.RepositoryStatus{DiffStat: []runtime.GitFileStat{{Path: "main.go", LinesAdded: 5, LinesDeleted: 1}}}}}
}

func (c *recordingContainer) RunPrePush(_ context.Context, _ runtime.ID, _ int, commit string, dest runtime.PushRefs, out io.Writer) error {
	c.prePushes = append(c.prePushes, prePushCall{commit, dest})
	c.prePushOuts = append(c.prePushOuts, out)
	return c.prePushErr
}

func (c *recordingContainer) RefreshRefs(_ context.Context, id runtime.ID) error {
	c.refreshIDs = append(c.refreshIDs, id)
	return c.refreshErr
}

func (c *recordingContainer) Fetch(ctx context.Context, id runtime.ID, opts runtime.FetchOpts) ([]runtime.FetchedBranch, error) {
	c.fetchIDs = append(c.fetchIDs, id)
	return c.FakeBackend.Fetch(ctx, id, opts)
}

func (c *recordingContainer) Diff(ctx context.Context, id runtime.ID, repoIdx int, patch runtime.DiffPatch) (string, error) {
	c.diffIDs = append(c.diffIDs, id)
	c.diffIdxs = append(c.diffIdxs, repoIdx)
	return c.FakeBackend.Diff(ctx, id, repoIdx, patch)
}

type prePushCall struct {
	commit string
	dest   runtime.PushRefs
}

// timeoutFetchBackend models a runtime fetch stalled until its operation timeout.
type timeoutFetchBackend struct {
	*runtimetest.FakeBackend
}

func (*timeoutFetchBackend) Fetch(ctx context.Context, _ runtime.ID, _ runtime.FetchOpts) ([]runtime.FetchedBranch, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// compactFailingBackend fails the compact status probe for one repository
// index, simulating the transient container probe failures the task card must
// survive.
type compactFailingBackend struct {
	*runtimetest.FakeBackend

	failIdx int
}

func (b *compactFailingBackend) CompactRepositoryStatus(_ context.Context, _ runtime.ID, repoIdx int) (runtime.RepositoryStatus, error) {
	if repoIdx == b.failIdx {
		return runtime.RepositoryStatus{}, errors.New("probe failed")
	}
	return b.RepositoryStatusValue, nil
}

// initTestRepo creates a bare "remote" and a local clone with one commit on
// main. Returns the clone directory. origin points to the bare repo so git
// fetch/push work locally.
func initTestRepo(t *testing.T) string {
	dir := t.TempDir()
	clone := filepath.Join(dir, "clone")
	if err := gittest.InitClonedRepo(t.Context(), filepath.Join(dir, "remote.git"), clone, "main"); err != nil {
		t.Fatal(err)
	}
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
		want := []v3.DiffFileStat{
			{Path: "src/main.go", LinesAdded: 10, LinesDeleted: 3},
			{Path: "src/util.go", LinesAdded: 5, LinesDeleted: 0},
		}
		for i, f := range ds {
			if f != want[i] {
				t.Errorf("files[%d] = %+v, want %+v", i, f, want[i])
			}
		}
	})

	t.Run("Binary", func(t *testing.T) {
		t.Parallel()
		input := strings.Join([]string{
			"-\t-\timage.png",
			" image.png | Bin 100 -> 250 bytes",
			" 1 file changed, 0 insertions(+), 0 deletions(-)",
		}, "\n") + "\n"
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
		if f.OldSize != 100 || f.NewSize != 250 {
			t.Errorf("sizes = %d -> %d, want 100 -> 250", f.OldSize, f.NewSize)
		}
	})

	t.Run("BinarySizesMatchStatOrder", func(t *testing.T) {
		t.Parallel()
		input := strings.Join([]string{
			"10\t3\tsrc/main.go",
			"-\t-\tassets/logo.png",
			"2\t1\tREADME.md",
			" src/main.go   | 10 +--",
			" assets/logo.png | Bin 0 -> 4096 bytes",
			" README.md     |  2 +-",
			" 3 files changed",
		}, "\n") + "\n"
		ds := ParseDiffNumstat(input)
		if len(ds) != 3 {
			t.Fatalf("files = %d, want 3", len(ds))
		}
		if ds[1].OldSize != 0 || ds[1].NewSize != 4096 {
			t.Errorf("logo sizes = %d -> %d, want 0 -> 4096", ds[1].OldSize, ds[1].NewSize)
		}
		if ds[0].Binary || ds[2].Binary {
			t.Errorf("text files marked binary: %+v", ds)
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

func BenchmarkParseDiffNumstat(b *testing.B) {
	var numstat, stat strings.Builder
	for i := range 100 {
		path := fmt.Sprintf("src/pkg/file-%03d.go", i)
		fmt.Fprintf(&numstat, "12\t3\t%s\n", path)
		fmt.Fprintf(&stat, " %s | 12 +--\n", path)
	}
	for i := range 10 {
		path := fmt.Sprintf("assets/image-%02d.bin", i)
		fmt.Fprintf(&numstat, "-\t-\t%s\n", path)
		fmt.Fprintf(&stat, " %s | Bin %d -> %d bytes\n", path, i*1024, (i+1)*1024)
	}
	input := numstat.String() + stat.String()
	b.ReportAllocs()
	for b.Loop() {
		if got := ParseDiffNumstat(input); len(got) != 110 {
			b.Fatalf("files = %d, want 110", len(got))
		}
	}
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
		clone := initTestRepo(t)
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
		clone := initTestRepo(t)
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
		clone := initTestRepo(t)
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

type summaryProbeBackend struct {
	*runtimetest.FakeBackend

	read func(runtime.ID)
	err  error
}

func (b *summaryProbeBackend) RepositoryStatus(_ context.Context, id runtime.ID, _ int) (runtime.RepositoryStatus, error) {
	b.read(id)
	return b.RepositoryStatusValue, b.err
}

func TestRepositoryStatusesSnapshot(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		checkout := newTestCheckout(t.TempDir())
		tv := &fakeTaskView{instanceID: "test-runtime:original", repo: []runtime.Repo{{Branch: "feature", ContainerPath: "/workspace/repo"}}}
		var readID runtime.ID
		backend := &summaryProbeBackend{
			FakeBackend: &runtimetest.FakeBackend{RepositoryStatusValue: runtime.RepositoryStatus{
				Branch: "feature", Behind: 2, DiffStat: []runtime.GitFileStat{{Path: "fresh.go", LinesAdded: 4}},
			}},
			read: func(id runtime.ID) { readID = id; tv.instanceID = "test-runtime:replacement" },
		}
		snapshot, err := checkout.RepositoryStatuses(t.Context(), newTestRuntime(t, backend), tv.GitTarget())
		if err != nil || len(snapshot.Statuses) != 1 {
			t.Fatalf("snapshot=%+v, err=%v", snapshot, err)
		}
		if snapshot.Read.InstanceID != readID || readID != "test-runtime:original" {
			t.Errorf("snapshot instance %q differs from read %q", snapshot.Read.InstanceID, readID)
		}
		if len(snapshot.DiffStat) != 1 || snapshot.DiffStat[0].Path != "fresh.go" || len(snapshot.RepoStates) != 1 || snapshot.RepoStates[0].Behind != 2 || snapshot.RepoStates[0].LinesAdded != 4 {
			t.Errorf("summary differs from read snapshot: %+v", snapshot)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		checkout := newTestCheckout(t.TempDir())
		tv := &fakeTaskView{instanceID: "test-runtime:original", repo: []runtime.Repo{{Branch: "feature", ContainerPath: "/workspace/repo"}}}
		backend := &summaryProbeBackend{
			FakeBackend: &runtimetest.FakeBackend{RepositoryStatusValue: runtime.RepositoryStatus{
				Branch: "feature", Behind: 2, DiffStat: []runtime.GitFileStat{{Path: "fresh.go", LinesAdded: 4}},
			}},
			read: func(id runtime.ID) { tv.instanceID = "test-runtime:replacement" },
			err:  errors.New("read failed"),
		}
		snapshot, err := checkout.RepositoryStatuses(t.Context(), newTestRuntime(t, backend), tv.GitTarget())
		if err == nil || snapshot.Read.NewerThan(GitRead{}) || len(snapshot.Statuses) != 0 {
			t.Fatalf("failed read returned publishable snapshot: %+v, %v", snapshot, err)
		}
	})
}

func TestGitProbeCompletionOrder(t *testing.T) {
	t.Parallel()
	checkout := newTestCheckout(t.TempDir())
	backend := &runtimetest.FakeBackend{RepositoryStatusValue: runtime.RepositoryStatus{Branch: "feature", DiffStat: []runtime.GitFileStat{{Path: "main.go", LinesAdded: 5, LinesDeleted: 1}}}}
	runtimes := newTestRuntime(t, backend)
	tv := &fakeTaskView{instanceID: "test-runtime:ctr", repo: []runtime.Repo{{Branch: "feature"}}}
	full, err := checkout.RepositoryStatuses(t.Context(), runtimes, tv.GitTarget())
	if err != nil {
		t.Fatal(err)
	}
	compact, err := checkout.DiffStat(t.Context(), logtest.Logger(t), runtimes, tv.GitTarget())
	if err != nil {
		t.Fatal(err)
	}
	diff, err := checkout.DiffStat(t.Context(), logtest.Logger(t), runtimes, tv.GitTarget())
	if err != nil {
		t.Fatal(err)
	}
	// Replacing the checkout must not restart the order used by live tasks.
	replacement, err := newTestCheckout(t.TempDir()).RepositoryStatuses(t.Context(), runtimes, tv.GitTarget())
	if err != nil {
		t.Fatal(err)
	}
	if !full.Read.NewerThan(GitRead{}) || !compact.Read.NewerThan(full.Read) || !diff.Read.NewerThan(compact.Read) || !replacement.Read.NewerThan(diff.Read) {
		t.Fatal("full, compact, result and replacement probes do not share completion order")
	}
	if len(diff.DiffStat) != 1 || diff.DiffStat[0].Path != "main.go" {
		t.Fatalf("result stats lost: %+v", diff.DiffStat)
	}
}

func writePushFile(t *testing.T, root, name, content string, mode os.FileMode) {
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { // #nosec G703 -- Directories belong to isolated fixture processes.
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func runPushGit(t *testing.T, root string, args ...string) string {
	out, err := (&git.Checkout{Root: root, Logger: logtest.Logger(t)}).RunGit(t.Context(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func runAllocateBranchTests(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		for _, mode := range []string{"default", "local_override", "local_fallback", "preferred_collision"} {
			t.Run(mode, func(t *testing.T) {
				t.Parallel()
				root := initTestRepo(t)
				w := newInitializedTestCheckout(t, root)
				g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
				base, preferred, start, upstream := "", "caic-0", "origin/main", "refs/remotes/origin/main"
				if mode == "local_override" || mode == "local_fallback" {
					runGit(t, root, "checkout", "-b", "feature")
					runGit(t, root, "commit", "--allow-empty", "-m", "local feature")
					runGit(t, root, "checkout", "main")
					start = "feature"
					upstream = "refs/heads/feature"
					if mode == "local_override" {
						base = "feature"
						runGit(t, root, "push", "origin", "main:feature")
					} else {
						w.BaseBranch = "feature"
					}
				}
				if mode == "preferred_collision" {
					runGit(t, root, "branch", "caic-0")
				}
				want, err := g.RevParse(t.Context(), start)
				if err != nil {
					t.Fatal(err)
				}
				b, err := w.AllocateBranch(t.Context(), logtest.Logger(t), base, preferred)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "preferred_collision" && b.Name != "caic-1" {
					t.Fatalf("fallback=%q", b.Name)
				}
				if got, err := g.RevParse(t.Context(), b.Name); err != nil || got != want {
					t.Fatalf("branch tip=%q %v want=%q", got, err, want)
				}
				if got, err := g.RunGit(t.Context(), "for-each-ref", "--format=%(upstream)", "refs/heads/"+b.Name); err != nil || got != upstream {
					t.Fatalf("upstream=%q %v want=%q", got, err, upstream)
				}
				if err := b.Rollback(t.Context(), logtest.Logger(t)); err != nil {
					t.Fatal(err)
				}
				if got, err := g.RunGit(t.Context(), "for-each-ref", "--format=%(refname)", "refs/heads/"+b.Name); err != nil || got != "" {
					t.Fatalf("rollback refs=%q %v", got, err)
				}
				if got, err := g.RunGit(t.Context(), "config", "--get-regexp", "^branch\\.caic-"); err == nil || got != "" {
					t.Fatalf("rollback config=%q %v", got, err)
				}
			})
		}
	})
	t.Run("error_upstream_setup_rolls_back_created_branch", func(t *testing.T) {
		if goruntime.GOOS == "windows" {
			t.Skip("Git hook fixture requires POSIX shell")
		}
		root := initTestRepo(t)
		w := newInitializedTestCheckout(t, root)
		runGit(t, root, "branch", "feature")
		writePushFile(t, root, ".git/hooks/reference-transaction", `#!/bin/sh
set -eu
while read -r old new ref; do
  if [ "$1" = committed ] && [ "$ref" = refs/heads/caic-0 ]; then
    git -c core.hooksPath=/dev/null update-ref -d refs/heads/feature
  fi
done
`, 0o700)
		if _, err := w.AllocateBranch(t.Context(), logtest.Logger(t), "feature", "caic-0"); err == nil {
			t.Fatal("missing upstream accepted")
		}
		g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
		if got, err := g.RunGit(t.Context(), "for-each-ref", "--format=%(refname)", "refs/heads/caic-*"); err != nil || got != "" {
			t.Fatalf("failed allocation left branch=%q %v", got, err)
		}
	})
	t.Run("pins_commit_before_creation", func(t *testing.T) {
		if goruntime.GOOS == "windows" {
			t.Skip("Git hook fixture requires POSIX shell")
		}
		root := initTestRepo(t)
		w := newInitializedTestCheckout(t, root)
		g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
		want, err := g.RevParse(t.Context(), "origin/main")
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, root, "commit", "--allow-empty", "-m", "new main")
		writePushFile(t, root, ".git/hooks/reference-transaction", `#!/bin/sh
set -eu
while read -r old new ref; do
  if [ "$1" = prepared ] && [ "$ref" = refs/heads/caic-0 ]; then
    git -c core.hooksPath=/dev/null update-ref refs/remotes/origin/main HEAD
  fi
done
`, 0o700)
		b, err := w.AllocateBranch(t.Context(), logtest.Logger(t), "", "caic-0")
		if err != nil {
			t.Fatal(err)
		}
		if got, err := g.RevParse(t.Context(), b.Name); err != nil || got != want {
			t.Fatalf("created tip=%q %v want pinned=%q", got, err, want)
		}
		if err := b.Rollback(t.Context(), logtest.Logger(t)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAllocatedBranch(t *testing.T) {
	t.Parallel()
	t.Run("Rollback", func(t *testing.T) {
		t.Parallel()
		t.Run("preserves_commit_moved_during_creation", func(t *testing.T) {
			t.Parallel()
			if goruntime.GOOS == "windows" {
				t.Skip("Git hook fixture requires POSIX shell")
			}
			root := initTestRepo(t)
			w := newInitializedTestCheckout(t, root)
			runGit(t, root, "commit", "--allow-empty", "-m", "external work")
			writePushFile(t, root, ".git/hooks/reference-transaction", `#!/bin/sh
set -eu
while read -r old new ref; do
  if [ "$1" = committed ] && [ "$ref" = refs/heads/caic-0 ]; then
    git -c core.hooksPath=/dev/null update-ref refs/heads/caic-0 HEAD
  fi
done
`, 0o700)
			b, err := w.AllocateBranch(t.Context(), logtest.Logger(t), "", "caic-0")
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Rollback(t.Context(), logtest.Logger(t)); err != nil {
				t.Fatal(err)
			}
			g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
			want, err := g.RevParse(t.Context(), "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if got, err := g.RevParse(t.Context(), b.Name); err != nil || got != want {
				t.Fatalf("external commit removed: %q %v", got, err)
			}
		})
		t.Run("error_ref_lock_preserves_branch", func(t *testing.T) {
			t.Parallel()
			root := initTestRepo(t)
			w := newInitializedTestCheckout(t, root)
			b, err := w.AllocateBranch(t.Context(), logtest.Logger(t), "", "caic-0")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".git", "refs", "heads", b.Name+".lock"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := b.Rollback(t.Context(), logtest.Logger(t)); err == nil {
				t.Fatal("rollback hid ref-lock failure")
			}
			g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
			if _, err := g.RevParse(t.Context(), b.Name); err != nil {
				t.Fatalf("locked branch removed: %v", err)
			}
		})
		t.Run("removes_config_of_deleted_branch", func(t *testing.T) {
			t.Parallel()
			root := initTestRepo(t)
			w := newInitializedTestCheckout(t, root)
			b, err := w.AllocateBranch(t.Context(), logtest.Logger(t), "", "caic-0")
			if err != nil {
				t.Fatal(err)
			}
			g := &git.Checkout{Root: root, Logger: logtest.Logger(t)}
			// AllocateBranch records the upstream, so the section must exist for
			// the deletion below to prove anything.
			if got, err := g.RunGit(t.Context(), "config", "--get-regexp", "^branch\\."+b.Name+"\\."); err != nil || got == "" {
				t.Fatalf("prepared config=%q %v", got, err)
			}
			// Another writer deletes the ref before the rollback compares it.
			runGit(t, root, "update-ref", "-d", "refs/heads/"+b.Name)
			if err := b.Rollback(t.Context(), logtest.Logger(t)); err != nil {
				t.Fatal(err)
			}
			if got, err := g.RunGit(t.Context(), "config", "--get-regexp", "^branch\\.caic-"); err == nil || got != "" {
				t.Fatalf("rollback config=%q %v", got, err)
			}
		})
	})
}

// TestMain isolates the package from the Git configuration of the host.
func TestMain(m *testing.M) {
	gittest.Isolate()
	os.Exit(m.Run())
}
