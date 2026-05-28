// Backend adapts *md.Client to task.ContainerBackend for launching and managing containers.

package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime/trace"
	"strconv"
	"strings"
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
	vncPorts          map[string]int32         // container name → host VNC port
}

// NewBackend creates a Backend wrapping the given md client.
func NewBackend(client *md.Client) *Backend {
	return &Backend{
		Client:   client,
		vncPorts: make(map[string]int32),
	}
}

// Launch implements task.ContainerBackend.
func (b *Backend) Launch(ctx context.Context, repos []md.Repo, labels []string, opts *task.StartOptions) (string, error) {
	defer trace.StartRegion(ctx, "container.launch").End()
	if len(repos) > 0 {
		slog.Info("md", "phase", "launch", "dir", repos[0].GitRoot, "br", repos[0].Branch, "hns", opts.Harness)
	} else {
		slog.Info("md", "phase", "launch", "hns", opts.Harness)
	}
	slog.DebugContext(ctx, "launch starting", "rt", b.Client.Runtime, "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "repos_count", len(repos))
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
	// Rootless podman does not support sudo (user namespace stacking prevents nested containers).
	if opts.Sudo && b.Client.Runtime == "podman" && os.Getuid() != 0 {
		return "", errors.New("sudo is not supported with rootless podman; use docker instead")
	}
	slog.DebugContext(ctx, "harness verified", "rt", b.Client.Runtime, "harness", opts.Harness)
	client, mdOpts := b.mdStartOpts(labels, opts)
	slog.DebugContext(ctx, "creating container", "rt", b.Client.Runtime, "repos_count", len(repos))
	c, err := client.Container(repos...)
	if err != nil {
		return "", err
	}
	stdout, stderr := logWriters(opts.LogWriter, "launch")
	slog.DebugContext(ctx, "launching", "rt", b.Client.Runtime)
	if err := c.Launch(ctx, stdout, stderr, mdOpts); err != nil {
		slog.ErrorContext(ctx, "launch failed", "rt", b.Client.Runtime, "err", err)
		return "", err
	}
	slog.DebugContext(ctx, "launch succeeded", "rt", b.Client.Runtime, "ctr", c.Name)
	b.mu.Lock()
	if b.pendingContainers == nil {
		b.pendingContainers = make(map[string]*md.Container)
	}
	b.pendingContainers[c.Name] = c
	b.vncPorts[c.Name] = c.VNCPort
	b.mu.Unlock()
	slog.DebugContext(ctx, "launch returning", "rt", b.Client.Runtime, "ctr", c.Name)
	return c.Name, nil
}

// Connect implements task.ContainerBackend.
func (b *Backend) Connect(ctx context.Context, name string, repos []md.Repo, opts *task.StartOptions) (tailscaleFQDN, tailscaleAuthURL string, err error) {
	defer trace.StartRegion(ctx, "container.connect").End()
	if len(repos) > 0 {
		slog.Info("md", "phase", "connect", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	slog.DebugContext(ctx, "connect starting", "rt", b.Client.Runtime, "ctr", name, "repos_count", len(repos))
	b.mu.Lock()
	c, ok := b.pendingContainers[name]
	if ok {
		delete(b.pendingContainers, name)
	}
	b.mu.Unlock()
	if !ok {
		slog.DebugContext(ctx, "no pending container", "rt", b.Client.Runtime, "ctr", name)
		return "", "", fmt.Errorf("no pending container %q", name)
	}
	slog.DebugContext(ctx, "found pending container", "rt", b.Client.Runtime, "ctr", name)
	_, mdOpts := b.mdStartOpts(nil, opts)
	stdout, stderr := logWriters(opts.LogWriter, "connect")
	slog.DebugContext(ctx, "calling connect", "rt", b.Client.Runtime, "ctr", name)
	sr, err := c.Connect(ctx, stdout, stderr, mdOpts)
	if err != nil {
		slog.ErrorContext(ctx, "connect failed", "rt", b.Client.Runtime, "ctr", name, "err", err)
		return "", "", err
	}
	slog.DebugContext(ctx, "connect succeeded", "rt", b.Client.Runtime, "ctr", name, "fqdn", sr.TailscaleFQDN, "authurl", sr.TailscaleAuthURL)
	return sr.TailscaleFQDN, sr.TailscaleAuthURL, nil
}

// Diff implements task.ContainerBackend.
func (b *Backend) Diff(ctx context.Context, repo *md.Repo, args ...string) (string, error) {
	defer trace.StartRegion(ctx, "container.diff").End()
	slog.Info("md diff", "dir", repo.GitRoot, "br", repo.Branch, "args", args)
	var stdout bytes.Buffer
	ct, err := b.Client.Container(*repo)
	if err != nil {
		return "", err
	}
	if err := ct.Diff(ctx, &stdout, &SlogWriter{Phase: "diff"}, 0, args); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// Fetch implements task.ContainerBackend.
func (b *Backend) Fetch(ctx context.Context, repos []md.Repo) error {
	defer trace.StartRegion(ctx, "container.fetch").End()
	if len(repos) > 0 {
		slog.Info("md fetch", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	ct, err := b.Client.Container(repos...)
	if err != nil {
		return err
	}
	for i := range repos {
		if err := ct.Fetch(ctx, &SlogWriter{Phase: "fetch"}, &SlogWriter{Phase: "fetch"}, i, b.Provider); err != nil {
			return err
		}
	}
	return nil
}

// Stop implements task.ContainerBackend.
func (b *Backend) Stop(ctx context.Context, name string) error {
	defer trace.StartRegion(ctx, "container.stop").End()
	slog.Info("md stop", "name", name)
	ct, _ := b.Client.Container()
	ct.Name = name
	return ct.Stop(ctx)
}

// Purge implements task.ContainerBackend.
func (b *Backend) Purge(ctx context.Context, name string, repos []md.Repo) error {
	defer trace.StartRegion(ctx, "container.purge").End()
	if len(repos) > 0 {
		slog.Info("md purge", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	} else {
		slog.Info("md purge", "name", name)
	}
	ct, err := b.Client.Container(repos...)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		ct.Name = name
	}
	return ct.Purge(ctx, &SlogWriter{Phase: "purge"}, &SlogWriter{Phase: "purge"})
}

// Revive implements task.ContainerBackend.
func (b *Backend) Revive(ctx context.Context, name string, repos []md.Repo) error {
	defer trace.StartRegion(ctx, "container.revive").End()
	if len(repos) > 0 {
		slog.Info("md revive", "dir", repos[0].GitRoot, "br", repos[0].Branch, "ctr", name)
	} else {
		slog.Info("md revive", "name", name)
	}
	slog.DebugContext(ctx, "revive starting", "rt", b.Client.Runtime, "ctr", name, "repos_count", len(repos))
	ct, err := b.Client.Container(repos...)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		ct.Name = name
	}
	slog.DebugContext(ctx, "calling revive", "rt", b.Client.Runtime, "ctr", name)
	if err = ct.Revive(ctx, &SlogWriter{Phase: "revive"}, &SlogWriter{Phase: "revive"}); err != nil {
		slog.ErrorContext(ctx, "revive failed", "rt", b.Client.Runtime, "ctr", name, "err", err)
		return err
	}
	slog.DebugContext(ctx, "revive succeeded", "rt", b.Client.Runtime, "ctr", name)
	// VNC port may have changed after restart (port remapping).
	b.mu.Lock()
	b.vncPorts[name] = ct.VNCPort
	b.mu.Unlock()
	return nil
}

// Fork implements task.ContainerBackend.
func (b *Backend) Fork(ctx context.Context, name string, repos []md.Repo, opts *task.ForkOptions) (string, []md.Repo, error) {
	defer trace.StartRegion(ctx, "container.fork").End()
	if len(repos) > 0 {
		slog.Info("md", "phase", "fork", "src", name, "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	// Rootless podman does not support sudo (user namespace stacking prevents nested containers).
	if opts.Sudo && b.Client.Runtime == "podman" && os.Getuid() != 0 {
		return "", nil, errors.New("sudo is not supported with rootless podman; use docker instead")
	}
	slog.DebugContext(ctx, "fork starting", "rt", b.Client.Runtime, "source", name, "repos_count", len(repos))

	// Look up the source container so Fork inherits Display, Tailscale,
	// USB, and Sudo from the source unless explicitly overridden by opts.
	ct, err := b.Client.Get(ctx, name)
	if err != nil {
		return "", nil, fmt.Errorf("source container %s: %w", name, err)
	}
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
	slog.DebugContext(ctx, "building fork options", "rt", b.Client.Runtime, "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "sudo", opts.Sudo)
	forkOpts := &md.ForkOpts{
		ExtraRepos: opts.ExtraRepos,
		Display:    opts.Display,
		Tailscale:  opts.Tailscale,
		USB:        opts.USB,
		Sudo:       opts.Sudo,
		Labels:     opts.Labels,
		AgentPaths: agentPaths,
		ExtraEnv:   opts.ExtraEnv,
		MaxCPUs:    maxCPUsOrDefault(opts.MaxCPUs),
	}
	stdout, stderr := logWriters(opts.LogWriter, "fork")
	slog.DebugContext(ctx, "calling fork", "rt", b.Client.Runtime, "source", name)
	forked, err := ct.Fork(ctx, stdout, stderr, forkOpts)
	if err != nil {
		slog.ErrorContext(ctx, "fork failed", "rt", b.Client.Runtime, "source", name, "err", err)
		return "", nil, err
	}
	slog.DebugContext(ctx, "fork succeeded", "rt", b.Client.Runtime, "source", name, "fork", forked.Name)
	b.mu.Lock()
	b.vncPorts[forked.Name] = forked.VNCPort
	b.mu.Unlock()
	return forked.Name, forked.Repos, nil
}

// VNCPort implements task.ContainerBackend.
func (b *Backend) VNCPort(ctx context.Context, containerName string) int {
	b.mu.Lock()
	port := int(b.vncPorts[containerName])
	b.mu.Unlock()
	if port != 0 {
		return port
	}
	// Fallback: query container label. Handles server
	// restarts where the in-memory map is empty but the container
	// is still running with a display.
	if v, err := LabelValue(ctx, b.Client.Runtime, containerName, "md.display"); err == nil && v == "1" {
		if hp, err := b.hostPort(ctx, containerName, "5901/tcp"); err == nil {
			b.mu.Lock()
			b.vncPorts[containerName] = int32(hp) //nolint:gosec // port numbers are 1-65535, safe for int32
			b.mu.Unlock()
			return hp
		}
	}
	return 0
}

// mdStartOpts builds the md.StartOpts for a given harness and task options.
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
	// Prevent agents from spawning interactive editors (neovim, vim, etc.)
	// during git commit, git mergetool, or any command invoking $EDITOR.
	extraEnv = append(extraEnv, "EDITOR=true")
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
		Sudo:       opts.Sudo,
		ExtraEnv:   extraEnv,
		MaxCPUs:    maxCPUsOrDefault(opts.MaxCPUs),
	}
	return client, mdOpts
}

// hostPort reads the container host port mapping for the given container port.
func (b *Backend) hostPort(ctx context.Context, containerName, containerPort string) (int, error) {
	cmd := exec.CommandContext(ctx, b.Client.Runtime, "port", containerName, containerPort) //nolint:gosec // containerName is internally-assigned
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	// Output format: "0.0.0.0:32768" or "127.0.0.1:32768"
	parts := strings.SplitN(strings.TrimSpace(string(out)), ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("unexpected %s port output: %q", b.Client.Runtime, out)
	}
	return strconv.Atoi(parts[1])
}

// logWriters returns stdout and stderr writers for md task operations.
func logWriters(w io.Writer, phase string) (stdout, stderr io.Writer) {
	return w, &SlogWriter{Phase: phase}
}

// maxCPUsOrDefault returns cpus if non-zero, otherwise [md.DefaultMaxCPUs].
func maxCPUsOrDefault(cpus int) int {
	if cpus <= 0 {
		return md.DefaultMaxCPUs()
	}
	return cpus
}
