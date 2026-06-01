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

	"github.com/caic-xyz/md"
	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// mdClient is the subset of *md.Client that Backend needs. Production wraps a
// *md.Client (mdClientAdapter); tests supply a fake. Container and Get return
// mdContainer so the whole launch→connect→fork flow can be exercised without
// Docker or SSH.
type mdClient interface {
	Runtime() string
	Container(repos ...md.Repo) (mdContainer, error)
	Get(ctx context.Context, name string) (mdContainer, error)
}

// mdContainer is the subset of *md.Container that Backend drives. It exposes
// the handful of mutated fields as accessors so a fake need not embed md types.
type mdContainer interface {
	Name() string
	SetName(name string)
	SetState(state string)
	VNCPort() int32
	Repos() []md.Repo
	SSHCommand(opts []string, cmd string) []string
	Launch(ctx context.Context, stdout, stderr io.Writer, opts *md.StartOpts) error
	Connect(ctx context.Context, stdout, stderr io.Writer, opts *md.StartOpts) (*md.StartResult, error)
	Diff(ctx context.Context, stdout, stderr io.Writer, repoIdx int, extraArgs []string) error
	Fetch(ctx context.Context, stdout, stderr io.Writer, repoIdx int, p genai.Provider) error
	Stop(ctx context.Context) error
	Purge(ctx context.Context, stdout, stderr io.Writer) error
	Revive(ctx context.Context, stdout, stderr io.Writer) error
	Fork(ctx context.Context, stdout, stderr io.Writer, opts *md.ForkOpts) (mdContainer, error)
}

var (
	_ mdClient              = mdClientAdapter{}
	_ mdContainer           = mdContainerAdapter{}
	_ task.ContainerBackend = (*Backend)(nil)
)

// mdClientAdapter adapts *md.Client to mdClient.
type mdClientAdapter struct{ c *md.Client }

// Runtime returns the container runtime name ("docker" or "podman").
func (a mdClientAdapter) Runtime() string { return a.c.Runtime }

// Container constructs a container handle for the given repos.
func (a mdClientAdapter) Container(repos ...md.Repo) (mdContainer, error) {
	ct, err := a.c.Container(repos...)
	if err != nil {
		return nil, err
	}
	return mdContainerAdapter{ct}, nil
}

// Get looks up a running container by name.
func (a mdClientAdapter) Get(ctx context.Context, name string) (mdContainer, error) {
	ct, err := a.c.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return mdContainerAdapter{ct}, nil
}

// mdContainerAdapter adapts *md.Container to mdContainer.
type mdContainerAdapter struct{ c *md.Container }

func (a mdContainerAdapter) Name() string          { return a.c.Name }
func (a mdContainerAdapter) SetName(name string)   { a.c.Name = name }
func (a mdContainerAdapter) SetState(state string) { a.c.State = state }
func (a mdContainerAdapter) VNCPort() int32        { return a.c.VNCPort }
func (a mdContainerAdapter) Repos() []md.Repo      { return a.c.Repos }
func (a mdContainerAdapter) SSHCommand(opts []string, cmd string) []string {
	return a.c.SSHCommand(opts, cmd)
}

func (a mdContainerAdapter) Launch(ctx context.Context, stdout, stderr io.Writer, opts *md.StartOpts) error {
	return a.c.Launch(ctx, stdout, stderr, opts)
}

func (a mdContainerAdapter) Connect(ctx context.Context, stdout, stderr io.Writer, opts *md.StartOpts) (*md.StartResult, error) {
	return a.c.Connect(ctx, stdout, stderr, opts)
}

func (a mdContainerAdapter) Diff(ctx context.Context, stdout, stderr io.Writer, repoIdx int, extraArgs []string) error {
	return a.c.Diff(ctx, stdout, stderr, repoIdx, extraArgs)
}

func (a mdContainerAdapter) Fetch(ctx context.Context, stdout, stderr io.Writer, repoIdx int, p genai.Provider) error {
	return a.c.Fetch(ctx, stdout, stderr, repoIdx, p)
}

func (a mdContainerAdapter) Stop(ctx context.Context) error { return a.c.Stop(ctx) }

func (a mdContainerAdapter) Purge(ctx context.Context, stdout, stderr io.Writer) error {
	return a.c.Purge(ctx, stdout, stderr)
}

func (a mdContainerAdapter) Revive(ctx context.Context, stdout, stderr io.Writer) error {
	return a.c.Revive(ctx, stdout, stderr)
}

func (a mdContainerAdapter) Fork(ctx context.Context, stdout, stderr io.Writer, opts *md.ForkOpts) (mdContainer, error) {
	f, err := a.c.Fork(ctx, stdout, stderr, opts)
	if err != nil {
		return nil, err
	}
	return mdContainerAdapter{f}, nil
}

// Backend adapts *md.Client to task.ContainerBackend.
type Backend struct {
	client     mdClient
	Provider   genai.Provider      // nil if LLM not configured
	HarnessEnv map[string][]string // per-harness KEY=VALUE env vars from config

	mu                sync.Mutex
	pendingContainers map[string]mdContainer // keyed by container name
	vncPorts          map[string]int32       // container name → host VNC port
}

// NewBackend creates a Backend wrapping the given md client.
func NewBackend(client *md.Client) *Backend {
	return &Backend{
		client:   mdClientAdapter{client},
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
	rt := b.client.Runtime()
	slog.DebugContext(ctx, "launch starting", "rt", rt, "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "repos_count", len(repos))
	if _, ok := harnessMap[opts.Harness]; !ok {
		return "", fmt.Errorf("unknown harness %q", opts.Harness)
	}
	// Rootless podman does not support sudo (user namespace stacking prevents nested containers).
	if opts.Sudo && rt == "podman" && os.Getuid() != 0 {
		return "", errors.New("sudo is not supported with rootless podman; use docker instead")
	}
	slog.DebugContext(ctx, "harness verified", "rt", rt, "harness", opts.Harness)
	mdOpts := b.mdStartOpts(labels, opts)
	slog.DebugContext(ctx, "creating container", "rt", rt, "repos_count", len(repos))
	c, err := b.client.Container(repos...)
	if err != nil {
		return "", err
	}
	stdout, stderr := logWriters(opts.LogWriter, "launch")
	slog.DebugContext(ctx, "launching", "rt", rt)
	if err := c.Launch(ctx, stdout, stderr, mdOpts); err != nil {
		slog.ErrorContext(ctx, "launch failed", "rt", rt, "err", err)
		return "", err
	}
	name := c.Name()
	slog.DebugContext(ctx, "launch succeeded", "rt", rt, "ctr", name)
	b.mu.Lock()
	if b.pendingContainers == nil {
		b.pendingContainers = make(map[string]mdContainer)
	}
	b.pendingContainers[name] = c
	b.vncPorts[name] = c.VNCPort()
	b.mu.Unlock()
	slog.DebugContext(ctx, "launch returning", "rt", rt, "ctr", name)
	return name, nil
}

// Connect implements task.ContainerBackend.
func (b *Backend) Connect(ctx context.Context, name string, repos []md.Repo, opts *task.StartOptions) (tailscaleFQDN, tailscaleAuthURL string, err error) {
	defer trace.StartRegion(ctx, "container.connect").End()
	if len(repos) > 0 {
		slog.Info("md", "phase", "connect", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	rt := b.client.Runtime()
	slog.DebugContext(ctx, "connect starting", "rt", rt, "ctr", name, "repos_count", len(repos))
	b.mu.Lock()
	c, ok := b.pendingContainers[name]
	if ok {
		delete(b.pendingContainers, name)
	}
	b.mu.Unlock()
	if !ok {
		slog.DebugContext(ctx, "no pending container", "rt", rt, "ctr", name)
		return "", "", fmt.Errorf("no pending container %q", name)
	}
	slog.DebugContext(ctx, "found pending container", "rt", rt, "ctr", name)
	mdOpts := b.mdStartOpts(nil, opts)
	stdout, stderr := logWriters(opts.LogWriter, "connect")
	slog.DebugContext(ctx, "calling connect", "rt", rt, "ctr", name)
	sr, err := c.Connect(ctx, stdout, stderr, mdOpts)
	if err != nil {
		slog.ErrorContext(ctx, "connect failed", "rt", rt, "ctr", name, "err", err)
		return "", "", err
	}
	slog.DebugContext(ctx, "connect succeeded", "rt", rt, "ctr", name, "fqdn", sr.TailscaleFQDN, "authurl", sr.TailscaleAuthURL)
	return sr.TailscaleFQDN, sr.TailscaleAuthURL, nil
}

// Diff implements task.ContainerBackend.
func (b *Backend) Diff(ctx context.Context, repo *md.Repo, args ...string) (string, error) {
	defer trace.StartRegion(ctx, "container.diff").End()
	slog.Info("md diff", "dir", repo.GitRoot, "br", repo.Branch, "args", args)
	var stdout bytes.Buffer
	ct, err := b.client.Container(*repo)
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
	ct, err := b.client.Container(repos...)
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
	ct, err := b.client.Container()
	if err != nil {
		return err
	}
	ct.SetName(name)
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
	ct, err := b.client.Container(repos...)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		ct.SetName(name)
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
	rt := b.client.Runtime()
	slog.DebugContext(ctx, "revive starting", "rt", rt, "ctr", name, "repos_count", len(repos))
	ct, err := b.client.Container(repos...)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		ct.SetName(name)
	}
	slog.DebugContext(ctx, "calling revive", "rt", rt, "ctr", name)
	if err = ct.Revive(ctx, &SlogWriter{Phase: "revive"}, &SlogWriter{Phase: "revive"}); err != nil {
		slog.ErrorContext(ctx, "revive failed", "rt", rt, "ctr", name, "err", err)
		return err
	}
	slog.DebugContext(ctx, "revive succeeded", "rt", rt, "ctr", name)
	// VNC port may have changed after restart (port remapping).
	b.mu.Lock()
	b.vncPorts[name] = ct.VNCPort()
	b.mu.Unlock()
	return nil
}

// Fork implements task.ContainerBackend.
func (b *Backend) Fork(ctx context.Context, name string, repos []md.Repo, opts *task.ForkOptions) (string, []md.Repo, error) {
	defer trace.StartRegion(ctx, "container.fork").End()
	if len(repos) > 0 {
		slog.Info("md", "phase", "fork", "src", name, "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	rt := b.client.Runtime()
	// Rootless podman does not support sudo (user namespace stacking prevents nested containers).
	if opts.Sudo && rt == "podman" && os.Getuid() != 0 {
		return "", nil, errors.New("sudo is not supported with rootless podman; use docker instead")
	}
	slog.DebugContext(ctx, "fork starting", "rt", rt, "source", name, "repos_count", len(repos))

	// Look up the source container so Fork inherits Display, Tailscale,
	// USB, and Sudo from the source unless explicitly overridden by opts.
	ct, err := b.client.Get(ctx, name)
	if err != nil {
		return "", nil, fmt.Errorf("source container %s: %w", name, err)
	}
	ct.SetState("running")
	var agentPaths []md.AgentPaths
	if mdH, ok := harnessMap[opts.Harness]; ok {
		agentPaths = []md.AgentPaths{md.HarnessMounts[mdH]}
	}
	slog.DebugContext(ctx, "building fork options", "rt", rt, "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "sudo", opts.Sudo)
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
	slog.DebugContext(ctx, "calling fork", "rt", rt, "source", name)
	forked, err := ct.Fork(ctx, stdout, stderr, forkOpts)
	if err != nil {
		slog.ErrorContext(ctx, "fork failed", "rt", rt, "source", name, "err", err)
		return "", nil, err
	}
	forkName := forked.Name()
	slog.DebugContext(ctx, "fork succeeded", "rt", rt, "source", name, "fork", forkName)
	b.mu.Lock()
	b.vncPorts[forkName] = forked.VNCPort()
	b.mu.Unlock()
	return forkName, forked.Repos(), nil
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
	if v, err := LabelValue(ctx, b.client.Runtime(), containerName, "md.display"); err == nil && v == "1" {
		if hp, err := b.hostPort(ctx, containerName, "5901/tcp"); err == nil {
			b.mu.Lock()
			b.vncPorts[containerName] = int32(hp) //nolint:gosec // port numbers are 1-65535, safe for int32
			b.mu.Unlock()
			return hp
		}
	}
	return 0
}

// harnessMap maps caic harnesses to their md equivalents.
var harnessMap = map[agent.Harness]md.Harness{
	agent.Claude:   md.HarnessClaude,
	agent.Codex:    md.HarnessCodex,
	agent.Gemini:   md.HarnessGemini,
	agent.Kilo:     md.HarnessKilo,
	agent.OpenCode: md.HarnessOpencode,
	agent.Pi:       md.HarnessPi,
}

// mdStartOpts builds the md.StartOpts for a given harness and task options.
func (b *Backend) mdStartOpts(labels []string, opts *task.StartOptions) *md.StartOpts {
	harnessPaths := md.HarnessMounts[harnessMap[opts.Harness]]
	image := opts.DockerImage
	if image == "" {
		image = md.DefaultBaseImage + ":latest"
	}
	var extraEnv []string
	// Prevent agents from spawning interactive editors (neovim, vim, etc.)
	// during git commit, git mergetool, or any command invoking $EDITOR.
	extraEnv = append(extraEnv, "EDITOR=true")
	extraEnv = append(extraEnv, b.HarnessEnv[string(opts.Harness)]...)
	if opts.GitHubToken != "" {
		extraEnv = append(extraEnv, "GITHUB_TOKEN="+opts.GitHubToken)
	}
	return &md.StartOpts{
		BaseImage:  image,
		Caches:     opts.Caches,
		Labels:     labels,
		AgentPaths: []md.AgentPaths{harnessPaths},
		USB:        opts.USB,
		Tailscale:  opts.Tailscale,
		Display:    opts.Display,
		Sudo:       opts.Sudo,
		ExtraEnv:   extraEnv,
		MaxCPUs:    maxCPUsOrDefault(opts.MaxCPUs),
	}
}

// hostPort reads the container host port mapping for the given container port.
func (b *Backend) hostPort(ctx context.Context, containerName, containerPort string) (int, error) {
	cmd := exec.CommandContext(ctx, b.client.Runtime(), "port", containerName, containerPort) //nolint:gosec // containerName is internally-assigned
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return parseHostPort(string(out))
}

// parseHostPort extracts the host port from `docker port` output, e.g.
// "0.0.0.0:32768" or "127.0.0.1:32768".
func parseHostPort(out string) (int, error) {
	parts := strings.SplitN(strings.TrimSpace(out), ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("unexpected port output: %q", out)
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
