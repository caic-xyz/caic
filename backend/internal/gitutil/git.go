// Package gitutil provides git operations for branch management and pushing.
package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultBranch returns the default branch of the origin remote (e.g. "main"
// or "master"). It reads refs/remotes/origin/HEAD which is set by the initial
// clone. If the symbolic ref is missing, it probes for common default branch
// names locally.
func DefaultBranch(ctx context.Context, dir string) (string, error) {
	// Fast path: read the symbolic ref set by clone.
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		// Output is e.g. "refs/remotes/origin/main\n".
		ref := strings.TrimSpace(string(out))
		const prefix = "refs/remotes/origin/"
		if after, ok := strings.CutPrefix(ref, prefix); ok {
			return after, nil
		}
	}

	// Fallback: check if common default branch names exist on origin.
	for _, name := range []string{"main", "master"} {
		cmd = exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", "origin/"+name) //nolint:gosec // name is from a constant list.
		cmd.Dir = dir
		if cmd.Run() == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not determine default branch for origin in %s", dir)
}

// Fetch fetches the latest refs from origin.
func Fetch(ctx context.Context, dir string) error {
	slog.Info("git fetch", "dir", dir)
	cmd := exec.CommandContext(ctx, "git", "fetch", "origin")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch origin: %w: %s", err, stderr.String())
	}
	return nil
}

// CreateBranch creates a new branch from startPoint and checks it out.
func CreateBranch(ctx context.Context, dir, name, startPoint string) error {
	slog.Info("git create branch", "branch", name, "startPoint", startPoint)
	cmd := exec.CommandContext(ctx, "git", "checkout", "-b", name, startPoint) //nolint:gosec // args are server-controlled branch names
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git checkout -b %s: %w: %s", name, err, stderr.String())
	}
	return nil
}

// CheckoutBranch switches to an existing branch.
func CheckoutBranch(ctx context.Context, dir, name string) error {
	slog.Info("git checkout", "branch", name)
	cmd := exec.CommandContext(ctx, "git", "checkout", name) //nolint:gosec // args are server-controlled branch names
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", name, err, stderr.String())
	}
	return nil
}

// MaxBranchSeqNum finds the highest sequence number N among remote branches
// matching "caic/wN" across all remotes. Returns -1 if no matching branches
// exist.
func MaxBranchSeqNum(ctx context.Context, dir string) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "branch", "-r", "--format=%(refname:short)")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return -1, fmt.Errorf("git branch -r: %w", err)
	}
	highest := -1
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		// Match "<remote>/caic/wN" for any remote name.
		_, after, ok := strings.Cut(line, "/caic/w")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(after)
		if err != nil {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	return highest, nil
}

// RemoteOriginURL returns the URL of the "origin" remote, or "" if
// unavailable.
func RemoteOriginURL(ctx context.Context, dir string) string {
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// RemoteToHTTPS converts a git remote URL to an HTTPS browse URL.
// SSH (git@host:owner/repo.git), ssh:// and https:// with .git suffix are
// normalised. Unrecognised formats are returned as-is.
func RemoteToHTTPS(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// git@host:owner/repo.git → https://host/owner/repo
	if after, ok := strings.CutPrefix(raw, "git@"); ok {
		if i := strings.IndexByte(after, ':'); i > 0 {
			host := after[:i]
			path := strings.TrimSuffix(after[i+1:], ".git")
			return "https://" + host + "/" + path
		}
	}
	// ssh://git@host/owner/repo.git → https://host/owner/repo
	if after, ok := strings.CutPrefix(raw, "ssh://"); ok {
		// Strip user@ if present.
		if i := strings.IndexByte(after, '@'); i >= 0 {
			after = after[i+1:]
		}
		return "https://" + strings.TrimSuffix(after, ".git")
	}
	// https://host/owner/repo.git → strip .git
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		return strings.TrimSuffix(raw, ".git")
	}
	return raw
}

// PushRef pushes a local ref to the origin remote as the given branch.
// ref can be a remote-tracking ref (e.g. "container/branch"), a branch
// name, or any valid git ref. When force is true, --force is passed.
func PushRef(ctx context.Context, dir, ref, branch string, force bool) error {
	slog.Info("git push", "ref", ref, "branch", branch, "force", force)
	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "origin", ref+":refs/heads/"+branch)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push origin %s:%s: %w: %s", ref, branch, err, stderr.String())
	}
	return nil
}

// SquashOnto creates a single squash commit of sourceRef's tree on top of
// origin/<targetBranch> and pushes it. Uses plumbing commands only — no
// working-tree checkout needed. The push is non-force so it fails with a
// non-fast-forward error if origin/<targetBranch> has advanced since fetch.
func SquashOnto(ctx context.Context, dir, sourceRef, targetBranch, message string) error {
	slog.Info("squash onto", "sourceRef", sourceRef, "targetBranch", targetBranch)

	// 1. Fetch so origin/<targetBranch> is up to date.
	if err := Fetch(ctx, dir); err != nil {
		return err
	}

	// 2. Create the squash commit: sourceRef's tree, parented on origin/<targetBranch>.
	target := "origin/" + targetBranch
	commitTreeArgs := []string{"commit-tree", "-p", target, "-m", message, sourceRef + "^{tree}"}
	cmd := exec.CommandContext(ctx, "git", commitTreeArgs...) //nolint:gosec // args are server-controlled refs
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git commit-tree: %w: %s", err, stderr.String())
	}
	newCommit := strings.TrimSpace(string(out))

	// 3. Push the new commit to origin/<targetBranch> (non-force).
	return PushRef(ctx, dir, newCommit, targetBranch, false)
}

// RevParse resolves a git ref to its full SHA-1 hash.
func RevParse(ctx context.Context, dir, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", ref) //nolint:gosec // ref is server-controlled
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w: %s", ref, err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// IsReachable reports whether commit is an ancestor of (or equal to) any ref
// in refs/heads/ or refs/remotes/origin/. Container remote-tracking refs
// (refs/remotes/<container>/*) are excluded by construction.
func IsReachable(ctx context.Context, dir, commit string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "for-each-ref", //nolint:gosec // commit is server-controlled
		"--contains", commit,
		"--format=%(refname)",
		"refs/heads/",
		"refs/remotes/origin/",
	)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git for-each-ref --contains %s: %w: %s", commit, err, stderr.String())
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// CreateBranchAt creates a local branch pointing at commit without checking it
// out. This does not touch the working tree or index.
func CreateBranchAt(ctx context.Context, dir, name, commit string) error {
	slog.Info("git create branch at", "branch", name, "commit", commit)
	cmd := exec.CommandContext(ctx, "git", "branch", name, commit) //nolint:gosec // args are server-controlled
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git branch %s %s: %w: %s", name, commit, err, stderr.String())
	}
	return nil
}

// DiscoverRepos recursively walks root up to maxDepth levels, returning
// absolute paths of directories containing a .git subdirectory. Hidden
// directories (prefix ".") are skipped. Recursion stops once .git is found.
func DiscoverRepos(root string, maxDepth int) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var repos []string
	err = discoverRepos(root, maxDepth, &repos)
	return repos, err
}

func discoverRepos(dir string, depth int, repos *[]string) error {
	if depth < 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", dir, err)
	}
	// Check if this directory contains .git.
	for _, e := range entries {
		if e.Name() == ".git" {
			*repos = append(*repos, dir)
			return nil // Don't recurse into repos.
		}
	}
	// Recurse into subdirectories.
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := discoverRepos(filepath.Join(dir, e.Name()), depth-1, repos); err != nil {
			// Skip directories we can't read.
			continue
		}
	}
	return nil
}
