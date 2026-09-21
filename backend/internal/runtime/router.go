// Runtime router multiplexes task runtime operations across backend instances.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/caic-xyz/caic/metrics"
)

// Operation names recorded for runtime router calls.
const (
	metricContainerLaunch  = "container.launch"
	metricContainerConnect = "container.connect"
	metricContainerStop    = "container.stop"
	metricContainerPurge   = "container.purge"
	metricContainerRevive  = "container.revive"
	metricContainerFork    = "container.fork"
	metricRepoDiff         = "repo.diff"
	metricRepoFileDiff     = "repo.file_diff"
	metricRepoCommitDiff   = "repo.commit_diff"
	metricRepoFetch        = "repo.fetch"
)

// Router dispatches runtime operations to one of several runtime backends.
type Router struct {
	Runtimes []System
	ByName   map[Name]System

	// Immutable.
	log     *slog.Logger
	metrics metrics.Recorder
}

// NewRouter creates a runtime router. A recorder is required; pass
// metrics.Nop{} to discard observations.
func NewRouter(log *slog.Logger, runtimes []System, rec metrics.Recorder) (*Router, error) {
	if log == nil {
		return nil, errors.New("logger is required")
	}
	if rec == nil {
		return nil, errors.New("metrics recorder is required")
	}
	r := &Router{
		Runtimes: slices.Clone(runtimes),
		ByName:   make(map[Name]System, len(runtimes)),
		log:      log.With("cmp", "runtime"),
		metrics:  rec,
	}
	if len(r.Runtimes) == 0 {
		return nil, errors.New("no runtimes configured")
	}
	for _, rt := range r.Runtimes {
		if rt == nil {
			return nil, errors.New("runtime system is nil")
		}
		name := rt.Name()
		if name == "" {
			return nil, errors.New("runtime has empty name")
		}
		if _, ok := r.ByName[name]; ok {
			return nil, fmt.Errorf("duplicate runtime %q", name)
		}
		r.ByName[name] = rt
	}
	return r, nil
}

// Launch starts a runtime instance on the selected backend.
func (r *Router) Launch(ctx context.Context, repos []Repo, opts *StartOptions) (id ID, err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricContainerLaunch, start, err) }()
	rt, err := r.runtimeForStart(opts)
	if err != nil {
		return "", err
	}
	delegateOpts := *opts
	delegateOpts.RuntimeName = rt.Name()
	id, err = rt.Launch(ctx, repos, &delegateOpts)
	if err != nil {
		return "", err
	}
	if err := validateRuntimeID(rt.Name(), id); err != nil {
		return "", err
	}
	return id, nil
}

// Connect waits for transport readiness on the selected backend.
func (r *Router) Connect(ctx context.Context, id ID, opts *StartOptions) (conn ConnectionInfo, err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricContainerConnect, start, err) }()
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return ConnectionInfo{}, err
	}
	delegateOpts := *opts
	delegateOpts.RuntimeName = rt.Name()
	return rt.Connect(ctx, id, &delegateOpts)
}

// Diff returns a diff from the owning backend.
func (r *Router) Diff(ctx context.Context, id ID, repoIdx int, args ...string) (out string, err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricRepoDiff, start, err) }()
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return "", err
	}
	return rt.Diff(ctx, id, repoIdx, args...)
}

// CommitDiffStat returns the net committed diff stat between two repository tips.
func (r *Router) CommitDiffStat(ctx context.Context, id ID, repoIdx int, from, to string) (out string, err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricRepoCommitDiff, start, err) }()
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return "", err
	}
	return rt.CommitDiffStat(ctx, id, repoIdx, from, to)
}

// FileDiff returns one committed or uncommitted file patch from the owning backend.
func (r *Router) FileDiff(ctx context.Context, id ID, repoIdx int, commit, path, originalPath string) (out string, err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricRepoFileDiff, start, err) }()
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return "", err
	}
	return rt.FileDiff(ctx, id, repoIdx, commit, path, originalPath)
}

// RepositoryStatus returns git branch, commit, and working-tree state from the
// owning backend.
func (r *Router) RepositoryStatus(ctx context.Context, id ID, repoIdx int) (RepositoryStatus, error) {
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return RepositoryStatus{}, err
	}
	return rt.RepositoryStatus(ctx, id, repoIdx)
}

// CompactRepositoryStatus returns the log-free repository status from the
// owning backend.
func (r *Router) CompactRepositoryStatus(ctx context.Context, id ID, repoIdx int) (RepositoryStatus, error) {
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return RepositoryStatus{}, err
	}
	return rt.CompactRepositoryStatus(ctx, id, repoIdx)
}

// Fetch fetches task repository changes from the owning backend and returns
// the exact branch tips observed.
func (r *Router) Fetch(ctx context.Context, id ID, opts FetchOpts) (branches []FetchedBranch, err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricRepoFetch, start, err) }()
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return nil, err
	}
	return rt.Fetch(ctx, id, opts)
}

// Stop gracefully stops a runtime instance on its owning backend.
func (r *Router) Stop(ctx context.Context, id ID) (err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricContainerStop, start, err) }()
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return err
	}
	return rt.Stop(ctx, id)
}

// Purge removes a runtime instance from its owning backend.
func (r *Router) Purge(ctx context.Context, id ID) (err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricContainerPurge, start, err) }()
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return err
	}
	return rt.Purge(ctx, id)
}

// Revive restarts a stopped runtime instance on its owning backend.
func (r *Router) Revive(ctx context.Context, id ID) (err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricContainerRevive, start, err) }()
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return err
	}
	return rt.Revive(ctx, id)
}

// Fork snapshots an instance on its owning backend. Cross-runtime forks are rejected.
func (r *Router) Fork(ctx context.Context, id ID, opts *ForkOptions) (forkID ID, conn ConnectionInfo, err error) {
	start := time.Now()
	defer func() { r.observe(ctx, metricContainerFork, start, err) }()
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return "", ConnectionInfo{}, err
	}
	if opts.RuntimeName != "" && opts.RuntimeName != rt.Name() {
		return "", ConnectionInfo{}, fmt.Errorf("fork cannot change runtime from %q to %q", rt.Name(), opts.RuntimeName)
	}
	delegateOpts := *opts
	delegateOpts.RuntimeName = rt.Name()
	forkID, conn, err = rt.Fork(ctx, id, &delegateOpts)
	if err != nil {
		return "", ConnectionInfo{}, err
	}
	if err := validateRuntimeID(rt.Name(), forkID); err != nil {
		return "", ConnectionInfo{}, err
	}
	return forkID, conn, nil
}

// VNCPort returns the VNC port for an instance.
func (r *Router) VNCPort(ctx context.Context, id ID) int {
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return 0
	}
	return rt.VNCPort(ctx, id)
}

// Processes returns the process list for an instance.
func (r *Router) Processes(ctx context.Context, id ID) ([]ProcessInfo, error) {
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return nil, err
	}
	return rt.Processes(ctx, id)
}

// Signal sends a signal to a process in an instance.
func (r *Router) Signal(ctx context.Context, id ID, pid int, sig string) error {
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return err
	}
	return rt.Signal(ctx, id, pid, sig)
}

// DiskUsage returns writable-layer sizes across the requested runtime
// instances. Each owning runtime receives one batched request.
func (r *Router) DiskUsage(ctx context.Context, ids []ID) (map[ID]int64, error) {
	groups := map[Name][]ID{}
	for _, id := range ids {
		rt, err := r.runtimeForInstance(id)
		if err != nil {
			return nil, err
		}
		groups[rt.Name()] = append(groups[rt.Name()], id)
	}
	result := make(map[ID]int64, len(ids))
	for runtimeName, runtimeIDs := range groups {
		usage, err := r.ByName[runtimeName].DiskUsage(ctx, runtimeIDs)
		if err != nil {
			return nil, fmt.Errorf("disk usage %s: %w", runtimeName, err)
		}
		maps.Copy(result, usage)
	}
	return result, nil
}

// WatchStats streams stats across the requested runtime instances.
func (r *Router) WatchStats(ctx context.Context, ids []ID) (iter.Seq2[StatsSample, error], error) {
	groups := map[Name][]ID{}
	for _, id := range ids {
		rt, err := r.runtimeForInstance(id)
		if err != nil {
			return nil, err
		}
		groups[rt.Name()] = append(groups[rt.Name()], id)
	}
	if len(groups) == 0 {
		return func(func(StatsSample, error) bool) {}, nil
	}
	streams := make([]statsStream, 0, len(groups))
	for runtimeName, runtimeIDs := range groups {
		rt := r.ByName[runtimeName]
		seq, err := rt.WatchStats(ctx, runtimeIDs)
		if err != nil {
			return nil, fmt.Errorf("watch stats %s: %w", runtimeName, err)
		}
		streams = append(streams, statsStream{runtimeName: runtimeName, seq: seq})
	}
	return func(yield func(StatsSample, error) bool) {
		out := make(chan statsItem, len(streams))
		var wg sync.WaitGroup
		for _, st := range streams {
			wg.Go(func() {
				for sample, err := range st.seq {
					if err != nil {
						select {
						case out <- statsItem{err: fmt.Errorf("watch stats %s: %w", st.runtimeName, err)}:
						case <-ctx.Done():
						}
						return
					}
					sample.InstanceID = qualifyID(st.runtimeName, sample.InstanceID)
					select {
					case out <- statsItem{sample: sample}:
					case <-ctx.Done():
						return
					}
				}
			})
		}
		go func() {
			wg.Wait()
			close(out)
		}()
		for it := range out {
			if !yield(it.sample, it.err) || it.err != nil {
				return
			}
		}
	}, nil
}

// WatchEvents streams lifecycle events across all runtime backends.
func (r *Router) WatchEvents(ctx context.Context, filter EventFilter) (<-chan Event, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	var watches []eventWatch
	for _, rt := range r.Runtimes {
		ch, err := rt.WatchEvents(watchCtx, filter)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("watch events %s: %w", rt.Name(), err)
		}
		watches = append(watches, eventWatch{runtimeName: rt.Name(), ch: ch})
	}

	out := make(chan Event, 16)
	var wg sync.WaitGroup
	for _, watch := range watches {
		wg.Go(func() {
			for ev := range watch.ch {
				ev.InstanceID = qualifyID(watch.runtimeName, ev.InstanceID)
				select {
				case out <- ev:
				case <-watchCtx.Done():
					return
				}
			}
		})
	}
	go func() {
		wg.Wait()
		close(out)
		cancel()
	}()
	return out, nil
}

// List returns known runtime instances from all inventory backends.
func (r *Router) List(ctx context.Context) ([]Instance, error) {
	var out []Instance
	var errs []error
	successes := 0
	for _, rt := range r.Runtimes {
		instances, err := rt.List(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list %s: %w", rt.Name(), err))
			continue
		}
		for _, instance := range instances {
			instance.ID = qualifyID(rt.Name(), instance.ID)
			if err := validateRuntimeID(rt.Name(), instance.ID); err != nil {
				errs = append(errs, fmt.Errorf("list %s: %w", rt.Name(), err))
				continue
			}
			out = append(out, instance)
		}
		successes++
	}
	if successes == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	for _, err := range errs {
		r.log.WarnContext(ctx, "runtime inventory failed", "err", err)
	}
	return out, nil
}

// Metadata returns runtime metadata for an instance.
func (r *Router) Metadata(ctx context.Context, id ID, key MetadataKey) (string, error) {
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return "", err
	}
	return rt.Metadata(ctx, id, key)
}

// Inspect returns observed runtime configuration for an instance.
func (r *Router) Inspect(ctx context.Context, id ID) (*InstanceInspect, error) {
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return nil, err
	}
	info, err := rt.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("inspect %s returned nil", rt.Name())
	}
	info.ID = qualifyID(rt.Name(), info.ID)
	if err := validateRuntimeID(rt.Name(), info.ID); err != nil {
		return nil, err
	}
	return info, nil
}

// SudoPassword fetches a sudo password from an instance's owning backend.
func (r *Router) SudoPassword(ctx context.Context, id ID) (string, error) {
	rt, err := r.runtimeForInstance(id)
	if err != nil {
		return "", err
	}
	return rt.SudoPassword(ctx, id)
}

// observe records the duration of one router operation.
func (r *Router) observe(ctx context.Context, name string, start time.Time, err error) {
	r.metrics.Record(ctx, name, metrics.OutcomeOf(err), time.Since(start))
}

func (r *Router) runtimeForStart(opts *StartOptions) (System, error) {
	if opts == nil {
		return nil, errors.New("runtime start options are required")
	}
	if opts.RuntimeName == "" {
		return nil, errors.New("runtime name is required")
	}
	rt, ok := r.ByName[opts.RuntimeName]
	if !ok {
		return nil, fmt.Errorf("unknown runtime %q", opts.RuntimeName)
	}
	return rt, nil
}

func (r *Router) runtimeForInstance(id ID) (System, error) {
	runtimeName := id.RuntimeName()
	if runtimeName == "" {
		return nil, errors.New("qualified runtime instance ID is required")
	}
	rt, ok := r.ByName[runtimeName]
	if !ok {
		return nil, fmt.Errorf("unknown runtime %q", runtimeName)
	}
	return rt, nil
}

type statsStream struct {
	runtimeName Name
	seq         iter.Seq2[StatsSample, error]
}

type statsItem struct {
	sample StatsSample
	err    error
}

type eventWatch struct {
	runtimeName Name
	ch          <-chan Event
}

func qualifyID(name Name, id ID) ID {
	if id.RuntimeName() == "" {
		return NewID(name, id.InstanceID())
	}
	return id
}

func validateRuntimeID(name Name, id ID) error {
	if id.RuntimeName() != name {
		return fmt.Errorf("runtime %q returned instance ID %q", name, id)
	}
	return nil
}
