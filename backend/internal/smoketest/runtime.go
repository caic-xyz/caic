// Package smoketest provides fake runtime and repository fixtures for smoke and e2e tests.
package smoketest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// InitRepo creates two fake repos (clone and clone2) in tmpDir so that the
// add-repo button is visible after the first repo is auto-selected on load.
// Returns the path to the primary clone.
func InitRepo(ctx context.Context, tmpDir string) (string, error) {
	if err := initOneRepo(ctx, tmpDir, filepath.Join("remotes", "remote.git"), filepath.Join("repos", "clone")); err != nil {
		return "", err
	}
	if err := initOneRepo(ctx, tmpDir, filepath.Join("remotes", "remote2.git"), filepath.Join("repos", "clone2")); err != nil {
		return "", err
	}
	return filepath.Join(tmpDir, "repos", "clone"), nil
}

// InitHarnessCache pre-populates the harness model cache with fresh dummy
// entries so refreshHarnessModels skips launching temp containers for real
// harness model discovery during smoke and e2e tests.
func InitHarnessCache(cacheDir string) error {
	cache := agent.OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json"))
	for _, h := range []harness.Name{harness.Codex, harness.Pi, harness.OpenCode} {
		cache.SetModelInventory(h, agent.ModelInventory{Models: []agent.Model{{ID: "fake-model", ContextWindow: 200_000}}}, "")
	}
	return nil
}

// RuntimeBackend implements runtime.System with no-op operations and canned
// process data.
type RuntimeBackend struct {
	// StreamStats makes WatchStats stream a paced resource history. The
	// documentation screenshots render the same server twice and compare the
	// results, so they leave it off to keep the statistics glyph stable.
	StreamStats bool

	vncPort int // non-zero when a fake VNC server is running.

	mu    sync.Mutex
	repos map[runtime.ID][]runtime.Repo
}

// NewRuntimeBackend creates a fake runtime backend for smoke and e2e tests.
func NewRuntimeBackend(vncPort int) *RuntimeBackend {
	return &RuntimeBackend{vncPort: vncPort, repos: map[runtime.ID][]runtime.Repo{}}
}

// Name returns the runtime backend name.
func (*RuntimeBackend) Name() runtime.Name { return "test-runtime" }

// Launch implements runtime.Lifecycle.
func (b *RuntimeBackend) Launch(_ context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.ID, error) {
	if _, err := opts.LogWriter.Write([]byte("- Fake runtime setup complete\n")); err != nil {
		return "", err
	}
	id := runtime.NewID(b.Name(), "md-test-no-repo")
	if len(repos) > 0 {
		id = runtime.NewID(b.Name(), runtime.InstanceID("md-test-"+strings.ReplaceAll(repos[0].Branch, "/", "-")))
	}
	b.mu.Lock()
	b.repos[id] = slices.Clone(repos)
	b.mu.Unlock()
	return id, nil
}

// Connect implements runtime.Lifecycle.
func (*RuntimeBackend) Connect(_ context.Context, id runtime.ID, _ *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	return runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: string(id.InstanceID())}}, nil
}

// Diff implements runtime.Repository.
func (*RuntimeBackend) Diff(_ context.Context, _ runtime.ID, _ int, _ ...string) (string, error) {
	return "", nil
}

// CommitDiffStat implements runtime.Repository.
func (*RuntimeBackend) CommitDiffStat(_ context.Context, _ runtime.ID, _ int, _, _ string) (string, error) {
	return "", nil
}

// FileDiff implements runtime.Repository.
func (*RuntimeBackend) FileDiff(_ context.Context, _ runtime.ID, repoIdx int, commit, path, _ string) (string, error) {
	switch {
	case repoIdx == 0 && commit != "" && path == "cmd/caic/main.go":
		return `diff --git a/cmd/caic/main.go b/cmd/caic/main.go
--- a/cmd/caic/main.go
+++ b/cmd/caic/main.go
@@ -30,0 +31,8 @@ func run() {
+	status := task.RepositoryStatus()
+	if status.ChangedFiles == 0 {
+		return nil
+	}
+	log.Info("repository changed",
+		"files", status.ChangedFiles,
+		"additions", status.Added)
+	return nil` + "\n", nil
	case repoIdx == 0 && commit == "" && path == "frontend/src/App.tsx":
		return `diff --git a/frontend/src/App.tsx b/frontend/src/App.tsx
--- a/frontend/src/App.tsx
+++ b/frontend/src/App.tsx
@@ -88,3 +88,5 @@ function TaskHeader() {
-  <span>Task status</span>
-  <span>{branch()}</span>
+  <span>Repository changes</span>
+  <span>{branch()} → {upstream()}</span>
+  <RepoStateIcons state={repoState()} />
+  <DiffLink task={task()} />
 }` + "\n", nil
	case repoIdx == 1 && commit == "" && path == "internal/service/api.go":
		return `diff --git a/internal/service/api.go b/internal/service/api.go
--- a/internal/service/api.go
+++ b/internal/service/api.go
@@ -52,2 +52,7 @@ func status() {
-	writeStatus(w)
+	state := repositoryState()
+	writeJSON(w, state)
+	log.Debug("repository state",
+		"ahead", state.Ahead,
+		"behind", state.Behind,
+	)
 }` + "\n", nil
	case repoIdx == 1 && commit == "" && path == "README.md":
		return `diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -10,5 +10,4 @@
-Open the task.
-Check the branch.
-Review the summary.
-Then push the branch.
+Open Repository changes.
+Review commits and working-tree files.
+Expand a file to inspect its patch.
 Keep changes isolated.` + "\n", nil
	default:
		return "", nil
	}
}

// RepositoryStatus implements runtime.Repository.
func (b *RuntimeBackend) RepositoryStatus(_ context.Context, id runtime.ID, repoIdx int) (runtime.RepositoryStatus, error) {
	b.mu.Lock()
	repos := slices.Clone(b.repos[id])
	b.mu.Unlock()
	if repoIdx < 0 || repoIdx >= len(repos) {
		return runtime.RepositoryStatus{}, fmt.Errorf("repo index %d out of range for %d repos", repoIdx, len(repos))
	}
	upstream := "origin/main"
	if repos[repoIdx].BaseBranch != "" {
		upstream = "origin/" + repos[repoIdx].BaseBranch
	}
	status := runtime.RepositoryStatus{Branch: repos[repoIdx].Branch, Upstream: upstream}
	if repoIdx == 0 {
		status.Ahead = 1
		status.DiffStat = []runtime.GitFileStat{
			{Path: "cmd/caic/main.go", LinesAdded: 8},
			{Path: "frontend/src/App.tsx", LinesAdded: 4, LinesDeleted: 2},
		}
		status.Commits = []runtime.GitCommit{{
			SHA:          "7b14c36e1f5a0d2c9e8f4b6a3c1d0e9f8a7b6c5d",
			Subject:      "Add task activity summary",
			AuthoredDate: "2026-09-01T10:30:00Z",
			Stat:         []runtime.GitFileStat{{Path: "cmd/caic/main.go", LinesAdded: 8}},
		}}
		status.Uncommitted = []runtime.GitFileStatus{{
			Path:           "frontend/src/App.tsx",
			WorktreeStatus: "M",
			LinesAdded:     4,
			LinesDeleted:   2,
		}}
		return status, nil
	}
	status.Behind = 1
	status.DiffStat = []runtime.GitFileStat{
		{Path: "internal/service/api.go", LinesAdded: 6, LinesDeleted: 1},
		{Path: "README.md", LinesAdded: 3, LinesDeleted: 4},
	}
	status.Uncommitted = []runtime.GitFileStatus{
		{Path: "internal/service/api.go", WorktreeStatus: "M", LinesAdded: 6, LinesDeleted: 1},
		{Path: "README.md", WorktreeStatus: "M", LinesAdded: 3, LinesDeleted: 4},
	}
	return status, nil
}

// CompactRepositoryStatus implements runtime.Repository. The fake has no log
// walk to skip, so it returns the same fixture as the full status.
func (b *RuntimeBackend) CompactRepositoryStatus(ctx context.Context, id runtime.ID, repoIdx int) (runtime.RepositoryStatus, error) {
	return b.RepositoryStatus(ctx, id, repoIdx)
}

// Fetch implements runtime.Repository.
func (*RuntimeBackend) Fetch(_ context.Context, _ runtime.ID, _ runtime.FetchOpts) ([]runtime.FetchedBranch, error) {
	return nil, nil
}

// Stop implements runtime.Lifecycle.
func (*RuntimeBackend) Stop(_ context.Context, _ runtime.ID) error { return nil }

// Purge implements runtime.Lifecycle.
func (*RuntimeBackend) Purge(_ context.Context, _ runtime.ID) error { return nil }

// Revive implements runtime.Lifecycle.
func (*RuntimeBackend) Revive(_ context.Context, _ runtime.ID) error { return nil }

// Fork implements runtime.Lifecycle.
func (b *RuntimeBackend) Fork(_ context.Context, _ runtime.ID, _ *runtime.ForkOptions) (runtime.ID, runtime.ConnectionInfo, error) {
	return runtime.NewID(b.Name(), "fake-fork"), runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: "fake-fork"}}, errors.New("fork not supported in fake runtime")
}

// VNCPort implements runtime.Lifecycle.
func (b *RuntimeBackend) VNCPort(_ context.Context, _ runtime.ID) int { return b.vncPort }

// Processes implements runtime.Lifecycle.
func (*RuntimeBackend) Processes(_ context.Context, _ runtime.ID) ([]runtime.ProcessInfo, error) {
	return fakeProcesses(), nil
}

// Signal implements runtime.Lifecycle.
func (*RuntimeBackend) Signal(_ context.Context, _ runtime.ID, _ int, _ string) error {
	return nil
}

// List implements runtime.Inventory.
func (*RuntimeBackend) List(context.Context) ([]runtime.Instance, error) {
	return nil, nil
}

// Metadata implements runtime.Inventory.
func (*RuntimeBackend) Metadata(context.Context, runtime.ID, runtime.MetadataKey) (string, error) {
	return "", nil
}

// Inspect implements runtime.Inventory.
func (*RuntimeBackend) Inspect(_ context.Context, id runtime.ID) (*runtime.InstanceInspect, error) {
	return &runtime.InstanceInspect{Runtime: "fake", ID: id, State: "running", OS: "linux", CPUArchitecture: stdruntime.GOARCH}, nil
}

// statsSampleInterval paces the fake resource history. The server stamps each
// sample on arrival, so pacing is what turns the cumulative byte counters into
// meaningful per-second throughput.
const statsSampleInterval = 150 * time.Millisecond

// fakeStatsHistory is the deterministic resource history a watched instance
// streams. The first sample reports a distinct CPU reading and every later one
// reports the same value, so a browser test can assert a readout that survives
// a stream restart: the manager resubscribes whenever any task changes state,
// which restarts this sequence.
func fakeStatsHistory() []runtime.Stats {
	const (
		count     = 12
		memLimit  = 4 << 30
		memStart  = 512 << 20
		memStep   = 64 << 20
		diskStart = 1 << 30
		diskStep  = 32 << 20
	)
	history := make([]runtime.Stats, 0, count)
	for i := range count {
		cpu := 50.0
		if i == 0 {
			cpu = 10
		}
		memUsed := uint64(memStart + i*memStep)
		history = append(history, runtime.Stats{
			CPUPerc:  cpu,
			MemUsed:  memUsed,
			MemLimit: memLimit,
			MemPerc:  float64(memUsed) / float64(memLimit) * 100,
			NetRx:    uint64(i) * (64 << 10),
			NetTx:    uint64(i) * (16 << 10),
			DiskUsed: int64(diskStart + i*diskStep),
		})
	}
	return history
}

// WatchStats implements runtime.Monitor.
func (b *RuntimeBackend) WatchStats(ctx context.Context, ids []runtime.ID) (iter.Seq2[runtime.StatsSample, error], error) {
	return func(yield func(runtime.StatsSample, error) bool) {
		if b.StreamStats {
			ticker := time.NewTicker(statsSampleInterval)
			defer ticker.Stop()
			for _, stats := range fakeStatsHistory() {
				for _, id := range ids {
					if !yield(runtime.StatsSample{InstanceID: id, Stats: stats}, nil) {
						return
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}
		<-ctx.Done()
	}, nil
}

// DiskUsage implements runtime.Monitor.
func (*RuntimeBackend) DiskUsage(_ context.Context, ids []runtime.ID) (map[runtime.ID]int64, error) {
	usage := make(map[runtime.ID]int64, len(ids))
	for _, id := range ids {
		usage[id] = 0
	}
	return usage, nil
}

// WatchEvents implements runtime.Monitor.
func (*RuntimeBackend) WatchEvents(ctx context.Context, _ runtime.EventFilter) (<-chan runtime.Event, error) {
	ch := make(chan runtime.Event)
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

// SudoPassword implements runtime.PrivilegeInfo.
func (*RuntimeBackend) SudoPassword(context.Context, runtime.ID) (string, error) {
	return "", nil
}

var _ runtime.System = (*RuntimeBackend)(nil)

// initOneRepo initialises a bare remote and a clone under tmpDir.
func initOneRepo(ctx context.Context, tmpDir, bareName, cloneName string) error {
	bare := filepath.Join(tmpDir, bareName)
	clone := filepath.Join(tmpDir, cloneName)
	if err := os.MkdirAll(filepath.Dir(bare), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "--bare", bare},
		{"init", clone},
		{"-C", clone, "config", "user.name", "Test"},
		{"-C", clone, "config", "user.email", "test@test.com"},
		{"-C", clone, "checkout", "-b", "main"},
	} {
		if err := runGit(ctx, args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("hello\n"), 0o600); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"-C", clone, "add", "."},
		{"-C", clone, "commit", "-m", "init"},
		{"-C", clone, "remote", "add", "origin", bare},
		{"-C", clone, "push", "-u", "origin", "main"},
	} {
		if err := runGit(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func runGit(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput() //nolint:gosec // args are hardcoded git subcommands
	if err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return nil
}

// fakeProcesses returns a canned process tree for e2e screenshots:
//
//	init(1)
//	  sshd(42)
//	    sshd-session(99)
//	      bash(100)
//	        node(200) - agent harness
//	        make(201)
//	          gcc(300)
//	          gcc(301)
//	        ps(202)
func fakeProcesses() []runtime.ProcessInfo {
	now := time.Now()
	return []runtime.ProcessInfo{
		{PID: 1, PPID: 0, PGRP: 1, User: "root", State: "S", Priority: 19, Threads: 1, OpenFDs: new(7), CPU: 0.0, Mem: 0.1, RSSBytes: 1_048_576, CPUTime: 0, StartedAt: now.Add(-2 * time.Hour), Command: "/sbin/init"},
		{PID: 42, PPID: 1, PGRP: 42, User: "root", State: "S", Priority: 19, Threads: 1, OpenFDs: new(5), CPU: 0.0, Mem: 0.2, RSSBytes: 2_097_152, CPUTime: time.Second, StartedAt: now.Add(-1*time.Hour - 59*time.Minute - 55*time.Second), Command: "sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups"},
		{PID: 99, PPID: 42, PGRP: 42, User: "root", State: "S", Priority: 19, Threads: 1, OpenFDs: new(4), CPU: 0.0, Mem: 0.3, RSSBytes: 3_145_728, CPUTime: 0, StartedAt: now.Add(-1*time.Hour - 59*time.Minute - 50*time.Second), Command: "sshd: user [priv]"},
		{PID: 100, PPID: 99, PGRP: 100, User: "user", State: "S", Priority: 19, Threads: 1, OpenFDs: new(6), CPU: 0.1, Mem: 0.5, RSSBytes: 5_242_880, CPUTime: 2 * time.Second, StartedAt: now.Add(-1*time.Hour - 59*time.Minute - 40*time.Second), Command: "-bash"},
		{PID: 200, PPID: 100, PGRP: 100, User: "user", State: "R", Priority: 19, Threads: 5, OpenFDs: new(31), CPU: 45.2, Mem: 12.3, RSSBytes: 128_974_848, CPUTime: time.Minute + 23*time.Second, StartedAt: now.Add(-1*time.Hour - 58*time.Minute - 40*time.Second), Command: "node /home/user/.npm/_npx/abc123/node_modules/.bin/claude --dangerously-skip-permissions"},
		{PID: 201, PPID: 100, PGRP: 100, User: "user", State: "S", Priority: 19, Threads: 1, OpenFDs: new(8), CPU: 0.0, Mem: 0.1, RSSBytes: 1_048_576, CPUTime: 0, StartedAt: now.Add(-45 * time.Second), Command: "make -j$(nproc)"},
		{PID: 300, PPID: 201, PGRP: 100, User: "user", State: "R", Priority: 19, Threads: 1, OpenFDs: new(4), CPU: 98.7, Mem: 5.6, RSSBytes: 58_720_256, CPUTime: 45 * time.Second, StartedAt: now.Add(-42 * time.Second), Command: "/usr/lib/gcc/x86_64-linux-gnu/14/cc1 -quiet -Iinclude -D_FORTIFY_SOURCE=2 src/main.c -o /tmp/ccXyz.s"},
		{PID: 301, PPID: 201, PGRP: 100, User: "user", State: "R", Priority: 19, Threads: 1, OpenFDs: new(4), CPU: 97.1, Mem: 4.8, RSSBytes: 50_331_648, CPUTime: 42 * time.Second, StartedAt: now.Add(-39 * time.Second), Command: "/usr/lib/gcc/x86_64-linux-gnu/14/cc1 -quiet -Iinclude -D_FORTIFY_SOURCE=2 src/parser.c -o /tmp/ccAbc.s"},
		{PID: 202, PPID: 100, PGRP: 100, User: "user", State: "R", Priority: 19, Threads: 1, OpenFDs: new(3), CPU: 0.3, Mem: 0.1, RSSBytes: 1_048_576, CPUTime: 0, StartedAt: now, Command: "ps -eo pid,ppid,pgrp,user,stat,pri,ni,nlwp,%cpu,%mem,rss,time,lstart,args"},
	}
}
