// Tests for harness model refresh.

package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

func TestRefreshHarnessModels(t *testing.T) {
	t.Parallel()

	t.Run("valid_fetches_stale_model_fetchers", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		fetchHarness := harness.Name("fetch")
		env := map[string][]string{string(fetchHarness): {"FETCH_API_KEY=secret"}}
		runtimeBackend := &modelRefreshRuntime{}
		inventory := &modelRefreshInventory{}
		router := newModelRefreshRouter(t, runtimeBackend, inventory)
		fetcher := &modelFetchBackend{FakeBackend: &agenttest.FakeBackend{}, harness: fetchHarness, models: []string{"z-model", "a-model"}}
		taskMgr := newModelRefreshTestManager(t.Context(), router, map[harness.Name]agent.Backend{
			fetchHarness: fetcher,
			"plain":      &agenttest.FakeBackend{ModelList: []string{"m1", "m2"}},
		})

		refreshHarnessModels(t.Context(), cacheDir, router, taskMgr, env)

		if runtimeBackend.launches != 1 || runtimeBackend.connects != 1 || runtimeBackend.purges != 1 {
			t.Fatalf("runtime calls = launch %d connect %d purge %d, want 1 each", runtimeBackend.launches, runtimeBackend.connects, runtimeBackend.purges)
		}
		if runtimeBackend.harness != fetchHarness {
			t.Fatalf("launched harness = %q, want %q", runtimeBackend.harness, fetchHarness)
		}
		if runtimeBackend.runtimeName != "test-runtime" {
			t.Fatalf("launched runtime = %q, want test-runtime", runtimeBackend.runtimeName)
		}
		if fetcher.target.SSHHost != "refresh-fetch" {
			t.Fatalf("fetch target = %q, want refresh-fetch", fetcher.target.SSHHost)
		}
		if !slices.Equal(fetcher.env, env[string(fetchHarness)]) {
			t.Fatalf("fetch env = %v, want %v", fetcher.env, env[string(fetchHarness)])
		}
		if !slices.Equal(fetcher.setModels, []string{"z-model", "a-model"}) {
			t.Fatalf("SetModels = %v, want fetched models", fetcher.setModels)
		}
		models, fresh := agent.OpenHarnessCache(cacheDir+"/harnesses.json").Models(fetchHarness, agent.APIKeyHash(env[string(fetchHarness)]))
		if !fresh || !slices.Equal(models, []string{"z-model", "a-model"}) {
			t.Fatalf("cache models = %v fresh=%t, want fetched fresh models", models, fresh)
		}
	})

	t.Run("valid_skips_fresh_cache", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		fetchHarness := harness.Name("fetch")
		cache := agent.OpenHarnessCache(cacheDir + "/harnesses.json")
		cache.SetModels(fetchHarness, []string{"cached-model"}, "")
		runtimeBackend := &modelRefreshRuntime{}
		inventory := &modelRefreshInventory{}
		router := newModelRefreshRouter(t, runtimeBackend, inventory)
		fetcher := &modelFetchBackend{FakeBackend: &agenttest.FakeBackend{}, harness: fetchHarness, models: []string{"new-model"}}
		taskMgr := newModelRefreshTestManager(t.Context(), router, map[harness.Name]agent.Backend{
			fetchHarness: fetcher,
		})

		refreshHarnessModels(t.Context(), cacheDir, router, taskMgr, nil)

		if runtimeBackend.launches != 0 {
			t.Fatalf("launches = %d, want 0 for fresh cache", runtimeBackend.launches)
		}
		if fetcher.target.SSHHost != "" {
			t.Fatalf("fetch target = %q, want no fetch", fetcher.target.SSHHost)
		}
	})

	t.Run("valid_purges_stale_model_refresh_instances", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		fetchHarness := harness.Name("fetch")
		cache := agent.OpenHarnessCache(cacheDir + "/harnesses.json")
		cache.SetModels(fetchHarness, []string{"cached-model"}, "")
		runtimeBackend := &modelRefreshRuntime{}
		inventory := &modelRefreshInventory{
			instances: []runtime.Instance{
				{ID: "test-runtime:stale-refresh"},
				{ID: "test-runtime:regular-task"},
			},
			metadata: map[runtime.ID]map[runtime.MetadataKey]string{
				"test-runtime:stale-refresh": {runtime.MetadataModelRefresh: "true"},
				"test-runtime:regular-task":  {runtime.MetadataTaskID: "task-id"},
			},
		}
		router := newModelRefreshRouter(t, runtimeBackend, inventory)
		fetcher := &modelFetchBackend{FakeBackend: &agenttest.FakeBackend{}, harness: fetchHarness, models: []string{"new-model"}}
		taskMgr := newModelRefreshTestManager(t.Context(), router, map[harness.Name]agent.Backend{
			fetchHarness: fetcher,
		})

		refreshHarnessModels(t.Context(), cacheDir, router, taskMgr, nil)

		if !slices.Equal(runtimeBackend.purgedIDs, []runtime.ID{"test-runtime:stale-refresh"}) {
			t.Fatalf("purged IDs = %v, want stale-refresh", runtimeBackend.purgedIDs)
		}
		if runtimeBackend.launches != 0 {
			t.Fatalf("launches = %d, want 0 for fresh cache", runtimeBackend.launches)
		}
	})
}

func newModelRefreshTestManager(ctx context.Context, router *runtime.Router, backends map[harness.Name]agent.Backend) *taskmgr.Manager {
	return taskmgr.New(taskmgr.Config{
		ServerCtx: ctx,
		Runtimes:  router,
		Backends:  backends,
	})
}

func newModelRefreshRouter(t *testing.T, runtimeBackend runtime.Lifecycle, inventory runtime.Inventory) *runtime.Router {
	router, err := runtime.NewRouter([]runtime.System{&modelRefreshSystem{Lifecycle: runtimeBackend, Inventory: inventory}})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

type modelRefreshSystem struct {
	runtime.Lifecycle
	runtimetest.FakeMonitor
	runtime.Inventory
	runtimetest.FakePrivilegeInfo
}

func (*modelRefreshSystem) Name() runtime.Name { return "test-runtime" }

type modelFetchBackend struct {
	*agenttest.FakeBackend

	harness   harness.Name
	models    []string
	target    runtime.ConnectionTarget
	env       []string
	setModels []string
}

func (b *modelFetchBackend) Harness() harness.Name { return b.harness }

func (b *modelFetchBackend) FetchModels(_ context.Context, target runtime.ConnectionTarget, env []string) ([]string, error) {
	b.target = target
	b.env = append([]string(nil), env...)
	return b.models, nil
}

func (b *modelFetchBackend) SetModels(models []string) {
	b.setModels = append([]string(nil), models...)
}

type modelRefreshRuntime struct {
	launches    int
	connects    int
	purges      int
	runtimeName runtime.Name
	harness     harness.Name
	purgedIDs   []runtime.ID
}

var _ runtime.Lifecycle = (*modelRefreshRuntime)(nil)

func (r *modelRefreshRuntime) Launch(_ context.Context, _ []runtime.Repo, opts *runtime.StartOptions) (runtime.ID, error) {
	r.launches++
	r.runtimeName = opts.RuntimeName
	r.harness = opts.Harness
	return runtime.NewID("test-runtime", runtime.InstanceID("refresh-"+string(opts.Harness))), nil
}

func (r *modelRefreshRuntime) Connect(_ context.Context, id runtime.ID, _ *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	r.connects++
	return runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: string(id.InstanceID())}}, nil
}

func (*modelRefreshRuntime) Diff(_ context.Context, _ runtime.ID, _ int, _ ...string) (string, error) {
	return "", nil
}

func (*modelRefreshRuntime) Fetch(_ context.Context, _ runtime.ID) error { return nil }

func (*modelRefreshRuntime) Stop(_ context.Context, _ runtime.ID) error { return nil }

func (r *modelRefreshRuntime) Purge(_ context.Context, id runtime.ID) error {
	r.purges++
	r.purgedIDs = append(r.purgedIDs, id)
	return nil
}

func (*modelRefreshRuntime) Revive(_ context.Context, _ runtime.ID) error { return nil }

func (*modelRefreshRuntime) Fork(_ context.Context, _ runtime.ID, _ *runtime.ForkOptions) (runtime.ID, runtime.ConnectionInfo, []runtime.Repo, error) {
	return "", runtime.ConnectionInfo{}, nil, errors.New("fork not implemented")
}

func (*modelRefreshRuntime) VNCPort(_ context.Context, _ runtime.ID) int { return 0 }

func (*modelRefreshRuntime) Processes(_ context.Context, _ runtime.ID) ([]runtime.ProcessInfo, error) {
	return nil, nil
}

func (*modelRefreshRuntime) Signal(_ context.Context, _ runtime.ID, _ int, _ string) error {
	return nil
}

type modelRefreshInventory struct {
	instances []runtime.Instance
	metadata  map[runtime.ID]map[runtime.MetadataKey]string
}

var _ runtime.Inventory = (*modelRefreshInventory)(nil)

func (i *modelRefreshInventory) List(context.Context) ([]runtime.Instance, error) {
	return append([]runtime.Instance(nil), i.instances...), nil
}

func (i *modelRefreshInventory) Metadata(_ context.Context, id runtime.ID, key runtime.MetadataKey) (string, error) {
	return i.metadata[id][key], nil
}

func (*modelRefreshInventory) Inspect(context.Context, runtime.ID) (*runtime.InstanceInspect, error) {
	return nil, errors.New("inspect not implemented")
}
