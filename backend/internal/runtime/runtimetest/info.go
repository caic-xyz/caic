// In-memory runtime.Monitor, Inventory, and PrivilegeInfo test double.

package runtimetest

import (
	"context"
	"iter"
	"slices"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// FakeInfo is an in-memory runtime.Monitor, runtime.Inventory, and
// runtime.PrivilegeInfo. Configure returned data through the exported fields;
// the zero value is usable. Fields are read-only after the fake is wired into a
// consumer, so no locking is needed.
type FakeInfo struct {
	// Meta answers Metadata, keyed by string(id)+"\x00"+string(key).
	Meta map[string]string
	// Events is returned by WatchEvents (unless WatchErr is set).
	Events <-chan runtime.Event
	// WatchErr, when set, is returned by WatchEvents.
	WatchErr error
	// Stats is streamed by WatchStats before it blocks on the context.
	Stats []runtime.StatsSample
	// WatchStarted, when set, receives the instance ids passed to WatchStats.
	WatchStarted chan []runtime.InstanceID
	// SudoResult and SudoErr are returned by SudoPassword.
	SudoResult string
	SudoErr    error
}

// Ensure the fake satisfies the interfaces at compile time.
var (
	_ runtime.Monitor       = (*FakeInfo)(nil)
	_ runtime.Inventory     = (*FakeInfo)(nil)
	_ runtime.PrivilegeInfo = (*FakeInfo)(nil)
)

// WatchStats implements runtime.Monitor.
func (f *FakeInfo) WatchStats(ctx context.Context, ids []runtime.InstanceID) (iter.Seq2[runtime.StatsSample, error], error) {
	if f.WatchStarted != nil {
		f.WatchStarted <- slices.Clone(ids)
	}
	return func(yield func(runtime.StatsSample, error) bool) {
		for _, s := range f.Stats {
			if !yield(s, nil) {
				return
			}
		}
		<-ctx.Done()
	}, nil
}

// WatchEvents implements runtime.Monitor.
func (f *FakeInfo) WatchEvents(context.Context, runtime.EventFilter) (<-chan runtime.Event, error) {
	if f.WatchErr != nil {
		return nil, f.WatchErr
	}
	return f.Events, nil
}

// List implements runtime.Inventory.
func (f *FakeInfo) List(context.Context) ([]runtime.Instance, error) { return nil, nil }

// Metadata implements runtime.Inventory.
func (f *FakeInfo) Metadata(_ context.Context, id runtime.InstanceID, key runtime.MetadataKey) (string, error) {
	return f.Meta[string(id)+"\x00"+string(key)], nil
}

// Inspect implements runtime.Inventory.
func (f *FakeInfo) Inspect(context.Context, runtime.InstanceID) (*runtime.InstanceInspect, error) {
	return &runtime.InstanceInspect{}, nil
}

// SudoPassword implements runtime.PrivilegeInfo.
func (f *FakeInfo) SudoPassword(context.Context, runtime.InstanceID) (string, error) {
	return f.SudoResult, f.SudoErr
}
