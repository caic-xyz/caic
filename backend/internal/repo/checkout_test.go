// Tests Checkout branch allocation, pooled pushes, restart safety, diffs, and ordered summaries.

package repo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/metrics"
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
		PushDir:     filepath.Join(filepath.Dir(dir), "push"),
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
	checkout, err := NewCheckout(t.Context(), logtest.Logger(t), dir, t.TempDir(), "main", liveBranches)
	if err != nil {
		t.Fatal(err)
	}
	return checkout
}

//nolint:tparallel // Environment-changing push fixtures run before parallel cases.
func TestCheckout(t *testing.T) {
	// These process-wide fixtures must run before parallel checkout cases.
	t.Run("pushWorktree", func(t *testing.T) {
		t.Run("OverallDeadlineIncludesCleanup", func(t *testing.T) {
			if goruntime.GOOS == "windows" {
				t.Skip("shell fixture requires Unix")
			}
			// PATH is process-wide, so this fixture deliberately runs without Parallel.
			root := initTestRepo(t, "main")
			w := newInitializedTestCheckout(t, root)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			marker := filepath.Join(bin, "cleanup-started")
			writePushFile(t, bin, "git", "#!/bin/sh\nif [ \"$1\" = worktree ] && [ \"$2\" = list ]; then\n  printf started > '"+marker+"'\n  sleep 3\n  exit 1\nfi\nexec '"+realGit+"' \"$@\"\n", 0o700)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			t.Cleanup(cancel)
			started := time.Now()
			err = w.pushWorktree(ctx, logtest.Logger(t), root, "HEAD", PushDestination{Remote: "origin", Branch: "deadline"}, false)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "retained") {
				t.Fatalf("overall deadline or retention outcome lost: %v", err)
			}
			if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
				t.Fatalf("cleanup extended overall push deadline: %v", elapsed)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal("deadline fixture never reached Git cleanup", err)
			}
			dirs := pushOperationDirs(t, w)
			if len(dirs) != 1 {
				t.Fatal("timed-out cleanup lost operation directory")
			}
			owner, err := os.ReadFile(filepath.Join(dirs[0], "owner-v1"))
			if err != nil || string(owner) != canonicalPath(t, filepath.Join(root, ".git")) {
				t.Fatalf("timed-out cleanup lost ownership: %q, %v", owner, err)
			}
			if got := runPushGit(t, root, "ls-remote", "origin", "refs/heads/deadline"); got == "" {
				t.Fatal("push did not finish before cleanup deadline")
			}
		})
		t.Run("CacheSystemd", func(t *testing.T) {
			// Process-wide HOME/XDG settings intentionally model a service environment.
			taskHome := t.TempDir()
			t.Setenv("HOME", taskHome)
			t.Setenv("XDG_CACHE_HOME", "")
			root := initTestRepo(t, "main")
			cache := t.TempDir()
			w, err := DiscoverCheckout(t.Context(), logtest.Logger(t), root, cache, nil)
			if err != nil {
				t.Fatal(err)
			}
			if w.PushDir != canonicalPath(t, filepath.Join(cache, "push")) {
				t.Fatalf("push directory=%q", w.PushDir)
			}
			if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, "HEAD", PushDestination{Remote: "origin", Branch: "service"}, false); err != nil {
				t.Fatal(err)
			}
			assertPushIdle(t, w, root)
			if _, err := os.Stat(filepath.Join(taskHome, ".cache")); !os.IsNotExist(err) {
				t.Fatalf("used ambient home cache: %v", err)
			}
		})
		t.Run("Core", func(t *testing.T) {
			t.Parallel()
			t.Run("OrdinaryHookDirtySource", func(t *testing.T) {
				t.Parallel()
				root := initTestRepo(t, "main")
				// Exercise the real HEAD-only hook with a small fixture-owned static gate.
				hook, err := os.ReadFile("../../../scripts/hooks/pre-push")
				if err != nil {
					t.Fatal(err)
				}
				writePushFile(t, root, "scripts/hooks/pre-push", string(hook), 0o700)
				writePushFile(t, root, "Makefile", "verify:\n\t@test \"$$(cat README.md)\" = task\n", 0o600)
				writePushFile(t, root, ".gitignore", "ignored/\n", 0o600)
				runGit(t, root, "add", ".")
				runGit(t, root, "commit", "-m", "Install checks")
				runGit(t, root, "checkout", "-b", "task")
				writePushFile(t, root, "README.md", "task\n", 0o600)
				runGit(t, root, "commit", "-am", "Task change")
				tip := strings.TrimSpace(runPushGit(t, root, "rev-parse", "HEAD"))
				runGit(t, root, "checkout", "main")
				runGit(t, root, "config", "core.hooksPath", "scripts/hooks")
				writePushFile(t, root, "README.md", "staged\n", 0o600)
				runGit(t, root, "add", "README.md")
				writePushFile(t, root, "README.md", "unstaged\n", 0o600)
				writePushFile(t, root, "untracked", "keep\n", 0o600)
				writePushFile(t, root, "ignored/cache", "source cache\n", 0o600)
				before := runPushGit(t, root, "status", "--porcelain=v2")
				index := runPushGit(t, root, "diff", "--cached")
				head := runPushGit(t, root, "rev-parse", "HEAD")
				w := newInitializedTestCheckout(t, root)
				if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, tip, PushDestination{Remote: "origin", Branch: "task"}, false); err != nil {
					t.Fatal(err)
				}
				if got := runPushGit(t, root, "ls-remote", "origin", "refs/heads/task"); !strings.HasPrefix(got, tip+"\t") {
					t.Fatal(got)
				}
				if runPushGit(t, root, "status", "--porcelain=v2") != before || runPushGit(t, root, "diff", "--cached") != index || runPushGit(t, root, "rev-parse", "HEAD") != head {
					t.Fatal("source checkout changed")
				}
				for name, want := range map[string]string{"README.md": "unstaged\n", "untracked": "keep\n", "ignored/cache": "source cache\n"} {
					got, err := os.ReadFile(filepath.Join(root, name)) //nolint:gosec // fixture-owned source file.
					if err != nil || string(got) != want {
						t.Fatalf("%s: %q, %v", name, got, err)
					}
				}
				assertPushIdle(t, w, root)
			})
			t.Run("ConcurrentAndActiveRetention", func(t *testing.T) {
				t.Parallel()
				root := initTestRepo(t, "main")
				w := newInitializedTestCheckout(t, root)
				markers := t.TempDir()
				// Rename publishes each marker with its content; readers never see it empty.
				staging := t.TempDir()
				hook := "#!/bin/sh\nset -eu\nname=$(basename \"$(dirname \"$PWD\")\")\npwd > '" + staging + "/'$name\nmv '" + staging + "/'$name '" + markers + "/'$name\nwhile [ ! -f '" + markers + "/release' ]; do sleep 0.01; done\n"
				writePushFile(t, root, ".git/hooks/pre-push", hook, 0o700)
				errs := make(chan error, 2)
				var wg sync.WaitGroup
				for _, branch := range []string{"one", "two"} {
					wg.Go(func() {
						errs <- w.pushWorktree(t.Context(), logtest.Logger(t), root, "HEAD", PushDestination{Remote: "origin", Branch: branch}, false)
					})
				}
				deadline := time.Now().Add(10 * time.Second)
				for {
					entries, err := os.ReadDir(markers)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) == 2 {
						break
					}
					if time.Now().After(deadline) {
						writePushFile(t, markers, "release", "", 0o600)
						wg.Wait()
						close(errs)
						t.Fatal("concurrent hooks did not start", <-errs, <-errs)
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err := w.reportRetainedPushWorktrees(t.Context(), logtest.Logger(t), root); err != nil {
					t.Fatal(err)
				}
				entries, err := os.ReadDir(markers)
				if err != nil {
					t.Fatal(err)
				}
				paths := make(map[string]struct{})
				for _, e := range entries {
					data, err := os.ReadFile(filepath.Join(markers, e.Name())) //nolint:gosec // fixture-owned hook marker.
					if err != nil {
						t.Fatal(err)
					}
					path := strings.TrimSpace(string(data))
					if _, err := os.Stat(filepath.Join(path, ".git")); err != nil { //nolint:gosec // path reported by fixture-owned hook.
						t.Fatal("active checkout removed", err)
					}
					paths[path] = struct{}{}
				}
				if len(paths) != 2 {
					t.Fatal("concurrent pushes shared a checkout")
				}
				writePushFile(t, markers, "release", "", 0o600)
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatal(err)
					}
				}
				assertPushIdle(t, w, root)
			})
			t.Run("RetentionProtectsAbandonedUserAndForeignWorktrees", func(t *testing.T) {
				t.Parallel()
				root := initTestRepo(t, "main")
				w := newInitializedTestCheckout(t, root)
				common, err := pushCommonDir(t.Context(), &git.Checkout{Root: root, Logger: logtest.Logger(t)})
				if err != nil {
					t.Fatal(err)
				}
				abandoned := filepath.Join(w.PushDir, "operation-abandoned")
				active := filepath.Join(w.PushDir, "operation-active")
				user := filepath.Join(w.PushDir, "operation-user")
				foreign := filepath.Join(w.PushDir, "operation-foreign")
				for _, dir := range []string{abandoned, active, user, foreign} {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					runGit(t, root, "worktree", "add", "--detach", filepath.Join(dir, "checkout"), "HEAD")
				}
				for _, dir := range []string{abandoned, active} {
					writePushFile(t, dir, "owner-v1", common, 0o600)
				}
				writePushFile(t, foreign, "owner-v1", "other repository", 0o600)
				if err := w.reportRetainedPushWorktrees(t.Context(), logtest.Logger(t), root); err != nil {
					t.Fatal(err)
				}
				for _, dir := range []string{abandoned, active, user, foreign} {
					if _, err := os.Stat(filepath.Join(dir, "checkout", ".git")); err != nil {
						t.Fatal("retained worktree removed", err)
					}
				}
			})
			for _, tc := range []struct {
				name, commit, hook string
				// hang runs a hook that blocks until the push is canceled.
				hang bool
			}{
				{name: "SetupFailure", commit: "nonexistent"},
				{name: "SetupHookFailure", commit: "HEAD", hook: "#!/bin/sh\nexit 1\n"},
				{name: "HookRejection", commit: "HEAD", hook: "#!/bin/sh\nexit 1\n"},
				{name: "HookCancellation", commit: "HEAD", hang: true},
				{name: "CleanupTimeout", commit: "HEAD"},
				{name: "SetupHookCancellation", commit: "HEAD", hang: true},
				{name: "CleanupFailure", commit: "HEAD", hook: "#!/bin/sh\ngit worktree lock \"$PWD\"\nexit 1\n"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					root := initTestRepo(t, "main")
					w := newInitializedTestCheckout(t, root)
					if tc.name == "CleanupTimeout" {
						w.GitTimeout = time.Nanosecond
					}
					hook := tc.hook
					hookStarted := filepath.Join(t.TempDir(), "started")
					if tc.hang {
						hook = "#!/bin/sh\n: > '" + hookStarted + "'\nsleep 2\nexit 1\n"
					}
					if hook != "" {
						hookName := "pre-push"
						if strings.HasPrefix(tc.name, "SetupHook") {
							hookName = "post-checkout"
						}
						writePushFile(t, root, ".git/hooks/"+hookName, hook, 0o700)
					}
					ctx, cancel := context.WithCancel(t.Context())
					t.Cleanup(cancel)
					// Cancel only once the hook runs; a fixed timer can expire before
					// the operation directory exists on a loaded machine.
					canceled := make(chan time.Time, 1)
					if tc.hang {
						go func() {
							for ctx.Err() == nil {
								if _, err := os.Stat(hookStarted); err == nil {
									canceled <- time.Now()
									cancel()
									return
								}
								time.Sleep(10 * time.Millisecond)
							}
						}()
					}
					err := w.pushWorktree(ctx, logtest.Logger(t), root, tc.commit, PushDestination{Remote: "origin", Branch: "task"}, false)
					if err == nil {
						t.Fatal("push unexpectedly succeeded")
					}
					switch {
					case tc.name == "CleanupFailure":
						if !strings.Contains(err.Error(), "locked push worktree") {
							t.Fatal("cleanup error lost", err)
						}
					case tc.hang || tc.name == "CleanupTimeout":
						if tc.hang {
							select {
							case at := <-canceled:
								if time.Since(at) > time.Second {
									t.Fatal("cancellation waited for the hook", err)
								}
							default:
								t.Fatal("hook did not start", err)
							}
						}
						if !strings.Contains(err.Error(), "retained") {
							t.Fatal("retention guidance lost", err)
						}
						if err := w.reportRetainedPushWorktrees(t.Context(), logtest.Logger(t), root); err != nil {
							t.Fatal(err)
						}
						if len(pushOperationDirs(t, w)) != 1 {
							t.Fatal("uncertain operation removed")
						}
					default:
						if tc.name == "HookRejection" {
							assertPushIdle(t, w, root)
						} else {
							assertPushCleaned(t, w, root)
						}
					}
				})
			}
		})
		t.Run("Crash", func(t *testing.T) {
			t.Parallel()
			root := initTestRepo(t, "main")
			w := newInitializedTestCheckout(t, root)
			markers := t.TempDir()
			// Rename publishes the marker with its content; readers never see it empty.
			staging := t.TempDir()
			writePushFile(t, root, ".git/hooks/pre-push", "#!/bin/sh\nset -eu\npwd > '"+staging+"/started'\nmv '"+staging+"/started' '"+markers+"/started'\nwhile [ ! -f '"+markers+"/release' ]; do sleep 0.01; done\nexit 1\n", 0o700)
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCheckout$/^pushWorktree$/^CrashHelper$") //nolint:gosec // fixture-owned test binary simulates a restarted server.
			cmd.Env = append(os.Environ(), "CAIC_PUSH_CRASH_ROOT="+root, "CAIC_PUSH_CRASH_CACHE="+w.PushDir)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var checkout string
			deadline := time.Now().Add(10 * time.Second)
			for {
				data, err := os.ReadFile(filepath.Join(markers, "started")) //nolint:gosec // fixture-owned hook marker.
				if err == nil {
					checkout = strings.TrimSpace(string(data))
					break
				}
				if !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					if err := cmd.Process.Kill(); err != nil {
						t.Error(err)
					}
					if err := cmd.Wait(); err == nil {
						t.Error("helper unexpectedly succeeded")
					}
					t.Fatal("hook did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("helper survived kill")
			}
			t.Cleanup(func() { writePushFile(t, markers, "release", "", 0o600) })
			marker := filepath.Join(filepath.Dir(checkout), "owner-v1")
			owner, err := os.ReadFile(marker) //nolint:gosec // marker reported by fixture-owned hook.
			if err != nil || string(owner) != canonicalPath(t, filepath.Join(root, ".git")) {
				t.Fatalf("initial ownership marker was replaced during setup: %q, %v", owner, err)
			}
			if err := w.reportRetainedPushWorktrees(t.Context(), logtest.Logger(t), root); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(checkout, ".git")); err != nil { //nolint:gosec // path reported by fixture-owned hook.
				t.Fatal("orphan checkout removed", err)
			}
			// Restart while the old hook is still alive. The interrupted checkout must
			// remain quarantined even though the original server process is dead.
			if err := os.Remove(filepath.Join(root, ".git", "hooks", "pre-push")); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewCheckout(t.Context(), logtest.Logger(t), root, filepath.Dir(w.PushDir), "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.pushWorktree(t.Context(), logtest.Logger(t), root, "HEAD", PushDestination{Remote: "origin", Branch: "restart"}, false); err != nil {
				t.Fatal(err)
			}
			if len(pushOperationDirs(t, restarted)) != 2 {
				t.Fatal("restart reused the interrupted checkout")
			}
			writePushFile(t, markers, "release", "", 0o600)
			// Even after its hook exits, a restarted host never infers that an abandoned
			// operation is safe to reclaim. Only explicit manual cleanup may remove it.
			if err := w.reportRetainedPushWorktrees(t.Context(), logtest.Logger(t), root); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(checkout, ".git")); err != nil { //nolint:gosec // path reported by fixture-owned hook.
				t.Fatal("abandoned checkout removed", err)
			}
		})
		t.Run("HookPolicies", func(t *testing.T) {
			t.Parallel()
			hook, err := os.ReadFile("../../../scripts/hooks/pre-push")
			if err != nil {
				t.Fatal(err)
			}
			for _, policy := range []string{"WIP", "MultipleMainCommits", "NonHead", "VerificationChanges", "DeletionOnly"} {
				t.Run(policy, func(t *testing.T) {
					t.Parallel()
					root := initTestRepo(t, "main")
					writePushFile(t, root, "scripts/hooks/pre-push", string(hook), 0o700)
					recipe := "verify:\n\t@true\n"
					if policy == "VerificationChanges" {
						recipe = "verify:\n\t@printf altered > README.md\n"
					}
					writePushFile(t, root, "Makefile", recipe, 0o600)
					runGit(t, root, "add", ".")
					runGit(t, root, "commit", "-m", "Install hook")
					runGit(t, root, "checkout", "-b", "task")
					writePushFile(t, root, "change", "task", 0o600)
					runGit(t, root, "add", "change")
					message := "Task change"
					if policy == "WIP" {
						message = "WIP task"
					}
					runGit(t, root, "commit", "-m", message)
					if policy == "DeletionOnly" {
						runGit(t, root, "push", "origin", "task")
						writePushFile(t, root, "README.md", "dirty", 0o600)
					}
					runGit(t, root, "config", "core.hooksPath", "scripts/hooks")
					args := []string{"push", "origin", "task"}
					switch policy {
					case "MultipleMainCommits":
						args = []string{"push", "origin", "HEAD:main"}
					case "NonHead":
						args = []string{"push", "origin", "main"}
					case "DeletionOnly":
						args = []string{"push", "origin", ":task"}
					}
					_, err := (&git.Checkout{Root: root, Logger: logtest.Logger(t)}).RunGit(t.Context(), args...)
					if policy == "DeletionOnly" {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil {
						t.Fatal("hook policy not enforced")
					}
				})
			}
		})
		t.Run("Restart", func(t *testing.T) {
			t.Parallel()
			root := initTestRepo(t, "main")
			w := newInitializedTestCheckout(t, root)
			writePushFile(t, root, ".gitignore", "ignored/\n", 0o600)
			runGit(t, root, "add", ".gitignore")
			runGit(t, root, "commit", "-m", "Ignore verification caches")
			if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, "HEAD", PushDestination{Remote: "origin", Branch: "first"}, false); err != nil {
				t.Fatal(err)
			}
			dir := assertPushIdle(t, w, root)
			checkout := filepath.Join(dir, "checkout")
			writePushFile(t, checkout, "ignored/cache", "warm cache", 0o600)
			writePushFile(t, checkout, "leftover", "remove me", 0o600)
			writePushFile(t, checkout, "README.md", "discard verification edits", 0o600)
			writePushFile(t, root, "README.md", "next task", 0o600)
			runGit(t, root, "commit", "-am", "Next task")
			tip := runPushGit(t, root, "rev-parse", "HEAD")
			writePushFile(t, root, "README.md", "host dirty", 0o600)
			restarted, err := NewCheckout(t.Context(), logtest.Logger(t), root, filepath.Dir(w.PushDir), "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCheckout$/^pushWorktree$/^CrashHelper$") //nolint:gosec // fixture-owned test binary simulates a restarted server.
			cmd.Env = append(os.Environ(), "CAIC_PUSH_CRASH_ROOT="+root, "CAIC_PUSH_CRASH_CACHE="+w.PushDir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("restarted process: %v: %s", err, out)
			}
			if got := assertPushIdle(t, restarted, root); got != dir {
				t.Fatalf("restart did not reuse checkout: %s, want %s", got, dir)
			}
			if got := runPushGit(t, checkout, "rev-parse", "HEAD"); got != tip {
				t.Fatalf("reused checkout pushed wrong commit: %s, want %s", got, tip)
			}
			if got := runPushGit(t, root, "ls-remote", "origin", "refs/heads/crash"); !strings.HasPrefix(got, tip+"\t") {
				t.Fatal("remote did not receive pinned commit", got)
			}
			for path, want := range map[string]string{filepath.Join(checkout, "ignored", "cache"): "warm cache", filepath.Join(root, "README.md"): "host dirty"} {
				got, err := os.ReadFile(path) //nolint:gosec // fixture-owned files.
				if err != nil || string(got) != want {
					t.Fatalf("%s: %q, %v", path, got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(checkout, "leftover")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("untracked leftover survived reuse: %v", err)
			}
		})
		t.Run("LockAcrossProcesses", func(t *testing.T) {
			t.Parallel()
			root := initTestRepo(t, "main")
			w := newInitializedTestCheckout(t, root)
			common, err := pushCommonDir(t.Context(), &git.Checkout{Root: root, Logger: logtest.Logger(t)})
			if err != nil {
				t.Fatal(err)
			}
			markers := t.TempDir()
			childCtx, childCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
			t.Cleanup(childCancel)
			cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestCheckout$/^pushWorktree$/^LockHelper$") //nolint:gosec // fixture-owned binary and arguments.
			cmd.Env = append(os.Environ(), "CAIC_PUSH_POOL_LOCK_CACHE="+w.PushDir, "CAIC_PUSH_POOL_LOCK_COMMON="+common, "CAIC_PUSH_POOL_LOCK_MARKERS="+markers)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				writePushFile(t, markers, "release", "", 0o600)
				if err := cmd.Wait(); err != nil {
					t.Error("pool lock holder", err)
				}
			})
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(markers, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("pool lock holder did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			start := time.Now()
			if _, err := NewCheckout(t.Context(), logtest.Logger(t), root, filepath.Dir(w.PushDir), "main", nil); err != nil {
				t.Fatal("discovery failed while another server held the pool lock", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("discovery waited for cache maintenance")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			t.Cleanup(cancel)
			// Claim directly: a short deadline could otherwise expire in Git before the lock.
			if _, _, err := acquirePushWorktree(ctx, w.PushDir, common); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("other server bypassed pool lock: %v", err)
			}
			if len(pushOperationDirs(t, w)) != 0 {
				t.Fatal("contender created a checkout without the shared pool lock")
			}
		})
		t.Run("ProtectsUncertainState", func(t *testing.T) {
			t.Parallel()
			for _, state := range []string{"claimed", "legacy", "partial", "foreign", "unowned"} {
				t.Run(state, func(t *testing.T) {
					t.Parallel()
					root := initTestRepo(t, "main")
					w := newInitializedTestCheckout(t, root)
					if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, "HEAD", PushDestination{Remote: "origin", Branch: "first"}, false); err != nil {
						t.Fatal(err)
					}
					dir := assertPushIdle(t, w, root)
					expired := time.Now().Add(-48 * time.Hour)
					writePushFile(t, dir, "idle-v1", expired.Format(time.RFC3339Nano)+"\n", 0o600)
					switch state {
					case "claimed", "legacy":
						if err := os.Remove(filepath.Join(dir, "idle-v1")); err != nil {
							t.Fatal(err)
						}
						if state == "legacy" {
							writePushFile(t, dir, "owner-v1", root, 0o600)
						}
					case "partial":
						writePushFile(t, dir, "idle-v1", expired.Format(time.RFC3339Nano), 0o600)
					case "foreign":
						writePushFile(t, dir, "owner-v1", "another repository", 0o600)
					case "unowned":
						if err := os.Remove(filepath.Join(dir, "owner-v1")); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.Chtimes(dir, expired, expired); err != nil {
						t.Fatal(err)
					}
					restarted, err := NewCheckout(t.Context(), logtest.Logger(t), root, filepath.Dir(w.PushDir), "main", nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := SweepPushWorktrees(t.Context(), logtest.Logger(t), filepath.Dir(w.PushDir)); err != nil {
						t.Fatal(err)
					}
					if err := restarted.pushWorktree(t.Context(), logtest.Logger(t), root, "HEAD", PushDestination{Remote: "origin", Branch: "second"}, false); err != nil {
						t.Fatal(err)
					}
					if len(pushOperationDirs(t, restarted)) != 2 {
						t.Fatal("uncertain worktree was reused or expired")
					}
					if _, err := os.Stat(filepath.Join(dir, "checkout", ".git")); err != nil {
						t.Fatal("uncertain checkout was removed", err)
					}
				})
			}
		})
		t.Run("MissingGitPointer", func(t *testing.T) {
			t.Parallel()
			root := initTestRepo(t, "main")
			writePushFile(t, root, ".gitignore", "cache/\n", 0o600)
			runGit(t, root, "add", ".gitignore")
			runGit(t, root, "commit", "-m", "Ignore private cache")
			// Exercise an owned cache below the user's repository, where Git could
			// discover the parent repository if a cached checkout loses its pointer.
			w, err := NewCheckout(t.Context(), logtest.Logger(t), root, filepath.Join(root, "cache"), "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			commit := runPushGit(t, root, "rev-parse", "HEAD")
			if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, commit, PushDestination{Remote: "origin", Branch: "first"}, false); err != nil {
				t.Fatal(err)
			}
			dir := assertPushIdle(t, w, root)
			if err := os.Remove(filepath.Join(dir, "checkout", ".git")); err != nil {
				t.Fatal(err)
			}
			writePushFile(t, root, "untracked", "keep", 0o600)
			before := runPushGit(t, root, "status", "--porcelain")
			if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, commit, PushDestination{Remote: "origin", Branch: "second"}, false); err == nil {
				t.Fatal("reused checkout without its Git pointer")
			}
			if got := runPushGit(t, root, "status", "--porcelain"); got != before {
				t.Fatal("reused checkout cleaned the parent repository", got)
			}
			data, err := os.ReadFile(filepath.Join(root, "untracked")) //nolint:gosec // fixture-owned parent repository sentinel.
			if err != nil || string(data) != "keep" {
				t.Fatalf("parent untracked file was removed: %q, %v", data, err)
			}
		})
		t.Run("SharedRepository", func(t *testing.T) {
			t.Parallel()
			root := initTestRepo(t, "main")
			w := newInitializedTestCheckout(t, root)
			commit := runPushGit(t, root, "rev-parse", "HEAD")
			if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, commit, PushDestination{Remote: "origin", Branch: "first"}, false); err != nil {
				t.Fatal(err)
			}
			dir := assertPushIdle(t, w, root)
			source := filepath.Join(t.TempDir(), "linked-source")
			runGit(t, root, "worktree", "add", "--detach", source, commit)
			other, err := NewCheckout(t.Context(), logtest.Logger(t), source, filepath.Dir(w.PushDir), "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := other.pushWorktree(t.Context(), logtest.Logger(t), source, commit, PushDestination{Remote: "origin", Branch: "second"}, false); err != nil {
				t.Fatal(err)
			}
			if dirs := pushOperationDirs(t, other); len(dirs) != 1 || dirs[0] != dir {
				t.Fatal("source checkout aliases did not share the repository pool", dirs)
			}
			if got := runPushGit(t, source, "rev-parse", "HEAD"); got != commit {
				t.Fatal("source alias was modified", got)
			}
		})
		t.Run("Expiry", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name     string
				restart  bool
				relative bool
			}{{name: "Periodic"}, {name: "PeriodicRelative", relative: true}, {name: "Startup", restart: true}} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					root := initTestRepo(t, "main")
					w := newInitializedTestCheckout(t, root)
					if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, "HEAD", PushDestination{Remote: "origin", Branch: "first"}, false); err != nil {
						t.Fatal(err)
					}
					dir := assertPushIdle(t, w, root)
					// Only a valid completion timestamp makes a slot eligible for expiry.
					writePushFile(t, dir, "idle-v1", time.Now().Add(-25*time.Hour).Format(time.RFC3339Nano)+"\n", 0o600)
					if tc.restart {
						if _, err := NewCheckout(t.Context(), logtest.Logger(t), root, filepath.Dir(w.PushDir), "main", nil); err != nil {
							t.Fatal(err)
						}
						// The background startup sweep owns expiry; discovery does not
						// wait on potentially large cached-directory deletion.
						if err := SweepPushWorktrees(t.Context(), logtest.Logger(t), filepath.Dir(w.PushDir)); err != nil {
							t.Fatal(err)
						}
					} else {
						cache := filepath.Dir(w.PushDir)
						if tc.relative {
							cwd, err := os.Getwd()
							if err != nil {
								t.Fatal(err)
							}
							cache, err = filepath.Rel(cwd, cache)
							if err != nil {
								t.Fatal(err)
							}
						}
						if err := SweepPushWorktrees(t.Context(), logtest.Logger(t), cache); err != nil {
							t.Fatal(err)
						}
					}
					assertPushCleaned(t, w, root)
				})
			}
		})
		t.Run("ReplacedCheckout", func(t *testing.T) {
			if goruntime.GOOS == "windows" {
				t.Skip("symlink fixture requires Unix")
			}
			t.Parallel()
			root := initTestRepo(t, "main")
			w := newInitializedTestCheckout(t, root)
			commit := runPushGit(t, root, "rev-parse", "HEAD")
			if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, commit, PushDestination{Remote: "origin", Branch: "first"}, false); err != nil {
				t.Fatal(err)
			}
			dir := assertPushIdle(t, w, root)
			user := filepath.Join(t.TempDir(), "user-checkout")
			runGit(t, root, "worktree", "add", "--detach", user, commit)
			writePushFile(t, user, "untracked", "keep", 0o600)
			if err := os.Rename(filepath.Join(dir, "checkout"), filepath.Join(dir, "original")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(user, filepath.Join(dir, "checkout")); err != nil {
				t.Fatal(err)
			}
			writePushFile(t, dir, "idle-v1", time.Now().Add(-25*time.Hour).Format(time.RFC3339Nano)+"\n", 0o600)
			if err := SweepPushWorktrees(t.Context(), logtest.Logger(t), filepath.Dir(w.PushDir)); err == nil {
				t.Fatal("expiry accepted a replaced checkout")
			}
			data, err := os.ReadFile(filepath.Join(user, "untracked")) //nolint:gosec // fixture-owned user checkout sentinel.
			if err != nil || string(data) != "keep" {
				t.Fatalf("expiry removed user files: %q, %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "owner-v1")); err != nil {
				t.Fatal("unsafe cleanup lost ownership", err)
			}
		})
		t.Run("CleanupLateCompletion", func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				writePushFile(t, dir, "owner-v1", "repository", 0o600)
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				started := make(chan struct{})
				release := make(chan struct{})
				finished := make(chan struct{})
				done := make(chan error, 1)
				go func() {
					done <- finishPushCleanup(ctx, dir, func() error {
						close(started)
						<-release
						close(finished)
						return nil
					})
				}()
				<-started
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("cleanup did not report retained cancellation: %v", err)
				}
				close(release)
				<-finished
				synctest.Wait()
				// The Git cleanup worker has now completed successfully after the caller
				// returned cancellation. It must never erase retained ownership afterwards.
				owner, err := os.ReadFile(filepath.Join(dir, "owner-v1")) //nolint:gosec // fixture-owned retained marker.
				if err != nil || string(owner) != "repository" {
					t.Fatalf("late cleanup erased retained ownership: %q, %v", owner, err)
				}
			})
		})
		t.Run("CrashHelper", func(t *testing.T) {
			t.Parallel()
			root := os.Getenv("CAIC_PUSH_CRASH_ROOT")
			if root == "" {
				t.Skip("subprocess fixture")
			}
			w, err := NewCheckout(t.Context(), logtest.Logger(t), root, filepath.Dir(os.Getenv("CAIC_PUSH_CRASH_CACHE")), "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			commit := runPushGit(t, root, "rev-parse", "HEAD")
			if err := w.pushWorktree(t.Context(), logtest.Logger(t), root, commit, PushDestination{Remote: "origin", Branch: "crash"}, false); err != nil {
				t.Fatal(err)
			}
		})
		t.Run("LockHelper", func(t *testing.T) {
			t.Parallel()
			cache := os.Getenv("CAIC_PUSH_POOL_LOCK_CACHE")
			if cache == "" {
				t.Skip("subprocess fixture")
			}
			markers := os.Getenv("CAIC_PUSH_POOL_LOCK_MARKERS")
			if err := withPushPool(t.Context(), cache, os.Getenv("CAIC_PUSH_POOL_LOCK_COMMON"), func() error {
				writePushFile(t, markers, "started", "", 0o600)
				for {
					if _, err := os.Stat(filepath.Join(markers, "release")); err == nil { //nolint:gosec // fixture-owned subprocess marker.
						return nil
					}
					time.Sleep(10 * time.Millisecond)
				}
			}); err != nil {
				t.Fatal(err)
			}
		})
	})
	t.Run("Push", func(t *testing.T) {
		t.Parallel()
		for _, diverged := range []bool{false, true} {
			name := "PreservesCommits"
			if diverged {
				name = "RejectsNonFastForward"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				clone := initTestRepo(t, "main")
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
				if diverged {
					if err := os.WriteFile(filepath.Join(clone, "other.txt"), []byte("remote change\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, clone, "add", "other.txt")
					runGit(t, clone, "commit", "-m", "Remote advanced")
					runGit(t, clone, "push", "origin", "main")
				}
				before, err := g.RevParse(t.Context(), "main")
				if err != nil {
					t.Fatal(err)
				}
				w := newInitializedTestCheckout(t, clone)
				tv := &fakeTaskView{instanceID: id, repo: []runtime.Repo{{GitRoot: clone, Branch: "caic-1", ContainerPath: "/repo"}}}
				sc := newRecordingContainer()
				sc.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: tip}}
				_, issues, err := w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), tv.GitTarget(), []PushDestination{{Remote: "origin", Branch: "main", BaseRef: "refs/remotes/origin/main"}}, PushOptions{})
				if fetches := sc.Fetches(); len(fetches) != 1 || fetches[0].Commit {
					t.Fatalf("fetch must leave task history and pending edits untouched: %+v", fetches)
				}
				if len(issues) != 0 {
					t.Fatalf("unexpected safety issues: %+v", issues)
				}
				want := tip
				if diverged {
					if err == nil {
						t.Fatal("diverged main push succeeded")
					}
					want = before
				} else if err != nil {
					t.Fatal(err)
				}
				if err := g.Fetch(t.Context()); err != nil {
					t.Fatal(err)
				}
				got, err := g.RevParse(t.Context(), "origin/main")
				if err != nil || got != want {
					t.Fatalf("origin/main = %q, %v; want %q", got, err, want)
				}
				got, err = g.RevParse(t.Context(), "HEAD")
				if err != nil || got != before {
					t.Fatalf("host HEAD = %q, %v; want %q", got, err, before)
				}
			})
		}
	})
	t.Run("PushDestinations", func(t *testing.T) {
		t.Parallel()
		id := runtime.NewID("test-runtime", "ctr-1")
		roots := []string{initTestRepo(t, "main"), initTestRepo(t, "main")}
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
		ds, issues, err := w.Push(ctx, logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
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
		_, issues, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{Force: true})
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
		_, _, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
		if err == nil || !strings.Contains(err.Error(), "fetch did not return commit") {
			t.Fatalf("incomplete fetch: %v", err)
		}
		sc.FetchErr = errors.New("fetch unavailable")
		_, _, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
		if !errors.Is(err, sc.FetchErr) {
			t.Fatalf("failed fetch: %v", err)
		}
	})
	t.Run("PushTaskDiffReport", func(t *testing.T) {
		t.Parallel()
		for _, remoteBase := range []bool{false, true} {
			t.Run(fmt.Sprintf("RemoteBase=%v", remoteBase), func(t *testing.T) {
				t.Parallel()
				root := initTestRepo(t, "main")
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
				sc.DiffOutput = ""
				sc.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: tip}}
				target, destinations, err := w.ResolvePushDestinations(t.Context(), logtest.Logger(t), target, false)
				if err != nil {
					t.Fatal(err)
				}
				ds, issues, err := w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
				if err != nil || len(ds) != 0 || len(issues) != 0 {
					t.Fatalf("unchanged task report: stats=%+v issues=%+v err=%v", ds, issues, err)
				}
				// Pending runtime edits belong in the public default-sync report
				// while the exact existing task commit is pushed without them.
				if err := os.WriteFile(filepath.Join(root, "pending.txt"), []byte("sk-"+strings.Repeat("a", 24)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				sc.DiffOutput = "1\t0\tpending.txt\n"
				target, destinations, err = w.ResolvePushDestinations(t.Context(), logtest.Logger(t), target, true)
				if err != nil {
					t.Fatal(err)
				}
				ds, issues, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
				if err != nil || len(ds) != 1 || ds[0].Path != "pending.txt" || len(issues) != 0 || destinations[0].Branch != "main" {
					t.Fatalf("pending report is separate from committed safety: stats=%+v issues=%+v destination=%+v err=%v", ds, issues, destinations[0], err)
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
				_, _, err = w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, destinations, PushOptions{})
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
		root := initTestRepo(t, "main")
		w := newInitializedTestCheckout(t, root)
		w.GitTimeout = 10 * time.Millisecond
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		target := GitTarget{InstanceID: runtime.NewID("test-runtime", "ctr-1"), Repos: []runtime.Repo{{GitRoot: root, ContainerPath: "/repo", Branch: "caic-1"}}}
		sc := &timeoutFetchBackend{FakeBackend: &runtimetest.FakeBackend{}}
		_, _, err := w.Push(ctx, logtest.Logger(t), newTestRuntime(t, sc), target, []PushDestination{{Remote: "origin", Branch: "caic-1", BaseRef: "refs/remotes/origin/main"}}, PushOptions{})
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
		for _, tc := range []struct {
			name     string
			opts     PushOptions
			secret   bool
			diverged bool
			blocked  bool
			wantErr  bool
		}{
			{name: "CommitPending", opts: PushOptions{CommitPending: true}},
			{name: "SafetyBlocksForce", opts: PushOptions{Force: true}, secret: true, blocked: true},
			{name: "BypassSafety", opts: PushOptions{BypassSafety: true}, secret: true},
			{name: "BypassDoesNotForce", opts: PushOptions{BypassSafety: true}, diverged: true, wantErr: true},
			{name: "Force", opts: PushOptions{Force: true}, diverged: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				root := initTestRepo(t, "main")
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
				_, issues, err := w.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), target, []PushDestination{{Remote: "origin", Branch: "caic-1", BaseRef: "refs/remotes/origin/main"}}, tc.opts)
				if (err != nil) != tc.wantErr || (len(issues) != 0) != tc.secret {
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
				if !tc.blocked && !tc.wantErr && !strings.HasPrefix(out, tip+"\t") {
					t.Fatalf("push did not preserve task commit: %q", out)
				}
			})
		}
	})
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
			if _, err := NewCheckout(t.Context(), logtest.Logger(t), "", t.TempDir(), "main", nil); err == nil {
				t.Fatal("want directory error")
			}
			if _, err := NewCheckout(t.Context(), nil, t.TempDir(), t.TempDir(), "main", nil); err == nil {
				t.Fatal("want logger error")
			}
		})
	})

	t.Run("BranchDiffStat", func(t *testing.T) {
		t.Parallel()
		sc := newRecordingContainer()
		r := newTestCheckout("/repo")
		tv := &fakeTaskView{instanceID: runtime.NewID("test-runtime", "ctr-1"), repo: []runtime.Repo{{GitRoot: "/repo", Branch: "feature"}}}
		snapshot, err := r.DiffStat(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), tv.GitTarget())
		if err != nil {
			t.Fatal(err)
		}
		ds := snapshot.DiffStat
		if len(sc.fetchIDs) != 0 {
			t.Errorf("BranchDiffStat called Fetch %d times, want 0", len(sc.fetchIDs))
		}
		if len(ds) != 1 || ds[0].Path != "main.go" || ds[0].LinesAdded != 5 || ds[0].LinesDeleted != 1 {
			t.Errorf("BranchDiffStat = %+v, want [{main.go +5 -1}]", ds)
		}
	})
	t.Run("AdoptLocalBranch", func(t *testing.T) {
		t.Parallel()
		clone := initTestRepo(t, "main")
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
	t.Run("BranchDiffStatMultiRepoUsesInstanceID", func(t *testing.T) {
		t.Parallel()
		sc := newRecordingContainer()
		r := newTestCheckout("/home/user/src/caic")
		tv := &fakeTaskView{instanceID: runtime.NewID("test-runtime", "ctr-2"), repo: []runtime.Repo{
			{GitRoot: "/home/user/src/caic", Branch: "caic-7", ContainerPath: "/home/user/src/caic"},
			{GitRoot: "/home/user/src/genai", Branch: "caic-0", ContainerPath: "/home/user/src/genai"},
		}}

		snapshot, err := r.DiffStat(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), tv.GitTarget())
		if err != nil {
			t.Fatal(err)
		}
		ds := snapshot.DiffStat

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
		if _, err := r.DiffStat(t.Context(), logtest.Logger(t), nil, GitTarget{}); err == nil {
			t.Error("BranchDiffStat with no instance succeeded")
		}
	})
}

// TestDiffStatAndRepoStates covers the compact card probe: the combined diff
// stat, the per-repo state summary, and how partial probe failures degrade so
// one failing repository cannot blank the whole task card.
func TestDiffStatAndRepoStates(t *testing.T) {
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

		snapshot, err := r.DiffStatAndRepoStates(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), tv.GitTarget())
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

		snapshot, err := r.DiffStatAndRepoStates(t.Context(), logtest.Logger(t), newTestRuntime(t, sc), GitTarget{InstanceID: "test-runtime:ctr-2", Repos: repos})
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

		snapshot, err := r.DiffStatAndRepoStates(t.Context(), logtest.Logger(t), runtimes, GitTarget{InstanceID: "test-runtime:ctr-2", Repos: repos})
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

		snapshot, err := r.DiffStatAndRepoStates(t.Context(), logtest.Logger(t), runtimes, GitTarget{InstanceID: "test-runtime:ctr-1", Repos: repos})
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
}

func TestQueryRuntime(t *testing.T) {
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

	fetchIDs   []runtime.ID
	refreshIDs []runtime.ID
	refreshErr error
	diffIDs    []runtime.ID
	diffIdxs   []int
}

// newRecordingContainer builds a recordingContainer with the fixed diff output.
func newRecordingContainer() *recordingContainer {
	return &recordingContainer{FakeBackend: &runtimetest.FakeBackend{DiffOutput: "5\t1\tmain.go\n"}}
}

func (c *recordingContainer) RefreshRefs(_ context.Context, id runtime.ID) error {
	c.refreshIDs = append(c.refreshIDs, id)
	return c.refreshErr
}

func (c *recordingContainer) Fetch(ctx context.Context, id runtime.ID, opts runtime.FetchOpts) ([]runtime.FetchedBranch, error) {
	c.fetchIDs = append(c.fetchIDs, id)
	return c.FakeBackend.Fetch(ctx, id, opts)
}

func (c *recordingContainer) Diff(ctx context.Context, id runtime.ID, repoIdx int, args ...string) (string, error) {
	c.diffIDs = append(c.diffIDs, id)
	c.diffIdxs = append(c.diffIdxs, repoIdx)
	return c.FakeBackend.Diff(ctx, id, repoIdx, args...)
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
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprintf("probe failure %v", fails), func(t *testing.T) {
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
			if fails {
				backend.err = errors.New("read failed")
			}
			snapshot, err := checkout.RepositoryStatuses(t.Context(), newTestRuntime(t, backend), tv.GitTarget())
			if fails {
				if err == nil || snapshot.Read.NewerThan(GitRead{}) || len(snapshot.Statuses) != 0 {
					t.Fatalf("failed read returned publishable snapshot: %+v, %v", snapshot, err)
				}
				return
			}
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
	}
}

func TestGitProbeCompletionOrder(t *testing.T) {
	t.Parallel()
	checkout := newTestCheckout(t.TempDir())
	backend := &runtimetest.FakeBackend{DiffOutput: "5\t1\tmain.go\n", RepositoryStatusValue: runtime.RepositoryStatus{Branch: "feature"}}
	runtimes := newTestRuntime(t, backend)
	tv := &fakeTaskView{instanceID: "test-runtime:ctr", repo: []runtime.Repo{{Branch: "feature"}}}
	full, err := checkout.RepositoryStatuses(t.Context(), runtimes, tv.GitTarget())
	if err != nil {
		t.Fatal(err)
	}
	compact, err := checkout.DiffStatAndRepoStates(t.Context(), logtest.Logger(t), runtimes, tv.GitTarget())
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
	if err := os.WriteFile(path, []byte(content), mode); err != nil { //nolint:gosec // fixture-owned path and contents.
		t.Fatal(err)
	}
}

// canonicalPath resolves temporary directory aliases such as macOS /var.
func canonicalPath(t *testing.T, path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
func runPushGit(t *testing.T, root string, args ...string) string {
	out, err := (&git.Checkout{Root: root, Logger: logtest.Logger(t)}).RunGit(t.Context(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func assertPushCleaned(t *testing.T, w *Checkout, root string) {
	if len(pushOperationDirs(t, w)) != 0 {
		t.Fatal("push operation retained")
	}
	if out := runPushGit(t, root, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Fatal("worktree registration retained", out)
	}
}

func pushOperationDirs(t *testing.T, w *Checkout) []string {
	entries, err := os.ReadDir(w.PushDir)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "operation-") {
			dirs = append(dirs, filepath.Join(w.PushDir, e.Name()))
		}
	}
	return dirs
}

func assertPushIdle(t *testing.T, w *Checkout, root string) string {
	dirs := pushOperationDirs(t, w)
	if len(dirs) != 1 {
		t.Fatalf("expected one pooled worktree, got %v", dirs)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "idle-v1")); err != nil {
		t.Fatal("completed push not reusable", err)
	}
	if out := runPushGit(t, root, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 2 {
		t.Fatal("idle worktree registration missing", out)
	}
	return dirs[0]
}
