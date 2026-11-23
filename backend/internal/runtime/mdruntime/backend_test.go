// Tests and benchmarks Backend runtime operations, file transfers, and disk-usage parsing through fake md seams.

package mdruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/containers"
	"github.com/caic-xyz/md/git"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/gittest"
	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/internal/testlog"
	"github.com/caic-xyz/caic/metrics"
)

// fakeMDContainer is a fake mdContainer that records driven operations and
// returns configurable results.
type fakeMDContainer struct {
	name    string
	vncPort int32
	repo    []md.Repo
	modules md.SubmoduleTree
	diffIdx int

	launchOpts  *md.StartOpts
	launchErr   error
	connectRes  *md.StartResult
	connectErr  error
	stopErr     error
	syncErr     error
	forkResult  mdContainer
	forkErr     error
	agentMounts []md.Mount
	agentErr    error

	calls        []string
	agentPaths   []md.AgentPaths
	diffOpts     *md.DiffOpts
	fetchOpts    *md.FetchOpts
	fetchResults [][]md.FetchedBranch
	forkOpts     *md.ForkOpts
}

func (f *fakeMDContainer) Name() string     { return f.name }
func (f *fakeMDContainer) SetName(n string) { f.name = n }
func (f *fakeMDContainer) VNCPort() int32   { return f.vncPort }
func (*fakeMDContainer) SSHCommand([]string, string) []string {
	return nil
}
func (f *fakeMDContainer) Repos() []md.Repo { return f.repo }

func (f *fakeMDContainer) AgentMounts(paths ...md.AgentPaths) ([]md.Mount, error) {
	f.agentPaths = append([]md.AgentPaths(nil), paths...)
	if f.agentErr != nil {
		return nil, f.agentErr
	}
	return slices.Clone(f.agentMounts), nil
}

func (f *fakeMDContainer) Launch(_ context.Context, _, _ io.Writer, opts *md.StartOpts) error {
	f.launchOpts = opts
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

func (f *fakeMDContainer) Diff(_ context.Context, _, _ io.Writer, repoIdx int, opts *md.DiffOpts) error {
	f.calls = append(f.calls, "Diff")
	f.diffIdx = repoIdx
	f.diffOpts = opts
	return nil
}

func (f *fakeMDContainer) FetchRecursive(_ context.Context, _, _ io.Writer, repoIdx int, opts *md.FetchOpts) (md.FetchedRepository, error) {
	f.calls = append(f.calls, "FetchRecursive")
	f.fetchOpts = opts
	result := md.FetchedRepository{Submodules: f.modules}
	if repoIdx < len(f.fetchResults) {
		result.Branches = slices.Clone(f.fetchResults[repoIdx])
	}
	return result, nil
}

func (f *fakeMDContainer) DiscoverSubmodules(context.Context, int) (md.SubmoduleTree, error) {
	return f.modules, nil
}

func (f *fakeMDContainer) SyncDefaultBranch(_ context.Context, repoIdx int) error {
	f.calls = append(f.calls, fmt.Sprintf("SyncDefaultBranch:%d", repoIdx))
	if repoIdx == 0 {
		return f.syncErr
	}
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

// fakeMDClient is a fake mdClient handing out preconfigured containers.
type fakeMDClient struct {
	runtime      string
	container    mdContainer // returned by Container()
	containerErr error
	info         *containers.Info
	infoErr      error
	infoCalls    int
	getResult    mdContainer // returned by Get()
	getErr       error

	containerCalls int
	containerRepos []md.Repo
	getCalls       int
	getName        string
	diskCalls      int
	diskIDs        []runtime.InstanceID
	diskSizes      map[runtime.InstanceID]int64
}

func (f *fakeMDClient) Runtime() string {
	if f.runtime == "" {
		return "docker"
	}
	return f.runtime
}

func (f *fakeMDClient) Info(context.Context) (containers.Info, error) {
	f.infoCalls++
	if f.info != nil {
		return *f.info, f.infoErr
	}
	return containers.Info{MemTotal: 16 << 30}, f.infoErr
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

func (f *fakeMDClient) DiskUsage(_ context.Context, ids []runtime.InstanceID) (map[runtime.InstanceID]int64, error) {
	f.diskCalls++
	f.diskIDs = slices.Clone(ids)
	return maps.Clone(f.diskSizes), nil
}

func (*fakeMDClient) WatchStats(context.Context, []runtime.InstanceID) (iter.Seq2[runtime.StatsSample, error], error) {
	return func(func(runtime.StatsSample, error) bool) {}, nil
}

func (*fakeMDClient) WatchEvents(context.Context, runtime.EventFilter) (iter.Seq2[runtime.Event, error], error) {
	return func(func(runtime.Event, error) bool) {}, nil
}

func (*fakeMDClient) SudoPassword(context.Context, runtime.InstanceID) (string, error) {
	return "", nil
}

type idleEventsClient struct{ fakeMDClient }

func (*idleEventsClient) WatchEvents(ctx context.Context, _ runtime.EventFilter) (iter.Seq2[runtime.Event, error], error) {
	return func(func(runtime.Event, error) bool) { <-ctx.Done() }, nil
}

func newTestBackend(tb testing.TB, c mdClient) *Backend {
	return &Backend{log: testlog.New(tb), client: c, containers: make(map[string]mdContainer), vncPorts: make(map[string]int32)}
}

type statsCapacityClient struct {
	fakeMDClient
}

func (*statsCapacityClient) WatchStats(context.Context, []runtime.InstanceID) (iter.Seq2[runtime.StatsSample, error], error) {
	return func(yield func(runtime.StatsSample, error) bool) {
		for range 2 {
			if !yield(runtime.StatsSample{InstanceID: "task", Stats: runtime.Stats{CPUPerc: 150, CPUCeiling: 2.5, CPUQuota: new(2.5), RuntimeCPUs: 8}}, nil) {
				return
			}
		}
	}, nil
}

// cpuInfoRuntime is the foreign md runtime boundary; its changing observations
// reproduce an update and then an observation failure without restarting stats.
type cpuInfoRuntime struct {
	containers.Runtime

	infoCalls  int
	names      []string
	delay      time.Duration
	infoErr    error
	inspectErr error
	inspect    *containers.InspectInfo
}

func (*cpuInfoRuntime) Name() string { return "docker" }

func (r *cpuInfoRuntime) Info(ctx context.Context) (containers.Info, error) {
	r.infoCalls++
	if r.infoErr != nil {
		return containers.Info{}, r.infoErr
	}
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return containers.Info{}, ctx.Err()
		}
	}
	if r.infoCalls == 3 {
		return containers.Info{}, errors.New("runtime unavailable")
	}
	return containers.Info{CPUs: 8}, nil
}

func (r *cpuInfoRuntime) InspectInfo(_ context.Context, name string) (*containers.InspectInfo, error) {
	if r.inspectErr != nil {
		return nil, r.inspectErr
	}
	if r.inspect != nil {
		return r.inspect, nil
	}
	if name != "task" {
		return nil, fmt.Errorf("expected local instance name, got %q", name)
	}
	if r.infoCalls == 3 {
		return nil, errors.New("inspect unavailable")
	}
	quota := 2.5
	if r.infoCalls == 2 {
		quota = 0.5
	}
	return &containers.InspectInfo{CPULimit: quota, CPUSet: []int{1, 3}}, nil
}

func (r *cpuInfoRuntime) WatchStats(_ context.Context, names []string) (iter.Seq2[containers.StatsSample, error], error) {
	r.names = names
	return func(yield func(containers.StatsSample, error) bool) {
		for i := range 3 {
			if i > 0 {
				time.Sleep(31 * time.Second)
			}
			synctest.Wait()
			if !yield(containers.StatsSample{Name: "task", Stats: &containers.Stats{CPUPerc: 150}}, nil) {
				return
			}
		}
	}, nil
}

func TestMDClientAdapter(t *testing.T) {
	t.Parallel()
	t.Run("metadata_observations", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			r       cpuInfoRuntime
			ceiling float64
			cpus    int
			quota   *float64
			err     string
		}{
			{name: "unrestricted", r: cpuInfoRuntime{inspect: &containers.InspectInfo{}}, ceiling: 8, cpus: 8, quota: new(0.0)},
			{name: "runtime_failure", r: cpuInfoRuntime{infoErr: errors.New("runtime unavailable"), inspect: &containers.InspectInfo{CPULimit: 1.5}}, ceiling: 1.5, quota: new(1.5), err: "runtime unavailable"},
			{name: "inspect_failure", r: cpuInfoRuntime{inspectErr: errors.New("inspect unavailable")}, ceiling: 8, cpus: 8, err: "inspect unavailable"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					log := testlog.New(t)
					a := mdClientAdapter{log: log, c: &md.Client{Runtime: &tc.r, Logger: log}}
					seq, err := a.WatchStats(t.Context(), []runtime.InstanceID{"task"})
					if err != nil {
						t.Fatal(err)
					}
					for sample, err := range seq {
						if err != nil {
							t.Fatal(err)
						}
						s := sample.Stats
						if s.CPUCeiling != tc.ceiling || s.RuntimeCPUs != tc.cpus || !reflect.DeepEqual(s.CPUQuota, tc.quota) || !strings.Contains(s.CPUError, tc.err) || (tc.err == "" && s.CPUError != "") {
							t.Fatalf("metadata = %+v", s)
						}
						break
					}
				})
			})
		}
	})
	t.Run("WatchStats", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := &cpuInfoRuntime{}
			log := testlog.New(t)
			a := mdClientAdapter{log: log, c: &md.Client{Runtime: r, Logger: log}}
			b := newTestBackend(t, a)
			seq, err := b.WatchStats(t.Context(), []runtime.ID{"docker:task"})
			if err != nil {
				t.Fatal(err)
			}
			var samples []runtime.Stats
			for sample, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				if sample.InstanceID != "docker:task" || sample.Stats.CPUPerc != 150 {
					t.Fatalf("identity or unclamped usage = %+v", sample)
				}
				samples = append(samples, sample.Stats)
			}
			if len(samples) != 3 || r.infoCalls != 3 || !slices.Equal(r.names, []string{"task"}) {
				t.Fatalf("samples %d, runtime queries %d, names %v", len(samples), r.infoCalls, r.names)
			}
			for i, want := range []float64{2, 0.5} {
				s := samples[i]
				if s.CPUCeiling != want || s.RuntimeCPUs != 8 || s.CPUQuota == nil || s.CPUError != "" {
					t.Fatalf("sample %d = %+v", i, s)
				}
			}
			if *samples[0].CPUQuota != 2.5 || *samples[1].CPUQuota != 0.5 {
				t.Fatalf("explicit quota changed or rounded: %+v", samples)
			}
			s := samples[2]
			if s.CPUCeiling != 0 || s.CPUQuota != nil || s.RuntimeCPUs != 0 || s.CPUError == "" {
				t.Fatalf("failure retained stale capacity: %+v", s)
			}
		})
	})
	t.Run("slow_cpu_metadata", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			r := &cpuInfoRuntime{delay: 5 * time.Second}
			log := testlog.New(t)
			a := mdClientAdapter{log: log, c: &md.Client{Runtime: r, Logger: log}}
			start := time.Now()
			seq, err := a.WatchStats(t.Context(), []runtime.InstanceID{"task"})
			if err != nil {
				t.Fatal(err)
			}
			for sample, err := range seq {
				if err != nil || sample.Stats.CPUPerc != 150 || sample.Stats.CPUCeiling != 0 || sample.Stats.RuntimeCPUs != 0 {
					t.Fatalf("usage during blocked metadata observation: %+v, %v", sample, err)
				}
				break
			}
			if elapsed := time.Since(start); elapsed != 0 {
				t.Fatalf("metadata observation delayed usage or cancellation by %v", elapsed)
			}
		})
	})
}

func TestBackend(t *testing.T) {
	t.Parallel()
	t.Run("MemoryLimit", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name         string
				total, limit uint64
			}{
				{"runtime_RAM", 16 << 30, 12 << 30},
				{"fractional_bytes", 16_000_000_001, 12_000_000_000},
				{"largest_total", ^uint64(0), 13_835_058_055_282_163_711},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					c := &fakeMDContainer{name: "task", forkResult: &fakeMDContainer{name: "fork"}}
					client := &fakeMDClient{container: c, getResult: c, info: &containers.Info{MemTotal: tc.total}}
					b := newTestBackend(t, client)
					opts := &runtime.StartOptions{Harness: harness.Claude}
					id, err := b.Launch(t.Context(), nil, opts)
					if err != nil {
						t.Fatal(err)
					}
					if c.launchOpts == nil || c.launchOpts.MaxMemory != tc.limit {
						t.Fatalf("launch opts = %+v, want memory %d", c.launchOpts, tc.limit)
					}
					if _, _, err = b.Fork(t.Context(), id, &runtime.ForkOptions{Harness: harness.Claude}); err != nil {
						t.Fatal(err)
					}
					if c.forkOpts == nil || c.forkOpts.MaxMemory != nil {
						t.Fatalf("fork opts = %+v, want inherited memory", c.forkOpts)
					}
					calls := client.infoCalls
					client.infoErr = errors.New("memory metadata unavailable during provisioning")
					if _, err = b.Connect(t.Context(), id, opts); err != nil {
						t.Fatal(err)
					}
					if client.infoCalls != calls {
						t.Fatal("Connect queried runtime RAM after container creation")
					}
					if _, _, err = b.Fork(t.Context(), id, &runtime.ForkOptions{Harness: harness.Claude}); err != nil {
						t.Fatal(err)
					}
					if client.infoCalls != calls || c.forkOpts == nil || c.forkOpts.MaxMemory != nil {
						t.Fatal("Fork queried runtime RAM or overrode inherited memory")
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name  string
				total uint64
				err   error
			}{
				{name: "unknown_RAM"},
				{name: "tiny_RAM", total: 1},
				{name: "runtime_failure", err: errors.New("runtime unavailable")},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					c := &fakeMDContainer{name: "task"}
					client := &fakeMDClient{container: c, getResult: c, info: &containers.Info{MemTotal: tc.total}, infoErr: tc.err}
					b := newTestBackend(t, client)
					_, err := b.Launch(t.Context(), nil, &runtime.StartOptions{Harness: harness.Claude})
					if err == nil || !strings.Contains(err.Error(), "memory") {
						t.Fatalf("launch error = %v", err)
					}
					if tc.err != nil && !errors.Is(err, tc.err) {
						t.Fatalf("launch lost runtime error: %v", err)
					}
					if slices.Contains(c.calls, "Launch") || slices.Contains(c.calls, "Fork") {
						t.Fatalf("mutated runtime with unavailable RAM: %v", c.calls)
					}
				})
			}
		})
	})
	t.Run("WatchEvents cancellation closes an idle stream", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		stream, err := newTestBackend(t, &idleEventsClient{}).WatchEvents(ctx, runtime.EventFilter{})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		for event, err := range stream {
			t.Fatalf("unexpected event after cancellation: %v, %v", event, err)
		}
	})
	t.Run("WatchStats", func(t *testing.T) {
		t.Parallel()
		b := newTestBackend(t, &statsCapacityClient{})
		stream, err := b.WatchStats(t.Context(), []runtime.ID{"docker:task"})
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for sample, err := range stream {
			if err != nil {
				t.Fatal(err)
			}
			if sample.InstanceID != "docker:task" || sample.Stats.CPUPerc != 150 || sample.Stats.CPUCeiling != 2.5 || sample.Stats.RuntimeCPUs != 8 || sample.Stats.CPUQuota == nil || *sample.Stats.CPUQuota != 2.5 {
				t.Fatalf("sample = %+v", sample)
			}
			count++
		}
		if count != 2 {
			t.Fatalf("samples %d", count)
		}
	})

	t.Run("ReadFile", func(t *testing.T) {
		t.Parallel()
		if goruntime.GOOS != "linux" {
			t.Skip("executes GNU dd from the Linux container locally")
		}
		dir := t.TempDir()
		name := filepath.Join(dir, "screenshot '$().png")
		data := bytes.Repeat([]byte("a"), 2*runtime.FileChunkSize+17)
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
		empty := filepath.Join(dir, "empty")
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		b := newTestBackend(t, &fakeMDClient{getResult: &fileCommandContainer{}})
		id := runtime.NewID("docker", "file-test")
		var got []byte
		count := 0
		for chunk, err := range b.ReadFile(t.Context(), id, name, 0, -1) {
			if err != nil {
				t.Fatal(err)
			}
			if len(chunk) > 64<<10 {
				t.Fatalf("chunk size = %d", len(chunk))
			}
			got = append(got, chunk...)
			count++
		}
		if !bytes.Equal(got, data) || count != 3 {
			t.Fatalf("read %d bytes in %d chunks", len(got), count)
		}
		count = 0
		for chunk, err := range b.ReadFile(t.Context(), id, empty, 0, -1) {
			if err != nil || len(chunk) != 0 {
				t.Fatalf("empty file chunk = %q, %v", chunk, err)
			}
			count++
		}
		if count != 1 {
			t.Fatalf("empty file yielded %d chunks", count)
		}
		for _, tc := range []struct {
			path string
			err  error
		}{
			{filepath.Join(dir, "missing"), fs.ErrNotExist},
			{dir, runtime.ErrIsDir},
			{"relative", fs.ErrInvalid},
		} {
			seen := false
			for _, err := range b.ReadFile(t.Context(), id, tc.path, 0, -1) {
				if !errors.Is(err, tc.err) {
					t.Fatalf("ReadFile(%q) = %v, want %v", tc.path, err, tc.err)
				}
				seen = true
			}
			if !seen {
				t.Fatalf("missing error for %q", tc.path)
			}
		}
	})
	t.Run("ReadFile offset", func(t *testing.T) {
		t.Parallel()
		if goruntime.GOOS != "linux" {
			t.Skip("executes GNU dd and stat from the Linux container locally")
		}
		name := filepath.Join(t.TempDir(), "offset")
		if err := os.WriteFile(name, []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
		b := newTestBackend(t, &fakeMDClient{getResult: &fileCommandContainer{}})
		id := runtime.NewID("docker", "file-test")
		size, err := b.FileSize(t.Context(), id, name)
		if err != nil || size != 10 {
			t.Fatalf("FileSize = %d, %v", size, err)
		}
		var got []byte
		for data, err := range b.ReadFile(t.Context(), id, name, 4, 3) {
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, data...)
		}
		if string(got) != "456" {
			t.Fatalf("offset read = %q", got)
		}
	})
	t.Run("ReadDir", func(t *testing.T) {
		t.Parallel()
		if goruntime.GOOS != "linux" {
			t.Skip("executes GNU find from the Linux container locally")
		}
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "a dir"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a dir", "nested.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, "a dir"), filepath.Join(dir, "c link")); err != nil {
			t.Fatal(err)
		}
		b := newTestBackend(t, &fakeMDClient{getResult: &fileCommandContainer{}})
		id := runtime.NewID("docker", "file-test")
		entries, truncated, err := b.ReadDir(t.Context(), id, dir)
		if err != nil || truncated {
			t.Fatalf("ReadDir = %v, %v", truncated, err)
		}
		got := make([]string, 0, len(entries))
		for _, e := range entries {
			kind := "-"
			if e.IsDir {
				kind = "d"
			}
			if e.Modified == nil || e.Modified.IsZero() {
				t.Fatalf("entry %q has no modification time", e.Name)
			}
			got = append(got, kind+" "+e.Name)
		}
		want := []string{"d a dir", "- b.txt", "d c link"}
		if !slices.Equal(got, want) {
			t.Fatalf("entries = %v, want %v", got, want)
		}
		if entries[1].Size != 1 {
			t.Fatalf("b.txt size = %d, want 1", entries[1].Size)
		}
		for _, tc := range []struct {
			path string
			err  error
		}{
			{filepath.Join(dir, "missing"), fs.ErrNotExist},
			{filepath.Join(dir, "b.txt"), runtime.ErrIsDir},
			{"relative", fs.ErrInvalid},
		} {
			if _, _, err := b.ReadDir(t.Context(), id, tc.path); !errors.Is(err, tc.err) {
				t.Fatalf("ReadDir(%q) = %v, want %v", tc.path, err, tc.err)
			}
		}
	})
	t.Run("parse directory entries", func(t *testing.T) {
		t.Parallel()
		entries, truncated, err := parseDirEntries([]byte("b.txt\tf\t3\t1700000000.2500000000\x00a\tdir\td\t4096\t1700000001.0000000000\x00"))
		if err != nil || truncated {
			t.Fatalf("parseDirEntries = %v, %v", truncated, err)
		}
		if len(entries) != 2 || entries[0].Name != "a\tdir" || !entries[0].IsDir || entries[0].Size != 4096 {
			t.Fatalf("entries = %+v", entries)
		}
		if entries[1].Name != "b.txt" || entries[1].IsDir || entries[1].Size != 3 {
			t.Fatalf("entries = %+v", entries)
		}
		if want := time.Unix(0, 1700000000*int64(time.Second)+250*int64(time.Millisecond)); !entries[1].Modified.Equal(want) {
			t.Fatalf("modified = %v, want %v", entries[1].Modified, want)
		}
		// A clipped transfer drops its partial record.
		if entries, _, err := parseDirEntries([]byte("a\tf\t1\t1.0\x00b\tf\t1\t1.0\x00tru")); err != nil || len(entries) != 2 {
			t.Fatalf("clipped = %v, %v", entries, err)
		}
		many := bytes.Repeat([]byte("x\tf\t1\t1.0\x00"), runtime.DirEntryLimit+1)
		if entries, truncated, err := parseDirEntries(many); err != nil || !truncated || len(entries) != runtime.DirEntryLimit {
			t.Fatalf("cap = %d entries, truncated %v, %v", len(entries), truncated, err)
		}
		for _, bad := range [][]byte{[]byte("notab\x00"), []byte("only\tf\x00"), []byte("a\tf\tbig\t1.0\x00"), []byte("a\tf\t1\tbad\x00")} {
			if _, _, err := parseDirEntries(bad); err == nil {
				t.Fatalf("parseDirEntries(%q) succeeded, want error", bad)
			}
		}
	})
	t.Run("ReadFile early stop", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		t.Cleanup(cancel)
		b := newTestBackend(t, &fakeMDClient{getResult: &streamCommandContainer{command: "exec yes x"}})
		count := 0
		for chunk, err := range b.ReadFile(ctx, runtime.NewID("docker", "file-test"), "/file", 0, -1) {
			if err != nil || len(chunk) != 64<<10 {
				t.Fatalf("first chunk: %d bytes, %v", len(chunk), err)
			}
			count++
			break
		}
		if count != 1 || ctx.Err() != nil {
			t.Fatalf("early stop did not complete before timeout: count %d, %v", count, ctx.Err())
		}
	})
	t.Run("ReadFile cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		b := newTestBackend(t, &fakeMDClient{getResult: &streamCommandContainer{command: "exec yes x"}})
		seenErr := false
		for _, err := range b.ReadFile(ctx, runtime.NewID("docker", "file-test"), "/file", 0, -1) {
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				seenErr = true
			} else {
				cancel()
			}
		}
		if !seenErr {
			t.Fatal("cancellation was not reported")
		}
	})
	t.Run("ReadFile late error", func(t *testing.T) {
		t.Parallel()
		b := newTestBackend(t, &fakeMDClient{getResult: &streamCommandContainer{command: "head -c 65536 /dev/zero; head -c 1048576 /dev/zero >&2; printf 'useful diagnostic' >&2; exit 1"}})
		bytesRead := 0
		seenErr := false
		for data, err := range b.ReadFile(t.Context(), runtime.NewID("docker", "file-test"), "/file", 0, -1) {
			if err != nil {
				if !strings.Contains(err.Error(), "useful diagnostic") || len(err.Error()) > 3000 {
					t.Fatalf("unbounded or missing diagnostic: %d bytes", len(err.Error()))
				}
				seenErr = true
			} else {
				bytesRead += len(data)
			}
		}
		if bytesRead != 64<<10 || !seenErr {
			t.Fatalf("read %d bytes, error reported %v", bytesRead, seenErr)
		}
	})
	t.Run("parse disk usage", func(t *testing.T) {
		t.Parallel()
		usage, err := parseDiskUsage("/one\t376657501\ntwo\t0", "docker")
		if err != nil {
			t.Fatal(err)
		}
		want := map[runtime.InstanceID]int64{"one": 376657501, "two": 0}
		if !reflect.DeepEqual(usage, want) {
			t.Fatalf("usage = %v, want %v", usage, want)
		}
		for _, out := range []string{"missing-tab", "one\tnull", "one\t-1", "one\tnot-json"} {
			if _, err := parseDiskUsage(out, "docker"); err == nil {
				t.Errorf("parseDiskUsage(%q) succeeded, want error", out)
			}
		}
	})
	t.Run("batch disk usage", func(t *testing.T) {
		t.Parallel()
		client := &fakeMDClient{diskSizes: map[runtime.InstanceID]int64{"one": 10, "two": 20}}
		backend := newTestBackend(t, client)
		usage, err := backend.DiskUsage(t.Context(), []runtime.ID{"docker:one", "docker:two"})
		if err != nil {
			t.Fatal(err)
		}
		if client.diskCalls != 1 || !slices.Equal(client.diskIDs, []runtime.InstanceID{"one", "two"}) {
			t.Fatalf("disk calls = %d ids = %v, want one call for [one two]", client.diskCalls, client.diskIDs)
		}
		want := map[runtime.ID]int64{"docker:one": 10, "docker:two": 20}
		if !reflect.DeepEqual(usage, want) {
			t.Fatalf("usage = %v, want %v", usage, want)
		}
	})

	t.Run("Launch", func(t *testing.T) {
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			ctr := &fakeMDContainer{name: "ctr-x", vncPort: 5901}
			b := newTestBackend(t, &fakeMDClient{container: ctr})
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
			b := newTestBackend(t, fc)
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
			b := newTestBackend(t, &fakeMDClient{container: ctr})
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
			{GitRoot: "/home/user/src/caic", Branches: []string{"caic-7"}, ContainerPath: "/home/user/src/caic"},
			{GitRoot: "/home/user/src/genai", Branches: []string{"caic-0"}, ContainerPath: "/home/user/src/genai"},
		}}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(t, fc)
		if _, err := b.Diff(t.Context(), "docker:ctr-1", 1, runtime.DiffPatch{Path: "file.txt", Prefix: "genai"}); err != nil {
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
		if ctr.diffOpts == nil || !ctr.diffOpts.Full || !slices.Equal(ctr.diffOpts.Args, []string{"--src-prefix=a/genai/", "--dst-prefix=b/genai/", "--", "file.txt"}) {
			t.Errorf("Diff opts = %+v, want a whole-branch patch with scoped paths", ctr.diffOpts)
		}
		t.Run("raw diff contains child file content", func(t *testing.T) {
			t.Parallel()
			f := newSubmoduleBackend(t)
			diff, err := f.backend.Diff(t.Context(), "docker:ctr", 0, runtime.DiffPatch{})
			if err != nil || !strings.Contains(diff, "lib with space/nested/tracked.txt") || !strings.Contains(diff, "+committed child content") {
				t.Fatalf("diff: %q, %v", diff, err)
			}
		})
	})

	checkSubmoduleStatus := func(t *testing.T, status *runtime.RepositoryStatus) {
		want := []string{"lib with space", "lib with space/nested"}
		if len(status.Submodules) != len(want) {
			t.Fatalf("children: %+v", status.Submodules)
		}
		for i := range status.Submodules {
			sub := &status.Submodules[i]
			if sub.Path != want[i] || sub.Status.Ahead != 1 || len(sub.Status.DiffStat) != 1 || sub.Status.DiffStat[0].Path != "tracked.txt" {
				t.Fatalf("child: %+v", sub)
			}
		}
	}
	t.Run("RepositoryStatus", func(t *testing.T) {
		t.Parallel()
		f := newSubmoduleBackend(t)
		status, err := f.backend.RepositoryStatus(t.Context(), "docker:ctr", 0)
		if err != nil {
			t.Fatal(err)
		}
		checkSubmoduleStatus(t, &status)
	})
	t.Run("CompactRepositoryStatus", func(t *testing.T) {
		t.Parallel()
		f := newSubmoduleBackend(t)
		status, err := f.backend.CompactRepositoryStatus(t.Context(), "docker:ctr", 0)
		if err != nil {
			t.Fatal(err)
		}
		checkSubmoduleStatus(t, &status)

		t.Run("checkout statistics contain qualified files and never patch rows", func(t *testing.T) {
			t.Parallel()
			f := newSubmoduleBackend(t)
			if err := os.WriteFile(filepath.Join(f.child, "tabbed.txt"), []byte("1\t1\tbogus-from-patch.go\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			router, err := runtime.NewRouter([]runtime.System{f.backend}, metrics.Nop{})
			if err != nil {
				t.Fatal(err)
			}
			checkout, err := repo.NewCheckout(t.Context(), logtest.Logger(t), f.root, "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			target := repo.GitTarget{InstanceID: "docker:ctr", Repos: []runtime.Repo{{GitRoot: f.root, ContainerPath: f.root, Branch: "main"}}}
			snapshot, err := checkout.DiffStat(t.Context(), logtest.Logger(t), router, target)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.DiffStat) != 3 {
				t.Fatalf("statistics contain patch rows or omit child files: %+v", snapshot.DiffStat)
			}
			paths := make([]string, 0, 3)
			for _, file := range snapshot.DiffStat {
				paths = append(paths, file.Path)
			}
			slices.Sort(paths)
			want := []string{"lib with space/nested/tracked.txt", "lib with space/tabbed.txt", "lib with space/tracked.txt"}
			if !slices.Equal(paths, want) {
				t.Fatalf("qualified statistics: %v", paths)
			}
		})
	})

	t.Run("FileDiff", func(t *testing.T) {
		t.Parallel()
		f := newSubmoduleBackend(t)
		if err := os.WriteFile(filepath.Join(f.nested, "new.txt"), []byte("untracked child line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct{ module, commit, path, want string }{{"lib with space", f.childCommit, "tracked.txt", "+committed child content"}, {"lib with space/nested", "", "new.txt", "+untracked child line"}} {
			patch, err := f.backend.FileDiff(t.Context(), "docker:ctr", 0, test.module, test.commit, test.path, "")
			if err != nil || !strings.Contains(patch, test.want) {
				t.Fatalf("patch %s: %q, %v", test.module, patch, err)
			}
		}
		if _, err := f.backend.FileDiff(t.Context(), "docker:ctr", 0, "../outside", "", "tracked.txt", ""); err == nil {
			t.Fatal("uninitialized child accepted")
		}
	})
	t.Run("TurnSnapshot", func(t *testing.T) {
		t.Parallel()
		f := newSubmoduleBackend(t)
		measured, err := f.backend.TurnSnapshot(t.Context(), "docker:ctr", f.previous)
		if err != nil {
			t.Fatal(err)
		}
		if len(measured) != 1 || len(measured[0].Branches) != 3 || len(measured[0].Status.Submodules) != 2 || len(measured[0].TurnDiff) != 2 {
			t.Fatalf("turn: %+v", measured)
		}
		if measured[0].TurnDiff[1].Path != "lib with space/nested/tracked.txt" {
			t.Fatalf("delta paths: %+v", measured[0].TurnDiff)
		}
	})
	t.Run("RefreshRefs", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			ctr := &fakeMDContainer{repo: []md.Repo{{ContainerPath: "/repo/one"}, {ContainerPath: "/repo/two"}}}
			b := newTestBackend(t, &fakeMDClient{getResult: ctr})
			if err := b.RefreshRefs(t.Context(), "docker:ctr-1"); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ctr.calls, []string{"SyncDefaultBranch:0", "SyncDefaultBranch:1"}) {
				t.Fatalf("refresh must sync every repository without committing: %v", ctr.calls)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			syncErr := errors.New("upstream sync failed")
			ctr := &fakeMDContainer{repo: []md.Repo{{ContainerPath: "/repo/one"}, {ContainerPath: "/repo/two"}}, syncErr: syncErr}
			b := newTestBackend(t, &fakeMDClient{getResult: ctr})
			err := b.RefreshRefs(t.Context(), "docker:ctr-1")
			if !errors.Is(err, syncErr) || !strings.Contains(err.Error(), "/repo/one") {
				t.Fatalf("refresh must surface repository error: %v", err)
			}
			if !slices.Equal(ctr.calls, []string{"SyncDefaultBranch:0", "SyncDefaultBranch:1"}) {
				t.Fatalf("refresh must sync every repository without committing: %v", ctr.calls)
			}
		})
	})

	t.Run("Fetch", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{repo: []md.Repo{
			{GitRoot: "/home/user/src/caic", Branches: []string{"caic-7"}, ContainerPath: "/home/user/src/caic"},
			{GitRoot: "/home/user/src/genai", Branches: []string{"caic-0"}, ContainerPath: "/home/user/src/genai"},
		}, fetchResults: [][]md.FetchedBranch{
			{{BranchName: "caic-7", CommitHash: "1111111"}},
			{{BranchName: "caic-0", CommitHash: "2222222"}},
		}}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(t, fc)
		fetched, err := b.Fetch(t.Context(), "docker:ctr-1", runtime.FetchOpts{Commit: true})
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		wantFetched := []runtime.FetchedBranch{
			{RepositoryPath: "/home/user/src/caic", BranchName: "caic-7", CommitHash: "1111111"},
			{RepositoryPath: "/home/user/src/genai", BranchName: "caic-0", CommitHash: "2222222"},
		}
		if !reflect.DeepEqual(fetched, wantFetched) {
			t.Errorf("Fetch result = %+v, want %+v", fetched, wantFetched)
		}
		if ctr.fetchOpts == nil || !ctr.fetchOpts.Commit {
			t.Errorf("Fetch opts = %+v, want a commit", ctr.fetchOpts)
		}
		if fc.getCalls != 1 || fc.getName != "ctr-1" {
			t.Fatalf("Get calls = %d name = %q, want 1 ctr-1", fc.getCalls, fc.getName)
		}
		if fc.containerCalls != 0 {
			t.Fatalf("Container() called %d times, want 0", fc.containerCalls)
		}
		gotFetchCalls := 0
		for _, call := range ctr.calls {
			if call == "FetchRecursive" {
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
			b := newTestBackend(t, &fakeMDClient{container: ctr})
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
			b := newTestBackend(t, &fakeMDClient{})
			if _, err := b.Connect(t.Context(), "docker:missing", &runtime.StartOptions{Harness: harness.Claude}); err == nil {
				t.Fatal("want error when no pending container")
			}
		})
	})

	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		ctr := &fakeMDContainer{name: "ctr-1"}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(t, fc)
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
		ctr := &fakeMDContainer{name: "ctr-1", repo: []md.Repo{{GitRoot: "/repo", Branches: []string{"caic-0"}, ContainerPath: "/repo"}}}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(t, fc)
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
		dir := t.TempDir()
		g := &git.Checkout{Root: dir, Logger: slog.Default()}
		runGit := func(args ...string) string {
			out, err := g.RunGit(t.Context(), args...)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		runGit("init", "-q", "--initial-branch=main")
		runGit("config", "user.name", "Test")
		runGit("config", "user.email", "test@test")
		runGit("commit", "-q", "--allow-empty", "-m", "main")
		runGit("remote", "add", "upstream", "https://example.com/repo.git")
		want := runGit("rev-parse", "HEAD")
		runGit("update-ref", "refs/remotes/upstream/main", want)
		runGit("branch", "--track", "caic-0", "upstream/main")
		runGit("branch", "-D", "caic-0")
		ctr := &fakeMDContainer{name: "ctr-1", vncPort: 5903, repo: []md.Repo{{GitRoot: dir, Branches: []string{"caic-0"}, ContainerPath: "/repo", DefaultRemote: "upstream", DefaultBranch: "main"}}}
		fc := &fakeMDClient{getResult: ctr}
		b := newTestBackend(t, fc)
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
		if got := runGit("rev-parse", "caic-0"); got != want {
			t.Errorf("recreated branch = %q, want %q", got, want)
		}
		if got := runGit("rev-parse", "--abbrev-ref", "caic-0@{upstream}"); got != "upstream/main" {
			t.Errorf("upstream = %q, want upstream/main", got)
		}
		if b.vncPorts["ctr-1"] != 5903 {
			t.Errorf("revived vncPort = %d, want 5903", b.vncPorts["ctr-1"])
		}
		// Repeated revival must preserve existing work on the host branch.
		runGit("checkout", "-q", "caic-0")
		runGit("commit", "-q", "--allow-empty", "-m", "host work")
		hostTip := runGit("rev-parse", "HEAD")
		runGit("branch", "--unset-upstream", "caic-0")
		if err := b.Revive(t.Context(), "docker:ctr-1"); err != nil {
			t.Fatal(err)
		}
		if got := runGit("rev-parse", "caic-0"); got != hostTip {
			t.Errorf("existing branch moved to %q, want %q", got, hostTip)
		}
		if got := runGit("rev-parse", "--abbrev-ref", "caic-0@{upstream}"); got != "upstream/main" {
			t.Errorf("repaired upstream = %q, want upstream/main", got)
		}
		// Preserve an explicitly configured upstream, including local tracking.
		runGit("branch", "--set-upstream-to=main", "caic-0")
		if err := b.Revive(t.Context(), "docker:ctr-1"); err != nil {
			t.Fatal(err)
		}
		if got := runGit("rev-parse", "--abbrev-ref", "caic-0@{upstream}"); got != "main" {
			t.Errorf("configured upstream changed to %q, want main", got)
		}
		// Fetch filtering must not make explicit tracking look unconfigured.
		runGit("config", "remote.upstream.fetch", "+refs/heads/main:refs/remotes/upstream/main")
		runGit("config", "branch.caic-0.remote", "upstream")
		runGit("config", "branch.caic-0.merge", "refs/heads/unfetched")
		if err := b.Revive(t.Context(), "docker:ctr-1"); err != nil {
			t.Fatal(err)
		}
		if got := runGit("config", "--get", "branch.caic-0.merge"); got != "refs/heads/unfetched" {
			t.Errorf("configured merge changed to %q", got)
		}
		// A task working directly on a base branch survives that branch's
		// deletion both locally and on the remote, e.g. after a merge.
		voice := &fakeMDContainer{name: "ctr-voice", repo: []md.Repo{{GitRoot: dir, Branches: []string{"voice"}, ContainerPath: "/repo", DefaultRemote: "upstream", DefaultBranch: "voice"}}}
		bv := newTestBackend(t, &fakeMDClient{getResult: voice})
		if err := bv.Revive(t.Context(), "docker:ctr-voice"); err != nil {
			t.Fatalf("Revive with deleted base branch: %v", err)
		}
		if got := runGit("rev-parse", "voice"); got != want {
			t.Errorf("recreated branch = %q, want %q", got, want)
		}
		if got := runGit("rev-parse", "--abbrev-ref", "voice@{upstream}"); got != "upstream/main" {
			t.Errorf("upstream = %q, want upstream/main", got)
		}
		runGit("checkout", "-q", "main")
		runGit("branch", "-D", "caic-0")
		runGit("update-ref", "-d", "refs/remotes/upstream/main")
		// A same-named local branch must not stand in for a missing remote ref.
		runGit("branch", "upstream/main", "main")
		ctr.calls = nil
		if err := b.Revive(t.Context(), "docker:ctr-1"); err == nil {
			t.Fatal("expected missing upstream error")
		}
		if slices.Contains(ctr.calls, "Revive") {
			t.Fatal("container revived without recovering host branch")
		}
	})

	t.Run("Fork", func(t *testing.T) {
		t.Parallel()
		src := &fakeMDContainer{forkResult: &fakeMDContainer{name: "fork-1", vncPort: 5902, repo: []md.Repo{{Branches: []string{"caic-2"}}}}}
		b := newTestBackend(t, &fakeMDClient{getResult: src})
		name, conn, err := b.Fork(t.Context(), "docker:src", &runtime.ForkOptions{Harness: harness.Claude})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		if name != "docker:fork-1" {
			t.Errorf("fork name = %q, want docker:fork-1", name)
		}
		if conn.AgentTarget.SSHHost != "fork-1" {
			t.Errorf("fork agent target = %q, want fork-1", conn.AgentTarget.SSHHost)
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
		wantRunArgs, err := containerRunArgs()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(src.forkOpts.ExtraRunArgs, wantRunArgs) {
			t.Errorf("Fork ExtraRunArgs = %v, want %v", src.forkOpts.ExtraRunArgs, wantRunArgs)
		}
	})

	t.Run("Fork extraEnv", func(t *testing.T) {
		t.Parallel()
		src := &fakeMDContainer{forkResult: &fakeMDContainer{name: "fork-1"}}
		b := newTestBackend(t, &fakeMDClient{getResult: src})
		b.HarnessEnv = map[string][]string{string(harness.Claude): {"FOO=bar"}}
		_, _, err := b.Fork(t.Context(), "docker:src", &runtime.ForkOptions{
			Harness:  harness.Claude,
			ExtraEnv: []string{"GITHUB_TOKEN=tok"},
		})
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		if src.forkOpts == nil {
			t.Fatal("Fork options not recorded")
		}
		for _, want := range []string{"FOO=bar", "GITHUB_TOKEN=tok"} {
			if !slices.Contains(src.forkOpts.ExtraEnv, want) {
				t.Errorf("ExtraEnv missing %s: %v", want, src.forkOpts.ExtraEnv)
			}
		}
		// Caller-set vars must come after the harness env so they win on conflict.
		for i, e := range src.forkOpts.ExtraEnv {
			if e == "FOO=bar" && !slices.Contains(src.forkOpts.ExtraEnv[i+1:], "GITHUB_TOKEN=tok") {
				t.Errorf("ExtraEnv order: GITHUB_TOKEN must follow FOO=bar: %v", src.forkOpts.ExtraEnv)
			}
		}
	})

	t.Run("VNCPort", func(t *testing.T) {
		t.Run("valid cached", func(t *testing.T) {
			t.Parallel()
			b := newTestBackend(t, &fakeMDClient{})
			b.vncPorts["c"] = 4242
			if got := b.VNCPort(t.Context(), "docker:c"); got != 4242 {
				t.Errorf("VNCPort = %d, want 4242 (in-memory hit)", got)
			}
		})
		t.Run("valid inspect fallback", func(t *testing.T) {
			t.Parallel()
			fc := &fakeMDClient{getResult: &fakeMDContainer{vncPort: 5901}}
			b := newTestBackend(t, fc)
			if got := b.VNCPort(t.Context(), "docker:c"); got != 5901 {
				t.Errorf("VNCPort = %d, want 5901", got)
			}
			if fc.getCalls != 1 || fc.getName != "c" {
				t.Errorf("Get calls = %d name = %q, want 1 c", fc.getCalls, fc.getName)
			}
		})
	})

	t.Run("DSHMounts", func(t *testing.T) {
		t.Parallel()
		fc := &fakeMDContainer{}
		b := newTestBackend(t, &fakeMDClient{})
		b.HarnessEnv = map[string][]string{"dsh": {"DSH_HOME=/home/user/.dsh"}}
		opts, err := b.mdStartOpts(fc, &runtime.StartOptions{Harness: harness.Dsh})
		if err != nil {
			t.Fatal(err)
		}
		if len(fc.agentPaths) != 1 || !slices.Contains(fc.agentPaths[0].HomePaths, ".dsh") {
			t.Fatalf("dsh agent paths = %+v, want persisted .dsh state", fc.agentPaths)
		}
		if !slices.Contains(opts.ExtraEnv, "DSH_HOME=/home/user/.dsh") {
			t.Fatalf("dsh env = %v", opts.ExtraEnv)
		}
	})

	t.Run("mdStartOpts", func(t *testing.T) {
		t.Parallel()
		fc := &fakeMDContainer{
			agentMounts: []md.Mount{{HostPath: "/home/user/.claude", ContainerPath: "/home/user/.claude"}},
		}
		b := newTestBackend(t, &fakeMDClient{})
		b.HarnessEnv = map[string][]string{string(harness.Claude): {"FOO=bar"}}
		opts, err := b.mdStartOpts(fc, &runtime.StartOptions{
			Metadata:          runtime.Metadata{runtime.MetadataTaskID: "task-1", runtime.MetadataSmokeRun: "run-token"},
			ContainerPlatform: "linux/amd64",
			Harness:           harness.Claude,
			GitHubToken:       "tok",
			Caches:            []runtime.CacheMount{{Name: "npm", HostPath: "~/.npm", ContainerPath: "/home/user/.npm"}},
			Mounts:            []runtime.Mount{{HostPath: "/host/work", ContainerPath: "/workspace/external", ReadOnly: true}},
		})
		if err != nil {
			t.Fatalf("mdStartOpts: %v", err)
		}
		if !slices.Contains(opts.ExtraEnv, "FOO=bar") {
			t.Errorf("ExtraEnv missing harness env FOO=bar: %v", opts.ExtraEnv)
		}
		if !slices.Contains(opts.ExtraEnv, "GITHUB_TOKEN=tok") {
			t.Errorf("ExtraEnv missing GITHUB_TOKEN=tok: %v", opts.ExtraEnv)
		}
		if !slices.Contains(opts.Labels, string(runtime.MetadataTaskID)+"=task-1") {
			t.Errorf("Labels missing task ID: %v", opts.Labels)
		}
		if !slices.Contains(opts.Labels, string(runtime.MetadataSmokeRun)+"=run-token") {
			t.Errorf("Labels missing smoke run: %v", opts.Labels)
		}
		if opts.BaseImage == "" {
			t.Error("BaseImage should default when BaseImage empty")
		}
		if opts.Platform != "linux/amd64" {
			t.Errorf("Platform = %q, want linux/amd64", opts.Platform)
		}
		if opts.MaxCPUs != -1 {
			t.Errorf("MaxCPUs = %d, want automatic runtime default", opts.MaxCPUs)
		}
		wantRunArgs, err := containerRunArgs()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(opts.ExtraRunArgs, wantRunArgs) {
			t.Errorf("ExtraRunArgs = %v, want %v", opts.ExtraRunArgs, wantRunArgs)
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

	t.Run("Antigravity state", func(t *testing.T) {
		t.Parallel()
		const dir = "/home/user/.gemini"
		src := &fakeMDContainer{
			agentMounts: []md.Mount{{HostPath: dir, ContainerPath: dir}},
			forkResult:  &fakeMDContainer{name: "agy-fork"},
		}
		b := newTestBackend(t, &fakeMDClient{getResult: src})
		opts, err := b.mdStartOpts(src, &runtime.StartOptions{Harness: harness.Antigravity})
		if err != nil {
			t.Fatal(err)
		}
		if len(src.agentPaths) != 1 || src.agentPaths[0].ReadOnly ||
			!slices.Equal(src.agentPaths[0].HomePaths, []string{".gemini"}) {
			t.Fatalf("Antigravity paths = %+v, want writable .gemini directory", src.agentPaths)
		}
		if len(opts.Mounts) != 1 || opts.Mounts[0].ContainerPath != dir || opts.Mounts[0].ReadOnly {
			t.Fatalf("start mounts = %+v, want writable .gemini directory", opts.Mounts)
		}
		if _, _, err := b.Fork(t.Context(), "docker:src", &runtime.ForkOptions{Harness: harness.Antigravity}); err != nil {
			t.Fatal(err)
		}
		if len(src.forkOpts.Mounts) != 1 || src.forkOpts.Mounts[0].ContainerPath != dir || src.forkOpts.Mounts[0].ReadOnly {
			t.Fatalf("fork mounts = %+v, want writable .gemini directory", src.forkOpts.Mounts)
		}
	})

	t.Run("CPU limits", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			cpus int
			want int
		}{
			{name: "default", want: -1},
			{name: "negative", cpus: -2, want: -1},
			{name: "automatic", cpus: -1, want: -1},
			{name: "explicit", cpus: 5, want: 5},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				src := &fakeMDContainer{forkResult: &fakeMDContainer{name: "fork-1"}}
				b := newTestBackend(t, &fakeMDClient{getResult: src})
				opts, err := b.mdStartOpts(src, &runtime.StartOptions{Harness: harness.Claude, MaxCPUs: tc.cpus})
				if err != nil {
					t.Fatal(err)
				}
				if opts.MaxCPUs != tc.want {
					t.Errorf("start MaxCPUs = %d, want %d", opts.MaxCPUs, tc.want)
				}
				if _, _, err = b.Fork(t.Context(), "docker:src", &runtime.ForkOptions{Harness: harness.Claude, MaxCPUs: tc.cpus}); err != nil {
					t.Fatal(err)
				}
				if src.forkOpts == nil || src.forkOpts.MaxCPUs != tc.want {
					t.Errorf("fork options = %+v, want MaxCPUs %d", src.forkOpts, tc.want)
				}
			})
		}
	})

	t.Run("incrementOOMScoreAdj", func(t *testing.T) {
		t.Parallel()
		for _, test := range []struct {
			name string
			raw  string
			want int
		}{
			{name: "negative", raw: "-1000\n", want: -900},
			{name: "service", raw: "100\n", want: 200},
			{name: "cap", raw: "1000\n", want: 1000},
		} {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				got, err := incrementOOMScoreAdj(test.raw)
				if err != nil {
					t.Fatal(err)
				}
				if got != test.want {
					t.Errorf("incrementOOMScoreAdj(%q) = %d, want %d", test.raw, got, test.want)
				}
			})
		}
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			if _, err := incrementOOMScoreAdj("invalid"); err == nil {
				t.Fatal("incrementOOMScoreAdj() error = nil, want parse error")
			}
		})
	})
}

func TestCommandOutputError(t *testing.T) {
	t.Parallel()
	ct := &fakeMDContainer{name: "md-caic-1"}
	exitErr := errors.New("exit status 128")
	t.Run("valid keeps short output", func(t *testing.T) {
		t.Parallel()
		got := commandOutputError("git status", ct, exitErr, []byte("fatal: boom\n")).Error()
		want := `git status in container md-caic-1: exit status 128 (output: "fatal: boom\n")`
		if got != want {
			t.Errorf("commandOutputError() = %q, want %q", got, want)
		}
	})
	t.Run("valid bounds long output to its tail", func(t *testing.T) {
		t.Parallel()
		out := append(bytes.Repeat([]byte("a"), 4096), []byte("fatal: boom")...)
		got := commandOutputError("git status", ct, exitErr, out).Error()
		if !strings.Contains(got, "fatal: boom") {
			t.Errorf("commandOutputError() dropped the fatal message: %q", got)
		}
		if strings.Contains(got, strings.Repeat("a", 600)) {
			t.Errorf("commandOutputError() kept the head of a long report: %q", got)
		}
	})
}

// sshCommandContainer returns a real local command from SSHCommand so tests can
// exercise commandOutput without a container. The command reports on stdout and
// emits a diagnostic on stderr, mirroring a container shell that prints a
// startup error such as a malformed ~/.env.
type sshCommandContainer struct {
	fakeMDContainer
}

func (*sshCommandContainer) SSHCommand([]string, string) []string {
	return []string{"sh", "-c", "printf 'report\\n'; printf 'diagnostic\\n' >&2"}
}

func TestCommandOutput(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t, nil)
	res, err := b.commandOutput(t.Context(), &sshCommandContainer{name: "md-caic-1"}, "ignored")
	if err != nil {
		t.Fatalf("commandOutput() error = %v", err)
	}
	if got, want := string(res.Stdout), "report\n"; got != want {
		t.Errorf("Stdout = %q, want %q", got, want)
	}
	if got, want := string(res.Stderr), "diagnostic\n"; got != want {
		t.Errorf("Stderr = %q, want %q", got, want)
	}
}

// fileCommandContainer executes Linux container commands locally. Callers must
// run only on Linux because the commands require GNU dd and stat.
type fileCommandContainer struct{ fakeMDContainer }

func (*fileCommandContainer) SSHCommand(_ []string, command string) []string {
	return []string{"sh", "-c", command}
}

// streamCommandContainer supplies a controlled long-lived or failing producer.
type streamCommandContainer struct {
	fakeMDContainer

	command string
}

func (c *streamCommandContainer) SSHCommand([]string, string) []string {
	return []string{"sh", "-c", c.command}
}

// gitCommandContainer executes the same Bash probes as a real container while
// its embedded md fake models branch fetching separately.
type gitCommandContainer struct{ fakeMDContainer }

func (*gitCommandContainer) SSHCommand(_ []string, cmd string) []string {
	return []string{"bash", "-c", cmd}
}

// homeCommandContainer runs Bash probes with a private HOME, because the
// pre-push checkout lives under it.
type homeCommandContainer struct {
	gitCommandContainer

	home string
	opts []string // SSH options of the last command.
}

func (c *homeCommandContainer) SSHCommand(opts []string, cmd string) []string {
	c.opts = opts
	return []string{"bash", "-c", "export HOME=" + shellQuote(c.home) + "; " + cmd}
}

// firstWriteSink records hook output and closes first on the first write.
type firstWriteSink struct {
	first chan struct{}
	once  sync.Once

	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *firstWriteSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.buf.Write(p)
	s.mu.Unlock()
	s.once.Do(func() { close(s.first) })
	return n, err
}

func (s *firstWriteSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// failingSink rejects every write.
type failingSink struct{ writes int }

func (s *failingSink) Write([]byte) (int, error) {
	s.writes++
	return 0, errors.New("sink closed")
}

func TestRunPrePush(t *testing.T) {
	t.Parallel()
	newRepo := func(t *testing.T, hook string) (b *Backend, dir, commit, home string) {
		dir = initStatusRepo(t)
		commit = runTestGitOutput(t, dir, "rev-parse", "HEAD")
		if hook != "" {
			if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-push"), []byte(hook), 0o700); err != nil { //nolint:gosec // fixture-owned executable hook.
				t.Fatal(err)
			}
		}
		home = t.TempDir()
		ct := &homeCommandContainer{repo: []md.Repo{{GitRoot: dir, ContainerPath: dir}}, home: home}
		return newTestBackend(t, &fakeMDClient{getResult: ct}), dir, commit, home
	}
	dest := func(base string) runtime.PushRefs {
		return runtime.PushRefs{Remote: "origin", Branch: "task", RemoteCommit: base}
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		out := filepath.Join(t.TempDir(), "seen")
		b, dir, commit, _ := newRepo(t, "#!/bin/sh\n{ echo \"$@\"; cat; git rev-parse HEAD; git status --porcelain; } > "+shellQuote(out)+"\n")
		// Pending edits in the task checkout never reach the hook's checkout.
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("dirty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, dest(commit), nil); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(out) //nolint:gosec // fixture-owned output.
		if err != nil {
			t.Fatal(err)
		}
		remote := dest(commit).Remote // The hook receives it as both name and URL.
		want := remote + " " + remote + "\nrefs/heads/task " + commit + " refs/heads/task " + commit + "\n" + commit + "\n"
		if string(got) != want {
			t.Fatalf("hook saw %q, want %q", got, want)
		}
		if err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, dest(commit), nil); err != nil {
			t.Fatalf("reused checkout: %v", err)
		}
	})

	t.Run("RetainedSubmodule", func(t *testing.T) {
		t.Parallel()
		b, root, _, _ := newRepo(t, "#!/bin/sh\nexit 1\n")
		source := initStatusRepo(t)
		commit := runTestGitOutput(t, source, "rev-parse", "HEAD")
		runTestGitOutput(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "--name", "retained-name", source, "child with space")
		runTestGitOutput(t, root, "rm", "-f", "child with space")
		gitDir := filepath.Join(root, ".git", "modules", "retained-name")
		hook := "#!/bin/sh\ngit rev-parse HEAD\n"
		if err := os.WriteFile(filepath.Join(gitDir, "hooks", "pre-push"), []byte(hook), 0o700); err != nil { //nolint:gosec // Fixture-owned executable hook.
			t.Fatal(err)
		}
		refs := dest(commit)
		refs.Submodule = &runtime.PushSubmodule{Path: "child with space", GitDir: "retained-name"}
		var out bytes.Buffer
		if err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, refs, &out); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != commit+"\n" {
			t.Fatalf("submodule hook output = %q, want commit %s", got, commit)
		}
	})

	t.Run("Streams", func(t *testing.T) {
		t.Parallel()
		release := filepath.Join(t.TempDir(), "release")
		b, _, commit, _ := newRepo(t, "#!/bin/sh\necho first\nwhile [ ! -e "+shellQuote(release)+" ]; do sleep 0.02; done\necho second\n")
		sink := &firstWriteSink{first: make(chan struct{})}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		done := make(chan error, 1)
		go func() { done <- b.RunPrePush(ctx, "docker:ctr", 0, commit, dest(commit), sink) }()
		select {
		case <-sink.first:
		case err := <-done:
			t.Fatalf("hook ended before the sink saw output: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatal("sink saw no output while the hook was running")
		}
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got, want := sink.String(), "first\nsecond\n"; got != want {
			t.Fatalf("sink got %q, want %q", got, want)
		}
	})

	t.Run("FailingSink", func(t *testing.T) {
		t.Parallel()
		b, _, commit, _ := newRepo(t, "#!/bin/sh\necho one\nsleep 0.1\necho two\nexit 3\n")
		sink := &failingSink{}
		err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, dest(commit), sink)
		// The rejection keeps the full tail although the sink failed on the first write.
		if err == nil || !strings.HasSuffix(err.Error(), "\none\ntwo\n") {
			t.Fatalf("err=%q, want the hook's output", err)
		}
		if sink.writes != 1 {
			t.Fatalf("failed sink was written %d times, want 1", sink.writes)
		}
	})

	t.Run("RequestsPty", func(t *testing.T) {
		t.Parallel()
		// Without a remote pty, dropping the SSH session leaves the hook running.
		dir := initStatusRepo(t)
		commit := runTestGitOutput(t, dir, "rev-parse", "HEAD")
		ct := &homeCommandContainer{repo: []md.Repo{{GitRoot: dir, ContainerPath: dir}}, home: t.TempDir()}
		b := newTestBackend(t, &fakeMDClient{getResult: ct})
		if err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, dest(commit), nil); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ct.opts, []string{"-tt"}) {
			t.Fatalf("ssh options = %q, want -tt", ct.opts)
		}
	})

	t.Run("CleansUp", func(t *testing.T) {
		t.Parallel()
		b, dir, commit, home := newRepo(t, "#!/bin/sh\nexit 0\n")
		// A directory left by a killed session is replaced.
		for range 2 {
			if err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, dest(commit), nil); err != nil {
				t.Fatal(err)
			}
			if left, _ := filepath.Glob(filepath.Join(home, "src", "*")); len(left) != 0 {
				t.Fatalf("worktrees left behind: %q", left)
			}
			stale := filepath.Join(home, "src", filepath.Base(dir)+"-"+commit[:7])
			if err := os.MkdirAll(stale, 0o750); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("MissingHook", func(t *testing.T) {
		t.Parallel()
		b, _, commit, _ := newRepo(t, "")
		if err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, dest(commit), nil); err != nil {
			t.Fatalf("no hook is nothing to run: %v", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		t.Run("Rejection", func(t *testing.T) {
			t.Parallel()
			b, _, commit, _ := newRepo(t, "#!/bin/sh\necho check one\necho check two\nexit 77\n")
			err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, dest(commit), nil)
			// The output keeps its line breaks, after the cause.
			if err == nil || !strings.HasSuffix(err.Error(), "\ncheck one\ncheck two\n") {
				t.Fatalf("err=%q, want the hook's output on its own lines", err)
			}
		})
		t.Run("LongOutput", func(t *testing.T) {
			t.Parallel()
			b, _, commit, _ := newRepo(t, "#!/bin/sh\nhead -c 40000 /dev/zero | tr '\\0' x\nprintf '\\nlast\\n'\nexit 1\n")
			err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, dest(commit), nil)
			if err == nil || !strings.HasSuffix(err.Error(), "\nlast\n") || !strings.Contains(err.Error(), "\n…x") || len(err.Error()) > prePushOutputLimit+512 {
				t.Fatalf("err has %d bytes, want the output tail only", len(err.Error()))
			}
		})
		t.Run("Arguments", func(t *testing.T) {
			t.Parallel()
			b, _, commit, _ := newRepo(t, "#!/bin/sh\n")
			for name, c := range map[string]string{"ShortCommit": commit[:8], "NoCommit": ""} {
				if err := b.RunPrePush(t.Context(), "docker:ctr", 0, c, dest(commit), nil); err == nil {
					t.Errorf("%s accepted", name)
				}
			}
			if err := b.RunPrePush(t.Context(), "docker:ctr", 0, commit, runtime.PushRefs{Remote: "origin", Branch: "task"}, nil); err == nil {
				t.Error("missing remote commit accepted")
			}
			if err := b.RunPrePush(t.Context(), "docker:ctr", 1, commit, dest(commit), nil); err == nil {
				t.Error("repo index out of range accepted")
			}
		})
	})
}

func TestTurnSnapshotBackend(t *testing.T) {
	t.Parallel()
	dir := initStatusRepo(t)
	tip := runTestGitOutput(t, dir, "rev-parse", "HEAD")

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		ct := &gitCommandContainer{repo: []md.Repo{{GitRoot: dir, ContainerPath: dir, Branches: []string{"main"}, DefaultRemote: "origin", DefaultBranch: "main"}}, fetchResults: [][]md.FetchedBranch{{{BranchName: "main", CommitHash: tip}}}}
		backend := newTestBackend(t, &fakeMDClient{getResult: ct})
		measured, err := backend.TurnSnapshot(t.Context(), "docker:ctr", []runtime.FetchedBranch{{RepositoryPath: dir, BranchName: "main", CommitHash: tip}})
		if err != nil {
			t.Fatalf("measurement error = %v", err)
		}
		if len(measured) != 1 || len(measured[0].Branches) != 1 || measured[0].Branches[0].CommitHash != tip {
			t.Fatalf("fetched tips lost: %+v", measured)
		}
		if measured[0].TurnDiff == nil || len(measured[0].TurnDiff) != 0 {
			t.Fatalf("clean delta unavailable: %+v", measured[0])
		}
		if !slices.Equal(ct.calls, []string{"FetchRecursive"}) {
			t.Fatalf("duplicated reference synchronization: %v", ct.calls)
		}
		if ct.fetchOpts.Commit {
			t.Fatal("turn snapshot committed pending work")
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		fetched := strings.Repeat("f", 40)
		ct := &gitCommandContainer{repo: []md.Repo{{GitRoot: dir, ContainerPath: dir, Branches: []string{"main"}, DefaultRemote: "origin", DefaultBranch: "main"}}, fetchResults: [][]md.FetchedBranch{{{BranchName: "main", CommitHash: fetched}}}}
		backend := newTestBackend(t, &fakeMDClient{getResult: ct})
		measured, err := backend.TurnSnapshot(t.Context(), "docker:ctr", []runtime.FetchedBranch{{RepositoryPath: dir, BranchName: "main", CommitHash: tip}})
		if err == nil {
			t.Fatal("measurement error = nil, want error")
		}
		if len(measured) != 1 || len(measured[0].Branches) != 1 || measured[0].Branches[0].CommitHash != fetched {
			t.Fatalf("fetched tips lost: %+v", measured)
		}
		if measured[0].StatusErr == nil {
			t.Fatal("moving HEAD was published as current")
		}
		if !slices.Equal(ct.calls, []string{"FetchRecursive"}) {
			t.Fatalf("duplicated reference synchronization: %v", ct.calls)
		}
		if ct.fetchOpts.Commit {
			t.Fatal("turn snapshot committed pending work")
		}
	})
}

// TestMain isolates the package from the Git configuration of the host.
func TestMain(m *testing.M) {
	gittest.Isolate()
	os.Exit(m.Run())
}

func BenchmarkParseDiskUsage(b *testing.B) {
	lines := make([]string, 0, 20)
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("/md-caic-task-%d\t%d", i, 100_000_000+i))
	}
	out := strings.Join(lines, "\n")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parseDiskUsage(out, "docker"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadFile(b *testing.B) {
	if goruntime.GOOS != "linux" {
		b.Skip("executes GNU dd from the Linux container locally")
	}
	for _, tc := range []struct {
		name string
		size int
	}{
		{"1MiB", 1 << 20},
		{"16MiB", 16 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "artifact")
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), tc.size), 0o600); err != nil {
				b.Fatal(err)
			}
			backend := newTestBackend(b, &fakeMDClient{getResult: &fileCommandContainer{}})
			id := runtime.NewID("docker", "file-benchmark")
			b.ReportAllocs()
			b.SetBytes(int64(tc.size))
			for b.Loop() {
				n := 0
				for data, err := range backend.ReadFile(b.Context(), id, path, 0, -1) {
					if err != nil {
						b.Fatal(err)
					}
					n += len(data)
				}
				if n != tc.size {
					b.Fatalf("read %d bytes", n)
				}
			}
		})
	}
}

type submoduleBackendFixture struct {
	root        string
	backend     *Backend
	child       string
	childCommit string
	previous    []runtime.FetchedBranch
	nested      string
}

func newSubmoduleBackend(t *testing.T) *submoduleBackendFixture {
	root, child, nested := initStatusRepo(t), initStatusRepo(t), initStatusRepo(t)
	previous := make([]runtime.FetchedBranch, 0, 3)
	parent := &md.Submodule{
		GitlinkPath: "lib with space", ContainerPath: child,
		Children: md.SubmoduleTree{&md.Submodule{GitlinkPath: "nested", ContainerPath: nested}},
	}
	modules := md.SubmoduleTree{parent}
	for _, module := range modules.ParentFirst() {
		baseline := runTestGitOutput(t, module.ContainerPath, "rev-parse", "HEAD")
		runTestGit(t, module.ContainerPath, "update-ref", "refs/md/sync/HEAD", baseline)
		runTestGit(t, module.ContainerPath, "checkout", "--detach")
		if err := os.WriteFile(filepath.Join(module.ContainerPath, "tracked.txt"), []byte("committed child content\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, module.ContainerPath, "add", ".")
		runTestGit(t, module.ContainerPath, "commit", "-m", "Change child")
		module.CommitHash = runTestGitOutput(t, module.ContainerPath, "rev-parse", "HEAD")
		previous = append(previous, runtime.FetchedBranch{RepositoryPath: module.ContainerPath, CommitHash: baseline})
	}
	rootTip := runTestGitOutput(t, root, "rev-parse", "HEAD")
	previous = append(previous, runtime.FetchedBranch{RepositoryPath: root, BranchName: "main", CommitHash: rootTip})
	ct := &gitCommandContainer{repo: []md.Repo{{ContainerPath: root, Branches: []string{"main"}, DefaultRemote: "origin", DefaultBranch: "main"}}, modules: modules, fetchResults: [][]md.FetchedBranch{{{BranchName: "main", CommitHash: rootTip}}}}
	backend := newTestBackend(t, &fakeMDClient{getResult: ct})

	return &submoduleBackendFixture{root: root, backend: backend, child: child, childCommit: parent.CommitHash, previous: previous, nested: nested}
}

// BenchmarkBackend measures the local Git work for recursive compact
// probes. Each descendant adds one independent Git probe; SSH discovery and
// network latency are excluded by the local command fixture.
func BenchmarkBackend(b *testing.B) {
	b.Run("CompactRepositoryStatus", func(b *testing.B) {
		for _, count := range []int{0, 1, 3} {
			b.Run(fmt.Sprintf("children=%d", count), func(b *testing.B) {
				root := initStatusRepo(b)
				modules := make(md.SubmoduleTree, 0, count)
				for i := range count {
					modules = append(modules, &md.Submodule{GitlinkPath: fmt.Sprintf("lib-%d", i), ContainerPath: initStatusRepo(b)})
				}
				ct := &gitCommandContainer{repo: []md.Repo{{ContainerPath: root, DefaultRemote: "origin", DefaultBranch: "main"}}, modules: modules}
				backend := newTestBackend(b, &fakeMDClient{getResult: ct})
				b.ReportAllocs()
				for b.Loop() {
					if _, err := backend.CompactRepositoryStatus(b.Context(), "docker:ctr", 0); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	})
}
