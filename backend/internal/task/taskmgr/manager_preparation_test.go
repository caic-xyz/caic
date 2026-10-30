// Tests task preparation isolation, owned-resource rollback, and atomic publication.

package taskmgr

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/maruel/ksid"
)

func preparationGit(t *testing.T, dir string, args ...string) string {
	c := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // Test helper receives controlled Git arguments.
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gatePreparationFetch blocks the repository's real upload-pack until release.
// FIFOs establish both ordering edges without polling Git process state.
func gatePreparationFetch(t *testing.T, dir string, fail bool) (entered <-chan struct{}, releaseFetch func()) {
	if runtime.GOOS == "windows" {
		t.Skip("fetch gate requires POSIX FIFOs")
	}
	d := t.TempDir()
	readyPath, releasePath := filepath.Join(d, "ready"), filepath.Join(d, "release")
	c := exec.CommandContext(t.Context(), "mkfifo", readyPath, releasePath) //nolint:gosec // FIFOs are inside this test's temporary directory.
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, out)
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0) //nolint:gosec // Test-owned readiness FIFO.
	if err != nil {
		t.Fatal(err)
	}
	release, err := os.OpenFile(releasePath, os.O_RDWR, 0) //nolint:gosec // Test-owned release FIFO.
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(d, "upload-pack")
	body := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf ready > '%s'\nread -r release < '%s'\nexec git-upload-pack \"$@\"\n", readyPath, releasePath)
	if fail {
		body = strings.Replace(body, `exec git-upload-pack "$@"`, "exit 1", 1)
	}
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // Git executes this test-owned upload-pack gate.
		t.Fatal(err)
	}
	preparationGit(t, dir, "config", "remote.origin.uploadpack", script)
	ch := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		b := make([]byte, 5)
		if _, err := io.ReadFull(ready, b); err == nil {
			close(ch)
		}
	})
	var once sync.Once
	unblock := func() {
		once.Do(func() {
			if _, err := release.WriteString("go\n"); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(func() {
		unblock()
		if err := ready.Close(); err != nil {
			t.Error(err)
		}
		wg.Wait()
		if err := release.Close(); err != nil {
			t.Error(err)
		}
	})
	return ch, unblock
}

func preparationManager(t *testing.T, names ...string) *Manager {
	m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: agent.Backends{"fake": &agenttest.FakeBackend{}}})
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, name := range names {
		dir := t.TempDir()
		initTaskRepo(t, dir)
		registerCheckout(t, m.Checkouts, name, &repo.Checkout{Dir: dir, BaseBranch: "main", GitTimeout: time.Minute})
	}
	return m
}

func preparationTask(t *testing.T, m *Manager, names ...string) *Entry {
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "preparation"}, "fake", "")
	for _, n := range names {
		c, _ := m.Checkouts.Checkout(n)
		tk.Repos = append(tk.Repos, taskslog.RepoMount{Name: n, GitRoot: c.Dir})
	}
	return m.NewEntry(tk, nil)
}

func runPreparationTests(t *testing.T) {
	t.Parallel()
	t.Run("disjoint_creation_during_fetch", func(t *testing.T) {
		m := preparationManager(t, "a", "b", "c")
		b, _ := m.Checkouts.Checkout("b")
		ready, release := gatePreparationFetch(t, b.Dir, false)
		first := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			_, err := m.Create(t.Context(), CreateParams{Prompt: agent.Prompt{Text: "first"}, Harness: "fake", Repos: []CreateRepo{{Name: "a"}, {Name: "b"}}})
			first <- err
		})
		t.Cleanup(func() { release(); wg.Wait() })
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("fetch did not reach gate")
		}
		second := make(chan error, 1)
		wg.Go(func() {
			_, err := m.Create(t.Context(), CreateParams{Prompt: agent.Prompt{Text: "second"}, Harness: "fake", Repos: []CreateRepo{{Name: "c"}}})
			second <- err
		})
		select {
		case err := <-second:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("unrelated creation blocked behind fetch")
		}
		release()
		if err := <-first; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("later_allocation_failure", func(t *testing.T) {
		m := preparationManager(t, "a", "b", "c")
		c, _ := m.Checkouts.Checkout("c")
		preparationGit(t, c.Dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))
		e := preparationTask(t, m, "a", "b", "c")
		if err := m.publishEntry(t.Context(), e, 1, false); err == nil {
			t.Fatal("preparation unexpectedly succeeded")
		}
		b, _ := m.Checkouts.Checkout("b")
		if got := preparationGit(t, b.Dir, "for-each-ref", "--format=%(refname)", "refs/heads/caic-*"); got != "" {
			t.Fatalf("owned branches survived: %s", got)
		}
		if m.Len() != 0 {
			t.Fatal("failed task published")
		}
	})
	t.Run("log_failure_after_branch", func(t *testing.T) {
		m := preparationManager(t, "a", "b")
		e := preparationTask(t, m, "a", "b")
		e.Task().StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := m.publishEntry(t.Context(), e, 1, false); err == nil {
			t.Fatal("invalid log metadata accepted")
		}
		b, _ := m.Checkouts.Checkout("b")
		if got := preparationGit(t, b.Dir, "for-each-ref", "--format=%(refname)", "refs/heads/caic-*"); got != "" {
			t.Errorf("owned branches survived: %s", got)
		}
		files, err := os.ReadDir(m.logStore.LogDir)
		if err != nil || len(files) != 0 {
			t.Errorf("failed log left files: %v %v", files, err)
		}
		if m.Len() != 0 {
			t.Fatal("failed task published")
		}
	})
	t.Run("changed_branches_survive_later_failure", func(t *testing.T) {
		for _, mode := range []string{"checked_out", "missing", "moved", "worktree"} {
			t.Run(mode, func(t *testing.T) {
				m := preparationManager(t, "a", "b", "c")
				b, _ := m.Checkouts.Checkout("b")
				c, _ := m.Checkouts.Checkout("c")
				ready, release := gatePreparationFetch(t, c.Dir, true)
				e := preparationTask(t, m, "a", "b", "c")
				result := make(chan error, 1)
				var wg sync.WaitGroup
				wg.Go(func() { result <- m.publishEntry(t.Context(), e, 1, false) })
				t.Cleanup(func() { release(); wg.Wait() })
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					t.Fatal("later fetch did not reach gate")
				}
				const ref = "refs/heads/caic-0"
				want := preparationGit(t, b.Dir, "rev-parse", ref)
				switch mode {
				case "checked_out":
					preparationGit(t, b.Dir, "checkout", "caic-0")
				case "missing":
					preparationGit(t, b.Dir, "update-ref", "-d", ref)
				case "moved":
					preparationGit(t, b.Dir, "commit", "--allow-empty", "-m", "external work")
					want = preparationGit(t, b.Dir, "rev-parse", "HEAD")
					preparationGit(t, b.Dir, "update-ref", ref, want)
				case "worktree":
					preparationGit(t, b.Dir, "worktree", "add", filepath.Join(t.TempDir(), "checkout"), "caic-0")
				}
				release()
				if err := <-result; err == nil {
					t.Fatal("later fetch failure accepted")
				}
				if mode == "missing" {
					if got := preparationGit(t, b.Dir, "for-each-ref", "--format=%(refname)", ref); got != "" {
						t.Fatalf("missing ref recreated: %q", got)
					}
				} else if got := preparationGit(t, b.Dir, "rev-parse", ref); got != want {
					t.Fatalf("preserved tip=%s want=%s", got, want)
				}
				if got := preparationGit(t, b.Dir, "config", "branch.caic-0.merge"); got != "refs/heads/main" {
					t.Fatalf("preserved branch tracking=%q", got)
				}
				if m.Len() != 0 {
					t.Fatal("failed task published")
				}
			})
		}
	})
	t.Run("adopted_and_preexisting_branches_survive", func(t *testing.T) {
		m := preparationManager(t, "a", "b")
		a, _ := m.Checkouts.Checkout("a")
		b, _ := m.Checkouts.Checkout("b")
		preparationGit(t, a.Dir, "branch", "local-work")
		preparationGit(t, a.Dir, "branch", "--set-upstream-to=origin/main", "local-work")
		preparationGit(t, b.Dir, "branch", "caic-0")
		e := preparationTask(t, m, "a", "b")
		e.Task().Repos[0].BaseBranch = "local-work"
		e.Task().StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := m.publishEntry(t.Context(), e, 1, true); err == nil {
			t.Fatal("bad header accepted")
		}
		if got := preparationGit(t, a.Dir, "for-each-ref", "--format=%(refname)", "refs/heads/"); got != "refs/heads/local-work\nrefs/heads/main" {
			t.Fatalf("adopted branches=%q", got)
		}
		if got := preparationGit(t, b.Dir, "for-each-ref", "--format=%(refname)", "refs/heads/"); got != "refs/heads/caic-0\nrefs/heads/main" {
			t.Fatalf("preexisting branches=%q", got)
		}
		if m.Len() != 0 {
			t.Fatal("failed task published")
		}
	})
	t.Run("reversed_repository_order", func(t *testing.T) {
		m := preparationManager(t, "a", "b")
		entries := []*Entry{preparationTask(t, m, "a", "b", "a"), preparationTask(t, m, "b", "a")}
		start := make(chan struct{})
		results := make(chan error, len(entries))
		var wg sync.WaitGroup
		for _, e := range entries {
			wg.Go(func() { <-start; results <- m.publishEntry(t.Context(), e, len(e.Task().Repos), false) })
		}
		close(start)
		for range entries {
			select {
			case err := <-results:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("repository preparation deadlocked")
			}
		}
		wg.Wait()
		if m.Len() != 2 {
			t.Fatalf("tasks=%d", m.Len())
		}
	})
	t.Run("concurrent_local_adoption", func(t *testing.T) {
		m := preparationManager(t, "a")
		a, _ := m.Checkouts.Checkout("a")
		preparationGit(t, a.Dir, "branch", "local-work")
		preparationGit(t, a.Dir, "branch", "--set-upstream-to=origin/main", "local-work")
		entries := []*Entry{preparationTask(t, m, "a"), preparationTask(t, m, "a")}
		start := make(chan struct{})
		results := make(chan error, len(entries))
		var wg sync.WaitGroup
		for _, e := range entries {
			e.Task().Repos[0].BaseBranch = "local-work"
			wg.Go(func() { <-start; results <- m.publishEntry(t.Context(), e, 1, true) })
		}
		close(start)
		for range entries {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		wg.Wait()
		adopted := 0
		for _, e := range entries {
			if e.Task().Primary().Branch == "local-work" {
				adopted++
			}
		}
		if adopted != 1 {
			t.Fatalf("adopted local branch %d times", adopted)
		}
	})
	t.Run("delegated_cap_across_disjoint_repositories", func(t *testing.T) {
		m := preparationManager(t, "a", "b", "c", "d")
		parent := ksid.NewID()
		for range maxDelegatedTasksPerParent - 1 {
			e := preparationTask(t, m)
			e.Task().ParentTaskID = parent
			m.Insert(e.Task().ID, e)
		}
		entries := []*Entry{preparationTask(t, m, "a", "b"), preparationTask(t, m, "c", "d")}
		b, _ := m.Checkouts.Checkout("b")
		d, _ := m.Checkouts.Checkout("d")
		readyB, releaseB := gatePreparationFetch(t, b.Dir, false)
		readyD, releaseD := gatePreparationFetch(t, d.Dir, false)
		results := make(chan error, len(entries))
		var wg sync.WaitGroup
		for _, e := range entries {
			e.Task().ParentTaskID = parent
			wg.Go(func() { results <- m.publishEntry(t.Context(), e, 1, false) })
		}
		t.Cleanup(func() { releaseB(); releaseD(); wg.Wait() })
		for _, ready := range []<-chan struct{}{readyB, readyD} {
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("disjoint delegated preparations did not both reach fetch")
			}
		}
		releaseB()
		releaseD()
		rejected := 0
		for range entries {
			if err := <-results; err != nil {
				if te, ok := errors.AsType[*Error](err); !ok || te.Kind != KindConflict {
					t.Fatal(err)
				}
				rejected++
			}
		}
		if rejected != 1 || m.Len() != maxDelegatedTasksPerParent {
			t.Fatalf("rejected=%d tasks=%d", rejected, m.Len())
		}
		for _, e := range entries {
			_, published := m.GetEntry(e.Task().ID)
			extra := e.Task().ReposSnapshot()[1]
			c, _ := m.Checkouts.Checkout(extra.Name)
			refs := preparationGit(t, c.Dir, "for-each-ref", "--format=%(refname)", "refs/heads/caic-*")
			_, err := os.Stat(filepath.Join(m.logStore.LogDir, e.Task().LogFilename()))
			if published {
				if refs == "" || err != nil {
					t.Fatalf("published resources missing: %q %v", refs, err)
				}
			} else {
				if refs != "" || !os.IsNotExist(err) {
					t.Fatalf("rejected resources retained: %q %v", refs, err)
				}
			}
		}
	})
}
