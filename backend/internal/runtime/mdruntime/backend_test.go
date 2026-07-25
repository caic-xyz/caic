// Tests for Backend's runtime.Lifecycle logic using fake md seams.

package mdruntime

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"slices"
	"testing"

	"github.com/caic-xyz/md"
	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// fakeMDContainer is a fake mdContainer that records driven operations and
// returns configurable results.
type fakeMDContainer struct {
	name    string
	vncPort int32
	repo    []md.Repo
	diffIdx int

	launchErr   error
	connectRes  *md.StartResult
	connectErr  error
	stopErr     error
	forkResult  mdContainer
	forkErr     error
	agentMounts []md.Mount
	agentErr    error
	processes   []md.ProcessInfo
	processErr  error
	signalErr   error

	calls        []string
	agentPaths   []md.AgentPaths
	forkOpts     *md.ForkOpts
	processCalls int
	signalCalls  int
	signalPID    int
	signalSig    string
}

func (f *fakeMDContainer) Name() string     { return f.name }
func (f *fakeMDContainer) SetName(n string) { f.name = n }
func (f *fakeMDContainer) VNCPort() int32   { return f.vncPort }
func (f *fakeMDContainer) Repos() []md.Repo { return f.repo }

func (f *fakeMDContainer) AgentMounts(paths ...md.AgentPaths) ([]md.Mount, error) {
	f.agentPaths = append([]md.AgentPaths(nil), paths...)
	if f.agentErr != nil {
		return nil, f.agentErr
	}
	return slices.Clone(f.agentMounts), nil
}

func (f *fakeMDContainer) Launch(_ context.Context, _, _ io.Writer, _ *md.StartOpts) error {
	f.calls = append(f.calls, "Launch")
	return f.launchErr
}

func (f *fakeMDContainer) Connect(_ context.Context, _, _ io.Writer, _ *md.StartOpts) (*md.StartResult, error) {
	f.calls = append(f.calls, "Connect")
	if f.connectErr != nil {
		return nil, f.connectErr
	}
	if f.connectRes != nil {
		return f.connectRes, nil
	}
	return &md.StartResult{}, nil
}

func (f *fakeMDContainer) Diff(_ context.Context, _, _ io.Writer, repoIdx int, _ []string) error {
	f.calls = append(f.calls, "Diff")
	f.diffIdx = repoIdx
	return nil
}

func (f *fakeMDContainer) Fetch(_ context.Context, _, _ io.Writer, _ int, _ genai.Provider) error {
	f.calls = append(f.calls, "Fetch")
	return nil
}

func (f *fakeMDContainer) Stop(_ context.Context) error {
	f.calls = append(f.calls, "Stop")
	return f.stopErr
}

func (f *fakeMDContainer) Purge(_ context.Context, _, _ io.Writer) error {
	f.calls = append(f.calls, "Purge")
	return nil
}

func (f *fakeMDContainer) Revive(_ context.Context, _, _ io.Writer) error {
	f.calls = append(f.calls, "Revive")
	return nil
}

func (f *fakeMDContainer) Fork(_ context.Context, _, _ io.Writer, opts *md.ForkOpts) (mdContainer, error) {
	f.calls = append(f.calls, "Fork")
	f.forkOpts = opts
	if f.forkErr != nil {
		return nil, f.forkErr
	}
	return f.forkResult, nil
}

func (f *fakeMDContainer) Processes(context.Context) ([]md.ProcessInfo, error) {
	f.processCalls++
	if f.processErr != nil {
		return nil, f.processErr
	}
	return slices.Clone(f.processes), nil
}

func (f *fakeMDContainer) Signal(_ context.Context, pid int, sig string) error {
	f.signalCalls++
	f.signalPID = pid
	f.signalSig = sig
	return f.signalErr
}

// fakeMDClient is a fake mdClient handing out preconfigured containers.
type fakeMDClient struct {
	runtime      string
	container    mdContainer // returned by Container()
	containerErr error
	getResult    mdContainer // returned by Get()
	getErr       error

	containerCalls int
	containerRepos []md.Repo
	getCalls       int
	getName        string
}

func (f *fakeMDClient) Runtime() string {
	if f.runtime == "" {
		return "docker"
	}
	return f.runtime
}

func (f *fakeMDClient) Container(repos ...md.Repo) (mdContainer, error) {
	f.containerCalls++
	f.containerRepos = append([]md.Repo(nil), repos...)
	if f.containerErr != nil {
		return nil, f.containerErr
	}
	if f.container != nil {
		return f.container, nil
	}
	return &fakeMDContainer{}, nil
}

func (f *fakeMDClient) Get(_ context.Context, name string) (mdContainer, error) {
	f.getCalls++
	f.getName = name
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getResult == nil {
		return &fakeMDContainer{}, nil
	}
	return f.getResult, nil
}

func (*fakeMDClient) List(context.Context) ([]runtime.Instance, error) {
	return []runtime.Instance{}, nil
}

func (*fakeMDClient) Metadata(context.Context, runtime.InstanceID, runtime.MetadataKey) (map[string]string, error) {
	return map[string]string{}, nil
}

func (*fakeMDClient) Inspect(context.Context, runtime.InstanceID) (*runtime.InstanceInspect, error) {
	return &runtime.InstanceInspect{}, nil
}

func (*fakeMDClient) WatchStats(context.Context, []runtime.InstanceID) (iter.Seq2[runtime.StatsSample, error], error) {
	return func(func(runtime.StatsSample, error) bool) {}, nil
}

func (*fakeMDClient) WatchEvents(context.Context, runtime.EventFilter) (<-chan runtime.Event, error) {
	ch := make(chan runtime.Event)
	close(ch)
	return ch, nil
}

func (*fakeMDClient) SudoPassword(context.Context, runtime.InstanceID) (string, error) {
	return "", nil
}

func newTestBackend(c mdClient) *Backend {
	return &Backend{log: slog.Default(), client: c, containers: make(map[string]mdContainer), vncPorts: make(map[string]int32)}
}

func TestBackend(t *testing.T) {
	t.Parallel()

	t.Run("Launch", func(t *testing.T) {
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			ctr := &fakeMDContainer{name: "ctr-x", vncPort: 5901}
			b := newTestBackend(&fakeMDClient{container: ctr})
			name, err := b.Launch(t.Context(), nil, &runtime.StartOptions{
				Metadata: runtime.Metadata{runtime.MetadataTaskID: "task-1"},
				Harness:  harness.Claude,
			})
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if name != "docker:ctr-x" {
				t.Errorf("name = %q, want docker:ctr-x", name)
			}
			if !slices.Contains(ctr.calls, "Launch") {
				t.Errorf("Launch not called on container, calls=%v", ctr.calls)
			}
			if _, ok := b.pendingContainers["ctr-x"]; !ok {
				t.Error("container not stored as pending")
			}
			if b.vncPorts["ctr-x"] != 5901 {
				t.Errorf("vncPort = %d, want 5901", b.vncPorts["ctr-x"])
			}
		})
		t.Run("error unknown harness", func(t *testing.T) {
			t.Parallel()
			fc := &fakeMDClient{}
			b := newTestBackend(fc)
			_, err := b.Launch(t.Context(), nil, &runtime.StartOptions{Harness: "bogus"})
			if err == nil {
				t.Fatal("want error for unknown harness")
			}
			if fc.containerCalls != 0 {
				t.Errorf("Container() called %d times, want 0 (should reject before provisioning)", fc.containerCalls)
			}
		})
		t.Run("error launch failure", func(t *testing.T) {
			t.Parallel()
			ctr := &fakeMDContainer{name: "ctr-y", launchErr: errors.New("boom")}
			b := newTestBackend(&fakeMDClient{container: ctr})
			if _, err := b.Launch(t.Context(), nil, &runtime.StartOptions{Harness: harness.Claude}); err == nil {
				t.Fatal("want error from container.Launch")
			}
			if _, ok := b.pendingContainers["ctr-y"]; ok {
				t.Error("failed container should not be stored as pending")
			}
		})
	})

	t.Run("Diff", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{repo: []md.Repo{
			{GitRoot: "/home/user/src/caic", Branches: []string{"caic-7"}, MountedPath: "/home/user/src/caic"},
			{GitRoot: "/home/user/src/genai", Branches: []string{"caic-0"}, MountedPath: "/home/user/src/genai"},
		}}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(fc)
		if _, err := b.Diff(t.Context(), "docker:ctr-1", 1, "--numstat"); err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if fc.getCalls != 1 || fc.getName != "ctr-1" {
			t.Fatalf("Get calls = %d name = %q, want 1 ctr-1", fc.getCalls, fc.getName)
		}
		if fc.containerCalls != 0 {
			t.Fatalf("Container() called %d times, want 0", fc.containerCalls)
		}
		if ctr.diffIdx != 1 {
			t.Errorf("Diff repoIdx = %d, want 1", ctr.diffIdx)
		}
	})

	t.Run("Fetch", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{repo: []md.Repo{
			{GitRoot: "/home/user/src/caic", Branches: []string{"caic-7"}, MountedPath: "/home/user/src/caic"},
			{GitRoot: "/home/user/src/genai", Branches: []string{"caic-0"}, MountedPath: "/home/user/src/genai"},
		}}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(fc)
		if err := b.Fetch(t.Context(), "docker:ctr-1"); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if fc.getCalls != 1 || fc.getName != "ctr-1" {
			t.Fatalf("Get calls = %d name = %q, want 1 ctr-1", fc.getCalls, fc.getName)
		}
		if fc.containerCalls != 0 {
			t.Fatalf("Container() called %d times, want 0", fc.containerCalls)
		}
		gotFetchCalls := 0
		for _, call := range ctr.calls {
			if call == "Fetch" {
				gotFetchCalls++
			}
		}
		if gotFetchCalls != 2 {
			t.Errorf("Fetch calls = %d, want 2", gotFetchCalls)
		}
	})

	t.Run("Connect", func(t *testing.T) {
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			ctr := &fakeMDContainer{name: "ctr-x", connectRes: &md.StartResult{TailscaleFQDN: "host.ts.net"}}
			b := newTestBackend(&fakeMDClient{container: ctr})
			if _, err := b.Launch(t.Context(), nil, &runtime.StartOptions{Harness: harness.Claude}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			conn, err := b.Connect(t.Context(), "docker:ctr-x", &runtime.StartOptions{Harness: harness.Claude})
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if conn.TailscaleFQDN != "host.ts.net" {
				t.Errorf("fqdn = %q, want host.ts.net", conn.TailscaleFQDN)
			}
			// Pending entry must be consumed: a second Connect fails.
			if _, err := b.Connect(t.Context(), "docker:ctr-x", &runtime.StartOptions{Harness: harness.Claude}); err == nil {
				t.Error("second Connect should fail; pending entry not consumed")
			}
		})
		t.Run("error no pending", func(t *testing.T) {
			t.Parallel()
			b := newTestBackend(&fakeMDClient{})
			if _, err := b.Connect(t.Context(), "docker:missing", &runtime.StartOptions{Harness: harness.Claude}); err == nil {
				t.Fatal("want error when no pending container")
			}
		})
	})

	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{name: "ctr-1"}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(fc)
		if err := b.Stop(t.Context(), "docker:ctr-1"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if fc.getCalls != 1 || fc.getName != "ctr-1" {
			t.Fatalf("Get calls = %d name = %q, want 1 ctr-1", fc.getCalls, fc.getName)
		}
		if fc.containerCalls != 0 {
			t.Fatalf("Container() called %d times, want 0", fc.containerCalls)
		}
		if !slices.Contains(ctr.calls, "Stop") {
			t.Errorf("Stop not called, calls=%v", ctr.calls)
		}
	})

	t.Run("Purge", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{name: "ctr-1", repo: []md.Repo{{GitRoot: "/repo", Branches: []string{"caic-0"}, MountedPath: "/repo"}}}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(fc)
		if err := b.Purge(t.Context(), "docker:ctr-1"); err != nil {
			t.Fatalf("Purge: %v", err)
		}
		if fc.getCalls != 1 || fc.getName != "ctr-1" {
			t.Fatalf("Get calls = %d name = %q, want 1 ctr-1", fc.getCalls, fc.getName)
		}
		if fc.containerCalls != 0 {
			t.Fatalf("Container() called %d times, want 0", fc.containerCalls)
		}
		if !slices.Contains(ctr.calls, "Purge") {
			t.Errorf("Purge not called, calls=%v", ctr.calls)
		}
	})

	t.Run("Revive", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{name: "ctr-1", vncPort: 5903, repo: []md.Repo{{GitRoot: "/repo", Branches: []string{"caic-0"}, MountedPath: "/repo"}}}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(fc)
		if err := b.Revive(t.Context(), "docker:ctr-1"); err != nil {
			t.Fatalf("Revive: %v", err)
		}
		if fc.getCalls != 1 || fc.getName != "ctr-1" {
			t.Fatalf("Get calls = %d name = %q, want 1 ctr-1", fc.getCalls, fc.getName)
		}
		if fc.containerCalls != 0 {
			t.Fatalf("Container() called %d times, want 0", fc.containerCalls)
		}
		if !slices.Contains(ctr.calls, "Revive") {
			t.Errorf("Revive not called, calls=%v", ctr.calls)
		}
		if b.vncPorts["ctr-1"] != 5903 {
			t.Errorf("revived vncPort = %d, want 5903", b.vncPorts["ctr-1"])
		}
	})

	t.Run("Fork", func(t *testing.T) {
		t.Parallel()
		src := &fakeMDContainer{forkResult: &fakeMDContainer{name: "fork-1", vncPort: 5902, repo: []md.Repo{{Branches: []string{"caic-2"}}}}}
		b := newTestBackend(&fakeMDClient{getResult: src})
		name, conn, repos, err := b.Fork(t.Context(), "docker:src", &runtime.ForkOptions{Harness: harness.Claude})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		if name != "docker:fork-1" {
			t.Errorf("fork name = %q, want docker:fork-1", name)
		}
		if conn.AgentTarget.SSHHost != "fork-1" {
			t.Errorf("fork agent target = %q, want fork-1", conn.AgentTarget.SSHHost)
		}
		if len(repos) != 1 || repos[0].Branch != "caic-2" {
			t.Errorf("repos = %+v, want one repo on caic-2", repos)
		}
		if b.vncPorts["fork-1"] != 5902 {
			t.Errorf("fork vncPort = %d, want 5902", b.vncPorts["fork-1"])
		}
		if src.forkOpts == nil {
			t.Fatal("Fork options not recorded")
		}
		if src.forkOpts.Sudo {
			t.Error("Fork Sudo = true, want explicit disabled value")
		}
	})

	t.Run("VNCPort", func(t *testing.T) {
		t.Run("valid cached", func(t *testing.T) {
			t.Parallel()
			b := newTestBackend(&fakeMDClient{})
			b.vncPorts["c"] = 4242
			if got := b.VNCPort(t.Context(), "docker:c"); got != 4242 {
				t.Errorf("VNCPort = %d, want 4242 (in-memory hit)", got)
			}
		})
		t.Run("valid inspect fallback", func(t *testing.T) {
			t.Parallel()
			fc := &fakeMDClient{getResult: &fakeMDContainer{vncPort: 5901}}
			b := newTestBackend(fc)
			if got := b.VNCPort(t.Context(), "docker:c"); got != 5901 {
				t.Errorf("VNCPort = %d, want 5901", got)
			}
			if fc.getCalls != 1 || fc.getName != "c" {
				t.Errorf("Get calls = %d name = %q, want 1 c", fc.getCalls, fc.getName)
			}
		})
	})

	t.Run("Processes", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{processes: []md.ProcessInfo{{PID: 7, Command: "agent"}}}
		fc := &fakeMDClient{}
		b := newTestBackend(fc)
		b.containers["ctr"] = ctr
		procs, err := b.Processes(t.Context(), "docker:ctr")
		if err != nil {
			t.Fatalf("Processes: %v", err)
		}
		if len(procs) != 1 || procs[0].PID != 7 || procs[0].Command != "agent" {
			t.Fatalf("Processes = %+v, want agent process", procs)
		}
		if ctr.processCalls != 1 || fc.getCalls != 0 {
			t.Fatalf("process calls = %d getCalls = %d, want cached container and no Get", ctr.processCalls, fc.getCalls)
		}
	})

	t.Run("Signal", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{}
		fc := &fakeMDClient{}
		b := newTestBackend(fc)
		b.containers["ctr"] = ctr
		if err := b.Signal(t.Context(), "docker:ctr", 123, "SIGTERM"); err != nil {
			t.Fatalf("Signal: %v", err)
		}
		if ctr.signalCalls != 1 || ctr.signalPID != 123 || ctr.signalSig != "SIGTERM" || fc.getCalls != 0 {
			t.Fatalf("signal = calls %d pid %d sig %q getCalls %d", ctr.signalCalls, ctr.signalPID, ctr.signalSig, fc.getCalls)
		}
	})

	t.Run("mdStartOpts", func(t *testing.T) {
		t.Parallel()
		fc := &fakeMDContainer{
			agentMounts: []md.Mount{{HostPath: "/home/user/.claude", ContainerPath: "/home/user/.claude"}},
		}
		b := newTestBackend(&fakeMDClient{})
		b.HarnessEnv = map[string][]string{string(harness.Claude): {"FOO=bar"}}
		opts, err := b.mdStartOpts(fc, &runtime.StartOptions{
			Metadata:          runtime.Metadata{runtime.MetadataTaskID: "task-1"},
			ContainerPlatform: "linux/amd64",
			Harness:           harness.Claude,
			GitHubToken:       "tok",
			Caches:            []runtime.CacheMount{{Name: "npm", HostPath: "~/.npm", MountPath: "/home/user/.npm"}},
			Mounts:            []runtime.Mount{{HostPath: "/host/work", MountPath: "/workspace/external", ReadOnly: true}},
		})
		if err != nil {
			t.Fatalf("mdStartOpts: %v", err)
		}
		if !slices.Contains(opts.ExtraEnv, "EDITOR=true") {
			t.Errorf("ExtraEnv missing EDITOR=true: %v", opts.ExtraEnv)
		}
		if !slices.Contains(opts.ExtraEnv, "FOO=bar") {
			t.Errorf("ExtraEnv missing harness env FOO=bar: %v", opts.ExtraEnv)
		}
		if !slices.Contains(opts.ExtraEnv, "GITHUB_TOKEN=tok") {
			t.Errorf("ExtraEnv missing GITHUB_TOKEN=tok: %v", opts.ExtraEnv)
		}
		if !slices.Contains(opts.Labels, string(runtime.MetadataTaskID)+"=task-1") {
			t.Errorf("Labels missing passthrough: %v", opts.Labels)
		}
		if opts.BaseImage == "" {
			t.Error("BaseImage should default when BaseImage empty")
		}
		if opts.Platform != "linux/amd64" {
			t.Errorf("Platform = %q, want linux/amd64", opts.Platform)
		}
		if opts.MaxCPUs <= 0 {
			t.Errorf("MaxCPUs = %d, want positive default", opts.MaxCPUs)
		}
		if len(opts.Caches) != 1 || opts.Caches[0].Name != "npm" {
			t.Errorf("Caches = %+v, want npm passthrough", opts.Caches)
		}
		if len(fc.agentPaths) != 1 || fc.agentPaths[0].Description != md.HarnessMounts[md.HarnessClaude].Description {
			t.Errorf("AgentMounts paths = %+v, want Claude harness paths", fc.agentPaths)
		}
		if len(opts.Mounts) != 2 {
			t.Fatalf("Mounts = %+v, want agent and custom mounts", opts.Mounts)
		}
		if opts.Mounts[0].HostPath != "/home/user/.claude" || opts.Mounts[0].ContainerPath != "/home/user/.claude" {
			t.Errorf("Mounts[0] = %+v, want agent mount first", opts.Mounts[0])
		}
		if opts.Mounts[1].HostPath != "/host/work" || opts.Mounts[1].ContainerPath != "/workspace/external" || !opts.Mounts[1].ReadOnly {
			t.Errorf("Mounts[1] = %+v, want read-only custom mount passthrough", opts.Mounts[1])
		}
	})

	t.Run("maxCPUsOrDefault", func(t *testing.T) {
		t.Parallel()
		if got := maxCPUsOrDefault(5); got != 5 {
			t.Errorf("maxCPUsOrDefault(5) = %d, want 5", got)
		}
		if got := maxCPUsOrDefault(0); got <= 0 {
			t.Errorf("maxCPUsOrDefault(0) = %d, want positive default", got)
		}
	})
}
