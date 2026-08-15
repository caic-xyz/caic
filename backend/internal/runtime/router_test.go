// Tests for runtime router backend selection and namespacing.

package runtime_test

import (
	"context"
	"errors"
	"io"
	"iter"
	"log/slog"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
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
		router, err := runtime.NewRouter(testLogger(), []runtime.System{docker, podman})
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

	t.Run("rejects unqualified instance IDs", func(t *testing.T) {
		t.Parallel()
		backend := newRouterFakeBackend("docker")
		router, err := runtime.NewRouter(testLogger(), []runtime.System{backend})
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
		router, err := runtime.NewRouter(testLogger(), []runtime.System{backend, newRouterFakeBackend("podman")})
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = router.Fork(t.Context(), "docker:source", &runtime.ForkOptions{RuntimeName: "podman"})
		if err == nil {
			t.Fatal("Fork succeeded, want cross-runtime error")
		}
	})

	t.Run("watch events setup error does not start partial fan-in", func(t *testing.T) {
		t.Parallel()
		events := make(chan runtime.Event, 1)
		ctxDone := make(chan struct{})
		router, err := runtime.NewRouter(testLogger(), []runtime.System{
			&routerEventSystem{FakeBackend: runtimetest.FakeBackend{RuntimeName: "docker"}, routerEventMonitor: routerEventMonitor{events: events, ctxDone: ctxDone}},
			&routerEventSystem{FakeBackend: runtimetest.FakeBackend{RuntimeName: "podman"}, routerEventMonitor: routerEventMonitor{err: errors.New("boom")}},
		})
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

	name     string
	launches int
	lastID   runtime.ID
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

func (f *routerFakeBackend) Fork(context.Context, runtime.ID, *runtime.ForkOptions) (runtime.ID, runtime.ConnectionInfo, []runtime.Repo, error) {
	return runtime.NewID(runtime.Name(f.name), runtime.InstanceID(f.name+"-fork")), runtime.ConnectionInfo{}, nil, nil
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

func (f *routerFakeBackend) WatchEvents(context.Context, runtime.EventFilter) (<-chan runtime.Event, error) {
	ch := make(chan runtime.Event)
	close(ch)
	return ch, nil
}
