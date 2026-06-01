// In-package fake MDBackend for Manager tests.

package tasks

import (
	"context"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/md"
)

// fakeMD is an in-package fake MDBackend. The zero value is usable: every
// method returns a benign default. Tests override individual behaviours via the
// func fields and inspect the recorded call counts. Safe for concurrent use.
type fakeMD struct {
	mu sync.Mutex

	statsAllFn func(ctx context.Context, names []string) (map[string]*md.ContainerStats, error)
	sudoFn     func(ctx context.Context, name string) (string, error)
	labels     map[string]string // key: name + "\x00" + label
	events     <-chan container.Event
	watchErr   error

	statsAllCalls int
	sudoCalls     int
}

func (f *fakeMD) StatsAll(ctx context.Context, names []string) (map[string]*md.ContainerStats, error) {
	f.mu.Lock()
	f.statsAllCalls++
	fn := f.statsAllFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, names)
	}
	return map[string]*md.ContainerStats{}, nil
}

func (f *fakeMD) SudoPassword(ctx context.Context, name string) (string, error) {
	f.mu.Lock()
	f.sudoCalls++
	fn := f.sudoFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return "", nil
}

func (f *fakeMD) LabelValue(_ context.Context, name, label string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.labels[name+"\x00"+label], nil
}

func (f *fakeMD) WatchEvents(_ context.Context, _ string) (<-chan container.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	return f.events, nil
}
