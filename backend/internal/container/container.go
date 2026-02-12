// Package container wraps md container lifecycle operations.
package container

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Entry represents a container returned by listing.
type Entry struct {
	Name   string
	Status string
}

// Ops abstracts md container lifecycle operations.
type Ops interface {
	Start(ctx context.Context, dir, branch string, labels []string) (name string, err error)
	Diff(ctx context.Context, dir, branch string, args ...string) (string, error)
	Pull(ctx context.Context, dir, branch string) error
	Push(ctx context.Context, dir, branch string) error
	Kill(ctx context.Context, dir, branch string) error
	List(ctx context.Context) ([]Entry, error)
}

// LabelValue returns the value of a Docker label on a running container.
//
// Returns empty string if the label is not set.
func LabelValue(ctx context.Context, containerName, label string) (string, error) {
	format := fmt.Sprintf("{{index .Config.Labels %q}}", label)
	cmd := exec.CommandContext(ctx, "docker", "inspect", containerName, "--format", format) //nolint:gosec // containerName and format are not user-controlled.
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect label %q on %s: %w", label, containerName, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "<no value>" {
		return "", nil
	}
	return v, nil
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
