// Tests for runtime router backend selection and namespacing.

package runtime_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/metrics"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestRouter(t *testing.T) {
	t.Parallel()
	t.Run("instance ID exposes runtime and local instance", func(t *testing.T) {
		t.Parallel()
		id := runtime.NewID("podman", "podman-1")
		if id != "podman:podman-1" {
			t.Fatalf("NewID() = %q, want podman:podman-1", id)
		}
		if got := id.RuntimeName(); got != "podman" {
			t.Fatalf("RuntimeName() = %q, want podman", got)
		}
		if got := id.InstanceID(); got != "podman-1" {
			t.Fatalf("InstanceID() = %q, want podman-1", got)
		}
		if got := runtime.NewID("docker", id.InstanceID()); got != "docker:podman-1" {
			t.Fatalf("requalified NewID() = %q, want docker:podman-1", got)
		}
		legacy := runtime.ID("legacy-1")
		if got := legacy.RuntimeName(); got != "" {
			t.Fatalf("legacy RuntimeName() = %q, want empty", got)
		}
		if got := legacy.InstanceID(); got != runtime.InstanceID(legacy) {
			t.Fatalf("legacy InstanceID() = %q, want %q", got, legacy)
		}
	})
	t.Run("routes by selected runtime and prefixes instance IDs", func(t *testing.T) {
		t.Parallel()
		docker := newRouterFakeBackend("docker")
		podman := newRouterFakeBackend("podman")
		router, err := runtime.NewRouter(testLogger(), []runtime.System{docker, podman}, metrics.Nop{})
		if err != nil {
			t.Fatal(err)
		}

		id, err := router.Launch(t.Context(), nil, &runtime.StartOptions{RuntimeName: "podman", Metadata: runtime.Metadata{}, LogWriter: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		if id != "podman:podman-1" {
			t.Fatalf("Launch ID = %q, want podman:podman-1", id)
		}
		if podman.launches != 1 || docker.launches != 0 {
			t.Fatalf("launches docker=%d podman=%d, want docker=0 podman=1", docker.launches, podman.launches)
		}
		if _, err := router.Diff(t.Context(), id, 0); err != nil {
			t.Fatal(err)
		}
		if podman.lastID != "podman:podman-1" {
			t.Fatalf("routed ID = %q, want podman:podman-1", podman.lastID)
		}

		instances, err := router.List(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(instances) != 2 {
			t.Fatalf("instances len = %d, want 2", len(instances))
		}
		if instances[0].ID != "docker:docker-existing" {
			t.Fatalf("instances[0] = %+v", instances[0])
		}
		if instances[1].ID != "podman:podman-existing" {
			t.Fatalf("instances[1] = %+v", instances[1])
		}
	})

	t.Run("batches disk usage by runtime", func(t *testing.T) {
		t.Parallel()
		docker := newRouterFakeBackend("docker")
		podman := newRouterFakeBackend("podman")
		router, err := runtime.NewRouter(testLogger(), []runtime.System{docker, podman}, metrics.Nop{})
		if err != nil {
			t.Fatal(err)
		}
		ids := []runtime.ID{"docker:one", "podman:two", "docker:three"}
		usage, err := router.DiskUsage(t.Context(), ids)
		if err != nil {
			t.Fatal(err)
		}
		if docker.diskCalls != 1 || podman.diskCalls != 1 {
			t.Fatalf("disk calls docker=%d podman=%d, want one each", docker.diskCalls, podman.diskCalls)
		}
		if !slices.Equal(docker.diskIDs, []runtime.ID{"docker:one", "docker:three"}) {
			t.Fatalf("docker disk ids = %v", docker.diskIDs)
		}
		if len(usage) != len(ids) {
			t.Fatalf("disk usage len = %d, want %d", len(usage), len(ids))
		}
	})

	t.Run("rejects unqualified instance IDs", func(t *testing.T) {
		t.Parallel()
		backend := newRouterFakeBackend("docker")
		router, err := runtime.NewRouter(testLogger(), []runtime.System{backend}, metrics.Nop{})
		if err != nil {
			t.Fatal(err)
		}
		if err := router.Stop(t.Context(), "source"); err == nil {
			t.Fatal("Stop succeeded, want unqualified instance ID error")
		}
	})

	t.Run("rejects cross runtime fork", func(t *testing.T) {
		t.Parallel()
		backend := newRouterFakeBackend("docker")
		router, err := runtime.NewRouter(testLogger(), []runtime.System{backend, newRouterFakeBackend("podman")}, metrics.Nop{})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = router.Fork(t.Context(), "docker:source", &runtime.ForkOptions{RuntimeName: "podman"})
		if err == nil {
			t.Fatal("Fork succeeded, want cross-runtime error")
		}
	})

	t.Run("watch events setup error does not start partial fan-in", func(t *testing.T) {
		t.Parallel()
		events := make(chan runtime.Event, 1)
		ctxDone := make(chan struct{})
		router, err := runtime.NewRouter(testLogger(), []runtime.System{
			&routerEventSystem{RuntimeName: "docker", events: events, ctxDone: ctxDone},
			&routerEventSystem{RuntimeName: "podman", err: errors.New("boom")},
		}, metrics.Nop{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := router.WatchEvents(t.Context(), runtime.EventFilter{}); err == nil {
			t.Fatal("WatchEvents succeeded, want setup error")
		}
		select {
		case <-ctxDone:
		case <-time.After(time.Second):
			t.Fatal("first runtime watch context was not cancelled")
		}
		events <- runtime.Event{InstanceID: "md-agent-1"}
	})
}

func TestRouterRecordsOperationMetrics(t *testing.T) {
	t.Parallel()
	store := metrics.NewStore(metrics.Resource{ServiceName: "caic"})
	backend := newRouterFakeBackend("docker")
	router, err := runtime.NewRouter(testLogger(), []runtime.System{backend}, store)
	if err != nil {
		t.Fatal(err)
	}

	id, err := router.Launch(t.Context(), nil, &runtime.StartOptions{RuntimeName: "docker", Metadata: runtime.Metadata{}, LogWriter: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Diff(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	if err := router.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := router.Purge(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	series := make(map[string]metrics.Series)
	for _, s := range store.Snapshot() {
		series[s.Name] = s
	}
	for _, name := range []string{"container.launch", "container.stop", "container.purge", "repo.diff"} {
		s, ok := series[name]
		if !ok {
			t.Fatalf("metrics = %+v, want %s", store.Snapshot(), name)
		}
		if s.Outcome != metrics.OutcomeOK || s.Calls != 1 {
			t.Errorf("%s = %+v, want one ok call", name, s)
		}
	}

	// A failing call must be recorded as an error. The deferred recorder reads
	// the named return value when the function returns, so any form that read
	// the error earlier would report every failure as a success and still look
	// healthy here.
	if _, err := router.Diff(t.Context(), runtime.NewID("missing-runtime", "ctr-1"), 0); err == nil {
		t.Fatal("Diff on an unknown runtime succeeded")
	}
	failures := int64(0)
	for _, s := range store.Snapshot() {
		if s.Name == "repo.diff" && s.Outcome == metrics.OutcomeError {
			failures = s.Calls
		}
	}
	if failures != 1 {
		t.Fatalf("metrics = %+v, want one repo.diff error call", store.Snapshot())
	}
}

type routerEventSystem struct {
	runtimetest.FakeBackend
	routerEventMonitor
	runtimetest.FakeInventory
	runtimetest.FakePrivilegeInfo
}

type routerEventMonitor struct {
	events  <-chan runtime.Event
	err     error
	ctxDone chan<- struct{}
}

func (m *routerEventMonitor) WatchStats(context.Context, []runtime.ID) (iter.Seq2[runtime.StatsSample, error], error) {
	return func(func(runtime.StatsSample, error) bool) {}, nil
}

func (m *routerEventMonitor) DiskUsage(context.Context, []runtime.ID) (map[runtime.ID]int64, error) {
	return map[runtime.ID]int64{}, nil
}

func (m *routerEventMonitor) WatchEvents(ctx context.Context, _ runtime.EventFilter) (<-chan runtime.Event, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.ctxDone != nil {
		go func() {
			<-ctx.Done()
			close(m.ctxDone)
		}()
	}
	return m.events, nil
}

type routerFakeBackend struct {
	*runtimetest.FakeBackend

	name      string
	launches  int
	lastID    runtime.ID
	diskCalls int
	diskIDs   []runtime.ID
}

func newRouterFakeBackend(name string) *routerFakeBackend {
	return &routerFakeBackend{FakeBackend: &runtimetest.FakeBackend{RuntimeName: runtime.Name(name)}, name: name}
}

func (f *routerFakeBackend) Launch(_ context.Context, _ []runtime.Repo, _ *runtime.StartOptions) (runtime.ID, error) {
	f.launches++
	return runtime.NewID(runtime.Name(f.name), runtime.InstanceID(f.name+"-1")), nil
}

func (f *routerFakeBackend) Diff(ctx context.Context, id runtime.ID, repoIdx int, args ...string) (string, error) {
	f.lastID = id
	return f.FakeBackend.Diff(ctx, id, repoIdx, args...)
}

func (f *routerFakeBackend) Fork(context.Context, runtime.ID, *runtime.ForkOptions) (runtime.ID, runtime.ConnectionInfo, error) {
	return runtime.NewID(runtime.Name(f.name), runtime.InstanceID(f.name+"-fork")), runtime.ConnectionInfo{}, nil
}

func (f *routerFakeBackend) List(context.Context) ([]runtime.Instance, error) {
	return []runtime.Instance{{ID: runtime.NewID(runtime.Name(f.name), runtime.InstanceID(f.name+"-existing"))}}, nil
}

func (f *routerFakeBackend) Metadata(context.Context, runtime.ID, runtime.MetadataKey) (string, error) {
	return "", nil
}

func (f *routerFakeBackend) Inspect(_ context.Context, id runtime.ID) (*runtime.InstanceInspect, error) {
	return &runtime.InstanceInspect{ID: id}, nil
}

func (f *routerFakeBackend) SudoPassword(context.Context, runtime.ID) (string, error) {
	return "", nil
}

func (f *routerFakeBackend) WatchStats(context.Context, []runtime.ID) (iter.Seq2[runtime.StatsSample, error], error) {
	return func(func(runtime.StatsSample, error) bool) {}, nil
}

func (f *routerFakeBackend) DiskUsage(_ context.Context, ids []runtime.ID) (map[runtime.ID]int64, error) {
	f.diskCalls++
	f.diskIDs = slices.Clone(ids)
	usage := make(map[runtime.ID]int64, len(ids))
	for _, id := range ids {
		usage[id] = 1
	}
	return usage, nil
}

func (f *routerFakeBackend) WatchEvents(context.Context, runtime.EventFilter) (<-chan runtime.Event, error) {
	ch := make(chan runtime.Event)
	close(ch)
	return ch, nil
}
