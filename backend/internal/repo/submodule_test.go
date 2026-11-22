// Tests pinned dependency publication independently of mutable checkout topology.

package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
)

func TestSubmodulePushTargets(t *testing.T) {
	t.Parallel()
	t.Run("conflicting pins for the same remote branch", func(t *testing.T) {
		t.Parallel()
		root, child := initTestRepo(t), initTestRepo(t)
		first := runPushGit(t, child, "rev-parse", "HEAD")
		runGit(t, child, "commit", "--allow-empty", "-m", "Second pin")
		second := runPushGit(t, child, "rev-parse", "HEAD")
		runGit(t, root, "checkout", "-b", "caic-1")
		runGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+first+",first")
		runGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+second+",second")
		runGit(t, root, "commit", "-m", "Record both pins")
		backend := newRecordingContainer()
		store := filepath.Join(child, ".git")
		backend.FetchedBranches = []runtime.FetchedBranch{
			{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: runPushGit(t, root, "rev-parse", "HEAD")},
			{RepositoryPath: "/repo/first", Submodule: "first", GitDir: store, CommitHash: first},
			{RepositoryPath: "/repo/second", Submodule: "second", GitDir: store, CommitHash: second},
		}
		target := GitTarget{InstanceID: "test-runtime:ctr", Repos: []runtime.Repo{{GitRoot: root, ContainerPath: "/repo", Branch: "caic-1"}}}
		log := logtest.Logger(t)
		checkout := newInitializedTestCheckout(t, root)
		target, destinations, err := checkout.ResolvePushDestinations(t.Context(), log, target, false)
		if err != nil {
			t.Fatal(err)
		}
		_, err = checkout.Push(t.Context(), log, newTestRuntime(t, backend), target, destinations, PushOptions{Force: true})
		if err == nil || !strings.Contains(err.Error(), "pin different commits for the same remote branch") {
			t.Fatalf("conflicting pins: %v", err)
		}
		if len(backend.prePushes) != 0 {
			t.Fatalf("preflight ran hooks: %+v", backend.prePushes)
		}
		for _, dir := range []string{root, child} {
			if got := runPushGit(t, dir, "ls-remote", "origin", "refs/heads/caic-1"); got != "" {
				t.Fatalf("preflight published %s: %s", dir, got)
			}
		}
	})

	for _, branch := range []string{"", "main"} {
		t.Run(fmt.Sprintf("child branch %q publishes pinned Gitlink on task branch", branch), func(t *testing.T) {
			t.Parallel()
			parent, child, nested := initTestRepo(t), initTestRepo(t), initTestRepo(t)
			childBase := runPushGit(t, child, "rev-parse", "HEAD")
			// The parent records an immutable child tip. That child then advances again;
			// publishing must use the recorded tip, never the mutable fetched HEAD.
			writePushFile(t, nested, "nested.txt", "nested change\n", 0o600)
			runGit(t, nested, "add", ".")
			runGit(t, nested, "commit", "-m", "Nested change")
			nestedTip := runPushGit(t, nested, "rev-parse", "HEAD")
			runGit(t, child, "update-index", "--add", "--cacheinfo", "160000,"+nestedTip+",nested")
			runGit(t, child, "commit", "-m", "Record nested child")
			childTip := runPushGit(t, child, "rev-parse", "HEAD")
			runGit(t, parent, "checkout", "-b", "caic-1")
			runGit(t, parent, "update-index", "--add", "--cacheinfo", "160000,"+childTip+",lib")
			runGit(t, parent, "commit", "-m", "Record child")
			parentTip := runPushGit(t, parent, "rev-parse", "HEAD")
			runGit(t, parent, "checkout", "main")
			writePushFile(t, child, "unpublished.txt", "later child change\n", 0o600)
			runGit(t, child, "add", ".")
			runGit(t, child, "commit", "-m", "Later child change")
			childHead := runPushGit(t, child, "rev-parse", "HEAD")
			childStore, nestedStore := filepath.Join(child, ".git"), filepath.Join(nested, ".git")
			if branch == "" {
				// Match modules created only inside a task: the host receives a
				// bare object store and cached upstream refs, without a checkout.
				makeStore := func(source string) string {
					store := filepath.Join(t.TempDir(), "module.git")
					runGit(t, "", "init", "--bare", store)
					remote := runPushGit(t, source, "remote", "get-url", "origin")
					runGit(t, store, "remote", "add", "origin", remote)
					runGit(t, store, "fetch", source, "+HEAD:refs/remotes/test/head", "+refs/remotes/origin/*:refs/remotes/origin/*")
					return store
				}
				childStore, nestedStore = makeStore(child), makeStore(nested)
			}
			backend := newRecordingContainer()
			backend.FetchedBranches = []runtime.FetchedBranch{
				{BranchName: branch, RepositoryPath: "/repo/lib", Submodule: "lib", GitDir: childStore, CommitHash: childHead},
				{BranchName: branch, RepositoryPath: "/repo/lib/nested", Submodule: "lib/nested", GitDir: nestedStore, CommitHash: nestedTip},
				{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: parentTip},
			}
			checkout := newInitializedTestCheckout(t, parent)
			target := GitTarget{InstanceID: "test-runtime:ctr", Repos: []runtime.Repo{{GitRoot: parent, ContainerPath: "/repo", Branch: "caic-1"}}}
			resolved, destinations, err := checkout.ResolvePushDestinations(t.Context(), logtest.Logger(t), target, branch == "")
			if err != nil {
				t.Fatal(err)
			}
			res, err := checkout.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, backend), resolved, destinations, PushOptions{Force: branch != ""})
			issues := res.SafetyIssues
			if err != nil || len(issues) != 0 {
				t.Fatalf("push: %v, issues %+v", err, issues)
			}
			parentBranch := "caic-1"
			if branch == "" {
				parentBranch = "main"
			}
			wantHooks := []prePushCall{
				{nestedTip, runtime.PushRefs{Submodule: &runtime.PushSubmodule{Path: "lib/nested"}, Remote: "origin", Branch: "caic-1", RemoteCommit: strings.Repeat("0", len(nestedTip))}},
				{childTip, runtime.PushRefs{Submodule: &runtime.PushSubmodule{Path: "lib"}, Remote: "origin", Branch: "caic-1", RemoteCommit: strings.Repeat("0", len(childTip))}},
				{parentTip, runtime.PushRefs{Remote: "origin", Branch: parentBranch, RemoteCommit: strings.Repeat("0", len(parentTip))}},
			}
			if parentBranch == "main" {
				wantHooks[2].dest.RemoteCommit = destinations[0].BaseRef
			}
			if !reflect.DeepEqual(backend.prePushes, wantHooks) {
				t.Fatalf("hook calls = %+v, want %+v", backend.prePushes, wantHooks)
			}
			for _, test := range []struct{ root, branch, want string }{{parent, parentBranch, parentTip}, {child, "caic-1", childTip}, {nested, "caic-1", nestedTip}} {
				remote := runPushGit(t, test.root, "remote", "get-url", "origin")
				if got := runPushGit(t, remote, "rev-parse", "refs/heads/"+test.branch); got != test.want {
					t.Fatalf("remote %s = %s, want %s", test.root, got, test.want)
				}
			}
			childRemote := runPushGit(t, child, "remote", "get-url", "origin")
			if got := runPushGit(t, childRemote, "rev-parse", "refs/heads/main"); got != childBase {
				t.Fatalf("child main moved: %s", got)
			}
			if got := runPushGit(t, parent, "symbolic-ref", "--short", "HEAD"); got != "main" {
				t.Fatalf("host parent moved: %s", got)
			}
			if got := runPushGit(t, child, "rev-parse", "HEAD"); got != childHead {
				t.Fatalf("host child moved: %s", got)
			}
			// A secret in a child blocks the whole push before even the root
			// remote can move, and identifies the affected module path.
			writePushFile(t, child, "secret.txt", "AK"+"IA1234567890ABCDEF\n", 0o600)
			runGit(t, child, "add", ".")
			runGit(t, child, "commit", "-m", "Unsafe child content")
			secretTip := runPushGit(t, child, "rev-parse", "HEAD")
			runGit(t, parent, "checkout", "caic-1")
			runGit(t, parent, "update-index", "--cacheinfo", "160000,"+secretTip+",lib")
			runGit(t, parent, "commit", "-m", "Record unsafe child")
			unsafeParent := runPushGit(t, parent, "rev-parse", "HEAD")
			runGit(t, parent, "checkout", "main")
			if branch == "" {
				runGit(t, childStore, "fetch", child, "+HEAD:refs/remotes/test/head")
			}
			backend.FetchedBranches[0].CommitHash = secretTip
			backend.FetchedBranches[2].CommitHash = unsafeParent
			res, err = checkout.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, backend), resolved, destinations, PushOptions{Force: true})
			issues = res.SafetyIssues
			if err != nil || len(issues) != 1 || issues[0].File != "lib/secret.txt" {
				t.Fatalf("unsafe child: %v, %+v", err, issues)
			}
			parentRemote := runPushGit(t, parent, "remote", "get-url", "origin")
			if got := runPushGit(t, parentRemote, "rev-parse", "refs/heads/"+parentBranch); got != parentTip {
				t.Fatalf("unsafe parent published: %s", got)
			}
		})
	}

	t.Run("pinned topology", func(t *testing.T) {
		t.Parallel()
		for _, fixture := range []struct {
			name            string
			reused, missing bool
		}{
			{name: "removed descendant"},
			{name: "reused path", reused: true},
			{name: "missing historic store", reused: true, missing: true},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				t.Parallel()
				root, child, leaf := initTestRepo(t), initTestRepo(t), initTestRepo(t)
				leafRemote := runPushGit(t, leaf, "remote", "get-url", "origin")
				runGit(t, child, "-c", "protocol.file.allow=always", "submodule", "add", "--name", "historic-name", leafRemote, "nested")
				nested := filepath.Join(child, "nested")
				runGit(t, nested, "config", "user.name", "Test")
				runGit(t, nested, "config", "user.email", "test@test.com")
				writePushFile(t, nested, "new.txt", "unpushed descendant\n", 0o600)
				runGit(t, nested, "add", ".")
				runGit(t, nested, "commit", "-m", "Descendant change")
				leafTip := runPushGit(t, nested, "rev-parse", "HEAD")
				runGit(t, child, "add", ".")
				runGit(t, child, "commit", "-m", "Pin descendant")
				childTip := runPushGit(t, child, "rev-parse", "HEAD")
				runGit(t, root, "checkout", "-b", "caic-1")
				runGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+childTip+",lib")
				runGit(t, root, "commit", "-m", "Pin child")
				rootTip := runPushGit(t, root, "rev-parse", "HEAD")
				runGit(t, root, "checkout", "main")
				runGit(t, child, "rm", "-f", "nested")
				runGit(t, child, "commit", "-am", "Remove descendant from current checkout")
				if fixture.reused {
					// Reuse the same path under another declaration identity. Its new
					// store has only the remote baseline, not the historic pinned tip.
					runGit(t, child, "-c", "protocol.file.allow=always", "submodule", "add", "--name", "replacement-name", leafRemote, "nested")
					runGit(t, child, "commit", "-am", "Reuse descendant path")
				}
				childHead := runPushGit(t, child, "rev-parse", "HEAD")
				if fixture.missing {
					if err := os.RemoveAll(filepath.Join(child, ".git", "modules", "historic-name")); err != nil {
						t.Fatal(err)
					}
				}
				backend := &runtimetest.FakeBackend{FetchedBranches: []runtime.FetchedBranch{
					{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: rootTip},
					{RepositoryPath: "/repo/lib", Submodule: "lib", GitDir: filepath.Join(child, ".git"), CommitHash: childHead},
				}}
				if fixture.reused {
					backend.FetchedBranches = append(backend.FetchedBranches, runtime.FetchedBranch{RepositoryPath: "/repo/lib/nested", Submodule: "lib/nested", GitDir: filepath.Join(child, ".git", "modules", "replacement-name"), CommitHash: runPushGit(t, nested, "rev-parse", "HEAD")})
				}
				checkout := newInitializedTestCheckout(t, root)
				target := GitTarget{InstanceID: "test-runtime:ctr", Repos: []runtime.Repo{{GitRoot: root, ContainerPath: "/repo", Branch: "caic-1"}}}
				resolved, destinations, err := checkout.ResolvePushDestinations(t.Context(), logtest.Logger(t), target, false)
				if err != nil {
					t.Fatal(err)
				}
				res, err := checkout.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, backend), resolved, destinations, PushOptions{})
				issues := res.SafetyIssues
				if fixture.missing {
					if err == nil || !strings.Contains(err.Error(), "/repo/lib/nested") {
						t.Fatalf("missing pinned descendant: %v", err)
					}
					for _, source := range []string{root, child, leaf} {
						remote := runPushGit(t, source, "remote", "get-url", "origin")
						if got := runPushGit(t, remote, "for-each-ref", "--format=%(refname)", "refs/heads/caic-1"); got != "" {
							t.Fatalf("preflight published %s: %s", source, got)
						}
					}
					return
				}
				if err != nil || len(issues) != 0 {
					t.Fatalf("push: %v, %+v", err, issues)
				}
				for _, test := range []struct{ source, want string }{{root, rootTip}, {child, childTip}, {leaf, leafTip}} {
					remote := runPushGit(t, test.source, "remote", "get-url", "origin")
					if got := runPushGit(t, remote, "rev-parse", "refs/heads/caic-1"); got != test.want {
						t.Fatalf("published %s = %s, want %s", test.source, got, test.want)
					}
				}
				if got := runPushGit(t, child, "rev-parse", "HEAD"); got != childHead {
					t.Fatal("host child checkout moved")
				}
			})
		}
	})

	t.Run("uninitialized published module", func(t *testing.T) {
		t.Parallel()
		for _, migration := range []bool{false, true} {
			t.Run(map[bool]string{false: "unchanged published pointer", true: "changed source"}[migration], func(t *testing.T) {
				t.Parallel()
				root, child := initTestRepo(t), initTestRepo(t)
				tip := runPushGit(t, child, "rev-parse", "HEAD")
				writePushFile(t, root, ".gitmodules", "[submodule \"lib\"]\npath = lib\nurl = https://example.invalid/old.git\n", 0o600)
				runGit(t, root, "add", ".gitmodules")
				runGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+tip+",lib")
				runGit(t, root, "commit", "-m", "Published dependency")
				runGit(t, root, "push", "origin", "main")
				runGit(t, root, "checkout", "-b", "caic-1")
				writePushFile(t, root, "ordinary.txt", "task content\n", 0o600)
				if migration {
					writePushFile(t, root, ".gitmodules", "[submodule \"lib\"]\npath = lib\nurl = https://example.invalid/new.git\n", 0o600)
				}
				runGit(t, root, "add", "ordinary.txt", ".gitmodules")
				runGit(t, root, "commit", "-m", "Task change")
				taskTip := runPushGit(t, root, "rev-parse", "HEAD")
				runGit(t, root, "checkout", "main")
				checkout := newInitializedTestCheckout(t, root)
				target := GitTarget{InstanceID: "test-runtime:ctr", Repos: []runtime.Repo{{GitRoot: root, ContainerPath: "/repo", Branch: "caic-1"}}}
				resolved, destinations, err := checkout.ResolvePushDestinations(t.Context(), logtest.Logger(t), target, false)
				if err != nil {
					t.Fatal(err)
				}
				backend := &runtimetest.FakeBackend{FetchedBranches: []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-1", CommitHash: taskTip}}}
				res, err := checkout.Push(t.Context(), logtest.Logger(t), newTestRuntime(t, backend), resolved, destinations, PushOptions{})
				issues := res.SafetyIssues
				if migration {
					if err == nil || !strings.Contains(err.Error(), "source URL") {
						t.Fatalf("source migration: %v", err)
					}
					return
				}
				if err != nil || len(issues) != 0 {
					t.Fatalf("unchanged uninitialized dependency: %v, %+v", err, issues)
				}
			})
		}
	})
}
