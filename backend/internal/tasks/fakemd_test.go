// In-package fake runtime inventory, monitor, and privilege info for Manager tests.

package tasks

import (
	"context"
	"iter"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// fakeMD is an in-package fake runtime info backend. The zero value is usable: every
// method returns a benign default. Tests override individual behaviours via the
// func fields and inspect the recorded call counts. Safe for concurrent use.
type fakeMD struct {
	mu sync.Mutex

	watchStatsFn func(ctx context.Context, ids []runtime.InstanceID) (iter.Seq2[runtime.StatsSample, error], error)
	sudoFn       func(ctx context.Context, id runtime.InstanceID) (string, error)
	metadata     map[string]string // key: name + "\x00" + metadata key
	events       <-chan runtime.Event
	watchErr     error

	watchStatsCalls int
	sudoCalls       int
}

func (f *fakeMD) WatchStats(ctx context.Context, ids []runtime.InstanceID) (iter.Seq2[runtime.StatsSample, error], error) {
	f.mu.Lock()
	f.watchStatsCalls++
	fn := f.watchStatsFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, ids)
	}
	return func(func(runtime.StatsSample, error) bool) {
		<-ctx.Done()
	}, nil
}

func (f *fakeMD) SudoPassword(ctx context.Context, id runtime.InstanceID) (string, error) {
	f.mu.Lock()
	f.sudoCalls++
	fn := f.sudoFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, id)
	}
	return "", nil
}

func (f *fakeMD) List(context.Context) ([]runtime.Instance, error) {
	return nil, nil
}

func (f *fakeMD) Metadata(_ context.Context, id runtime.InstanceID, key runtime.MetadataKey) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metadata[string(id)+"\x00"+string(key)], nil
}

func (f *fakeMD) Inspect(context.Context, runtime.InstanceID) (*runtime.InstanceInspect, error) {
	return &runtime.InstanceInspect{}, nil
}

func (f *fakeMD) WatchEvents(_ context.Context, _ runtime.EventFilter) (<-chan runtime.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	return f.events, nil
}
