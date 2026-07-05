// Package mdruntime adapts md containers to caic runtime interfaces.

package mdruntime

import (
	"bytes"
	"context"
	"log/slog"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/containers"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// New creates an md.Client for the md runtime adapter.
// runtimeName selects the container runtime ("docker" or "podman"); empty means auto-detect.
func New(tailscaleAPIKey, githubToken, runtimeName string) (*md.Client, error) {
	logger := slog.Default()
	var containerRuntime containers.Runtime
	if runtimeName != "" {
		var err error
		containerRuntime, err = containers.New(runtimeName, logger, nil)
		if err != nil {
			return nil, err
		}
	}
	c, err := md.New(logger, containerRuntime, &SlogWriter{Phase: "init"})
	if err != nil {
		return nil, err
	}
	c.TailscaleAPIKey = tailscaleAPIKey
	c.GithubToken = githubToken
	return c, nil
}

// InstancesFromMD converts md container handles to runtime-neutral instances.
func InstancesFromMD(ctx context.Context, mdContainers []*md.Container) []runtime.Instance {
	if len(mdContainers) == 0 {
		return nil
	}
	out := make([]runtime.Instance, len(mdContainers))
	for i, c := range mdContainers {
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
