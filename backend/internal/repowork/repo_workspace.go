// Package repowork owns repo-level branch allocation, git sync, safety
// checks, and runtime diff operations.
package repowork

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime/trace"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// TaskView is the read/write surface RepoWorkspace needs from a task.
// *task.Task satisfies it structurally.
type TaskView interface {
	RuntimeInstanceID() runtime.InstanceID
	RuntimeRepos() []runtime.Repo
	SetRepoBranch(i int, branch string)
	PrimaryBaseBranch() string // "" when no primary/override
}

// RepoWorkspace holds one repo's config and serializes branch/git/fetch/diff
// operations across every task backed by that repo.
type RepoWorkspace struct {
	BaseBranch string
	Dir        string          // Absolute path to the git repository.
	RepoName   string          // Relative repo path (e.g. "github/caic"); empty for no-repo workspaces.
	GitTimeout time.Duration   // Timeout for git/instance ops. Must be non-zero.
	Runtime    runtime.Backend // Runtime provides runtime instance and repo diff/sync operations.

	branchMu sync.Mutex // Serializes branch creation (nextID + git branch) to avoid duplicate names.
	nextID   int        // Next branch sequence number (protected by branchMu).
	Log      *slog.Logger
}

// NewRepoWorkspace creates a workspace for a managed git repository.
func NewRepoWorkspace(baseBranch, dir, repoName string, gitTimeout time.Duration, backend runtime.Backend, log *slog.Logger) (*RepoWorkspace, error) {
	if gitTimeout == 0 {
		return nil, errors.New("git timeout must be non-zero")
	}
	if log == nil {
		return nil, errors.New("log must be non-nil")
	}
	return &RepoWorkspace{
		BaseBranch: baseBranch,
		Dir:        dir,
		RepoName:   repoName,
		GitTimeout: gitTimeout,
		Runtime:    backend,
		Log:        log,
	}, nil
}

// Init sets nextID past any existing caic-* branches so that restarts don't
// waste attempts on branches that already exist. No-op for no-repo workspaces.
func (w *RepoWorkspace) Init(ctx context.Context) error {
	if w.Dir == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	highest, err := maxBranchSeqNum(ctx, w.Dir, w.Log)
	if err != nil {
		return err
	}
	if highest >= w.nextID {
		w.nextID = highest + 1
	}
	return nil
}

// AllocateBranch allocates a caic-N branch for this workspace's repo using the
// workspace's base branch. Used by the server to allocate branches for extra
// repos before starting an instance.
func (w *RepoWorkspace) AllocateBranch(ctx context.Context) (string, error) {
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	return w.allocateBranchLocked(ctx, nil)
}

// SyncToOrigin pushes each repo's task branch to origin and returns the
// combined diff stat across all repos and any safety issues found. Safety is
// checked per-repo; when force is false, issues in any repo block the push.
func (w *RepoWorkspace) SyncToOrigin(ctx context.Context, t TaskView, force bool) (agent.DiffStat, []SafetyIssue, error) {
	if w.Dir == "" {
		return nil, nil, errors.New("sync is not supported for no-repo tasks")
	}
	id, repos, err := w.taskRuntime(t)
	if err != nil {
		return nil, nil, err
	}
	region := trace.StartRegion(ctx, "sync-fetch")
	ds, err := w.DiffStat(ctx, id, repos, DiffFetchRequired, "fetch")
	region.End()
	if err != nil {
		return nil, nil, err
	}

	// Phase 1: safety check each repo, collect all issues.
	multi := len(repos) > 1
	var allIssues []SafetyIssue
	for _, repo := range repos {
		branch := repo.Branch
		ref := "refs/remotes/" + string(id) + "/" + branch
		repoDS := extractRepoDS(ds, diffRepoPrefix(&repo), multi)
		safetyCtx, safetyCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
		issues, err := CheckSafety(safetyCtx, repo.HostPath, ref, w.BaseBranch, repoDS)
		safetyCancel()
		if err != nil {
			return ds, allIssues, fmt.Errorf("safety check %s: %w", repo.MountPath, err)
		}
		allIssues = append(allIssues, issues...)
	}
	if len(allIssues) > 0 && !force {
		return ds, allIssues, nil
	}

	// Phase 2: push each repo.
	for _, repo := range repos {
		branch := repo.Branch
		ref := "refs/remotes/" + string(id) + "/" + branch
		pushCtx, pushCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
		checkout := &git.Checkout{Root: repo.HostPath, Logger: w.Log}
		if err := checkout.PushRef(pushCtx, ref, branch, true); err != nil {
			pushCancel()
			return ds, allIssues, fmt.Errorf("push %s to origin: %w", repo.MountPath, err)
		}
		pushCancel()
	}
	return ds, allIssues, nil
}

// SyncToDefault fetches changes from the instance, runs safety checks per repo,
// and squash-pushes each repo's task branch onto its default branch. Safety
// issues always block (no force override). The commit message is built from the
// task title.
func (w *RepoWorkspace) SyncToDefault(ctx context.Context, t TaskView, message string) (agent.DiffStat, []SafetyIssue, error) {
	if w.Dir == "" {
		return nil, nil, errors.New("sync is not supported for no-repo tasks")
	}
	id, repos, err := w.taskRuntime(t)
	if err != nil {
		return nil, nil, err
	}
	region := trace.StartRegion(ctx, "sync-default-fetch")
	ds, err := w.DiffStat(ctx, id, repos, DiffFetchRequired, "fetch for default sync")
	region.End()
	if err != nil {
		return nil, nil, err
	}

	// Phase 1: safety check each repo, collect all issues.
	multi := len(repos) > 1
	var allIssues []SafetyIssue
	for _, repo := range repos {
		branch := repo.Branch
		ref := "refs/remotes/" + string(id) + "/" + branch
		repoDS := extractRepoDS(ds, diffRepoPrefix(&repo), multi)
		safetyCtx, safetyCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
		issues, err := CheckSafety(safetyCtx, repo.HostPath, ref, w.BaseBranch, repoDS)
		safetyCancel()
		if err != nil {
			return ds, allIssues, fmt.Errorf("safety check %s: %w", repo.MountPath, err)
		}
		allIssues = append(allIssues, issues...)
	}
	if len(allIssues) > 0 {
		return ds, allIssues, nil
	}

	// Phase 2: squash each repo onto its default branch.
	for _, repo := range repos {
		branch := repo.Branch
		ref := "refs/remotes/" + string(id) + "/" + branch
		squashCtx, squashCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
		checkout := &git.Checkout{Root: repo.HostPath, Logger: w.Log}
		if err := checkout.SquashOnto(squashCtx, ref, w.BaseBranch, message); err != nil {
			squashCancel()
			return ds, allIssues, fmt.Errorf("squash %s onto %s: %w", repo.MountPath, w.BaseBranch, err)
		}
		squashCancel()
	}
	return ds, allIssues, nil
}

// DiffContent returns the unified diff for the given repos, optionally filtered
// to a single file path. When there are multiple repos, file paths are prefixed
// with `<repoName>/` so the frontend can distinguish changes from different
// repos. Holds branchMu during diff.
func (w *RepoWorkspace) DiffContent(ctx context.Context, t TaskView, path string) (string, error) {
	if w.Dir == "" {
		return "", errors.New("diff is not supported for no-repo tasks")
	}
	id, repos, err := w.taskRuntime(t)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	var buf strings.Builder
	for i := range repos {
		repo := &repos[i]
		args := diffContentArgs(path, repo, len(repos) > 1)
		diff, err := w.Runtime.Diff(ctx, id, i, args...)
		if err != nil {
			w.Log.Warn("diff failed", "repo", repo.MountPath, "br", repo.Branch, "err", err)
			continue
		}
		if diff == "" {
			continue
		}
		buf.WriteString(diff)
	}
	return buf.String(), nil
}

// BranchDiffStat fetches from the instance and returns the per-repo branch diff
// stat (md diff --numstat). Unlike the relay's diff_watcher which only tracks
// uncommitted changes, this captures the full branch diff relative to the base.
// Used by adoptOne to restore the diff stat after server restart.
func (w *RepoWorkspace) BranchDiffStat(ctx context.Context, t TaskView) agent.DiffStat {
	if w.Runtime == nil || w.Dir == "" {
		return nil
	}
	id, repos, err := w.taskRuntime(t)
	if err != nil {
		w.Log.Warn("resolve task runtime for branch diff stat failed", "err", err)
		return nil
	}
	ds, err := w.DiffStat(ctx, id, repos, DiffFetchRequired, "fetch for branch diff stat")
	if err != nil {
		w.Log.Warn("fetch for branch diff stat failed", "err", err)
		return nil
	}
	return ds
}

// ReserveBranch instantly reserves the next branch name (under lock, ~µs). The
// branch itself is created concurrently with runtime launch by
// FetchAndCreateBranch.
func (w *RepoWorkspace) ReserveBranch(t TaskView) {
	if w.Dir == "" {
		return
	}
	w.branchMu.Lock()
	t.SetRepoBranch(0, fmt.Sprintf("caic-%d", w.nextID))
	w.nextID++
	w.branchMu.Unlock()
}

// FetchAndCreateBranch fetches origin and creates the given branch from the
// resolved base. Acquires branchMu to serialize git operations across concurrent
// task setups on the same repo.
func (w *RepoWorkspace) FetchAndCreateBranch(ctx context.Context, t TaskView, branch string) error {
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	gitCtx, gitCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer gitCancel()
	checkout := &git.Checkout{Root: w.Dir, Logger: w.Log}
	if err := checkout.Fetch(gitCtx); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	effectiveBase := w.effectiveBaseBranch(t)
	startPoint := "origin/" + effectiveBase
	if _, err := checkout.RevParse(gitCtx, startPoint); err != nil {
		startPoint = effectiveBase
	}
	w.Log.Info("creating branch", "br", branch, "base", effectiveBase)
	if err := checkout.CreateBranch(gitCtx, branch, startPoint, true); err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	return nil
}

// DiffFetchMode controls whether DiffStat fetches from the runtime instance
// before computing the diff.
type DiffFetchMode int

const (
	// DiffWithoutFetch computes the diff stat without fetching first.
	DiffWithoutFetch DiffFetchMode = iota
	// DiffFetchBestEffort fetches first but tolerates fetch failures.
	DiffFetchBestEffort
	// DiffFetchRequired fetches first and fails if the fetch fails.
	DiffFetchRequired
)

// DiffStat optionally fetches from the instance, then returns the combined
// per-repo diff stat (git diff --numstat).
func (w *RepoWorkspace) DiffStat(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, fetchMode DiffFetchMode, fetchLogMsg string) (agent.DiffStat, error) {
	if w.Dir == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	if fetchMode != DiffWithoutFetch {
		if fetchLogMsg != "" {
			w.Log.Info(fetchLogMsg, "repos", len(repos))
		}
		if err := w.Runtime.Fetch(ctx, id); err != nil {
			if fetchMode == DiffFetchRequired {
				return nil, err
			}
			w.Log.Warn("fetch on result failed", "err", err)
		}
	}
	return w.diffStatLocked(ctx, id, repos), nil
}

func (w *RepoWorkspace) taskRuntime(t TaskView) (runtime.InstanceID, []runtime.Repo, error) {
	if t == nil {
		return "", nil, errors.New("task is nil")
	}
	id := t.RuntimeInstanceID()
	if id == "" {
		return "", nil, errors.New("task has no runtime instance")
	}
	repos := t.RuntimeRepos()
	if len(repos) == 0 {
		return id, nil, nil
	}
	if repos[0].HostPath == "" {
		repos[0].HostPath = w.Dir
	}
	return id, repos, nil
}

// allocateBranchLocked fetches origin, resolves the start point, and creates
// the task branch. Must be called under branchMu.
func (w *RepoWorkspace) allocateBranchLocked(ctx context.Context, t TaskView) (string, error) {
	detached := context.WithoutCancel(ctx)
	gitCtx, gitCancel := context.WithTimeout(detached, w.GitTimeout)
	defer gitCancel()
	checkout := &git.Checkout{Root: w.Dir, Logger: w.Log}
	// Fetch so that origin/<base> is up to date.
	if err := checkout.Fetch(gitCtx); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	// Resolve effective base branch: use task override if provided.
	effectiveBase := w.effectiveBaseBranch(t)
	// Prefer the remote tracking ref, but fall back to the local branch when
	// the base branch only exists locally (not yet pushed to origin).
	startPoint := "origin/" + effectiveBase
	if _, err := checkout.RevParse(gitCtx, startPoint); err != nil {
		startPoint = effectiveBase
	}
	// Assign a sequential branch name, skipping existing ones.
	var branch string
	var err error
	for range 100 {
		if gitCtx.Err() != nil {
			return "", gitCtx.Err()
		}
		branch = fmt.Sprintf("caic-%d", w.nextID)
		w.nextID++
		w.Log.Info("creating branch", "br", branch, "base", effectiveBase)
		err = checkout.CreateBranch(gitCtx, branch, startPoint, true)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("create branch: %w", err)
	}
	return branch, nil
}

func (w *RepoWorkspace) effectiveBaseBranch(t TaskView) string {
	if t != nil {
		if b := t.PrimaryBaseBranch(); b != "" {
			return b
		}
	}
	return w.BaseBranch
}

// diffStatLocked runs Diff("--numstat") on each repo and returns the combined
// diff stat. File paths are prefixed with `<repoName>/` when there are multiple
// repos so the frontend can distinguish changes per repo. Returns nil for
// no-repo workspaces (dir == ""). The caller must hold branchMu.
func (w *RepoWorkspace) diffStatLocked(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo) agent.DiffStat {
	if w.Dir == "" {
		return nil
	}
	var result agent.DiffStat
	for i := range repos {
		repo := &repos[i]
		numstat, err := w.Runtime.Diff(ctx, id, i, "--numstat")
		if err != nil {
			w.Log.Warn("diff numstat failed", "repo", repo.MountPath, "br", repo.Branch, "err", err)
			continue
		}
		ds := ParseDiffNumstat(numstat)
		if len(repos) > 1 {
			prefix := diffRepoPrefix(repo)
			for i := range ds {
				ds[i].Path = prefix + "/" + ds[i].Path
			}
		}
		result = append(result, ds...)
	}
	return result
}

// maxBranchSeqNum finds the highest sequence number N among all local and
// remote branches matching "caic-N". Returns -1 if no matching branches exist.
func maxBranchSeqNum(ctx context.Context, dir string, log *slog.Logger) (int, error) {
	checkout := &git.Checkout{Root: dir, Logger: log}
	remotes := []string{""}
	if out, err := checkout.RunGit(ctx, "remote"); err == nil && out != "" {
		seen := map[string]struct{}{"": {}}
		for remote := range strings.SplitSeq(out, "\n") {
			remote = strings.TrimSpace(remote)
			if remote == "" {
				continue
			}
			if _, ok := seen[remote]; ok {
				continue
			}
			seen[remote] = struct{}{}
			remotes = append(remotes, remote)
		}
	}
	highest := -1
	for _, remote := range remotes {
		branches, err := checkout.ListBranches(ctx, remote)
		if err != nil {
			return -1, fmt.Errorf("list %s branches: %w", branchListName(remote), err)
		}
		for _, branch := range branches {
			name := branch[0]
			if n, ok := caicBranchNumber(name); ok && n > highest {
				highest = n
			}
		}
	}
	return highest, nil
}

func branchListName(remote string) string {
	if remote == "" {
		return "local"
	}
	return remote
}

func caicBranchNumber(name string) (int, bool) {
	numStr, ok := strings.CutPrefix(strings.TrimSpace(name), "caic-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, false
	}
	return n, true
}

// extractRepoDS filters the combined diff stat to entries belonging to repoName,
// stripping the name prefix. When multi is false (single repo), ds is returned
// unchanged since no prefix was applied.
func extractRepoDS(ds agent.DiffStat, repoName string, multi bool) agent.DiffStat {
	if !multi {
		return ds
	}
	prefix := repoName + "/"
	var result agent.DiffStat
	for _, f := range ds {
		if path, ok := strings.CutPrefix(f.Path, prefix); ok {
			f.Path = path
			result = append(result, f)
		}
	}
	return result
}

func diffContentArgs(path string, repo *runtime.Repo, multi bool) []string {
	var args []string
	if multi {
		prefix := diffRepoPrefix(repo)
		args = append(args, "--src-prefix=a/"+prefix+"/", "--dst-prefix=b/"+prefix+"/")
	} else {
		args = append(args, "--src-prefix=", "--dst-prefix=")
	}
	if path != "" {
		args = append(args, "--", path)
	}
	return args
}

func diffRepoPrefix(repo *runtime.Repo) string {
	if repo == nil {
		return "repo"
	}
	for _, raw := range []string{repo.MountPath, repo.HostPath} {
		prefix := cleanDiffRepoPrefix(raw)
		if prefix != "" {
			return prefix
		}
	}
	return "repo"
}

func cleanDiffRepoPrefix(raw string) string {
	path := filepath.ToSlash(strings.TrimSpace(raw))
	path = strings.TrimRight(path, "/")
	for _, prefix := range []string{"~/src/", "/home/user/src/", "~/"} {
		path = strings.TrimPrefix(path, prefix)
	}
	path = strings.TrimLeft(path, "/")
	path = strings.TrimPrefix(path, "./")
	if path == "." {
		return ""
	}
	return path
}
