// Backend adapts md containers to runtime.Lifecycle for launching and managing runtime instances.

package mdruntime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"runtime/trace"
	"slices"
	"sync"

	"github.com/caic-xyz/md"
	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
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
	List(ctx context.Context) ([]runtime.Instance, error)
	Metadata(ctx context.Context, id runtime.InstanceID, key runtime.MetadataKey) (map[string]string, error)
	Inspect(ctx context.Context, id runtime.InstanceID) (*runtime.InstanceInspect, error)
	WatchStats(ctx context.Context, ids []runtime.InstanceID) (iter.Seq2[runtime.StatsSample, error], error)
	WatchEvents(ctx context.Context, filter runtime.EventFilter) (<-chan runtime.Event, error)
	SudoPassword(ctx context.Context, id runtime.InstanceID) (string, error)
}

// mdContainer is the subset of *md.Container that Backend drives. It exposes
// the handful of mutated fields as accessors so a fake need not embed md types.
type mdContainer interface {
	Name() string
	SetName(name string)
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
	_ mdClient       = mdClientAdapter{}
	_ mdContainer    = mdContainerAdapter{}
	_ runtime.System = (*Backend)(nil)
)

// mdClientAdapter adapts *md.Client to mdClient.
type mdClientAdapter struct {
	// Immutable.
	log *slog.Logger
	c   *md.Client
}

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

func (a mdClientAdapter) List(ctx context.Context) ([]runtime.Instance, error) {
	containers, err := a.c.List(ctx)
	if err != nil {
		return nil, err
	}
	return InstancesFromMD(ctx, containers), nil
}

func (a mdClientAdapter) Inspect(ctx context.Context, id runtime.InstanceID) (*runtime.InstanceInspect, error) {
	info, err := (&md.Container{Client: a.c, Name: string(id)}).Inspect(ctx)
	if err != nil {
		return nil, err
	}
	mounts := make([]runtime.Mount, len(info.Mounts))
	for i, m := range info.Mounts {
		mounts[i] = runtime.Mount{HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly}
	}
	caches := make([]runtime.CacheMount, len(info.Caches))
	for i, c := range info.Caches {
		caches[i] = runtime.CacheMount{
			Name:          c.Name,
			Description:   c.Description,
			HostPath:      c.HostPath,
			ContainerPath: c.ContainerPath,
			ReadOnly:      c.ReadOnly,
			Shallow:       c.Shallow,
		}
	}
	inspectID := runtime.ID(info.ID)
	if inspectID == "" {
		inspectID = runtime.ID(id)
	}
	return &runtime.InstanceInspect{
		Runtime:         info.Runtime,
		ID:              inspectID,
		State:           info.State,
		ImageRef:        info.ImageRef,
		ImageID:         info.ImageID,
		OS:              info.OS,
		CPUArchitecture: info.Architecture,
		CPULimit:        info.CPULimit,
		Mounts:          mounts,
		Caches:          caches,
	}, nil
}

func (a mdClientAdapter) WatchStats(ctx context.Context, ids []runtime.InstanceID) (iter.Seq2[runtime.StatsSample, error], error) {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = string(id)
	}
	stats, err := a.c.Runtime.WatchStats(ctx, names)
	if err != nil {
		return nil, err
	}
	return func(yield func(runtime.StatsSample, error) bool) {
		for sample, err := range stats {
			if err != nil {
				_ = yield(runtime.StatsSample{}, err)
				return
			}
			out := runtime.StatsSample{
				InstanceID: runtime.ID(sample.Name),
				Stats: runtime.Stats{
					CPUPerc:    sample.Stats.CPUPerc,
					MemUsed:    sample.Stats.MemUsed,
					MemLimit:   sample.Stats.MemLimit,
					MemPerc:    sample.Stats.MemPerc,
					NetRx:      sample.Stats.NetRx,
					NetTx:      sample.Stats.NetTx,
					BlockRead:  sample.Stats.BlockRead,
					BlockWrite: sample.Stats.BlockWrite,
					DiskUsed:   sample.Stats.DiskUsed,
				},
			}
			if !yield(out, nil) {
				return
			}
		}
	}, nil
}

func (a mdClientAdapter) SudoPassword(ctx context.Context, id runtime.InstanceID) (string, error) {
	return (&md.Container{Client: a.c, Name: string(id)}).SudoPassword(ctx)
}

func (a mdClientAdapter) Metadata(ctx context.Context, id runtime.InstanceID, _ runtime.MetadataKey) (map[string]string, error) {
	ct, err := a.c.Get(ctx, string(id))
	if err != nil {
		return nil, err
	}
	return cloneLabelMap(ct.Labels), nil
}

func (a mdClientAdapter) WatchEvents(ctx context.Context, filter runtime.EventFilter) (<-chan runtime.Event, error) {
	events, err := a.c.Runtime.WatchDieEvents(ctx, string(filter.MetadataKey))
	if err != nil {
		return nil, err
	}
	out := make(chan runtime.Event, 16)
	go func() {
		defer close(out)
		for ev, err := range events {
			if err != nil {
				if ctx.Err() == nil {
					a.log.WarnContext(ctx, "runtime events stream failed", "err", err)
				}
				return
			}
			select {
			case out <- runtime.Event{InstanceID: runtime.ID(ev.Name)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// mdContainerAdapter adapts *md.Container to mdContainer.
type mdContainerAdapter struct{ c *md.Container }

func (a mdContainerAdapter) Name() string        { return a.c.Name }
func (a mdContainerAdapter) SetName(name string) { a.c.Name = name }
func (a mdContainerAdapter) VNCPort() int32      { return a.c.VNCPort }
func (a mdContainerAdapter) Repos() []md.Repo    { return a.c.Repos }
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

// Backend adapts *md.Client to runtime.Lifecycle.
type Backend struct {
	Provider   genai.Provider      // nil if LLM not configured
	HarnessEnv map[string][]string // per-harness KEY=VALUE env vars from config

	// Immutable.
	log    *slog.Logger
	client mdClient

	// Guarded by mu.
	mu                sync.Mutex
	containers        map[string]mdContainer           // keyed by runtime instance name
	pendingContainers map[string]mdContainer           // keyed by runtime instance name
	vncPorts          map[string]int32                 // runtime instance name to host VNC port
	labels            map[runtime.ID]map[string]string // keyed by qualified runtime instance ID
}

// NewBackend creates a Backend wrapping the given md client.
func NewBackend(client *md.Client) *Backend {
	log := slog.Default().With(slog.String("cmp", "md"), slog.String("runtime", client.Runtime.Name()))
	return &Backend{
		log:        log,
		client:     mdClientAdapter{log: log, c: client},
		containers: make(map[string]mdContainer),
		vncPorts:   make(map[string]int32),
	}
}

// Name returns the runtime backend name.
func (b *Backend) Name() runtime.Name { return runtime.Name(b.client.Runtime()) }

// Launch implements runtime.Lifecycle.
func (b *Backend) Launch(ctx context.Context, repos []runtime.Repo, opts *runtime.StartOptions) (runtime.ID, error) {
	defer trace.StartRegion(ctx, "container.launch").End()
	if len(repos) > 0 {
		b.log.InfoContext(ctx, "md", "phase", "launch", "dir", repos[0].GitRoot, "br", repos[0].Branch, "hns", opts.Harness)
	} else {
		b.log.InfoContext(ctx, "md", "phase", "launch", "hns", opts.Harness)
	}
	b.log.DebugContext(ctx, "launch starting", "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "repos_count", len(repos))
	if _, ok := harnessMap[opts.Harness]; !ok {
		return "", fmt.Errorf("unknown harness %q", opts.Harness)
	}
	b.log.DebugContext(ctx, "harness verified", "harness", opts.Harness)
	b.log.DebugContext(ctx, "creating container", "repos_count", len(repos))
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
	b.log.DebugContext(ctx, "launching")
	if err := c.Launch(ctx, stdout, stderr, mdOpts); err != nil {
		b.log.ErrorContext(ctx, "launch failed", "err", err)
		return "", err
	}
	name := c.Name()
	b.log.DebugContext(ctx, "launch succeeded", "ctr", name)
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
	b.log.DebugContext(ctx, "launch returning", "ctr", name)
	return runtime.NewID(b.Name(), runtime.InstanceID(name)), nil
}

// Connect implements runtime.Lifecycle.
func (b *Backend) Connect(ctx context.Context, id runtime.ID, opts *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	defer trace.StartRegion(ctx, "container.connect").End()
	localID, err := b.localID(id)
	if err != nil {
		return runtime.ConnectionInfo{}, err
	}
	name := string(localID)
	b.log.DebugContext(ctx, "connect starting", "ctr", name)
	b.mu.Lock()
	c, ok := b.pendingContainers[name]
	if ok {
		delete(b.pendingContainers, name)
	}
	b.mu.Unlock()
	if !ok {
		b.log.DebugContext(ctx, "no pending container", "ctr", name)
		return runtime.ConnectionInfo{}, fmt.Errorf("no pending container %q", name)
	}
	b.log.DebugContext(ctx, "found pending container", "ctr", name)
	mdOpts, err := b.mdStartOpts(c, opts)
	if err != nil {
		return runtime.ConnectionInfo{}, err
	}
	stdout, stderr := logWriters(opts.LogWriter, "connect")
	b.log.DebugContext(ctx, "calling connect", "ctr", name)
	sr, err := c.Connect(ctx, stdout, stderr, mdOpts)
	if err != nil {
		b.log.ErrorContext(ctx, "connect failed", "ctr", name, "err", err)
		return runtime.ConnectionInfo{}, err
	}
	b.log.DebugContext(ctx, "connect succeeded", "ctr", name, "fqdn", sr.TailscaleFQDN, "authurl", sr.TailscaleAuthURL)
	return runtime.ConnectionInfo{
		AgentTarget:      runtime.ConnectionTarget{SSHHost: name},
		TailscaleFQDN:    sr.TailscaleFQDN,
		TailscaleAuthURL: sr.TailscaleAuthURL,
	}, nil
}

// Diff implements runtime.Lifecycle.
func (b *Backend) Diff(ctx context.Context, id runtime.ID, repoIdx int, args ...string) (string, error) {
	defer trace.StartRegion(ctx, "instance.diff").End()
	localID, err := b.localID(id)
	if err != nil {
		return "", err
	}
	name := string(localID)
	ct, err := b.container(ctx, name)
	if err != nil {
		return "", err
	}
	repos := ct.Repos()
	if repoIdx < 0 || repoIdx >= len(repos) {
		return "", fmt.Errorf("repo index %d out of range for %d repos", repoIdx, len(repos))
	}
	repo := &repos[repoIdx]
	b.log.DebugContext(ctx, "md diff", "ctr", name, "dir", repo.GitRoot, "br", primaryBranch(repo), "args", args)
	var stdout bytes.Buffer
	if err := ct.Diff(ctx, &stdout, &SlogWriter{Phase: "diff"}, repoIdx, args); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// Fetch implements runtime.Lifecycle.
func (b *Backend) Fetch(ctx context.Context, id runtime.ID) error {
	defer trace.StartRegion(ctx, "instance.fetch").End()
	localID, err := b.localID(id)
	if err != nil {
		return err
	}
	name := string(localID)
	ct, err := b.container(ctx, name)
	if err != nil {
		return err
	}
	repos := ct.Repos()
	if len(repos) > 0 {
		b.log.DebugContext(ctx, "md fetch", "ctr", name, "dir", repos[0].GitRoot, "br", primaryBranch(&repos[0]))
	}
	for i := range repos {
		if err := ct.Fetch(ctx, &SlogWriter{Phase: "fetch"}, &SlogWriter{Phase: "fetch"}, i, b.Provider); err != nil {
			return err
		}
	}
	return nil
}

// Stop implements runtime.Lifecycle.
func (b *Backend) Stop(ctx context.Context, id runtime.ID) error {
	defer trace.StartRegion(ctx, "instance.stop").End()
	localID, err := b.localID(id)
	if err != nil {
		return err
	}
	name := string(localID)
	b.log.InfoContext(ctx, "md stop", "name", name)
	ct, err := b.container(ctx, name)
	if err != nil {
		return err
	}
	return ct.Stop(ctx)
}

// Purge implements runtime.Lifecycle.
func (b *Backend) Purge(ctx context.Context, id runtime.ID) error {
	defer trace.StartRegion(ctx, "instance.purge").End()
	localID, err := b.localID(id)
	if err != nil {
		return err
	}
	name := string(localID)
	ct, err := b.container(ctx, name)
	if err != nil {
		return err
	}
	ctRepos := ct.Repos()
	if len(ctRepos) > 0 {
		b.log.InfoContext(ctx, "md purge", "ctr", name, "dir", ctRepos[0].GitRoot, "br", primaryBranch(&ctRepos[0]))
	} else {
		b.log.InfoContext(ctx, "md purge", "name", name)
	}
	if err := ct.Purge(ctx, &SlogWriter{Phase: "purge"}, &SlogWriter{Phase: "purge"}); err != nil {
		return err
	}
	b.forgetContainer(name)
	return nil
}

// Revive implements runtime.Lifecycle.
func (b *Backend) Revive(ctx context.Context, id runtime.ID) error {
	defer trace.StartRegion(ctx, "instance.revive").End()
	localID, err := b.localID(id)
	if err != nil {
		return err
	}
	name := string(localID)
	ct, err := b.container(ctx, name)
	if err != nil {
		return err
	}
	ctRepos := ct.Repos()
	if len(ctRepos) > 0 {
		b.log.InfoContext(ctx, "md revive", "ctr", name, "dir", ctRepos[0].GitRoot, "br", primaryBranch(&ctRepos[0]))
	} else {
		b.log.InfoContext(ctx, "md revive", "name", name)
	}
	b.log.DebugContext(ctx, "revive starting", "ctr", name, "repos_count", len(ctRepos))
	b.log.DebugContext(ctx, "calling revive", "ctr", name)
	if err = ct.Revive(ctx, &SlogWriter{Phase: "revive"}, &SlogWriter{Phase: "revive"}); err != nil {
		b.log.ErrorContext(ctx, "revive failed", "ctr", name, "err", err)
		return err
	}
	b.log.DebugContext(ctx, "revive succeeded", "ctr", name)
	// VNC port may have changed after restart (port remapping).
	b.mu.Lock()
	b.vncPorts[name] = ct.VNCPort()
	b.mu.Unlock()
	return nil
}

// Fork implements runtime.Lifecycle.
func (b *Backend) Fork(ctx context.Context, id runtime.ID, opts *runtime.ForkOptions) (runtime.ID, runtime.ConnectionInfo, []runtime.Repo, error) {
	defer trace.StartRegion(ctx, "instance.fork").End()
	localID, err := b.localID(id)
	if err != nil {
		return "", runtime.ConnectionInfo{}, nil, err
	}
	name := string(localID)
	if len(opts.Repos) > 0 {
		b.log.InfoContext(ctx, "md", "phase", "fork", "src", name, "dir", opts.Repos[0].GitRoot, "br", opts.Repos[0].DestPrimary)
	}
	b.log.DebugContext(ctx, "fork starting", "source", name, "repos_count", len(opts.Repos))

	// Look up the source instance so Fork inherits Display, Tailscale,
	// USB, and Sudo from the source unless explicitly overridden by opts.
	ct, err := b.container(ctx, name)
	if err != nil {
		return "", runtime.ConnectionInfo{}, nil, fmt.Errorf("source instance %s: %w", name, err)
	}
	mounts, err := mdMounts(ct, opts.Harness, opts.Mounts)
	if err != nil {
		return "", runtime.ConnectionInfo{}, nil, err
	}
	b.log.DebugContext(ctx, "building fork options", "harness", opts.Harness, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display, "sudo", opts.Sudo)
	forkRepos := make([]md.ForkRepo, len(opts.Repos))
	for i, r := range opts.Repos {
		forkRepos[i] = md.ForkRepo{GitRoot: r.GitRoot, SourceBranches: r.SourceBranches, ContainerPath: r.ContainerPath, DestPrimary: r.DestPrimary}
	}
	forkOpts := &md.ForkOpts{
		Repos:     forkRepos,
		Display:   opts.Display,
		Tailscale: opts.Tailscale,
		USB:       opts.USB,
		Sudo:      opts.Sudo,
		Labels:    metadataLabels(opts.Metadata),
		ExtraEnv:  opts.ExtraEnv,
		Mounts:    mounts,
		MaxCPUs:   maxCPUsOrDefault(opts.MaxCPUs),
	}
	stdout, stderr := logWriters(opts.LogWriter, "fork")
	b.log.DebugContext(ctx, "calling fork", "source", name)
	forked, err := ct.Fork(ctx, stdout, stderr, forkOpts)
	if err != nil {
		b.log.ErrorContext(ctx, "fork failed", "source", name, "err", err)
		return "", runtime.ConnectionInfo{}, nil, err
	}
	forkName := forked.Name()
	b.log.DebugContext(ctx, "fork succeeded", "source", name, "fork", forkName)
	b.rememberContainer(forkName, forked)
	return runtime.NewID(b.Name(), runtime.InstanceID(forkName)), runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: forkName}}, fromMDRepos(forked.Repos()), nil
}

// VNCPort implements runtime.Lifecycle.
func (b *Backend) VNCPort(ctx context.Context, id runtime.ID) int {
	localID, err := b.localID(id)
	if err != nil {
		return 0
	}
	instanceID := string(localID)
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
func (b *Backend) Processes(ctx context.Context, id runtime.ID) ([]runtime.ProcessInfo, error) {
	localID, err := b.localID(id)
	if err != nil {
		return nil, err
	}
	ct, err := b.container(ctx, string(localID))
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
func (b *Backend) Signal(ctx context.Context, id runtime.ID, pid int, sig string) error {
	localID, err := b.localID(id)
	if err != nil {
		return err
	}
	ct, err := b.container(ctx, string(localID))
	if err != nil {
		return err
	}
	return ct.Signal(ctx, pid, sig)
}

// List returns known runtime instances.
func (b *Backend) List(ctx context.Context) ([]runtime.Instance, error) {
	instances, err := b.client.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range instances {
		instances[i].ID = runtime.NewID(b.Name(), instances[i].ID.InstanceID())
	}
	return instances, nil
}

// Inspect returns observed runtime configuration for a runtime instance.
func (b *Backend) Inspect(ctx context.Context, id runtime.ID) (*runtime.InstanceInspect, error) {
	localID, err := b.localID(id)
	if err != nil {
		return nil, err
	}
	info, err := b.client.Inspect(ctx, localID)
	if err != nil {
		return nil, err
	}
	info.ID = runtime.NewID(b.Name(), info.ID.InstanceID())
	return info, nil
}

// WatchStats streams resource stats for the named runtime instances.
func (b *Backend) WatchStats(ctx context.Context, ids []runtime.ID) (iter.Seq2[runtime.StatsSample, error], error) {
	localIDs := make([]runtime.InstanceID, len(ids))
	for i, id := range ids {
		localID, err := b.localID(id)
		if err != nil {
			return nil, err
		}
		localIDs[i] = localID
	}
	seq, err := b.client.WatchStats(ctx, localIDs)
	if err != nil {
		return nil, err
	}
	return func(yield func(runtime.StatsSample, error) bool) {
		for sample, err := range seq {
			if err != nil {
				_ = yield(runtime.StatsSample{}, err)
				return
			}
			sample.InstanceID = runtime.NewID(b.Name(), sample.InstanceID.InstanceID())
			if !yield(sample, nil) {
				return
			}
		}
	}, nil
}

// SudoPassword fetches the sudo password for a runtime instance over SSH.
func (b *Backend) SudoPassword(ctx context.Context, id runtime.ID) (string, error) {
	localID, err := b.localID(id)
	if err != nil {
		return "", err
	}
	return b.client.SudoPassword(ctx, localID)
}

// Metadata reads a single runtime instance metadata value, returning "" when unset.
func (b *Backend) Metadata(ctx context.Context, id runtime.ID, key runtime.MetadataKey) (string, error) {
	if err := b.validateID(id); err != nil {
		return "", err
	}
	if value, ok := b.cachedLabel(id, string(key)); ok {
		return value, nil
	}
	labels, err := b.client.Metadata(ctx, id.InstanceID(), key)
	if err != nil {
		return "", err
	}
	b.rememberLabelMap(id, labels)
	return labels[string(key)], nil
}

// WatchEvents streams lifecycle events for instances matching filter.
func (b *Backend) WatchEvents(ctx context.Context, filter runtime.EventFilter) (<-chan runtime.Event, error) {
	ch, err := b.client.WatchEvents(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make(chan runtime.Event, 16)
	go func() {
		defer close(out)
		for ev := range ch {
			ev.InstanceID = runtime.NewID(b.Name(), ev.InstanceID.InstanceID())
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
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

func (b *Backend) localID(id runtime.ID) (runtime.InstanceID, error) {
	if err := b.validateID(id); err != nil {
		return "", err
	}
	return id.InstanceID(), nil
}

func (b *Backend) validateID(id runtime.ID) error {
	if id.RuntimeName() != b.Name() {
		return fmt.Errorf("runtime %q cannot use instance %q", b.Name(), id)
	}
	return nil
}

func (b *Backend) rememberLabelMap(id runtime.ID, labels map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.labels == nil {
		b.labels = make(map[runtime.ID]map[string]string)
	}
	b.labels[id] = cloneLabelMap(labels)
}

func (b *Backend) cachedLabel(id runtime.ID, key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	labels, ok := b.labels[id]
	if !ok {
		return "", false
	}
	return labels[key], true
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
		var branches []string
		if r.Branch != "" {
			branches = []string{r.Branch}
		}
		out[i] = md.Repo{
			GitRoot:       r.GitRoot,
			Branches:      branches,
			ContainerPath: r.ContainerPath,
			DefaultRemote: r.Remote,
			DefaultBranch: r.BaseBranch,
		}
	}
	return out
}

// primaryBranch returns the primary branch of an md repo, or "" if it has none.
func primaryBranch(r *md.Repo) string {
	if len(r.Branches) == 0 {
		return ""
	}
	return r.Branches[0]
}

func fromMDRepos(repos []md.Repo) []runtime.Repo {
	if len(repos) == 0 {
		return nil
	}
	out := make([]runtime.Repo, len(repos))
	for i := range repos {
		r := &repos[i]
		out[i] = runtime.Repo{
			GitRoot:       r.GitRoot,
			ContainerPath: r.ContainerPath,
			Branch:        primaryBranch(r),
			BaseBranch:    r.DefaultBranch,
			Remote:        r.DefaultRemote,
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
			ContainerPath: c.ContainerPath,
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
			ContainerPath: m.ContainerPath,
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
