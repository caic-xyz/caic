// Checkout owns pooled task pushes and Git queries with snapshots ordered by probe completion.

package repo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/trace"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// errBranchCheckedOut reports that a task branch could not be deleted because it
// is the currently checked-out branch of the host repo. Expected when caic hosts
// its own repository, so callers log it below warning level.
var errBranchCheckedOut = errors.New("branch is currently checked out")

// ParseDiffNumstat parses git diff --numstat output into a DiffStat.
// Each line has the format: <added>\t<deleted>\t<path>.
// Binary files use "-\t-\t<path>".
// When the output also contains an appended git diff --stat block (as produced
// by --numstat --stat), binary sizes are attached to the matching entries.
// Returns nil if there are no changed files.
func ParseDiffNumstat(numstat string) v3.DiffStat {
	numstat = strings.TrimSpace(numstat)
	if numstat == "" {
		return nil
	}
	var files v3.DiffStat
	statIndex := 0
	for line := range strings.SplitSeq(numstat, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// A git diff --stat row is "<path> | <graph>"; numstat rows are
		// tab-separated. --stat rows follow all numstat rows, so attach each
		// binary size to the numstat entry at the same position.
		if !strings.Contains(line, "\t") && strings.Contains(line, " | ") {
			if statIndex < len(files) {
				if oldSize, newSize, ok := parseBinaryStatSizes(line); ok {
					files[statIndex].Binary = true
					files[statIndex].OldSize = oldSize
					files[statIndex].NewSize = newSize
				}
			}
			statIndex++
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		fs := v3.DiffFileStat{Path: parts[2]}
		if parts[0] == "-" && parts[1] == "-" {
			fs.Binary = true
		} else {
			fs.LinesAdded, _ = strconv.Atoi(parts[0])
			fs.LinesDeleted, _ = strconv.Atoi(parts[1])
		}
		files = append(files, fs)
	}
	return files
}

// parseBinaryStatSizes extracts the "Bin <old> -> <new> bytes" sizes from one
// git diff --stat row, reporting whether the row describes a binary file.
func parseBinaryStatSizes(line string) (oldSize, newSize int64, ok bool) {
	const marker = "| Bin "
	_, after, ok := strings.Cut(line, marker)
	if !ok {
		return 0, 0, false
	}
	rest := strings.TrimSuffix(after, " bytes")
	oldStr, newStr, found := strings.Cut(rest, " -> ")
	if !found {
		return 0, 0, false
	}
	oldSize, oldErr := strconv.ParseInt(oldStr, 10, 64)
	newSize, newErr := strconv.ParseInt(newStr, 10, 64)
	if oldErr != nil || newErr != nil {
		return 0, 0, false
	}
	return oldSize, newSize, true
}

// LiveBranchesByRoot groups the branch names of instances by their repo's
// GitRoot, for use as the liveBranches argument to NewCheckout/DiscoverCheckout.
func LiveBranchesByRoot(instances []runtime.Instance) map[string][]string {
	byRoot := make(map[string][]string)
	for i := range instances {
		for _, r := range instances[i].Repos {
			if r.GitRoot == "" || r.Branch == "" {
				continue
			}
			byRoot[r.GitRoot] = append(byRoot[r.GitRoot], r.Branch)
		}
	}
	return byRoot
}

// TaskView is the read/write surface Checkout needs from a task.
// *task.Task satisfies it structurally.
type TaskView interface {
	GitTarget() GitTarget
	SetRepoBranch(i int, branch string)
	PrimaryBaseBranch() string // "" when no primary/override
}

// Checkout owns one current local checkout and serializes its branch, git,
// fetch, and diff operations across tasks using it.
type Checkout struct {
	// Immutable.
	Repository       *Repository
	RelPath          string
	Dir              string
	BaseBranch       string
	BaseBranchRemote string
	GitTimeout       time.Duration
	// PushTimeout bounds setup, hooks, network, and Git cleanup; verification
	// can require a cold tool/dependency install, unlike ordinary Git queries.
	PushTimeout time.Duration
	PushDir     string

	branchMu sync.Mutex // Serializes branch creation (nextID + git branch) to avoid duplicate names.
	nextID   int        // Next branch sequence number (protected by branchMu).
}

// NewCheckout creates the initialized checkout at dir.
//
// liveBranches are branch names ("caic-N") taken from running containers mapped to
// dir; pass the container-derived branches for this repo so a container
// whose branch never made it into git (e.g. a launch that failed mid-setup)
// still reserves its sequence number. See maxBranchSeqNum.
// cacheDir is the configured application cache root. Persisted idle push
// worktrees remain reusable across restarts; interrupted operations are reported.
func NewCheckout(ctx context.Context, log *slog.Logger, dir, cacheDir, baseBranch string, liveBranches []string) (*Checkout, error) {
	if dir == "" {
		return nil, errors.New("checkout directory is required")
	}
	if log == nil {
		return nil, errors.New("checkout logger is required")
	}
	if cacheDir == "" {
		return nil, errors.New("checkout cache directory is required")
	}
	pushDir, err := pushCacheDir(cacheDir, true)
	if err != nil {
		return nil, err
	}
	checkout := &Checkout{
		BaseBranch: baseBranch,
		Dir:        dir,
		GitTimeout: time.Minute,
		// Cold verification may install dependencies and build its tools.
		PushTimeout: 10 * time.Minute,
		PushDir:     pushDir,
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkout.GitTimeout)
	defer cancel()
	highest, err := maxBranchSeqNum(ctx, log, checkout.Dir, liveBranches)
	if err != nil {
		return nil, err
	}
	if err := checkout.reportRetainedPushWorktrees(ctx, log, dir); err != nil {
		return nil, fmt.Errorf("report retained push worktrees: %w", err)
	}
	checkout.nextID = highest + 1
	return checkout, nil
}

// AllocateBranch allocates a caic-N branch from baseBranch. An empty baseBranch
// uses the checkout default. Used to allocate branches for extra repositories.
// preferred, when non-empty, is a branch name shared across a multi-repo task's
// repos (Manager.allocateBranches): it is tried first, and allocation falls back
// to the next sequential name when it already exists in git.
func (w *Checkout) AllocateBranch(ctx context.Context, log *slog.Logger, baseBranch, preferred string) (string, error) {
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	return w.allocateBranchLocked(ctx, log, baseBranch, preferred)
}

// PushDestination selects one repository's remote, destination branch, and
// safety comparison revision. Callers resolve one destination per captured task repository.
type PushDestination struct {
	Remote string
	Branch string
	// BaseRef is an explicit safety comparison ref or immutable commit ID.
	BaseRef string
}

// PushOptions keeps committing pending edits, accepting safety issues, and
// allowing non-fast-forward updates independent.
type PushOptions struct {
	CommitPending bool
	BypassSafety  bool
	Force         bool
}

// ResolvePushDestinations fills host roots and discovers destinations for a
// captured task target under bounded, request-independent Git contexts.
// defaultTarget selects each remote's default branch as the destination; otherwise
// it selects the task branch. Safety always compares against that remote's
// default branch, independently of the runtime's task-diff baseline.
func (w *Checkout) ResolvePushDestinations(ctx context.Context, log *slog.Logger, target GitTarget, defaultTarget bool) (GitTarget, []PushDestination, error) {
	id, repos, err := w.queryRuntime(target)
	if err != nil {
		return GitTarget{}, nil, err
	}
	target = GitTarget{InstanceID: id, Repos: repos}
	destinations := make([]PushDestination, len(repos))
	ctx = context.WithoutCancel(ctx)
	for i := range repos {
		rp := &repos[i]
		g := &git.Checkout{Root: rp.GitRoot, Logger: log}
		gitCtx, cancel := context.WithTimeout(ctx, w.GitTimeout)
		remote, err := g.DefaultRemote(gitCtx)
		if err != nil {
			cancel()
			return GitTarget{}, nil, fmt.Errorf("remote for %s: %w", rp.ContainerPath, err)
		}
		base, err := g.DefaultBranch(gitCtx, remote)
		if err != nil {
			cancel()
			return GitTarget{}, nil, fmt.Errorf("default branch for %s: %w", rp.ContainerPath, err)
		}
		branch := rp.Branch
		if defaultTarget {
			branch = base
		}
		ref := "refs/remotes/" + remote + "/" + base
		baseCommit, err := g.RevParse(gitCtx, ref)
		cancel()
		if err != nil {
			return GitTarget{}, nil, fmt.Errorf("comparison base for %s: %w", rp.ContainerPath, err)
		}
		destinations[i] = PushDestination{Remote: remote, Branch: branch, BaseRef: baseCommit}
	}
	return target, destinations, nil
}

// Push fetches and pins each task commit before checking and pushing it to the
// corresponding destination.
//
// Public diff statistics remain runtime owned, including pending edits.
// Safety issues in any repo block all pushes unless explicitly bypassed.
// No host checkout files or branches change.
func (w *Checkout) Push(ctx context.Context, log *slog.Logger, runtimes *runtime.Router, target GitTarget, destinations []PushDestination, opts PushOptions) (v3.DiffStat, []SafetyIssue, error) {
	ctx = context.WithoutCancel(ctx)
	id, repos, err := w.queryRuntime(target)
	if err != nil {
		return nil, nil, err
	}
	if len(repos) == 0 {
		return nil, nil, errors.New("push requires at least one repository")
	}
	if len(destinations) != len(repos) {
		return nil, nil, errors.New("one push destination is required per repository")
	}
	for _, d := range destinations {
		if d.Remote == "" || d.Branch == "" || d.BaseRef == "" {
			return nil, nil, errors.New("push destination requires remote, branch, and comparison revision")
		}
	}
	region := trace.StartRegion(ctx, "push-fetch")
	fetchCtx, fetchCancel := context.WithTimeout(ctx, w.GitTimeout)
	branches, err := runtimes.Fetch(fetchCtx, id, runtime.FetchOpts{Commit: opts.CommitPending})
	fetchCancel()
	region.End()
	if err != nil {
		return nil, nil, fmt.Errorf("fetch: %w", err)
	}
	// Fetch returns immutable object IDs. Never resolve the mutable runtime
	// tracking ref again, even if another refresh moves it during verification.
	commits := make([]string, len(repos))
	for i := range repos {
		for _, b := range branches {
			if b.RepositoryPath == repos[i].ContainerPath && b.BranchName == repos[i].Branch {
				commits[i] = b.CommitHash
				break
			}
		}
		if commits[i] == "" {
			return nil, nil, fmt.Errorf("fetch did not return commit for %s branch %s", repos[i].ContainerPath, repos[i].Branch)
		}
	}
	// The runtime owns the public task diff: its integration base/upstream
	// and pending edits are distinct from the committed content we push.
	// Preserve that result even when the runtime's best-effort probe fails.
	snapshot, _ := w.DiffStat(ctx, log, runtimes, GitTarget{InstanceID: id, Repos: repos})
	ds := snapshot.DiffStat
	var allIssues []SafetyIssue
	for i, d := range destinations {
		rp := &repos[i]
		g := &git.Checkout{Root: rp.GitRoot, Logger: log}
		checkCtx, cancel := context.WithTimeout(ctx, w.GitTimeout)
		base := d.BaseRef
		stat, err := g.RunGit(checkCtx, "diff", "--numstat", "--stat", base+"..."+commits[i])
		if err != nil {
			cancel()
			return ds, allIssues, fmt.Errorf("diff %s: %w", rp.ContainerPath, err)
		}
		repoDS := ParseDiffNumstat(stat)
		issues, err := CheckSafety(checkCtx, log, rp.GitRoot, commits[i], base, repoDS)
		cancel()
		if err != nil {
			return ds, allIssues, fmt.Errorf("safety check %s: %w", rp.ContainerPath, err)
		}
		allIssues = append(allIssues, issues...)
	}
	if len(allIssues) > 0 && !opts.BypassSafety {
		return ds, allIssues, nil
	}
	for i, d := range destinations {
		rp := &repos[i]
		pushCtx, cancel := context.WithTimeout(ctx, w.PushTimeout)
		err := w.pushWorktree(pushCtx, log, rp.GitRoot, commits[i], d, opts.Force)
		cancel()
		if err != nil {
			return ds, allIssues, fmt.Errorf("push %s to %s/%s: %w", rp.ContainerPath, d.Remote, d.Branch, err)
		}
	}
	return ds, allIssues, nil
}

// DiffContent returns the unified diff for the given repos, optionally filtered
// to a single file path. When there are multiple repos, file paths are prefixed
// with `<repoName>/` so the frontend can distinguish changes from different
// repos. Holds branchMu during diff.
func (w *Checkout) DiffContent(ctx context.Context, log *slog.Logger, runtimes *runtime.Router, target GitTarget, path string) (string, error) {
	log = log.With("repo", w.RelPath)
	id, repos, err := w.queryRuntime(target)
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
		diff, err := runtimes.Diff(ctx, id, i, args...)
		if err != nil {
			log.Warn("diff failed", "repo", repo.ContainerPath, "br", repo.Branch, "err", err)
			continue
		}
		if diff == "" {
			continue
		}
		buf.WriteString(diff)
	}
	return buf.String(), nil
}

// FileDiff returns one committed or uncommitted file patch from a task repository.
func (w *Checkout) FileDiff(ctx context.Context, runtimes *runtime.Router, target GitTarget, repoIdx int, commit, path, originalPath string) (string, error) {
	id, repos, err := w.queryRuntime(target)
	if err != nil {
		return "", err
	}
	if repoIdx < 0 || repoIdx >= len(repos) {
		return "", fmt.Errorf("repo index %d out of range for %d repos", repoIdx, len(repos))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	return runtimes.FileDiff(ctx, id, repoIdx, commit, path, originalPath)
}

// RepositoryStatuses reads branch, upstream commits, and working-tree status
// for every task repository. The returned snapshot is stamped under branchMu;
// callers can publish it later without overwriting a newer applied snapshot.
func (w *Checkout) RepositoryStatuses(ctx context.Context, runtimes *runtime.Router, target GitTarget) (GitSnapshot, error) {
	id, repos, err := w.queryRuntime(target)
	if err != nil {
		return GitSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	statuses := make([]runtime.RepositoryStatus, len(repos))
	for i := range repos {
		statuses[i], err = runtimes.RepositoryStatus(ctx, id, i)
		if err != nil {
			return GitSnapshot{}, fmt.Errorf("git status for %s: %w", repos[i].ContainerPath, err)
		}
	}
	stats, states := summarizeRepositoryStatuses(repos, statuses)
	return GitSnapshot{Read: NewGitRead(id), Target: GitTarget{InstanceID: id, Repos: repos}, DiffStat: stats, RepoStates: states, Statuses: statuses}, nil
}

// DeleteUnmodifiedTaskBranches deletes generated task branches that never diverged from their base.
func (w *Checkout) DeleteUnmodifiedTaskBranches(ctx context.Context, log *slog.Logger, t TaskView) {
	log = log.With("repo", w.RelPath)
	repos := t.GitTarget().Repos
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
		if repo.Branch == baseBranch {
			continue
		}
		checkout := &git.Checkout{Root: dir, Logger: log}
		deleted, err := deleteLocalBranchIfUnmodified(gitCtx, checkout, repo.Branch, baseBranch)
		if err != nil {
			// A checked-out task branch is expected when caic hosts its own repo
			// (the developer's working branch shares the task namespace); it is not
			// a fault, so keep it out of the warning stream.
			if errors.Is(err, errBranchCheckedOut) {
				log.DebugContext(ctx, "delete empty task branch skipped: checked out", "br", repo.Branch)
			} else {
				log.WarnContext(ctx, "delete empty task branch skipped", "br", repo.Branch, "err", err)
			}
			continue
		}
		if deleted {
			log.InfoContext(ctx, "deleted empty task branch", "br", repo.Branch)
		}
	}
}

// NextBranchSeq returns the next unused caic-N sequence number without
// consuming it. Callers must serialize branch allocation across tasks (see
// Manager.allocateBranches) for the value to stay meaningful.
func (w *Checkout) NextBranchSeq() int {
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	return w.nextID
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

// ReserveBranchNumber reserves the exact branch name "caic-n" without touching
// git, advancing the sequence counter past n. Multi-repo tasks use it to share
// one branch name across their repos: n is the highest next sequence number
// across the task's checkouts (see NextBranchSeq), so the name is free in each.
// Must run while branch allocation is serialized across tasks.
func (w *Checkout) ReserveBranchNumber(n int) string {
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	if w.nextID <= n {
		w.nextID = n + 1
	}
	return fmt.Sprintf("caic-%d", n)
}

// AdoptableBranches returns local branches that have a configured upstream.
func (w *Checkout) AdoptableBranches(ctx context.Context, log *slog.Logger) ([]string, error) {
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	gitCtx, gitCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer gitCancel()
	checkout := &git.Checkout{Root: w.Dir, Logger: log.With("repo", w.RelPath)}
	out, err := checkout.RunGit(gitCtx, "for-each-ref", "--format=%(refname:short)%09%(upstream:short)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var branches []string
	for line := range strings.SplitSeq(out, "\n") {
		name, upstream, ok := strings.Cut(line, "\t")
		if ok && name != "" && upstream != "" {
			branches = append(branches, name)
		}
	}
	return branches, nil
}

// FetchAndCreateBranch adopts branch when it is the selected local base;
// otherwise it fetches origin and creates branch from the resolved base.
// It acquires branchMu to serialize git operations across concurrent task setups.
func (w *Checkout) FetchAndCreateBranch(ctx context.Context, log *slog.Logger, t TaskView, branch string) error {
	log = log.With("repo", w.RelPath)
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	gitCtx, gitCancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer gitCancel()
	checkout := &git.Checkout{Root: w.Dir, Logger: log}
	effectiveBase := w.effectiveBaseBranch(t)
	if branch == effectiveBase {
		if _, err := checkout.RevParse(gitCtx, "refs/heads/"+branch); err != nil {
			return fmt.Errorf("adopt local branch: %w", err)
		}
		log.Info("adopting branch", "br", branch)
		return nil
	}
	if err := checkout.Fetch(gitCtx); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	startPoint := "origin/" + effectiveBase
	if t != nil && t.PrimaryBaseBranch() != "" {
		if _, err := checkout.RevParse(gitCtx, "refs/heads/"+effectiveBase); err == nil {
			startPoint = effectiveBase
		}
	}
	if _, err := checkout.RevParse(gitCtx, startPoint); err != nil {
		startPoint = effectiveBase
	}
	log.Info("creating branch", "br", branch, "base", effectiveBase)
	if err := checkout.CreateBranch(gitCtx, branch, startPoint, true); err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	return nil
}

// DiffStat reads combined per-repository numstat. Its snapshot is stamped after
// the probe under branchMu, even when an error accompanies partial statistics.
func (w *Checkout) DiffStat(ctx context.Context, log *slog.Logger, runtimes *runtime.Router, target GitTarget) (GitSnapshot, error) {
	id, repos, err := w.queryRuntime(target)
	if err != nil {
		return GitSnapshot{}, err
	}

	log = log.With("repo", w.RelPath)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	stats, failed, err := w.diffStatLocked(ctx, log, runtimes, id, repos)
	return GitSnapshot{Read: NewGitRead(id), Target: GitTarget{InstanceID: id, Repos: repos}, FailedRepos: failed, DiffStat: stats}, err
}

// TurnSnapshot fetches branch tips and measures branch/turn changes as one
// runtime operation, stamped under the checkout lock like every Git query.
func (w *Checkout) TurnSnapshot(ctx context.Context, log *slog.Logger, runtimes *runtime.Router, target GitTarget, previous []v3.RepositoryCommit) (GitSnapshot, []v3.RepositoryCommit, *v3.ChangeStat, error) {
	id, repos, err := w.queryRuntime(target)
	if err != nil {
		return GitSnapshot{}, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	baseline := make([]runtime.FetchedBranch, len(previous))
	for i, c := range previous {
		baseline[i] = runtime.FetchedBranch{RepositoryPath: c.RepositoryPath, BranchName: c.BranchName, CommitHash: c.CommitHash}
	}
	measured, err := runtimes.TurnSnapshot(ctx, id, baseline)
	snapshot := GitSnapshot{Read: NewGitRead(id), Target: GitTarget{InstanceID: id, Repos: repos}}
	var commits []v3.RepositoryCommit
	change := &v3.ChangeStat{}
	hasBaseline := len(previous) > 0 && len(measured) == len(repos)
	for i := range repos {
		var entry *runtime.TurnRepository
		for j := range measured {
			if measured[j].RepoIndex == i {
				entry = &measured[j]
				break
			}
		}
		if entry == nil || entry.StatusErr != nil {
			snapshot.FailedRepos = append(snapshot.FailedRepos, i)
		} else {
			stats, state := repositorySummary(&repos[i], i, len(repos), &entry.Status)
			snapshot.DiffStat = append(snapshot.DiffStat, stats...)
			snapshot.RepoStates = append(snapshot.RepoStates, state)
		}
		if entry == nil {
			hasBaseline = false
			continue
		}
		for _, c := range entry.Branches {
			commits = append(commits, v3.RepositoryCommit{RepositoryPath: c.RepositoryPath, BranchName: c.BranchName, CommitHash: c.CommitHash})
		}
		if entry.TurnDiff == nil {
			hasBaseline = false
		}
		for _, f := range entry.TurnDiff {
			change.Files++
			change.LinesAdded += f.LinesAdded
			change.LinesDeleted += f.LinesDeleted
			if f.Binary {
				change.BinaryFiles++
			}
		}
	}
	if !hasBaseline {
		change = nil
	}
	if err != nil {
		log.WarnContext(ctx, "turn git snapshot incomplete", "id", id, "err", err)
	}
	return snapshot, commits, change, err
}

// DiffStatAndRepoStates reads branch statistics and compact repository state
// with one log-free probe per repository. The snapshot carries completion order
// and can contain partial results when some probes fail.
func (w *Checkout) DiffStatAndRepoStates(ctx context.Context, log *slog.Logger, runtimes *runtime.Router, target GitTarget) (GitSnapshot, error) {
	id, repos, err := w.queryRuntime(target)
	if err != nil {
		return GitSnapshot{}, err
	}

	log = log.With("repo", w.RelPath)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.GitTimeout)
	defer cancel()
	w.branchMu.Lock()
	defer w.branchMu.Unlock()
	var result v3.DiffStat
	var states []v3.RepoState
	var errs []error
	var failed []int
	for i := range repos {
		repo := &repos[i]
		status, err := runtimes.CompactRepositoryStatus(ctx, id, i)
		if err != nil {
			log.Warn("repository status failed", "repo", repo.ContainerPath, "br", repo.Branch, "err", err)
			errs = append(errs, err)
			failed = append(failed, i)
			continue
		}
		stat, state := repositorySummary(repo, i, len(repos), &status)
		result = append(result, stat...)
		states = append(states, state)
	}
	return GitSnapshot{Read: NewGitRead(id), Target: GitTarget{InstanceID: id, Repos: repos}, FailedRepos: failed, DiffStat: result, RepoStates: states}, errors.Join(errs...)
}

// summarizeRepositoryStatuses derives card statistics from the same snapshots
// used by the diff view. Repos and statuses must have matching lengths.
func summarizeRepositoryStatuses(repos []runtime.Repo, statuses []runtime.RepositoryStatus) (v3.DiffStat, []v3.RepoState) {
	var result v3.DiffStat
	states := make([]v3.RepoState, len(statuses))
	for i := range statuses {
		stat, state := repositorySummary(&repos[i], i, len(repos), &statuses[i])
		result = append(result, stat...)
		states[i] = state
	}
	return result, states
}

func repositorySummary(repo *runtime.Repo, i, repoCount int, status *runtime.RepositoryStatus) (v3.DiffStat, v3.RepoState) {
	result := make(v3.DiffStat, 0, len(status.DiffStat))
	for j := range status.DiffStat {
		stat := status.DiffStat[j]
		path := stat.Path
		if repoCount > 1 {
			path = diffRepoPrefix(repo) + "/" + path
		}
		result = append(result, v3.DiffFileStat{
			Path:         path,
			LinesAdded:   stat.LinesAdded,
			LinesDeleted: stat.LinesDeleted,
			Binary:       stat.Binary,
			OldSize:      stat.OldSize,
			NewSize:      stat.NewSize,
		})
	}
	added, deleted := 0, 0
	for _, stat := range status.DiffStat {
		added += stat.LinesAdded
		deleted += stat.LinesDeleted
	}
	conflicts := 0
	for _, file := range status.Uncommitted {
		if file.IndexStatus == "U" || file.WorktreeStatus == "U" {
			conflicts++
		}
	}
	state := v3.RepoState{
		RepoIndex:        i,
		Branch:           status.Branch,
		Operation:        string(status.Operation),
		Ahead:            status.Ahead,
		Behind:           status.Behind,
		ChangedFiles:     len(status.DiffStat),
		LinesAdded:       added,
		LinesDeleted:     deleted,
		UncommittedFiles: len(status.Uncommitted),
		Conflicts:        conflicts,
	}
	return result, state
}

// queryRuntime validates the captured target and resolves its primary host root.
func (w *Checkout) queryRuntime(target GitTarget) (runtime.ID, []runtime.Repo, error) {
	if target.InstanceID == "" {
		return "", nil, errors.New("task has no runtime instance")
	}
	repos := slices.Clone(target.Repos)
	if len(repos) > 0 && repos[0].GitRoot == "" {
		repos[0].GitRoot = w.Dir
	}
	return target.InstanceID, repos, nil
}

// allocateBranchLocked fetches origin, resolves the start point, and creates
// the task branch. Must be called under branchMu.
func (w *Checkout) allocateBranchLocked(ctx context.Context, log *slog.Logger, baseBranch, preferred string) (string, error) {
	detached := context.WithoutCancel(ctx)
	gitCtx, gitCancel := context.WithTimeout(detached, w.GitTimeout)
	defer gitCancel()
	checkout := &git.Checkout{Root: w.Dir, Logger: log}
	// Fetch so that origin/<base> is up to date.
	if err := checkout.Fetch(gitCtx); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	effectiveBase := baseBranch
	if effectiveBase == "" {
		effectiveBase = w.BaseBranch
	}
	// Prefer the remote tracking ref, but fall back to the local branch when
	// the base branch only exists locally (not yet pushed to origin).
	startPoint := "origin/" + effectiveBase
	if baseBranch != "" {
		if _, err := checkout.RevParse(gitCtx, "refs/heads/"+effectiveBase); err == nil {
			startPoint = effectiveBase
		}
	}
	if _, err := checkout.RevParse(gitCtx, startPoint); err != nil {
		startPoint = effectiveBase
	}
	// Assign a sequential branch name, skipping existing ones. preferred, when
	// set, is tried once first so multi-repo tasks can share one branch name.
	var branch string
	var err error
	triedPreferred := false
	for range 100 {
		if gitCtx.Err() != nil {
			return "", gitCtx.Err()
		}
		if preferred != "" && !triedPreferred {
			triedPreferred = true
			branch = preferred
			// Consume the number even when creation fails, so a later
			// allocation cannot reissue the name.
			if n, ok := caicBranchNumber(preferred); ok && w.nextID <= n {
				w.nextID = n + 1
			}
		} else {
			branch = fmt.Sprintf("caic-%d", w.nextID)
			w.nextID++
		}
		log.Info("creating branch", "br", branch, "base", effectiveBase)
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
// branchMu. It returns an error if any repo's diff fails.
func (w *Checkout) diffStatLocked(ctx context.Context, log *slog.Logger, runtimes *runtime.Router, id runtime.ID, repos []runtime.Repo) (v3.DiffStat, []int, error) {
	var result v3.DiffStat
	var errs []error
	var failed []int
	for i := range repos {
		repo := &repos[i]
		numstat, err := runtimes.Diff(ctx, id, i, "--numstat", "--stat")
		if err != nil {
			log.Warn("diff numstat failed", "repo", repo.ContainerPath, "br", repo.Branch, "err", err)
			errs = append(errs, err)
			failed = append(failed, i)
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
	return result, failed, errors.Join(errs...)
}

// pushWorktree exclusively claims an idle checkout or creates a detached one.
// Completed pushes return one slot to the persistent idle pool; interrupted
// operations remain quarantined across server restarts.
func (w *Checkout) pushWorktree(ctx context.Context, log *slog.Logger, root, commit string, d PushDestination, force bool) error {
	g := &git.Checkout{Root: root, Logger: log}
	common, err := pushCommonDir(ctx, g)
	if err != nil {
		return err
	}
	dir, reused, err := acquirePushWorktree(ctx, w.PushDir, common)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "checkout")
	reusable := false
	err = awaitPushCommand(ctx, func() error {
		checkout := &git.Checkout{Root: path, Logger: log}
		if reused {
			if err := validateReusablePushWorktree(ctx, g, common, dir); err != nil {
				return err
			}
			// Only this caic-owned checkout is reset. Keep ignored verification
			// caches while removing leftovers, including nested untracked repos.
			if _, err := checkout.RunGit(ctx, "clean", "-ffd"); err != nil {
				return err
			}
			if _, err := checkout.RunGit(ctx, "checkout", "--detach", "--force", commit); err != nil {
				return err
			}
			if _, err := checkout.RunGit(ctx, "clean", "-ffd"); err != nil {
				return err
			}
		} else {
			if _, err := g.RunGit(ctx, "worktree", "add", "--detach", path, commit); err != nil {
				return fmt.Errorf("create push worktree: %w", err)
			}
		}
		reusable = true
		// Preserve native Git transport and lock configuration, including the
		// user's SSH configuration. The outer deadline also bounds slow hooks.
		return checkout.PushRef(ctx, d.Remote, "HEAD", d.Branch, force)
	})
	if ctx.Err() != nil {
		return errors.Join(err, retainPushWorktree(ctx, log, root, dir))
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, w.GitTimeout)
	defer cancel()
	cleanupErr := releasePushWorktree(cleanupCtx, g, w.PushDir, common, dir, reusable)
	if cleanupErr != nil {
		return errors.Join(err, cleanupErr, retainPushWorktree(ctx, log, root, dir))
	}
	return err
}

func (w *Checkout) reportRetainedPushWorktrees(ctx context.Context, log *slog.Logger, root string) error {
	common, err := pushCommonDir(ctx, &git.Checkout{Root: root, Logger: log})
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(w.PushDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "operation-") {
			continue
		}
		// Never reclaim abandoned operations automatically: neither a dead
		// host nor a released lock establishes hook-process completion.
		dir := filepath.Join(w.PushDir, e.Name())
		owner, err := readPushMarker(dir, "owner-v1")
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errInvalidPushMarker) {
			continue
		}
		if err != nil {
			return err
		}
		if owner == common || owner == root {
			if _, err := readPushIdleSince(dir); err == nil {
				continue
			}
			_ = retainPushWorktree(ctx, log, root, dir)
		}
	}
	return nil
}

// maxBranchSeqNum finds the highest sequence number N among all local and
// remote branches matching "caic-N", plus liveBranches (branch names taken
// from currently running containers for this repo). liveBranches covers
// branches that a container already holds but that never made it into dir's
// git refs — e.g. a task whose container launch failed after the branch
// name was decided but before "git branch" ran. Relying on git alone would
// let the next allocation reissue that same name, producing two containers
// mapped to the same repo+branch (see checkRepoOverlap in the md package).
// Returns -1 if no matching name exists.
func maxBranchSeqNum(ctx context.Context, log *slog.Logger, dir string, liveBranches []string) (int, error) {
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
	for _, name := range liveBranches {
		if n, ok := caicBranchNumber(name); ok && n > highest {
			highest = n
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
