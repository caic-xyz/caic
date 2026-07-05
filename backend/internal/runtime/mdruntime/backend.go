// Backend adapts md containers to runtime.Backend for launching and managing runtime instances.

package mdruntime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime/trace"
	"slices"
	"sync"

	"github.com/caic-xyz/md"
	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/harness"
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
	AgentMounts(paths ...md.AgentPaths) ([]md.Mount, error)
	Launch(ctx context.Context, stdout, stderr io.Writer, opts *md.StartOpts) error
	Connect(ctx context.Context, stdout, stderr io.Writer, opts *md.StartOpts) (*md.StartResult, error)
	Diff(ctx context.Context, stdout, stderr io.Writer, repoIdx int, extraArgs []string) error
	Fetch(ctx context.Context, stdout, stderr io.Writer, repoIdx int, p genai.Provider) error
	Stop(ctx context.Context) error
	Purge(ctx context.Context, stdout, stderr io.Writer) error
	Revive(ctx context.Context, stdout, stderr io.Writer) error
	Fork(ctx context.Context, stdout, stderr io.Writer, opts *md.ForkOpts) (mdContainer, error)
	Processes(ctx context.Context) ([]md.ProcessInfo, error)
	Signal(ctx context.Context, pid int, sig string) error
}

var (
	_ mdClient        = mdClientAdapter{}
	_ mdContainer     = mdContainerAdapter{}
	_ runtime.Backend = (*Backend)(nil)
)

// mdClientAdapter adapts *md.Client to mdClient.
type mdClientAdapter struct{ c *md.Client }

// Runtime returns the underlying container runtime name ("docker" or "podman").
func (a mdClientAdapter) Runtime() string { return a.c.Runtime.Name() }

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
func (a mdContainerAdapter) AgentMounts(paths ...md.AgentPaths) ([]md.Mount, error) {
	return a.c.AgentMounts(paths...)
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

func (a mdContainerAdapter) Processes(ctx context.Context) ([]md.ProcessInfo, error) {
	return a.c.Processes(ctx)
}

func (a mdContainerAdapter) Signal(ctx context.Context, pid int, sig string) error {
	return a.c.Signal(ctx, pid, sig)
}

// Backend adapts *md.Client to runtime.Backend.
type Backend struct {
	client     mdClient
	Provider   genai.Provider      // nil if LLM not configured
	HarnessEnv map[string][]string // per-harness KEY=VALUE env vars from config

	mu                sync.Mutex
	containers        map[string]mdContainer // keyed by runtime instance name
	pendingContainers map[string]mdContainer // keyed by runtime instance name
	vncPorts          map[string]int32       // runtime instance name to host VNC port
}

// NewBackend creates a Backend wrapping the given md client.
func NewBackend(client *md.Client) *Backend {
	return &Backend{
		client:     mdClientAdapter{client},
		containers: make(map[string]mdContainer),
		vncPorts:   make(map[string]int32),
	}
}

// Launch implements runtime.Backend.
func (b *Backend) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.InstanceID, error) {
	defer trace.StartRegion(ctx, "container.launch").End()
	if len(repos) > 0 {
		slog.InfoContext(ctx, "md", "phase", "launch", "dir", repos[0].HostPath, "br", repos[0].Branch, "hns", opts.Harness)
	} else {
		slog.InfoContext(ctx, "md", "phase", "launch", "hns", opts.Harness)
	}
	rt := b.client.Runtime()
	slog.DebugContext(ctx, "launch starting", "rt", rt, "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "repos_count", len(repos))
	if _, ok := harnessMap[opts.Harness]; !ok {
		return "", fmt.Errorf("unknown harness %q", opts.Harness)
	}
	slog.DebugContext(ctx, "harness verified", "rt", rt, "harness", opts.Harness)
	slog.DebugContext(ctx, "creating container", "rt", rt, "repos_count", len(repos))
	mdRepos := toMDRepos(repos)
	c, err := b.client.Container(mdRepos...)
	if err != nil {
		return "", err
	}
	mdOpts, err := b.mdStartOpts(c, opts)
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
	if b.containers == nil {
		b.containers = make(map[string]mdContainer)
	}
	if b.vncPorts == nil {
		b.vncPorts = make(map[string]int32)
	}
	b.pendingContainers[name] = c
	b.containers[name] = c
	if port := c.VNCPort(); port != 0 {
		b.vncPorts[name] = port
	}
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
	mdOpts, err := b.mdStartOpts(c, opts)
	if err != nil {
		return runtime.ConnectionInfo{}, err
	}
	stdout, stderr := logWriters(opts.LogWriter, "connect")
	slog.DebugContext(ctx, "calling connect", "rt", rt, "ctr", name)
	sr, err := c.Connect(ctx, stdout, stderr, mdOpts)
	if err != nil {
		slog.ErrorContext(ctx, "connect failed", "rt", rt, "ctr", name, "err", err)
		return runtime.ConnectionInfo{}, err
	}
	slog.DebugContext(ctx, "connect succeeded", "rt", rt, "ctr", name, "fqdn", sr.TailscaleFQDN, "authurl", sr.TailscaleAuthURL)
	return runtime.ConnectionInfo{
		AgentTarget:      runtime.ConnectionTarget{SSHHost: name},
		TailscaleFQDN:    sr.TailscaleFQDN,
		TailscaleAuthURL: sr.TailscaleAuthURL,
	}, nil
}

// Diff implements runtime.Backend.
func (b *Backend) Diff(ctx context.Context, id runtime.InstanceID, repoIdx int, args ...string) (string, error) {
	defer trace.StartRegion(ctx, "instance.diff").End()
	name := string(id)
	ct, err := b.container(ctx, name)
	if err != nil {
		return "", err
	}
	repos := ct.Repos()
	if repoIdx < 0 || repoIdx >= len(repos) {
		return "", fmt.Errorf("repo index %d out of range for %d repos", repoIdx, len(repos))
	}
	repo := &repos[repoIdx]
	slog.DebugContext(ctx, "md diff", "ctr", name, "dir", repo.GitRoot, "br", repo.Branch, "args", args)
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
	ct, err := b.container(ctx, name)
	if err != nil {
		return err
	}
	repos := ct.Repos()
	if len(repos) > 0 {
		slog.DebugContext(ctx, "md fetch", "ctr", name, "dir", repos[0].GitRoot, "br", repos[0].Branch)
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
	slog.InfoContext(ctx, "md stop", "name", name)
	ct, err := b.container(ctx, name)
	if err != nil {
		return err
	}
	return ct.Stop(ctx)
}

// Purge implements runtime.Backend.
func (b *Backend) Purge(ctx context.Context, id runtime.InstanceID) error {
	defer trace.StartRegion(ctx, "instance.purge").End()
	name := string(id)
	ct, err := b.container(ctx, name)
	if err != nil {
		return err
	}
	ctRepos := ct.Repos()
	if len(ctRepos) > 0 {
		slog.InfoContext(ctx, "md purge", "ctr", name, "dir", ctRepos[0].GitRoot, "br", ctRepos[0].Branch)
	} else {
		slog.InfoContext(ctx, "md purge", "name", name)
	}
	if err := ct.Purge(ctx, &SlogWriter{Phase: "purge"}, &SlogWriter{Phase: "purge"}); err != nil {
		return err
	}
	b.forgetContainer(name)
	return nil
}

// Revive implements runtime.Backend.
func (b *Backend) Revive(ctx context.Context, id runtime.InstanceID) error {
	defer trace.StartRegion(ctx, "instance.revive").End()
	name := string(id)
	rt := b.client.Runtime()
	ct, err := b.container(ctx, name)
	if err != nil {
		return err
	}
	ctRepos := ct.Repos()
	if len(ctRepos) > 0 {
		slog.InfoContext(ctx, "md revive", "ctr", name, "dir", ctRepos[0].GitRoot, "br", ctRepos[0].Branch)
	} else {
		slog.InfoContext(ctx, "md revive", "name", name)
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
func (b *Backend) Fork(ctx context.Context, id runtime.InstanceID, repos []runtime.Repo, opts *runtime.ForkOptions) (runtime.InstanceID, runtime.ConnectionInfo, []runtime.Repo, error) {
	defer trace.StartRegion(ctx, "instance.fork").End()
	name := string(id)
	if len(repos) > 0 {
		slog.InfoContext(ctx, "md", "phase", "fork", "src", name, "dir", repos[0].HostPath, "br", repos[0].Branch)
	}
	rt := b.client.Runtime()
	slog.DebugContext(ctx, "fork starting", "rt", rt, "source", name, "repos_count", len(repos))

	// Look up the source instance so Fork inherits Display, Tailscale,
	// USB, and Sudo from the source unless explicitly overridden by opts.
	ct, err := b.container(ctx, name)
	if err != nil {
		return "", runtime.ConnectionInfo{}, nil, fmt.Errorf("source instance %s: %w", name, err)
	}
	ct.SetState("running")
	mounts, err := mdMounts(ct, opts.Harness, opts.Mounts)
	if err != nil {
		return "", runtime.ConnectionInfo{}, nil, err
	}
	slog.DebugContext(ctx, "building fork options", "rt", rt, "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "sudo", opts.Sudo)
	forkOpts := &md.ForkOpts{
		ExtraRepos: toMDRepos(opts.ExtraRepos),
		Display:    opts.Display,
		Tailscale:  opts.Tailscale,
		USB:        opts.USB,
		Sudo:       opts.Sudo,
		Labels:     metadataLabels(opts.Metadata),
		ExtraEnv:   opts.ExtraEnv,
		Mounts:     mounts,
		MaxCPUs:    maxCPUsOrDefault(opts.MaxCPUs),
	}
	stdout, stderr := logWriters(opts.LogWriter, "fork")
	slog.DebugContext(ctx, "calling fork", "rt", rt, "source", name)
	forked, err := ct.Fork(ctx, stdout, stderr, forkOpts)
	if err != nil {
		slog.ErrorContext(ctx, "fork failed", "rt", rt, "source", name, "err", err)
		return "", runtime.ConnectionInfo{}, nil, err
	}
	forkName := forked.Name()
	slog.DebugContext(ctx, "fork succeeded", "rt", rt, "source", name, "fork", forkName)
	b.rememberContainer(forkName, forked)
	return runtime.InstanceID(forkName), runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: forkName}}, fromMDRepos(forked.Repos()), nil
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
	// Fallback: resolve a proper md container. Handles server restarts where the
	// in-memory map is empty but the instance is still running with a display.
	ct, err := b.container(ctx, instanceID)
	if err != nil {
		return 0
	}
	port = int(ct.VNCPort())
	if port != 0 {
		b.mu.Lock()
		b.vncPorts[instanceID] = int32(port) //nolint:gosec // port numbers are 1-65535, safe for int32
		b.mu.Unlock()
	}
	return port
}

// Processes returns the process list inside the runtime instance.
func (b *Backend) Processes(ctx context.Context, id runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	ct, err := b.container(ctx, string(id))
	if err != nil {
		return nil, err
	}
	procs, err := ct.Processes(ctx)
	if err != nil {
		return nil, err
	}
	return fromMDProcessInfos(procs), nil
}

// Signal sends a signal to a process inside the runtime instance.
func (b *Backend) Signal(ctx context.Context, id runtime.InstanceID, pid int, sig string) error {
	ct, err := b.container(ctx, string(id))
	if err != nil {
		return err
	}
	return ct.Signal(ctx, pid, sig)
}

func (b *Backend) container(ctx context.Context, name string) (mdContainer, error) {
	b.mu.Lock()
	c := b.containers[name]
	b.mu.Unlock()
	if c != nil {
		return c, nil
	}
	c, err := b.client.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	b.rememberContainer(name, c)
	return c, nil
}

func (b *Backend) rememberContainer(name string, c mdContainer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rememberContainerLocked(name, c)
}

func (b *Backend) rememberMDContainers(containers []*md.Container) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range containers {
		if c.Name == "" {
			continue
		}
		b.rememberContainerLocked(c.Name, mdContainerAdapter{c})
	}
}

func (b *Backend) rememberContainerLocked(name string, c mdContainer) {
	if b.containers == nil {
		b.containers = make(map[string]mdContainer)
	}
	b.containers[name] = c
	if b.vncPorts == nil {
		b.vncPorts = make(map[string]int32)
	}
	if port := c.VNCPort(); port != 0 {
		b.vncPorts[name] = port
	}
}

func (b *Backend) forgetContainer(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.containers, name)
	delete(b.pendingContainers, name)
	delete(b.vncPorts, name)
}

// harnessMap maps caic harnesses to their md equivalents.
var harnessMap = map[harness.Name]md.Harness{
	harness.Claude:   md.HarnessClaude,
	harness.Codex:    md.HarnessCodex,
	harness.OpenCode: md.HarnessOpencode,
	harness.Pi:       md.HarnessPi,
}

// mdStartOpts builds the md.StartOpts for a given harness and task options.
func (b *Backend) mdStartOpts(c mdContainer, opts *runtime.StartOptions) (*md.StartOpts, error) {
	image := opts.BaseImage
	if image == "" {
		image = md.DefaultBaseImage + ":latest"
	}
	mounts, err := mdMounts(c, opts.Harness, opts.Mounts)
	if err != nil {
		return nil, err
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
		BaseImage: image,
		Platform:  opts.ContainerPlatform,
		Caches:    toMDCacheMounts(opts.Caches),
		Labels:    metadataLabels(opts.Metadata),
		USB:       opts.USB,
		Tailscale: opts.Tailscale,
		Display:   opts.Display,
		Sudo:      opts.Sudo,
		ExtraEnv:  extraEnv,
		Mounts:    mounts,
		MaxCPUs:   maxCPUsOrDefault(opts.MaxCPUs),
	}, nil
}

func mdMounts(c mdContainer, h harness.Name, mounts []runtime.Mount) ([]md.Mount, error) {
	mdH, ok := harnessMap[h]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q", h)
	}
	agentMounts, err := c.AgentMounts(md.HarnessMounts[mdH])
	if err != nil {
		return nil, err
	}
	return append(agentMounts, toMDMounts(mounts)...), nil
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

func fromMDProcessInfos(procs []md.ProcessInfo) []runtime.ProcessInfo {
	if len(procs) == 0 {
		return nil
	}
	out := make([]runtime.ProcessInfo, len(procs))
	for i, p := range procs {
		out[i] = runtime.ProcessInfo{
			PID:     p.PID,
			PPID:    p.PPID,
			User:    p.User,
			State:   p.State,
			CPU:     p.CPU,
			Mem:     p.Mem,
			Time:    p.Time,
			Command: p.Command,
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
