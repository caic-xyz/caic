// Package container wraps md container lifecycle operations.
package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/caic-xyz/md"
)

// New creates an md.Client for container operations.
// runtime selects the container runtime ("docker" or "podman"); empty means auto-detect.
func New(tailscaleAPIKey, githubToken, runtime string) (*md.Client, error) {
	c, err := md.New(&SlogWriter{Phase: "init"})
	if err != nil {
		return nil, err
	}
	if runtime != "" {
		c.Runtime = runtime
	}
	c.TailscaleAPIKey = tailscaleAPIKey
	c.GithubToken = githubToken
	return c, nil
}

// SlogWriter is an io.Writer that logs each complete line via slog.Info.
// Use it instead of io.Discard so md output is captured in structured logs.
type SlogWriter struct {
	// Phase labels the log entries (e.g. "launch", "warmup").
	Phase string
	buf   []byte
}

func (w *SlogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimSpace(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			slog.Info("md", "phase", w.Phase, "msg", line)
		}
	}
	return len(p), nil
}

// LabelValue returns the value of a container label on a running container.
//
// Returns empty string if the label is not set.
func LabelValue(ctx context.Context, runtime, containerName, label string) (string, error) {
	format := fmt.Sprintf("{{index .Config.Labels %q}}", label)
	cmd := exec.CommandContext(ctx, runtime, "inspect", containerName, "--format", format) //nolint:gosec // containerName and format are not user-controlled.
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s inspect label %q on %s: %w", runtime, label, containerName, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "<no value>" {
		return "", nil
	}
	return v, nil
}

// Event represents a container lifecycle event.
type Event struct {
	Name string // Container name.
}

// containerEvent is the JSON structure emitted by `<runtime> events --format '{{json .}}'`.
type containerEvent struct {
	Actor struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// WatchEvents monitors container die events filtered by a label.
// It runs `<runtime> events --filter event=die --filter label=<labelFilter>`
// and sends an Event for each death. The caller handles reconnection
// on stream errors. The channel is closed when the context is cancelled or
// the events process exits.
func WatchEvents(ctx context.Context, runtime, labelFilter string) (<-chan Event, error) {
	cmd := exec.CommandContext(ctx, runtime, "events", //nolint:gosec // labelFilter is a trusted constant
		"--filter", "event=die",
		"--filter", "label="+labelFilter,
		"--format", "{{json .}}",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s events stdout: %w", runtime, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s events start: %w", runtime, err)
	}
	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		defer func() { _ = cmd.Wait() }()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var ev containerEvent
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
