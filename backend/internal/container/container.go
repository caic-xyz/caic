// Package container wraps md container lifecycle operations.
package container

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/maruel/md"
)

// New creates an md.Client for container operations.
func New() (*md.Client, error) {
	c, err := md.New()
	if err != nil {
		return nil, err
	}
	c.W = os.Stderr
	return c, nil
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

// Event represents a Docker container lifecycle event.
type Event struct {
	Name string // Container name from docker.
}

// dockerEvent is the JSON structure emitted by `docker events --format '{{json .}}'`.
type dockerEvent struct {
	Actor struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// WatchEvents monitors Docker container die events filtered by a label.
// It runs `docker events --filter event=die --filter label=<labelFilter>`
// and sends a Event for each death. The caller handles reconnection
// on stream errors. The channel is closed when the context is cancelled or
// the docker events process exits.
func WatchEvents(ctx context.Context, labelFilter string) (<-chan Event, error) {
	cmd := exec.CommandContext(ctx, "docker", "events", //nolint:gosec // labelFilter is a trusted constant
		"--filter", "event=die",
		"--filter", "label="+labelFilter,
		"--format", "{{json .}}",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("docker events stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker events start: %w", err)
	}
	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		defer func() { _ = cmd.Wait() }()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var ev dockerEvent
			if json.Unmarshal(scanner.Bytes(), &ev) != nil {
				continue
			}
			name := ev.Actor.Attributes["name"]
			if name == "" {
				continue
			}
			select {
			case ch <- Event{Name: name}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// BranchFromContainer derives the git branch name from a container name by
// stripping the "md-<repo>-" prefix and restoring the "caic/" prefix that was
// flattened to "caic-" by md.
func BranchFromContainer(containerName, repoName string) (string, bool) {
	prefix := "md-" + repoName + "-"
	if !strings.HasPrefix(containerName, prefix) {
		return "", false
	}
	slug := containerName[len(prefix):]
	// md replaces "/" with "-", so "caic/foo" becomes "caic-foo".
	if strings.HasPrefix(slug, "caic-") {
		return "caic/" + slug[len("caic-"):], true
	}
	return slug, true
}
