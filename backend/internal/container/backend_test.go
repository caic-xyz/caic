// Tests for Backend's task.ContainerBackend logic using fake md seams.

package container

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/caic-xyz/md"
	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// fakeMDContainer is a fake mdContainer that records driven operations and
// returns configurable results.
type fakeMDContainer struct {
	name    string
	vncPort int32
	repos   []md.Repo
	state   string
	diffIdx int

	launchErr  error
	connectRes *md.StartResult
	connectErr error
	stopErr    error
	forkResult mdContainer
	forkErr    error

	calls []string
}

func (f *fakeMDContainer) Name() string      { return f.name }
func (f *fakeMDContainer) SetName(n string)  { f.name = n }
func (f *fakeMDContainer) SetState(s string) { f.state = s }
func (f *fakeMDContainer) VNCPort() int32    { return f.vncPort }
func (f *fakeMDContainer) Repos() []md.Repo  { return f.repos }

func (f *fakeMDContainer) SSHCommand(_ []string, cmd string) []string {
	f.calls = append(f.calls, "SSHCommand")
	return []string{"ssh", cmd}
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

func (f *fakeMDContainer) Fork(_ context.Context, _, _ io.Writer, _ *md.ForkOpts) (mdContainer, error) {
	f.calls = append(f.calls, "Fork")
	if f.forkErr != nil {
		return nil, f.forkErr
	}
	return f.forkResult, nil
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

func (f *fakeMDClient) Get(_ context.Context, _ string) (mdContainer, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

func newTestBackend(c mdClient) *Backend {
	return &Backend{client: c, vncPorts: make(map[string]int32)}
}

func TestBackend(t *testing.T) {
	t.Parallel()

	t.Run("Launch", func(t *testing.T) {
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			ctr := &fakeMDContainer{name: "ctr-x", vncPort: 5901}
			b := newTestBackend(&fakeMDClient{container: ctr})
			name, err := b.Launch(t.Context(), nil, []string{"l"}, &task.StartOptions{Harness: agent.Claude})
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if name != "ctr-x" {
				t.Errorf("name = %q, want ctr-x", name)
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
			_, err := b.Launch(t.Context(), nil, nil, &task.StartOptions{Harness: "bogus"})
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
			if _, err := b.Launch(t.Context(), nil, nil, &task.StartOptions{Harness: agent.Claude}); err == nil {
				t.Fatal("want error from container.Launch")
			}
			if _, ok := b.pendingContainers["ctr-y"]; ok {
				t.Error("failed container should not be stored as pending")
			}
		})
	})

	t.Run("Diff", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{}
		fc := &fakeMDClient{container: ctr}
		b := newTestBackend(fc)
		repos := []runtime.Repo{
			{HostPath: "/home/user/src/caic", Branch: "caic-7", MountPath: "/home/user/src/caic"},
			{HostPath: "/home/user/src/genai", Branch: "caic-0", MountPath: "/home/user/src/genai"},
		}
		if _, err := b.Diff(t.Context(), repos, 1, "--numstat"); err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if len(fc.containerRepos) != 2 {
			t.Fatalf("Container repos len = %d, want 2", len(fc.containerRepos))
		}
		if fc.containerRepos[0].GitRoot != "/home/user/src/caic" || fc.containerRepos[1].GitRoot != "/home/user/src/genai" {
			t.Fatalf("Container repos = %+v, want primary and secondary repos", fc.containerRepos)
		}
		if ctr.diffIdx != 1 {
			t.Errorf("Diff repoIdx = %d, want 1", ctr.diffIdx)
		}
	})

	t.Run("Connect", func(t *testing.T) {
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			ctr := &fakeMDContainer{name: "ctr-x", connectRes: &md.StartResult{TailscaleFQDN: "host.ts.net"}}
			b := newTestBackend(&fakeMDClient{container: ctr})
			if _, err := b.Launch(t.Context(), nil, nil, &task.StartOptions{Harness: agent.Claude}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			conn, err := b.Connect(t.Context(), "ctr-x", nil, &task.StartOptions{Harness: agent.Claude})
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			if conn.TailscaleFQDN != "host.ts.net" {
				t.Errorf("fqdn = %q, want host.ts.net", conn.TailscaleFQDN)
			}
			// Pending entry must be consumed: a second Connect fails.
			if _, err := b.Connect(t.Context(), "ctr-x", nil, &task.StartOptions{Harness: agent.Claude}); err == nil {
				t.Error("second Connect should fail; pending entry not consumed")
			}
		})
		t.Run("error no pending", func(t *testing.T) {
			t.Parallel()
			b := newTestBackend(&fakeMDClient{})
			if _, err := b.Connect(t.Context(), "missing", nil, &task.StartOptions{Harness: agent.Claude}); err == nil {
				t.Fatal("want error when no pending container")
			}
		})
	})

	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{}
		b := newTestBackend(&fakeMDClient{container: ctr})
		if err := b.Stop(t.Context(), "ctr-1"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if ctr.name != "ctr-1" {
			t.Errorf("SetName not applied: name = %q, want ctr-1", ctr.name)
		}
		if !slices.Contains(ctr.calls, "Stop") {
			t.Errorf("Stop not called, calls=%v", ctr.calls)
		}
	})

	t.Run("Fork", func(t *testing.T) {
		t.Parallel()
		src := &fakeMDContainer{forkResult: &fakeMDContainer{name: "fork-1", vncPort: 5902, repos: []md.Repo{{Branch: "caic-2"}}}}
		b := newTestBackend(&fakeMDClient{getResult: src})
		name, repos, err := b.Fork(t.Context(), "src", nil, &task.ForkOptions{Harness: agent.Claude})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		if name != "fork-1" {
			t.Errorf("fork name = %q, want fork-1", name)
		}
		if len(repos) != 1 || repos[0].Branch != "caic-2" {
			t.Errorf("repos = %+v, want one repo on caic-2", repos)
		}
		if src.state != "running" {
			t.Errorf("source state = %q, want running (SetState not applied)", src.state)
		}
		if b.vncPorts["fork-1"] != 5902 {
			t.Errorf("fork vncPort = %d, want 5902", b.vncPorts["fork-1"])
		}
	})

	t.Run("VNCPort", func(t *testing.T) {
		t.Parallel()
		b := newTestBackend(&fakeMDClient{})
		b.vncPorts["c"] = 4242
		if got := b.VNCPort(t.Context(), "c"); got != 4242 {
			t.Errorf("VNCPort = %d, want 4242 (in-memory hit)", got)
		}
	})

	t.Run("mdStartOpts", func(t *testing.T) {
		t.Parallel()
		b := newTestBackend(&fakeMDClient{})
		b.HarnessEnv = map[string][]string{string(agent.Claude): {"FOO=bar"}}
		opts := b.mdStartOpts([]string{"label-a"}, &task.StartOptions{
			Harness:     agent.Claude,
			GitHubToken: "tok",
			Caches:      []runtime.CacheMount{{Name: "npm", HostPath: "~/.npm", MountPath: "/home/user/.npm"}},
		})
		if !slices.Contains(opts.ExtraEnv, "EDITOR=true") {
			t.Errorf("ExtraEnv missing EDITOR=true: %v", opts.ExtraEnv)
		}
		if !slices.Contains(opts.ExtraEnv, "FOO=bar") {
			t.Errorf("ExtraEnv missing harness env FOO=bar: %v", opts.ExtraEnv)
		}
		if !slices.Contains(opts.ExtraEnv, "GITHUB_TOKEN=tok") {
			t.Errorf("ExtraEnv missing GITHUB_TOKEN=tok: %v", opts.ExtraEnv)
		}
		if !slices.Contains(opts.Labels, "label-a") {
			t.Errorf("Labels missing passthrough: %v", opts.Labels)
		}
		if opts.BaseImage == "" {
			t.Error("BaseImage should default when DockerImage empty")
		}
		if opts.MaxCPUs <= 0 {
			t.Errorf("MaxCPUs = %d, want positive default", opts.MaxCPUs)
		}
		if len(opts.Caches) != 1 || opts.Caches[0].Name != "npm" {
			t.Errorf("Caches = %+v, want npm passthrough", opts.Caches)
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

	t.Run("parseHostPort", func(t *testing.T) {
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			got, err := parseHostPort("0.0.0.0:32768\n")
			if err != nil {
				t.Fatalf("parseHostPort: %v", err)
			}
			if got != 32768 {
				t.Errorf("port = %d, want 32768", got)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			if _, err := parseHostPort("garbage"); err == nil {
				t.Error("want error for malformed output")
			}
		})
	})
}
