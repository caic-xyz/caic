// Package gitutil provides git operations for branch management and pushing.
package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// CurrentBranch returns the current git branch name.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

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
	cmd := exec.CommandContext(ctx, "git", "checkout", "-b", name, startPoint)
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
	cmd := exec.CommandContext(ctx, "git", "checkout", name)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", name, err, stderr.String())
	}
	return nil
}

// Push pushes the branch to origin. Returns an error if it fails.
func Push(ctx context.Context, dir, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "push", "origin", branch)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push origin %s: %w: %s", branch, err, stderr.String())
	}
	return nil
}

// RepoName returns the repository directory name (last component of the
// top-level path).
func RepoName(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	top := strings.TrimSpace(string(out))
	parts := strings.Split(top, "/")
	return parts[len(parts)-1], nil
}

// MaxBranchSeqNum finds the highest sequence number N among branches matching
// "caic/wN". Returns -1 if no matching branches exist.
func MaxBranchSeqNum(ctx context.Context, dir string) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "branch", "--list", "caic/w*", "--format=%(refname:short)")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return -1, fmt.Errorf("git branch --list: %w", err)
	}
	highest := -1
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "caic/w") {
			continue
		}
		numStr := line[len("caic/w"):]
		n, err := strconv.Atoi(numStr)
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
