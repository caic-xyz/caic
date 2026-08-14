// Checkout owns local git, branch allocation, safety checks, and runtime diff operations.

package repo

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

// errBranchCheckedOut reports that a task branch could not be deleted because it
// is the currently checked-out branch of the host repo. Expected when caic hosts
// its own repository, so callers log it below warning level.
var errBranchCheckedOut = errors.New("branch is currently checked out")

func runtimeRemoteRef(id runtime.ID, branch string) string {
	return "refs/remotes/" + string(id.InstanceID()) + "/" + branch
}

// ParseDiffNumstat parses git diff --numstat output into a DiffStat.
// Each line has the format: <added>\t<deleted>\t<path>.
// Binary files use "-\t-\t<path>".
// Returns nil if there are no changed files.
func ParseDiffNumstat(numstat string) agent.DiffStat {
	numstat = strings.TrimSpace(numstat)
	if numstat == "" {
		return nil
	}
	var files agent.DiffStat
	for line := range strings.SplitSeq(numstat, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		fs := agent.DiffFileStat{Path: parts[2]}
		if parts[0] == "-" && parts[1] == "-" {
			fs.Binary = true
		} else {
			fs.Added, _ = strconv.Atoi(parts[0])
			fs.Deleted, _ = strconv.Atoi(parts[1])
		}
		files = append(files, fs)
	}
	return files
}

// TaskView is the read/write surface Checkout needs from a task.
// *task.Task satisfies it structurally.
type TaskView interface {
	RuntimeInstanceID() runtime.ID
	RuntimeRepos() []runtime.Repo
	SetRepoBranch(i int, branch string)
	PrimaryBaseBranch() string // "" when no primary/override
}

// Checkout owns one current local checkout and serializes its branch, git,
// fetch, and diff operations across tasks using it.
type Checkout struct {
	// Immutable.
	BaseBranch string
	Dir        string          // Absolute path to the git repository.
	RepoName   string          // Relative repo path (e.g. "github/caic").
	GitTimeout time.Duration   // Timeout for git/instance ops. Must be non-zero.
	Runtimes   *runtime.Router // Runtime provides runtime instance and repo diff/sync operations.
	Log        *slog.Logger

	branchMu sync.Mutex // Serializes branch creation (nextID + git branch) to avoid duplicate names.
	nextID   int        // Next branch sequence number (protected by branchMu).
}

// NewCheckout creates the initialized checkout at dir.
func NewCheckout(ctx context.Context, dir, baseBranch string, runtimes *runtime.Router, log *slog.Logger) (*Checkout, error) {
	if dir == "" {
		return nil, errors.New("checkout directory is required")
	}
	if log == nil {
		return nil, errors.New("checkout logger is required")
	}
	checkout := &Checkout{
		BaseBranch: baseBranch,
		Dir:        dir,
		GitTimeout: time.Minute,
		Runtimes:   runtimes,
		Log:        log,
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkout.GitTimeout)
	defer cancel()
	highest, err := maxBranchSeqNum(ctx, checkout.Dir, checkout.Log)
	if err != nil {
		return nil, err
	}
	checkout.nextID = highest + 1
	return checkout, nil
}

// AllocateBranch allocates a caic-N branch for this checkout's repo using the
// checkout's base branch. Used by the server to allocate branches for extra
// repos before starting an instance.
func (w *Checkout) AllocateBranch(ctx context.Context) (string, error) {
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	return w.allocateBranchLocked(ctx, nil)
}

// SyncToOrigin pushes each repo's task branch to origin and returns the
// combined diff stat across all repos and any safety issues found. Safety is
// checked per-repo; when force is false, issues in any repo block the push.
func (w *Checkout) SyncToOrigin(ctx context.Context, t TaskView, force bool) (agent.DiffStat, []SafetyIssue, error) {
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
		ref := runtimeRemoteRef(id, branch)
		repoDS := extractRepoDS(ds, diffRepoPrefix(&repo), multi)
		safetyCtx, safetyCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
		issues, err := CheckSafety(safetyCtx, repo.GitRoot, ref, w.BaseBranch, repoDS)
		safetyCancel()
		if err != nil {
			return ds, allIssues, fmt.Errorf("safety check %s: %w", repo.ContainerPath, err)
		}
		allIssues = append(allIssues, issues...)
	}
	if len(allIssues) > 0 && !force {
		return ds, allIssues, nil
	}

	// Phase 2: push each repo.
	for _, repo := range repos {
		branch := repo.Branch
		ref := runtimeRemoteRef(id, branch)
		pushCtx, pushCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
		checkout := &git.Checkout{Root: repo.GitRoot, Logger: w.Log}
		if err := checkout.PushRef(pushCtx, ref, branch, true); err != nil {
			pushCancel()
			return ds, allIssues, fmt.Errorf("push %s to origin: %w", repo.ContainerPath, err)
		}
		pushCancel()
	}
	return ds, allIssues, nil
}

// SyncToDefault fetches changes from the instance, runs safety checks per repo,
// and squash-pushes each repo's task branch onto its default branch. Safety
// issues always block (no force override). The commit message is built from the
// task title.
func (w *Checkout) SyncToDefault(ctx context.Context, t TaskView, message string) (agent.DiffStat, []SafetyIssue, error) {
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
		ref := runtimeRemoteRef(id, branch)
		repoDS := extractRepoDS(ds, diffRepoPrefix(&repo), multi)
		safetyCtx, safetyCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
		issues, err := CheckSafety(safetyCtx, repo.GitRoot, ref, w.BaseBranch, repoDS)
		safetyCancel()
		if err != nil {
			return ds, allIssues, fmt.Errorf("safety check %s: %w", repo.ContainerPath, err)
		}
		allIssues = append(allIssues, issues...)
	}
	if len(allIssues) > 0 {
		return ds, allIssues, nil
	}

	// Phase 2: squash each repo onto its default branch.
	for _, repo := range repos {
		branch := repo.Branch
		ref := runtimeRemoteRef(id, branch)
		squashCtx, squashCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
		checkout := &git.Checkout{Root: repo.GitRoot, Logger: w.Log}
		if err := checkout.SquashOnto(squashCtx, ref, w.BaseBranch, message); err != nil {
			squashCancel()
			return ds, allIssues, fmt.Errorf("squash %s onto %s: %w", repo.ContainerPath, w.BaseBranch, err)
		}
		squashCancel()
	}
	return ds, allIssues, nil
}

// DiffContent returns the unified diff for the given repos, optionally filtered
// to a single file path. When there are multiple repos, file paths are prefixed
// with `<repoName>/` so the frontend can distinguish changes from different
// repos. Holds branchMu during diff.
func (w *Checkout) DiffContent(ctx context.Context, t TaskView, path string) (string, error) {
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
		diff, err := w.Runtimes.Diff(ctx, id, i, args...)
		if err != nil {
			w.Log.Warn("diff failed", "repo", repo.ContainerPath, "br", repo.Branch, "err", err)
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
func (w *Checkout) BranchDiffStat(ctx context.Context, t TaskView) agent.DiffStat {
	if w.Runtimes == nil {
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

// DeleteUnmodifiedTaskBranches deletes generated task branches that never diverged from their base.
func (w *Checkout) DeleteUnmodifiedTaskBranches(ctx context.Context, t TaskView) {
	repos := t.RuntimeRepos()
	if len(repos) == 0 {
		return
	}
	gitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	for i := range repos {
		repo := &repos[i]
		dir := repo.GitRoot
		if dir == "" && i == 0 {
			dir = w.Dir
		}
		if dir == "" {
			continue
		}
		baseBranch := w.BaseBranch
		if repo.BaseBranch != "" {
			baseBranch = repo.BaseBranch
		}
		checkout := &git.Checkout{Root: dir, Logger: w.Log}
		deleted, err := deleteLocalBranchIfUnmodified(gitCtx, checkout, repo.Branch, baseBranch)
		if err != nil {
			// A checked-out task branch is expected when caic hosts its own repo
			// (the developer's working branch shares the task namespace); it is not
			// a fault, so keep it out of the warning stream.
			if errors.Is(err, errBranchCheckedOut) {
				w.Log.DebugContext(ctx, "delete empty task branch skipped: checked out", "br", repo.Branch)
			} else {
				w.Log.WarnContext(ctx, "delete empty task branch skipped", "br", repo.Branch, "err", err)
			}
			continue
		}
		if deleted {
			w.Log.InfoContext(ctx, "deleted empty task branch", "br", repo.Branch)
		}
	}
}

// ReserveBranchName reserves and returns the next branch name ("caic-N") without
// touching git (under branchMu, ~µs). The branch itself is created later — by the
// runtime when forking, or by FetchAndCreateBranch for a fresh task.
func (w *Checkout) ReserveBranchName() string {
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	name := fmt.Sprintf("caic-%d", w.nextID)
	w.nextID++
	return name
}

// FetchAndCreateBranch fetches origin and creates the given branch from the
// resolved base. Acquires branchMu to serialize git operations across concurrent
// task setups on the same repo.
func (w *Checkout) FetchAndCreateBranch(ctx context.Context, t TaskView, branch string) error {
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
func (w *Checkout) DiffStat(ctx context.Context, id runtime.ID, repos []runtime.Repo, fetchMode DiffFetchMode, fetchLogMsg string) (agent.DiffStat, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	if fetchMode != DiffWithoutFetch {
		if fetchLogMsg != "" {
			w.Log.Info(fetchLogMsg, "repos", len(repos))
		}
		if err := w.Runtimes.Fetch(ctx, id); err != nil {
			if fetchMode == DiffFetchRequired {
				return nil, err
			}
			w.Log.Warn("fetch on result failed", "err", err)
		}
	}
	return w.diffStatLocked(ctx, id, repos), nil
}

func (w *Checkout) taskRuntime(t TaskView) (runtime.ID, []runtime.Repo, error) {
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
	if repos[0].GitRoot == "" {
		repos[0].GitRoot = w.Dir
	}
	return id, repos, nil
}

// allocateBranchLocked fetches origin, resolves the start point, and creates
// the task branch. Must be called under branchMu.
func (w *Checkout) allocateBranchLocked(ctx context.Context, t TaskView) (string, error) {
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

func (w *Checkout) effectiveBaseBranch(t TaskView) string {
	if t != nil {
		if b := t.PrimaryBaseBranch(); b != "" {
			return b
		}
	}
	return w.BaseBranch
}

// diffStatLocked runs Diff("--numstat") on each repo and returns the combined
// diff stat. File paths are prefixed with `<repoName>/` when there are multiple
// repos so the frontend can distinguish changes per repo. The caller must hold
// branchMu.
func (w *Checkout) diffStatLocked(ctx context.Context, id runtime.ID, repos []runtime.Repo) agent.DiffStat {
	var result agent.DiffStat
	for i := range repos {
		repo := &repos[i]
		numstat, err := w.Runtimes.Diff(ctx, id, i, "--numstat")
		if err != nil {
			w.Log.Warn("diff numstat failed", "repo", repo.ContainerPath, "br", repo.Branch, "err", err)
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

func branchNameExists(branches [][2]string, name string) bool {
	for _, branch := range branches {
		if branch[0] == name {
			return true
		}
	}
	return false
}

func deleteLocalBranchIfUnmodified(ctx context.Context, checkout *git.Checkout, branch, baseBranch string) (bool, error) {
	if branch == "" || baseBranch == "" {
		return false, nil
	}
	localBranches, err := checkout.ListBranches(ctx, "")
	if err != nil {
		return false, err
	}
	if !branchNameExists(localBranches, branch) {
		return false, nil
	}
	branchRef := "refs/heads/" + branch
	current, err := checkout.RunGit(ctx, "branch", "--show-current")
	if err != nil {
		return false, err
	}
	if current == branch {
		return false, errBranchCheckedOut
	}
	baseRef := "refs/remotes/origin/" + baseBranch
	remoteBranches, remoteErr := checkout.ListBranches(ctx, "origin")
	if !branchNameExists(remoteBranches, baseBranch) {
		baseRef = "refs/heads/" + baseBranch
		if !branchNameExists(localBranches, baseBranch) {
			if remoteErr != nil {
				return false, fmt.Errorf("list origin branches: %w", remoteErr)
			}
			return false, fmt.Errorf("base branch %q not found", baseBranch)
		}
	}
	out, err := checkout.RunGit(ctx, "rev-list", "--count", branchRef, "--not", baseRef)
	if err != nil {
		return false, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return false, fmt.Errorf("parse unique commit count: %w", err)
	}
	if count != 0 {
		return false, nil
	}
	if _, err := checkout.RunGit(ctx, "branch", "-D", "--", branch); err != nil {
		return false, err
	}
	return true, nil
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
	for _, raw := range []string{repo.ContainerPath, repo.GitRoot} {
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
