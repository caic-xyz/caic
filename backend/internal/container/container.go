// Package container wraps md CLI operations for container lifecycle management.
package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
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

// Start creates and starts an md container for the given branch.
// It does not SSH into it (--no-ssh).
func Start(ctx context.Context, branch string) (string, error) {
	// md start --no-ssh will create the container and return.
	// The container name is md-<repo>-<branch>.
	cmd := exec.CommandContext(ctx, "md", "start", "--no-ssh")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("md start: %w: %s", err, stderr.String())
	}
	// Derive the container name. md uses the repo name from the current
	// directory and the current branch.
	name, err := containerName(ctx)
	if err != nil {
		return "", err
	}
	return name, nil
}

// Diff runs `md diff` and returns the diff output.
func Diff(ctx context.Context, args ...string) (string, error) {
	cmdArgs := append([]string{"diff"}, args...)
	cmd := exec.CommandContext(ctx, "md", cmdArgs...) //nolint:gosec // args are not user-controlled.
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("md diff: %w", err)
	}
	return string(out), nil
}

// Pull pulls changes from the container to the local branch.
func Pull(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "md", "pull")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("md pull: %w: %s", err, stderr.String())
	}
	return nil
}

// Kill stops and removes the container.
func Kill(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "md", "kill")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("md kill: %w: %s", err, stderr.String())
	}
	return nil
}

// containerName returns the md container name for the current repo+branch.
func containerName(ctx context.Context) (string, error) {
	entries, err := List(ctx)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", errors.New("no md container found in md list output")
	}
	// Take the most recently created one (last in the list).
	return entries[len(entries)-1].Name, nil
}
