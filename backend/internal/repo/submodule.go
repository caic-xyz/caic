// Resolve pinned submodule graphs from host object stores for child-first publication.

package repo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
		links, declarations, err := submoduleTree(ctx, g, commits[i])
		if err != nil {
			return nil, nil, nil, fmt.Errorf("submodule graph for %s: %w", parent.ContainerPath, err)
		}
		baseline, oldDeclarations, err := submoduleTree(ctx, g, destinations[i].BaseRef)
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
func pinnedSubmoduleStore(ctx context.Context, parent gitRepository, declaration pinnedSubmodule, containerPath, commit string, fetched []runtime.FetchedBranch) (string, error) {
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

type pinnedGitlink struct{ Path, Commit string }
type pinnedSubmodule struct{ Name, Path, URL string }

// submoduleTree scans a tree with bounded per-record memory, retaining only its
// Gitlinks and small .gitmodules declaration set rather than every tracked path.
func submoduleTree(ctx context.Context, g gitRepository, commit string) ([]pinnedGitlink, []pinnedSubmodule, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := g.Command(commandCtx, "ls-tree", "-r", "-z", commit)
	cmd.WaitDelay = time.Second
	// Repository-controlled diagnostics can contain source or credentials.
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	scanner.Split(func(data []byte, eof bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0); i >= 0 {
			return i + 1, data[:i], nil
		}
		if eof && len(data) > 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	})
	var links []pinnedGitlink
	hasDeclarations := false
	for scanner.Scan() {
		metadata, file, ok := strings.Cut(scanner.Text(), "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			cancel()
			return nil, nil, errors.Join(errors.New("invalid Git tree record"), cmd.Wait())
		}
		if fields[0] == "160000" {
			links = append(links, pinnedGitlink{Path: file, Commit: fields[2]})
		}
		if file == ".gitmodules" && fields[1] == "blob" {
			hasDeclarations = true
		}
	}
	if err := scanner.Err(); err != nil {
		cancel()
		return nil, nil, errors.Join(err, cmd.Wait())
	}
	if err := cmd.Wait(); err != nil {
		return nil, nil, fmt.Errorf("read pinned Git tree: %w", err)
	}
	if !hasDeclarations {
		return links, nil, nil
	}
	declarations, err := readSubmoduleDeclarations(ctx, g, commit)
	return links, declarations, err
}

func readSubmoduleDeclarations(ctx context.Context, g gitRepository, commit string) ([]pinnedSubmodule, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := g.Command(commandCtx, "config", "--no-includes", "--null", "--blob="+commit+":.gitmodules", "--list")
	cmd.WaitDelay = time.Second
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Declarations are needed together to associate names, paths and URLs.
	const maxDeclarations = 1 << 20
	data, err := io.ReadAll(io.LimitReader(stdout, maxDeclarations+1))
	if err != nil || len(data) > maxDeclarations {
		cancel()
		return nil, errors.Join(errors.New("cannot read pinned submodule declarations within 1 MiB"), err, cmd.Wait())
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("read pinned .gitmodules: %w", err)
	}
	var declarations []pinnedSubmodule
	for record := range strings.SplitSeq(string(data), "\x00") {
		key, value, ok := strings.Cut(record, "\n")
		if !ok {
			continue
		}
		key, ok = strings.CutPrefix(key, "submodule.")
		if !ok {
			continue
		}
		name, field, ok := strings.CutLast(key, ".")
		if !ok || field != "path" && field != "url" {
			continue
		}
		idx := slices.IndexFunc(declarations, func(d pinnedSubmodule) bool { return d.Name == name })
		if idx < 0 {
			declarations = append(declarations, pinnedSubmodule{Name: name})
			idx = len(declarations) - 1
		}
		if field == "path" {
			declarations[idx].Path = value
		} else {
			declarations[idx].URL = value
		}
	}
	for i := range declarations {
		for j := range i {
			if declarations[i].Path != "" && declarations[i].Path == declarations[j].Path {
				return nil, fmt.Errorf("multiple pinned submodules declare path %q", declarations[i].Path)
			}
		}
	}
	return declarations, nil
}

func submoduleDeclarationAt(declarations []pinnedSubmodule, modulePath string) (pinnedSubmodule, bool) {
	for _, declaration := range declarations {
		if declaration.Path == modulePath {
			return declaration, true
		}
	}
	return pinnedSubmodule{}, false
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
