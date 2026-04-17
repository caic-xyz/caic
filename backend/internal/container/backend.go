// Backend adapts *md.Client to task.ContainerBackend for launching and managing containers.

package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md"
	"github.com/maruel/genai"
)

// Backend adapts *md.Client to task.ContainerBackend.
type Backend struct {
	Client     *md.Client
	Provider   genai.Provider      // nil if LLM not configured
	HarnessEnv map[string][]string // per-harness KEY=VALUE env vars from config

	mu                sync.Mutex
	pendingContainers map[string]*md.Container // keyed by container name
}

func (b *Backend) mdStartOpts(labels []string, opts *task.StartOptions) (client *md.Client, mdOpts *md.StartOpts) {
	harnessMap := map[agent.Harness]md.Harness{
		agent.Claude:   md.HarnessClaude,
		agent.Codex:    md.HarnessCodex,
		agent.Gemini:   md.HarnessGemini,
		agent.Kilo:     md.HarnessKilo,
		agent.OpenCode: md.HarnessOpencode,
		agent.Pi:       md.HarnessPi,
	}
	mdHarness := harnessMap[opts.Harness]
	harnessPaths := md.HarnessMounts[mdHarness]
	image := opts.DockerImage
	if image == "" {
		image = md.DefaultBaseImage + ":latest"
	}
	client = b.Client
	var extraEnv []string
	extraEnv = append(extraEnv, b.HarnessEnv[string(opts.Harness)]...)
	if opts.GitHubToken != "" {
		extraEnv = append(extraEnv, "GITHUB_TOKEN="+opts.GitHubToken)
	}
	mdOpts = &md.StartOpts{
		BaseImage:  image,
		Labels:     labels,
		AgentPaths: []md.AgentPaths{harnessPaths},
		USB:        opts.USB,
		Tailscale:  opts.Tailscale,
		Display:    opts.Display,
		ExtraEnv:   extraEnv,
		MaxCPUs:    md.DefaultMaxCPUs(),
	}
	return client, mdOpts
}

// Launch implements task.ContainerBackend.
func (b *Backend) Launch(ctx context.Context, repos []md.Repo, labels []string, opts *task.StartOptions) (string, error) {
	if len(repos) > 0 {
		slog.Info("md", "phase", "launch", "dir", repos[0].GitRoot, "br", repos[0].Branch, "hns", opts.Harness)
	} else {
		slog.Info("md", "phase", "launch", "hns", opts.Harness)
	}
	slog.Debug("container", "msg", "Launch starting", "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "repos_count", len(repos))
	if _, ok := map[agent.Harness]md.Harness{
		agent.Claude:   md.HarnessClaude,
		agent.Codex:    md.HarnessCodex,
		agent.Gemini:   md.HarnessGemini,
		agent.Kilo:     md.HarnessKilo,
		agent.OpenCode: md.HarnessOpencode,
		agent.Pi:       md.HarnessPi,
	}[opts.Harness]; !ok {
		return "", fmt.Errorf("unknown harness %q", opts.Harness)
	}
	slog.Debug("container", "msg", "harness verified", "harness", opts.Harness)
	client, mdOpts := b.mdStartOpts(labels, opts)
	slog.Debug("container", "msg", "creating container object", "repos_count", len(repos))
	c := client.Container(repos...)
	stdout, stderr := logWriters(opts.LogWriter, "launch")
	slog.Debug("container", "msg", "calling c.Launch")
	if err := c.Launch(ctx, stdout, stderr, mdOpts); err != nil {
		slog.Error("container", "msg", "c.Launch failed", "err", err)
		return "", err
	}
	slog.Debug("container", "msg", "c.Launch succeeded", "container", c.Name)
	b.mu.Lock()
	if b.pendingContainers == nil {
		b.pendingContainers = make(map[string]*md.Container)
	}
	b.pendingContainers[c.Name] = c
	b.mu.Unlock()
	slog.Debug("container", "msg", "Launch returning", "container", c.Name)
	return c.Name, nil
}

// Connect implements task.ContainerBackend.
func (b *Backend) Connect(ctx context.Context, name string, repos []md.Repo, opts *task.StartOptions) (tailscaleFQDN string, err error) {
	if len(repos) > 0 {
		slog.Info("md", "phase", "connect", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	slog.Debug("container", "msg", "Connect starting", "container", name, "repos_count", len(repos))
	b.mu.Lock()
	c, ok := b.pendingContainers[name]
	if ok {
		delete(b.pendingContainers, name)
	}
	b.mu.Unlock()
	if !ok {
		slog.Debug("container", "msg", "no pending container", "container", name)
		return "", fmt.Errorf("no pending container %q", name)
	}
	slog.Debug("container", "msg", "found pending container", "container", name)
	_, mdOpts := b.mdStartOpts(nil, opts)
	stdout, stderr := logWriters(opts.LogWriter, "connect")
	slog.Debug("container", "msg", "calling c.Connect", "container", name)
	sr, err := c.Connect(ctx, stdout, stderr, mdOpts)
	if err != nil {
		slog.Error("container", "msg", "c.Connect failed", "container", name, "err", err)
		return "", err
	}
	slog.Debug("container", "msg", "c.Connect succeeded", "container", name, "fqdn", sr.TailscaleFQDN)
	return sr.TailscaleFQDN, nil
}

// Diff implements task.ContainerBackend.
func (b *Backend) Diff(ctx context.Context, repo md.Repo, args ...string) (string, error) {
	slog.Info("md diff", "dir", repo.GitRoot, "br", repo.Branch, "args", args)
	var stdout bytes.Buffer
	if err := b.Client.Container(repo).Diff(ctx, &stdout, &SlogWriter{Phase: "diff"}, 0, args); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// Fetch implements task.ContainerBackend.
func (b *Backend) Fetch(ctx context.Context, repos []md.Repo) error {
	if len(repos) > 0 {
		slog.Info("md fetch", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	ct := b.Client.Container(repos...)
	for i := range repos {
		if err := ct.Fetch(ctx, &SlogWriter{Phase: "fetch"}, &SlogWriter{Phase: "fetch"}, i, b.Provider); err != nil {
			return err
		}
	}
	return nil
}

// Stop implements task.ContainerBackend.
func (b *Backend) Stop(ctx context.Context, name string) error {
	slog.Info("md stop", "name", name)
	ct := b.Client.Container()
	ct.Name = name
	return ct.Stop(ctx)
}

// Purge implements task.ContainerBackend.
func (b *Backend) Purge(ctx context.Context, name string, repos []md.Repo) error {
	if len(repos) > 0 {
		slog.Info("md purge", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	} else {
		slog.Info("md purge", "name", name)
	}
	ct := b.Client.Container(repos...)
	if len(repos) == 0 {
		ct.Name = name
	}
	return ct.Purge(ctx, &SlogWriter{Phase: "purge"}, &SlogWriter{Phase: "purge"})
}

// Revive implements task.ContainerBackend.
func (b *Backend) Revive(ctx context.Context, name string, repos []md.Repo) error {
	if len(repos) > 0 {
		slog.Info("md revive", "dir", repos[0].GitRoot, "br", repos[0].Branch, "ctr", name)
	} else {
		slog.Info("md revive", "name", name)
	}
	slog.Debug("container", "msg", "Revive starting", "container", name, "repos_count", len(repos))
	ct := b.Client.Container(repos...)
	if len(repos) == 0 {
		ct.Name = name
	}
	slog.Debug("container", "msg", "calling ct.Revive", "container", name)
	err := ct.Revive(ctx, &SlogWriter{Phase: "revive"}, &SlogWriter{Phase: "revive"})
	if err != nil {
		slog.Error("container", "msg", "ct.Revive failed", "container", name, "err", err)
		return err
	}
	slog.Debug("container", "msg", "Revive succeeded", "container", name)
	return nil
}

// Fork implements task.ContainerBackend.
func (b *Backend) Fork(ctx context.Context, name string, repos []md.Repo, opts *task.ForkOptions) (string, []md.Repo, error) {
	if len(repos) > 0 {
		slog.Info("md", "phase", "fork", "src", name, "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	slog.Debug("container", "msg", "Fork starting", "source", name, "repos_count", len(repos))
	ct := b.Client.Container(repos...)
	ct.Name = name
	ct.State = "running"
	harnessMap := map[agent.Harness]md.Harness{
		agent.Claude:   md.HarnessClaude,
		agent.Codex:    md.HarnessCodex,
		agent.Gemini:   md.HarnessGemini,
		agent.Kilo:     md.HarnessKilo,
		agent.OpenCode: md.HarnessOpencode,
		agent.Pi:       md.HarnessPi,
	}
	var agentPaths []md.AgentPaths
	if mdH, ok := harnessMap[opts.Harness]; ok {
		agentPaths = []md.AgentPaths{md.HarnessMounts[mdH]}
	}
	slog.Debug("container", "msg", "building fork options", "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display)
	forkOpts := &md.ForkOpts{
		ExtraRepos: opts.ExtraRepos,
		Display:    opts.Display,
		Tailscale:  opts.Tailscale,
		USB:        opts.USB,
		Labels:     opts.Labels,
		AgentPaths: agentPaths,
		ExtraEnv:   opts.ExtraEnv,
		MaxCPUs:    md.DefaultMaxCPUs(),
	}
	stdout, stderr := logWriters(opts.LogWriter, "fork")
	slog.Debug("container", "msg", "calling ct.Fork", "source", name)
	forked, err := ct.Fork(ctx, stdout, stderr, forkOpts)
	if err != nil {
		slog.Error("container", "msg", "ct.Fork failed", "source", name, "err", err)
		return "", nil, err
	}
	slog.Debug("container", "msg", "Fork succeeded", "source", name, "fork", forked.Name)
	return forked.Name, forked.Repos, nil
}

// logWriters returns stdout and stderr writers for md task operations.
func logWriters(w io.Writer, phase string) (stdout, stderr io.Writer) {
	return w, &SlogWriter{Phase: phase}
}
