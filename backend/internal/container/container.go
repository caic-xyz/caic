// Package container wraps md CLI operations for container lifecycle management.
package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Entry represents a container returned by md list.
type Entry struct {
	Name   string
	Status string
}

// List returns all md containers.
func List(ctx context.Context) ([]Entry, error) {
	cmd := exec.CommandContext(ctx, "md", "list")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("md list: %w", err)
	}
	return parseList(string(out)), nil
}

// parseList parses md list output into entries.
func parseList(raw string) []Entry {
	var entries []Entry
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "md-") {
			entries = append(entries, Entry{Name: fields[0], Status: fields[1]})
		}
	}
	return entries
}

// BranchFromContainer derives the git branch name from a container name by
// stripping the "md-<repo>-" prefix and restoring the "wmao/" prefix that was
// flattened to "wmao-" by md.
func BranchFromContainer(containerName, repoName string) (string, bool) {
	prefix := "md-" + repoName + "-"
	if !strings.HasPrefix(containerName, prefix) {
		return "", false
	}
	slug := containerName[len(prefix):]
	// md replaces "/" with "-", so "wmao/foo" becomes "wmao-foo".
	if strings.HasPrefix(slug, "wmao-") {
		return "wmao/" + slug[len("wmao-"):], true
	}
	return slug, true
}

// Start creates and starts an md container for the current branch.
// It does not SSH into it (--no-ssh).
func Start(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "md", "start", "--no-ssh")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("md start: %w: %s", err, stderr.String())
	}
	name, err := containerName(ctx, dir)
	if err != nil {
		return "", err
	}
	return name, nil
}

// Diff runs `md diff` and returns the diff output.
func Diff(ctx context.Context, dir string, args ...string) (string, error) {
	cmdArgs := append([]string{"diff"}, args...)
	cmd := exec.CommandContext(ctx, "md", cmdArgs...) //nolint:gosec // args are not user-controlled.
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("md diff: %w", err)
	}
	return string(out), nil
}

// Pull pulls changes from the container to the local branch.
func Pull(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "md", "pull")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("md pull: %w: %s", err, stderr.String())
	}
	return nil
}

// Push pushes local changes into the container.
func Push(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "md", "push")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("md push: %w: %s", err, stderr.String())
	}
	return nil
}

// Kill stops and removes the container.
func Kill(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "md", "kill")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("md kill: %w: %s", err, stderr.String())
	}
	return nil
}

// containerName returns the md container name for the current repo+branch by
// filtering the global container list to entries matching the repo derived
// from dir.
func containerName(ctx context.Context, dir string) (string, error) {
	entries, err := List(ctx)
	if err != nil {
		return "", err
	}
	repo := filepath.Base(dir)
	prefix := "md-" + repo + "-"
	var match string
	for _, e := range entries {
		if strings.HasPrefix(e.Name, prefix) {
			match = e.Name
		}
	}
	if match == "" {
		return "", errors.New("no md container found for repo " + repo)
	}
	return match, nil
}
