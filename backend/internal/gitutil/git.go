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
// "wmao/wN". Returns -1 if no matching branches exist.
func MaxBranchSeqNum(ctx context.Context, dir string) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "branch", "--list", "wmao/w*", "--format=%(refname:short)")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return -1, fmt.Errorf("git branch --list: %w", err)
	}
	highest := -1
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "wmao/w") {
			continue
		}
		numStr := line[len("wmao/w"):]
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
