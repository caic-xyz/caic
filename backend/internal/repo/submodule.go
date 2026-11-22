// Resolve pinned submodule graphs from host object stores for child-first publication.

package repo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// submodulePushTargets walks the pinned commits, independently of current
// checkout topology. An unchanged Gitlink and source URL already rooted in a
// cached published remote tree need no republication. Every changed dependency
// must have its objects available locally before any reference is published.
// Children always publish under the mapped task branch, including when the
// child is attached to main or the root is syncing to its default branch.
func submodulePushTargets(ctx context.Context, log *slog.Logger, roots []runtime.Repo, destinations []PushDestination, commits []string, fetched []runtime.FetchedBranch) ([]runtime.Repo, []PushDestination, []string, error) {
	repos := slices.Clone(roots)
	destinations = slices.Clone(destinations)
	commits = slices.Clone(commits)
	for i := range destinations {
		destinations[i].repoIdx = i
	}
	rootGitDirs := make([]string, len(roots))
	for i := 0; i < len(repos); i++ {
		parent := repos[i]
		g := hostRepository(&parent, log)
		gitDir := parent.GitDir
		if gitDir == "" {
			var err error
			gitDir, err = g.RunGit(ctx, "rev-parse", "--absolute-git-dir")
			if err != nil {
				return nil, nil, nil, fmt.Errorf("submodule graph metadata for %s: %w", parent.ContainerPath, err)
			}
		}
		objectStore := &git.ObjectStore{GitDir: gitDir}
		links, declarations, err := submoduleTree(ctx, objectStore, commits[i])
		if err != nil {
			return nil, nil, nil, fmt.Errorf("submodule graph for %s: %w", parent.ContainerPath, err)
		}
		baseline, oldDeclarations, err := submoduleTree(ctx, objectStore, destinations[i].BaseRef)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("published submodule baseline for %s: %w", parent.ContainerPath, err)
		}
		for _, link := range links {
			declaration, declared := submoduleDeclarationAt(declarations, link.Path)
			oldDeclaration, previouslyDeclared := submoduleDeclarationAt(oldDeclarations, link.Path)
			if previouslyDeclared && (!declared || declaration.URL != oldDeclaration.URL) {
				return nil, nil, nil, fmt.Errorf("submodule %s/%s changes source URL; source migrations require synchronizing the module destination separately", parent.ContainerPath, link.Path)
			}
			published := false
			for _, old := range baseline {
				if old.Path == link.Path && old.Commit == link.Commit {
					published = true
					break
				}
			}
			if published && declared == previouslyDeclared {
				continue
			}
			containerPath := path.Join(md.ResolveContainerPath(parent.ContainerPath), link.Path)
			store, err := pinnedSubmoduleStore(ctx, g, declaration, containerPath, link.Commit, fetched)
			if err != nil {
				return nil, nil, nil, err
			}
			child := runtime.Repo{GitRoot: filepath.Join(parent.GitRoot, filepath.FromSlash(link.Path)), GitDir: store, ContainerPath: containerPath, Branch: parent.Branch}
			childGit := hostRepository(&child, log)
			remote, err := childGit.DefaultRemote(ctx)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("submodule %s remote: %w", containerPath, err)
			}
			base, err := childGit.DefaultBranch(ctx, remote)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("submodule %s published baseline: %w", containerPath, err)
			}
			baseCommit, err := childGit.RevParse(ctx, "refs/remotes/"+remote+"/"+base)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("submodule %s published baseline: %w", containerPath, err)
			}
			rootIdx := destinations[i].repoIdx
			if rootGitDirs[rootIdx] == "" {
				rootGitDirs[rootIdx], err = hostRepository(&repos[rootIdx], log).RunGit(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
				if err != nil {
					return nil, nil, nil, fmt.Errorf("submodule %s mapped repository metadata: %w", containerPath, err)
				}
			}
			// md retains module Git directories under the same paths on both
			// sides. Select that identity even if the worktree moved or vanished.
			moduleGitDir := ""
			rel, err := filepath.Rel(filepath.Join(rootGitDirs[rootIdx], "modules"), store)
			if err == nil && rel != "." && filepath.IsLocal(rel) {
				moduleGitDir = filepath.ToSlash(rel)
			}
			modulePath := link.Path
			if parentModule := destinations[i].submodule; parentModule != nil {
				modulePath = path.Join(parentModule.Path, modulePath)
			}
			repos = append(repos, child)
			destinations = append(destinations, PushDestination{
				Remote: remote, Branch: child.Branch, BaseRef: baseCommit, repoIdx: rootIdx,
				submodule: &runtime.PushSubmodule{Path: modulePath, GitDir: moduleGitDir},
			})
			commits = append(commits, link.Commit)
		}
	}
	if len(repos) > len(roots) {
		if err := checkSubmodulePushDestinations(ctx, log, repos, destinations, commits); err != nil {
			return nil, nil, nil, err
		}
	}
	return repos, destinations, commits, nil
}

type remoteBranch struct{ url, branch string }

// Reject conflicting pins before publication. The task branch must keep every
// Gitlink reachable even when the same module remote appears at multiple paths.
func checkSubmodulePushDestinations(ctx context.Context, log *slog.Logger, repos []runtime.Repo, destinations []PushDestination, commits []string) error {
	branchCounts := make(map[string]int)
	for i := range destinations {
		branchCounts[destinations[i].Branch]++
	}
	seen := make(map[remoteBranch]int)
	for i := range destinations {
		d := &destinations[i]
		if branchCounts[d.Branch] < 2 {
			continue
		}
		g := hostRepository(&repos[i], log)
		cmd := g.Command(ctx, "remote", "get-url", "--push", "--all", d.Remote)
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("read push destination for %s: %w", repos[i].ContainerPath, err)
		}
		for url := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
			if !filepath.IsAbs(url) && !strings.Contains(url, ":") {
				url = filepath.Join(cmd.Dir, url)
			}
			key := remoteBranch{url: url, branch: d.Branch}
			if prior, ok := seen[key]; ok && commits[prior] != commits[i] {
				return fmt.Errorf("%s and %s pin different commits for the same remote branch %s", repos[prior].ContainerPath, repos[i].ContainerPath, d.Branch)
			}
			seen[key] = i
		}
	}
	return nil
}

// Prefer the pinned declaration's identity over today's path mapping. A path
// can be reused under a different name while the historic store still retains
// the required unpublished commit. Container-only stores remain valid fallbacks.
func pinnedSubmoduleStore(ctx context.Context, parent gitRepository, declaration git.SubmoduleDeclaration, containerPath, commit string, fetched []runtime.FetchedBranch) (string, error) {
	var candidates []string
	if declaration.Name != "" && declaration.Name != "." && filepath.IsLocal(filepath.FromSlash(declaration.Name)) {
		common, err := parent.RunGit(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return "", fmt.Errorf("submodule %s metadata: %w", containerPath, err)
		}
		candidates = append(candidates, filepath.Join(common, "modules", filepath.FromSlash(declaration.Name)))
	}
	for _, child := range fetched {
		if md.ResolveContainerPath(child.RepositoryPath) == containerPath && child.GitDir != "" && !slices.Contains(candidates, child.GitDir) {
			candidates = append(candidates, child.GitDir)
		}
	}
	var failures []error
	for _, store := range candidates {
		candidate := &git.ObjectStore{GitDir: store}
		if _, err := candidate.RevParse(ctx, commit+"^{commit}"); err == nil {
			return store, nil
		} else {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("submodule %s object lookup: %w", containerPath, ctx.Err())
		}
	}
	return "", fmt.Errorf("submodule %s has no available store containing pinned commit %s: %w", containerPath, commit, errors.Join(append(failures, errors.New("required local module objects unavailable"))...))
}

// submoduleTree reads the pinned graph and rejects ambiguous publication paths.
func submoduleTree(ctx context.Context, store *git.ObjectStore, commit string) ([]git.Gitlink, []git.SubmoduleDeclaration, error) {
	tree, err := store.ReadGitlinkTree(ctx, commit)
	if err != nil {
		return nil, nil, err
	}
	if tree.GitmodulesBlob == "" {
		return tree.Gitlinks, nil, nil
	}
	declarations, err := store.ReadSubmoduleDeclarations(ctx, tree.GitmodulesBlob)
	if err != nil {
		return tree.Gitlinks, nil, err
	}
	paths := make(map[string]struct{}, len(declarations))
	for _, declaration := range declarations {
		if declaration.Path == "" {
			continue
		}
		if _, ok := paths[declaration.Path]; ok {
			return nil, nil, fmt.Errorf("multiple pinned submodules declare path %q", declaration.Path)
		}
		paths[declaration.Path] = struct{}{}
	}
	return tree.Gitlinks, declarations, nil
}

func submoduleDeclarationAt(declarations []git.SubmoduleDeclaration, modulePath string) (git.SubmoduleDeclaration, bool) {
	for _, declaration := range declarations {
		if declaration.Path == modulePath {
			return declaration, true
		}
	}
	return git.SubmoduleDeclaration{}, false
}

// gitRepository is the ref and object scope used for publication.
type gitRepository interface {
	Command(ctx context.Context, args ...string) *exec.Cmd
	RunGit(ctx context.Context, args ...string) (string, error)
	RevParse(ctx context.Context, ref string) (string, error)
	DefaultRemote(ctx context.Context) (string, error)
	DefaultBranch(ctx context.Context, remote string) (string, error)
}

// hostRepository keeps metadata-only child operations independent of missing
// worktrees, including when several enclosing checkout directories are absent.
func hostRepository(repo *runtime.Repo, log *slog.Logger) gitRepository {
	if repo.GitDir != "" {
		return &git.ObjectStore{GitDir: repo.GitDir}
	}
	return &git.Checkout{Root: repo.GitRoot, Logger: log}
}
