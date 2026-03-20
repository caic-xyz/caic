// mdBackend adapts *md.Client to task.ContainerBackend for launching and managing containers.

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md"
	"github.com/maruel/genai"
)

// mdBackend adapts *md.Client to task.ContainerBackend.
type mdBackend struct {
	client   *md.Client
	provider genai.Provider // nil if LLM not configured

	mu                sync.Mutex
	pendingContainers map[string]*md.Container // keyed by container name
}

func (b *mdBackend) mdStartOpts(labels []string, opts *task.StartOptions) (client *md.Client, mdOpts *md.StartOpts) {
	harnessMap := map[agent.Harness]md.Harness{
		agent.Claude:   md.HarnessClaude,
		agent.Codex:    md.HarnessCodex,
		agent.Gemini:   md.HarnessGemini,
		agent.Kilo:     md.HarnessKilo,
		agent.OpenCode: md.HarnessOpencode,
	}
	mdHarness := harnessMap[opts.Harness]
	harnessPaths := md.HarnessMounts[mdHarness]
	image := opts.DockerImage
	if image == "" {
		image = md.DefaultBaseImage + ":latest"
	}
	client = b.client
	mdOpts = &md.StartOpts{
		BaseImage:  image,
		Labels:     labels,
		AgentPaths: []md.AgentPaths{harnessPaths},
		USB:        opts.USB,
		Tailscale:  opts.Tailscale,
		Display:    opts.Display,
	}
	return client, mdOpts
}

func (b *mdBackend) Launch(ctx context.Context, repos []md.Repo, labels []string, opts *task.StartOptions) error {
	if len(repos) > 0 {
		slog.Info("md", "phase", "launch", "dir", repos[0].GitRoot, "br", repos[0].Branch, "hns", opts.Harness)
	} else {
		slog.Info("md", "phase", "launch", "hns", opts.Harness)
	}
	if _, ok := map[agent.Harness]md.Harness{
		agent.Claude:   md.HarnessClaude,
		agent.Codex:    md.HarnessCodex,
		agent.Gemini:   md.HarnessGemini,
		agent.Kilo:     md.HarnessKilo,
		agent.OpenCode: md.HarnessOpencode,
	}[opts.Harness]; !ok {
		return fmt.Errorf("unknown harness %q", opts.Harness)
	}
	client, mdOpts := b.mdStartOpts(labels, opts)
	c := client.Container(repos...)
	stdout, stderr := logWriters(opts.LogWriter, "launch")
	if err := c.Launch(ctx, stdout, stderr, mdOpts); err != nil {
		return err
	}
	b.mu.Lock()
	if b.pendingContainers == nil {
		b.pendingContainers = make(map[string]*md.Container)
	}
	b.pendingContainers[c.Name] = c
	b.mu.Unlock()
	return nil
}

func (b *mdBackend) Connect(ctx context.Context, repos []md.Repo, opts *task.StartOptions) (name, tailscaleFQDN string, err error) {
	if len(repos) > 0 {
		slog.Info("md", "phase", "connect", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	// Derive container name from repos (deterministic, same as Launch used).
	tmpClient := b.client
	c := tmpClient.Container(repos...)
	b.mu.Lock()
	if stored, ok := b.pendingContainers[c.Name]; ok {
		c = stored
		delete(b.pendingContainers, c.Name)
	}
	b.mu.Unlock()
	_, mdOpts := b.mdStartOpts(nil, opts)
	stdout, stderr := logWriters(opts.LogWriter, "connect")
	sr, err := c.Connect(ctx, stdout, stderr, mdOpts)
	if err != nil {
		return "", "", err
	}
	return c.Name, sr.TailscaleFQDN, nil
}

func (b *mdBackend) Diff(ctx context.Context, repo md.Repo, args ...string) (string, error) {
	slog.Info("md diff", "dir", repo.GitRoot, "br", repo.Branch, "args", args)
	var stdout bytes.Buffer
	if err := b.client.Container(repo).Diff(ctx, &stdout, &container.SlogWriter{Phase: "diff"}, 0, args); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

func (b *mdBackend) Fetch(ctx context.Context, repos []md.Repo) error {
	if len(repos) > 0 {
		slog.Info("md fetch", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	ct := b.client.Container(repos...)
	for i := range repos {
		if err := ct.Fetch(ctx, &container.SlogWriter{Phase: "fetch"}, &container.SlogWriter{Phase: "fetch"}, i, b.provider); err != nil {
			return err
		}
	}
	return nil
}

func (b *mdBackend) Stop(ctx context.Context, name string) error {
	slog.Info("md stop", "name", name)
	ct := b.client.Container()
	ct.Name = name
	return ct.Stop(ctx)
}

func (b *mdBackend) Purge(ctx context.Context, name string, repos []md.Repo) error {
	if len(repos) > 0 {
		slog.Info("md purge", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	} else {
		slog.Info("md purge", "name", name)
	}
	ct := b.client.Container(repos...)
	if len(repos) == 0 {
		ct.Name = name
	}
	return ct.Purge(ctx, &container.SlogWriter{Phase: "purge"}, &container.SlogWriter{Phase: "purge"})
}

func (b *mdBackend) Revive(ctx context.Context, name string, repos []md.Repo) error {
	if len(repos) > 0 {
		slog.Info("md revive", "dir", repos[0].GitRoot, "br", repos[0].Branch, "ctr", name)
	} else {
		slog.Info("md revive", "name", name)
	}
	ct := b.client.Container(repos...)
	if len(repos) == 0 {
		ct.Name = name
	}
	return ct.Revive(ctx, &container.SlogWriter{Phase: "revive"}, &container.SlogWriter{Phase: "revive"})
}

// logWriters returns stdout and stderr writers for md task operations.
func logWriters(w io.Writer, phase string) (stdout, stderr io.Writer) {
	return w, &container.SlogWriter{Phase: phase}
}
