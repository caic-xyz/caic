// Tests for harness model refresh.

package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
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
		fetcher := &modelFetchBackend{harness: fetchHarness, models: []string{"z-model", "a-model"}}
		taskMgr := newModelRefreshTestManager(t.Context(), runtimeBackend, map[harness.Name]agent.Backend{
			fetchHarness: fetcher,
			"plain":      stubBackend{},
		})

		refreshHarnessModels(t.Context(), cacheDir, runtimeBackend, taskMgr, env)

		if runtimeBackend.launches != 1 || runtimeBackend.connects != 1 || runtimeBackend.purges != 1 {
			t.Fatalf("runtime calls = launch %d connect %d purge %d, want 1 each", runtimeBackend.launches, runtimeBackend.connects, runtimeBackend.purges)
		}
		if runtimeBackend.harness != fetchHarness {
			t.Fatalf("launched harness = %q, want %q", runtimeBackend.harness, fetchHarness)
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
		fetcher := &modelFetchBackend{harness: fetchHarness, models: []string{"new-model"}}
		taskMgr := newModelRefreshTestManager(t.Context(), runtimeBackend, map[harness.Name]agent.Backend{
			fetchHarness: fetcher,
		})

		refreshHarnessModels(t.Context(), cacheDir, runtimeBackend, taskMgr, nil)

		if runtimeBackend.launches != 0 {
			t.Fatalf("launches = %d, want 0 for fresh cache", runtimeBackend.launches)
		}
		if fetcher.target.SSHHost != "" {
			t.Fatalf("fetch target = %q, want no fetch", fetcher.target.SSHHost)
		}
	})
}

func newModelRefreshTestManager(ctx context.Context, runtimeBackend runtime.Backend, backends map[harness.Name]agent.Backend) *taskmgr.Manager {
	return taskmgr.New(taskmgr.Config{
		ServerCtx: ctx,
		Backend:   runtimeBackend,
		Backends:  backends,
	})
}

type stubBackend struct{}

func (stubBackend) Harness() harness.Name { return "stub" }

func (stubBackend) Start(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("start not implemented")
}

func (stubBackend) AttachRelay(context.Context, *agent.Options) (*agent.Session, error) {
	return nil, errors.New("attach relay not implemented")
}

func (stubBackend) Models() []string { return []string{"m1", "m2"} }

func (stubBackend) SetModels([]string) {}

func (stubBackend) SupportsImages() bool { return false }

func (stubBackend) AgentArgs(agent.HarnessArgs) []string { return nil }

func (stubBackend) NewWire() agent.WireFormat { return claudecode.New() }

func (stubBackend) SupportsCompact() bool { return false }

func (stubBackend) ContextWindowLimit(string) int { return 180_000 }

type modelFetchBackend struct {
	stubBackend

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
	launches int
	connects int
	purges   int
	harness  harness.Name
}

var _ runtime.Backend = (*modelRefreshRuntime)(nil)

func (r *modelRefreshRuntime) Launch(_ context.Context, _ []runtime.Repo, opts *runtime.StartOptions) (runtime.InstanceID, error) {
	r.launches++
	r.harness = opts.Harness
	return runtime.InstanceID("refresh-" + string(opts.Harness)), nil
}

func (r *modelRefreshRuntime) Connect(_ context.Context, id runtime.InstanceID, _ *runtime.StartOptions) (runtime.ConnectionInfo, error) {
	r.connects++
	return runtime.ConnectionInfo{AgentTarget: runtime.ConnectionTarget{SSHHost: string(id)}}, nil
}

func (*modelRefreshRuntime) Diff(_ context.Context, _ runtime.InstanceID, _ int, _ ...string) (string, error) {
	return "", nil
}

func (*modelRefreshRuntime) Fetch(_ context.Context, _ runtime.InstanceID) error { return nil }

func (*modelRefreshRuntime) Stop(_ context.Context, _ runtime.InstanceID) error { return nil }

func (r *modelRefreshRuntime) Purge(_ context.Context, _ runtime.InstanceID) error {
	r.purges++
	return nil
}

func (*modelRefreshRuntime) Revive(_ context.Context, _ runtime.InstanceID) error { return nil }

func (*modelRefreshRuntime) Fork(_ context.Context, _ runtime.InstanceID, _ []runtime.Repo, _ *runtime.ForkOptions) (runtime.InstanceID, runtime.ConnectionInfo, []runtime.Repo, error) {
	return "", runtime.ConnectionInfo{}, nil, errors.New("fork not implemented")
}

func (*modelRefreshRuntime) VNCPort(_ context.Context, _ runtime.InstanceID) int { return 0 }

func (*modelRefreshRuntime) Processes(_ context.Context, _ runtime.InstanceID) ([]runtime.ProcessInfo, error) {
	return nil, nil
}

func (*modelRefreshRuntime) Signal(_ context.Context, _ runtime.InstanceID, _ int, _ string) error {
	return nil
}
