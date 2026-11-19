// Repository fixtures shared by packages that need real Git objects.

package gittest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InitClonedRepo initialises a bare remote at bare and clones it at clone with
// one commit on baseBranch. The clone's origin points at the bare remote, so
// callers can push, fetch, and use the bare path as a submodule source.
//
// The bare remote advertises baseBranch explicitly: Git initialises a bare
// repository at the host's default branch name, which need not be baseBranch
// and which a clone of an unborn branch cannot check out.
func InitClonedRepo(ctx context.Context, bare, clone, baseBranch string) error {
	for _, dir := range []string{filepath.Dir(bare), filepath.Dir(clone)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	steps := [][]string{
		{"init", "--bare", "--initial-branch=" + baseBranch, bare},
		{"init", clone},
		{"-C", clone, "config", "user.name", "Test"},
		{"-C", clone, "config", "user.email", "test@test.com"},
		{"-C", clone, "checkout", "-b", baseBranch},
	}
	if err := runAll(ctx, steps); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("hello\n"), 0o600); err != nil {
		return err
	}
	return runAll(ctx, [][]string{
		{"-C", clone, "add", "."},
		{"-C", clone, "commit", "-m", "init"},
		{"-C", clone, "remote", "add", "origin", bare},
		{"-C", clone, "push", "-u", "origin", baseBranch},
	})
}

// runAll runs each Git command in order and stops at the first failure.
func runAll(ctx context.Context, commands [][]string) error {
	for _, args := range commands {
		if err := runGit(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

// runGit runs one Git command, reporting its combined output on failure.
func runGit(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput() //nolint:gosec // fixture-owned Git commands
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}
