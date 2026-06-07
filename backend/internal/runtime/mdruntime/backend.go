// Backend adapts md containers to runtime.Backend for launching and managing runtime instances.

package mdruntime

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
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/caic-xyz/md"
	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
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
	_ mdClient        = mdClientAdapter{}
	_ mdContainer     = mdContainerAdapter{}
	_ runtime.Backend = (*Backend)(nil)
)

// mdClientAdapter adapts *md.Client to mdClient.
type mdClientAdapter struct{ c *md.Client }

// Runtime returns the underlying container runtime name ("docker" or "podman").
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

// Backend adapts *md.Client to runtime.Backend.
type Backend struct {
	client     mdClient
	Provider   genai.Provider      // nil if LLM not configured
	HarnessEnv map[string][]string // per-harness KEY=VALUE env vars from config

	mu                sync.Mutex
	pendingContainers map[string]mdContainer // keyed by runtime instance name
	vncPorts          map[string]int32       // runtime instance name to host VNC port
}

// NewBackend creates a Backend wrapping the given md client.
func NewBackend(client *md.Client) *Backend {
	return &Backend{
		client:   mdClientAdapter{client},
		vncPorts: make(map[string]int32),
	}
}

// Launch implements runtime.Backend.
func (b *Backend) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.InstanceID, error) {
	defer trace.StartRegion(ctx, "container.launch").End()
	if len(repos) > 0 {
		slog.Info("md", "phase", "launch", "dir", repos[0].HostPath, "br", repos[0].Branch, "hns", opts.Harness)
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
	mdOpts := b.mdStartOpts(opts)
	slog.DebugContext(ctx, "creating container", "rt", rt, "repos_count", len(repos))
	mdRepos := toMDRepos(repos)
	c, err := b.client.Container(mdRepos...)
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
	return runtime.InstanceID(name), nil
}

// Connect implements runtime.Backend.
func (b *Backend) Connect(ctx context.Context, id runtime.InstanceID, opts *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	defer trace.StartRegion(ctx, "container.connect").End()
	name := string(id)
	rt := b.client.Runtime()
	slog.DebugContext(ctx, "connect starting", "rt", rt, "ctr", name)
	b.mu.Lock()
	c, ok := b.pendingContainers[name]
	if ok {
		delete(b.pendingContainers, name)
	}
	b.mu.Unlock()
	if !ok {
		slog.DebugContext(ctx, "no pending container", "rt", rt, "ctr", name)
		return runtime.ConnectionInfo{}, fmt.Errorf("no pending container %q", name)
	}
	slog.DebugContext(ctx, "found pending container", "rt", rt, "ctr", name)
	mdOpts := b.mdStartOpts(opts)
	stdout, stderr := logWriters(opts.LogWriter, "connect")
	slog.DebugContext(ctx, "calling connect", "rt", rt, "ctr", name)
	sr, err := c.Connect(ctx, stdout, stderr, mdOpts)
	if err != nil {
		slog.ErrorContext(ctx, "connect failed", "rt", rt, "ctr", name, "err", err)
		return runtime.ConnectionInfo{}, err
	}
	slog.DebugContext(ctx, "connect succeeded", "rt", rt, "ctr", name, "fqdn", sr.TailscaleFQDN, "authurl", sr.TailscaleAuthURL)
	return runtime.ConnectionInfo{TailscaleFQDN: sr.TailscaleFQDN, TailscaleAuthURL: sr.TailscaleAuthURL}, nil
}

// Diff implements runtime.Backend.
func (b *Backend) Diff(ctx context.Context, id runtime.InstanceID, repoIdx int, args ...string) (string, error) {
	defer trace.StartRegion(ctx, "instance.diff").End()
	name := string(id)
	ct, err := b.client.Get(ctx, name)
	if err != nil {
		return "", err
	}
	repos := ct.Repos()
	if repoIdx < 0 || repoIdx >= len(repos) {
		return "", fmt.Errorf("repo index %d out of range for %d repos", repoIdx, len(repos))
	}
	repo := &repos[repoIdx]
	slog.Info("md diff", "ctr", name, "dir", repo.GitRoot, "br", repo.Branch, "args", args)
	var stdout bytes.Buffer
	if err := ct.Diff(ctx, &stdout, &SlogWriter{Phase: "diff"}, repoIdx, args); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// Fetch implements runtime.Backend.
func (b *Backend) Fetch(ctx context.Context, id runtime.InstanceID) error {
	defer trace.StartRegion(ctx, "instance.fetch").End()
	name := string(id)
	ct, err := b.client.Get(ctx, name)
	if err != nil {
		return err
	}
	repos := ct.Repos()
	if len(repos) > 0 {
		slog.Info("md fetch", "ctr", name, "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	for i := range repos {
		if err := ct.Fetch(ctx, &SlogWriter{Phase: "fetch"}, &SlogWriter{Phase: "fetch"}, i, b.Provider); err != nil {
			return err
		}
	}
	return nil
}

// Stop implements runtime.Backend.
func (b *Backend) Stop(ctx context.Context, id runtime.InstanceID) error {
	defer trace.StartRegion(ctx, "instance.stop").End()
	name := string(id)
	slog.Info("md stop", "name", name)
	ct, err := b.client.Get(ctx, name)
	if err != nil {
		return err
	}
	return ct.Stop(ctx)
}

// Purge implements runtime.Backend.
func (b *Backend) Purge(ctx context.Context, id runtime.InstanceID) error {
	defer trace.StartRegion(ctx, "instance.purge").End()
	name := string(id)
	ct, err := b.client.Get(ctx, name)
	if err != nil {
		return err
	}
	ctRepos := ct.Repos()
	if len(ctRepos) > 0 {
		slog.Info("md purge", "ctr", name, "dir", ctRepos[0].GitRoot, "br", ctRepos[0].Branch)
	} else {
		slog.Info("md purge", "name", name)
	}
	return ct.Purge(ctx, &SlogWriter{Phase: "purge"}, &SlogWriter{Phase: "purge"})
}

// Revive implements runtime.Backend.
func (b *Backend) Revive(ctx context.Context, id runtime.InstanceID) error {
	defer trace.StartRegion(ctx, "instance.revive").End()
	name := string(id)
	rt := b.client.Runtime()
	ct, err := b.client.Get(ctx, name)
	if err != nil {
		return err
	}
	ctRepos := ct.Repos()
	if len(ctRepos) > 0 {
		slog.Info("md revive", "ctr", name, "dir", ctRepos[0].GitRoot, "br", ctRepos[0].Branch)
	} else {
		slog.Info("md revive", "name", name)
	}
	slog.DebugContext(ctx, "revive starting", "rt", rt, "ctr", name, "repos_count", len(ctRepos))
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

// Fork implements runtime.Backend.
func (b *Backend) Fork(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, opts *runtime.ForkOptions) (runtime.InstanceID, []runtime.Repo, error) {
	defer trace.StartRegion(ctx, "instance.fork").End()
	name := string(id)
	if len(repos) > 0 {
		slog.Info("md", "phase", "fork", "src", name, "dir", repos[0].HostPath, "br", repos[0].Branch)
	}
	rt := b.client.Runtime()
	// Rootless podman does not support sudo (user namespace stacking prevents nested containers).
	if opts.Sudo && rt == "podman" && os.Getuid() != 0 {
		return "", nil, errors.New("sudo is not supported with rootless podman; use docker instead")
	}
	slog.DebugContext(ctx, "fork starting", "rt", rt, "source", name, "repos_count", len(repos))

	// Look up the source instance so Fork inherits Display, Tailscale,
	// USB, and Sudo from the source unless explicitly overridden by opts.
	ct, err := b.client.Get(ctx, name)
	if err != nil {
		return "", nil, fmt.Errorf("source instance %s: %w", name, err)
	}
	ct.SetState("running")
	var agentPaths []md.AgentPaths
	if mdH, ok := harnessMap[opts.Harness]; ok {
		agentPaths = []md.AgentPaths{md.HarnessMounts[mdH]}
	}
	slog.DebugContext(ctx, "building fork options", "rt", rt, "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "sudo", opts.Sudo)
	forkOpts := &md.ForkOpts{
		ExtraRepos: toMDRepos(opts.ExtraRepos),
		Display:    opts.Display,
		Tailscale:  opts.Tailscale,
		USB:        opts.USB,
		Sudo:       opts.Sudo,
		Labels:     metadataLabels(opts.Metadata),
		AgentPaths: agentPaths,
		ExtraEnv:   opts.ExtraEnv,
		Mounts:     toMDMounts(opts.Mounts),
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
	return runtime.InstanceID(forkName), fromMDRepos(forked.Repos()), nil
}

// VNCPort implements runtime.Backend.
func (b *Backend) VNCPort(ctx context.Context, id runtime.InstanceID) int {
	instanceID := string(id)
	b.mu.Lock()
	port := int(b.vncPorts[instanceID])
	b.mu.Unlock()
	if port != 0 {
		return port
	}
	// Fallback: query instance label. Handles server
	// restarts where the in-memory map is empty but the instance
	// is still running with a display.
	if v, err := labelValue(ctx, b.client.Runtime(), instanceID, string(runtime.MetadataDisplayCapability)); err == nil && v == "1" {
		if hp, err := b.hostPort(ctx, instanceID, "5901/tcp"); err == nil {
			b.mu.Lock()
			b.vncPorts[instanceID] = int32(hp) //nolint:gosec // port numbers are 1-65535, safe for int32
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
func (b *Backend) mdStartOpts(opts *runtime.StartOptions) *md.StartOpts {
	harnessPaths := md.HarnessMounts[harnessMap[opts.Harness]]
	image := opts.BaseImage
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
		Platform:   opts.ContainerPlatform,
		Caches:     toMDCacheMounts(opts.Caches),
		Labels:     metadataLabels(opts.Metadata),
		AgentPaths: []md.AgentPaths{harnessPaths},
		USB:        opts.USB,
		Tailscale:  opts.Tailscale,
		Display:    opts.Display,
		Sudo:       opts.Sudo,
		ExtraEnv:   extraEnv,
		Mounts:     toMDMounts(opts.Mounts),
		MaxCPUs:    maxCPUsOrDefault(opts.MaxCPUs),
	}
}

func toMDRepos(repos []runtime.Repo) []md.Repo {
	if len(repos) == 0 {
		return nil
	}
	out := make([]md.Repo, len(repos))
	for i, r := range repos {
		out[i] = md.Repo{
			GitRoot:       r.HostPath,
			Branch:        r.Branch,
			MountedPath:   r.MountPath,
			DefaultRemote: r.Remote,
			DefaultBranch: r.BaseBranch,
		}
	}
	return out
}

func fromMDRepos(repos []md.Repo) []runtime.Repo {
	if len(repos) == 0 {
		return nil
	}
	out := make([]runtime.Repo, len(repos))
	for i, r := range repos {
		out[i] = runtime.Repo{
			HostPath:   r.GitRoot,
			MountPath:  r.MountedPath,
			Branch:     r.Branch,
			BaseBranch: r.DefaultBranch,
			Remote:     r.DefaultRemote,
		}
	}
	return out
}

func toMDCacheMounts(caches []runtime.CacheMount) []md.CacheMount {
	if len(caches) == 0 {
		return nil
	}
	out := make([]md.CacheMount, len(caches))
	for i, c := range caches {
		out[i] = md.CacheMount{
			Name:          c.Name,
			Description:   c.Description,
			HostPath:      c.HostPath,
			ContainerPath: c.MountPath,
			ReadOnly:      c.ReadOnly,
			Shallow:       c.Shallow,
		}
	}
	return out
}

func toMDMounts(mounts []runtime.Mount) []md.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]md.Mount, len(mounts))
	for i, m := range mounts {
		out[i] = md.Mount{
			HostPath:      m.HostPath,
			ContainerPath: m.MountPath,
			ReadOnly:      m.ReadOnly,
		}
	}
	return out
}

func metadataLabels(metadata runtime.Metadata) []string {
	if len(metadata) == 0 {
		return nil
	}
	labels := make([]string, 0, len(metadata))
	for key, value := range metadata {
		labels = append(labels, string(key)+"="+value)
	}
	slices.Sort(labels)
	return labels
}

// hostPort reads the instance host port mapping for the given instance port.
func (b *Backend) hostPort(ctx context.Context, instanceID, containerPort string) (int, error) {
	cmd := exec.CommandContext(ctx, b.client.Runtime(), "port", instanceID, containerPort) //nolint:gosec // instanceID is internally-assigned
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
