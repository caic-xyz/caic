// Package mdruntime adapts md containers to caic runtime interfaces.

package mdruntime

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

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// New creates an md.Client for the md runtime adapter.
// runtimeName selects the container runtime ("docker" or "podman"); empty means auto-detect.
func New(tailscaleAPIKey, githubToken, runtimeName string) (*md.Client, error) {
	c, err := md.New(&SlogWriter{Phase: "init"})
	if err != nil {
		return nil, err
	}
	if runtimeName != "" {
		c.Runtime = runtimeName
	}
	c.TailscaleAPIKey = tailscaleAPIKey
	c.GithubToken = githubToken
	return c, nil
}

// InstancesFromMD converts md container handles to runtime-neutral instances.
func InstancesFromMD(ctx context.Context, containers []*md.Container) []runtime.Instance {
	if len(containers) == 0 {
		return nil
	}
	out := make([]runtime.Instance, len(containers))
	for i, c := range containers {
		out[i] = runtime.Instance{
			ID:            runtime.InstanceID(c.Name),
			AgentTarget:   runtime.ConnectionTarget{SSHHost: c.Name},
			State:         c.State,
			Repos:         fromMDRepos(c.Repos),
			Tailscale:     c.Tailscale,
			TailscaleFQDN: c.TailscaleFQDN(ctx),
			USB:           c.USB,
			Display:       c.Display,
			Sudo:          c.Sudo,
			VNCPort:       int(c.VNCPort),
		}
	}
	return out
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

// labelValue returns the value of a container label on a running container.
//
// Returns empty string if the label is not set.
func labelValue(ctx context.Context, runtimeName, containerName, label string) (string, error) {
	format := fmt.Sprintf("{{index .Config.Labels %q}}", label)
	cmd := exec.CommandContext(ctx, runtimeName, "inspect", containerName, "--format", format) //nolint:gosec // containerName and format are not user-controlled.
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s inspect label %q on %s: %w", runtimeName, label, containerName, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "<no value>" {
		return "", nil
	}
	return v, nil
}

// containerEvent is the JSON structure emitted by `<runtime> events --format '{{json .}}'`.
type containerEvent struct {
	Actor struct {
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// WatchEvents monitors container die events filtered by runtime metadata.
// It runs `<runtime> events --filter event=die --filter label=<metadata key>`
// and sends an Event for each death. The caller handles reconnection
// on stream errors. The channel is closed when the context is cancelled or
// the events process exits.
func WatchEvents(ctx context.Context, containerRuntime string, filter runtime.EventFilter) (<-chan runtime.Event, error) {
	cmd := exec.CommandContext(ctx, containerRuntime, "events", //nolint:gosec // filter is built from caic-owned constants
		"--filter", "event=die",
		"--filter", "label="+string(filter.MetadataKey),
		"--format", "{{json .}}",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s events stdout: %w", containerRuntime, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s events start: %w", containerRuntime, err)
	}
	ch := make(chan runtime.Event, 16)
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
			case ch <- runtime.Event{InstanceID: runtime.InstanceID(name)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
