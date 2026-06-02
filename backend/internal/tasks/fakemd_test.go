// In-package fake RuntimeInfoBackend for Manager tests.

package tasks

import (
	"context"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// fakeMD is an in-package fake RuntimeInfoBackend. The zero value is usable: every
// method returns a benign default. Tests override individual behaviours via the
// func fields and inspect the recorded call counts. Safe for concurrent use.
type fakeMD struct {
	mu sync.Mutex

	statsAllFn func(ctx context.Context, ids []runtime.InstanceID) (map[runtime.InstanceID]*runtime.Stats, error)
	sudoFn     func(ctx context.Context, id runtime.InstanceID) (string, error)
	labels     map[string]string // key: name + "\x00" + label
	events     <-chan runtime.Event
	watchErr   error

	statsAllCalls int
	sudoCalls     int
}

func (f *fakeMD) StatsAll(ctx context.Context, ids []runtime.InstanceID) (map[runtime.InstanceID]*runtime.Stats, error) {
	f.mu.Lock()
	f.statsAllCalls++
	fn := f.statsAllFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, ids)
	}
	return map[runtime.InstanceID]*runtime.Stats{}, nil
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

func (f *fakeMD) LabelValue(_ context.Context, id runtime.InstanceID, label string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.labels[string(id)+"\x00"+label], nil
}

func (f *fakeMD) WatchEvents(_ context.Context, _ string) (<-chan runtime.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	return f.events, nil
}
