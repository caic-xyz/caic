// Tests for Backend's runtime.System logic using fake md seams.

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
	"testing"
	"time"

	"github.com/caic-xyz/md"

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

func (f *fakeMDContainer) Diff(_ context.Context, _, _ io.Writer, repoIdx int, opts *md.DiffOpts) error {
	f.calls = append(f.calls, "Diff")
	f.diffIdx = repoIdx
	f.diffOpts = opts
	return nil
}

func (f *fakeMDContainer) Fetch(_ context.Context, _, _ io.Writer, repoIdx int, opts *md.FetchOpts) ([]md.FetchedBranch, error) {
	f.calls = append(f.calls, "Fetch")
	f.fetchOpts = opts
	if repoIdx >= len(f.fetchResults) {
		return nil, nil
	}
	return slices.Clone(f.fetchResults[repoIdx]), nil
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

func (*fakeMDClient) WatchEvents(context.Context, runtime.EventFilter) (<-chan runtime.Event, error) {
	ch := make(chan runtime.Event)
	close(ch)
	return ch, nil
}

func (*fakeMDClient) SudoPassword(context.Context, runtime.InstanceID) (string, error) {
	return "", nil
}

func newTestBackend(c mdClient) *Backend {
	return &Backend{log: slog.New(slog.DiscardHandler), client: c, containers: make(map[string]mdContainer), vncPorts: make(map[string]int32)}
}

func TestBackend(t *testing.T) {
	t.Parallel()
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
		b := newTestBackend(&fakeMDClient{getResult: &fileCommandContainer{}})
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
			{dir, fs.ErrInvalid},
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
		b := newTestBackend(&fakeMDClient{getResult: &fileCommandContainer{}})
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
	t.Run("ReadFile early stop", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		t.Cleanup(cancel)
		b := newTestBackend(&fakeMDClient{getResult: &streamCommandContainer{command: "exec yes x"}})
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
		b := newTestBackend(&fakeMDClient{getResult: &streamCommandContainer{command: "exec yes x"}})
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
		b := newTestBackend(&fakeMDClient{getResult: &streamCommandContainer{command: "head -c 65536 /dev/zero; head -c 1048576 /dev/zero >&2; printf 'useful diagnostic' >&2; exit 1"}})
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
		backend := newTestBackend(client)
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
			{GitRoot: "/home/user/src/caic", Branches: []string{"caic-7"}, ContainerPath: "/home/user/src/caic"},
			{GitRoot: "/home/user/src/genai", Branches: []string{"caic-0"}, ContainerPath: "/home/user/src/genai"},
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
		if ctr.diffOpts == nil || !ctr.diffOpts.Full || !slices.Equal(ctr.diffOpts.Args, []string{"--numstat"}) {
			t.Errorf("Diff opts = %+v, want the whole branch with --numstat", ctr.diffOpts)
		}
	})

	t.Run("RefreshRefs", func(t *testing.T) {
		t.Parallel()
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("error=%v", fail), func(t *testing.T) {
				t.Parallel()
				ctr := &fakeMDContainer{repo: []md.Repo{{ContainerPath: "/repo/one"}, {ContainerPath: "/repo/two"}}}
				if fail {
					ctr.syncErr = errors.New("upstream sync failed")
				}
				b := newTestBackend(&fakeMDClient{getResult: ctr})
				err := b.RefreshRefs(t.Context(), "docker:ctr-1")
				if fail && (!errors.Is(err, ctr.syncErr) || !strings.Contains(err.Error(), "/repo/one")) {
					t.Fatalf("refresh must surface repository error: %v", err)
				}
				if !fail && err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(ctr.calls, []string{"SyncDefaultBranch:0", "SyncDefaultBranch:1"}) {
					t.Fatalf("refresh must sync every repository without committing: %v", ctr.calls)
				}
			})
		}
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
		b := newTestBackend(fc)
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
		ctr := &fakeMDContainer{name: "ctr-1", repo: []md.Repo{{GitRoot: "/repo", Branches: []string{"caic-0"}, ContainerPath: "/repo"}}}
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
		ctr := &fakeMDContainer{name: "ctr-1", vncPort: 5903, repo: []md.Repo{{GitRoot: "/repo", Branches: []string{"caic-0"}, ContainerPath: "/repo"}}}
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
		b := newTestBackend(&fakeMDClient{getResult: src})
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
		for _, want := range []string{"EDITOR=true", "GIT_EDITOR=true", "FOO=bar", "GITHUB_TOKEN=tok"} {
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

	t.Run("mdStartOpts", func(t *testing.T) {
		t.Parallel()
		fc := &fakeMDContainer{
			agentMounts: []md.Mount{{HostPath: "/home/user/.claude", ContainerPath: "/home/user/.claude"}},
		}
		b := newTestBackend(&fakeMDClient{})
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
		if !slices.Contains(opts.ExtraEnv, "EDITOR=true") {
			t.Errorf("ExtraEnv missing EDITOR=true: %v", opts.ExtraEnv)
		}
		if !slices.Contains(opts.ExtraEnv, "GIT_EDITOR=true") {
			t.Errorf("ExtraEnv missing GIT_EDITOR=true: %v", opts.ExtraEnv)
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
		b := newTestBackend(&fakeMDClient{getResult: src})
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
				b := newTestBackend(&fakeMDClient{getResult: src})
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
	b := newTestBackend(nil)
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

func TestTurnSnapshotBackend(t *testing.T) {
	t.Parallel()
	dir := initStatusRepo(t)
	tip := runTestGitOutput(t, dir, "rev-parse", "HEAD")
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid fetched tip %v", invalid), func(t *testing.T) {
			t.Parallel()
			fetched := tip
			if invalid {
				fetched = strings.Repeat("f", 40)
			}
			ct := &gitCommandContainer{repo: []md.Repo{{GitRoot: dir, ContainerPath: dir, Branches: []string{"main"}, DefaultRemote: "origin", DefaultBranch: "main"}}, fetchResults: [][]md.FetchedBranch{{{BranchName: "main", CommitHash: fetched}}}}
			backend := newTestBackend(&fakeMDClient{getResult: ct})
			measured, err := backend.TurnSnapshot(t.Context(), "docker:ctr", []runtime.FetchedBranch{{RepositoryPath: dir, BranchName: "main", CommitHash: tip}})
			if (err != nil) != invalid {
				t.Fatalf("measurement error = %v", err)
			}
			if len(measured) != 1 || len(measured[0].Branches) != 1 || measured[0].Branches[0].CommitHash != fetched {
				t.Fatalf("fetched tips lost: %+v", measured)
			}
			if invalid {
				if measured[0].StatusErr == nil {
					t.Fatal("moving HEAD was published as current")
				}
			} else {
				if measured[0].TurnDiff == nil || len(measured[0].TurnDiff) != 0 {
					t.Fatalf("clean delta unavailable: %+v", measured[0])
				}
			}
			if !slices.Equal(ct.calls, []string{"Fetch"}) {
				t.Fatalf("duplicated reference synchronization: %v", ct.calls)
			}
			if ct.fetchOpts.Commit {
				t.Fatal("turn snapshot committed pending work")
			}
		})
	}
}
