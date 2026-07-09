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
	WatchStarted chan []runtime.ID
	// SudoResult and SudoErr are returned by SudoPassword.
	SudoResult string
	SudoErr    error
}

// FakeMonitor is an in-memory runtime.Monitor test double.
type FakeMonitor struct {
	// Events is returned by WatchEvents (unless WatchErr is set).
	Events <-chan runtime.Event
	// WatchErr, when set, is returned by WatchEvents.
	WatchErr error
	// Stats is streamed by WatchStats before it blocks on the context.
	Stats []runtime.StatsSample
	// WatchStarted, when set, receives the instance ids passed to WatchStats.
	WatchStarted chan []runtime.ID
}

// FakeInventory is an in-memory runtime.Inventory test double.
type FakeInventory struct {
	// Instances is returned by List.
	Instances []runtime.Instance
	// Meta answers Metadata, keyed by string(id)+"\x00"+string(key).
	Meta map[string]string
}

// FakePrivilegeInfo is an in-memory runtime.PrivilegeInfo test double.
type FakePrivilegeInfo struct {
	// SudoResult and SudoErr are returned by SudoPassword.
	SudoResult string
	SudoErr    error
}

// Ensure the fakes satisfy the interfaces at compile time.
var (
	_ runtime.Monitor       = (*FakeInfo)(nil)
	_ runtime.Inventory     = (*FakeInfo)(nil)
	_ runtime.PrivilegeInfo = (*FakeInfo)(nil)
	_ runtime.Monitor       = (*FakeMonitor)(nil)
	_ runtime.Inventory     = (*FakeInventory)(nil)
	_ runtime.PrivilegeInfo = (*FakePrivilegeInfo)(nil)
)

// WatchStats implements runtime.Monitor.
func (f *FakeInfo) WatchStats(ctx context.Context, ids []runtime.ID) (iter.Seq2[runtime.StatsSample, error], error) {
	return watchStats(ctx, ids, f.WatchStarted, f.Stats)
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
func (f *FakeInfo) Metadata(_ context.Context, id runtime.ID, key runtime.MetadataKey) (string, error) {
	return metadataValue(f.Meta, id, key), nil
}

// Inspect implements runtime.Inventory.
func (f *FakeInfo) Inspect(context.Context, runtime.ID) (*runtime.InstanceInspect, error) {
	return &runtime.InstanceInspect{}, nil
}

// SudoPassword implements runtime.PrivilegeInfo.
func (f *FakeInfo) SudoPassword(context.Context, runtime.ID) (string, error) {
	return f.SudoResult, f.SudoErr
}

// WatchStats implements runtime.Monitor.
func (f *FakeMonitor) WatchStats(ctx context.Context, ids []runtime.ID) (iter.Seq2[runtime.StatsSample, error], error) {
	return watchStats(ctx, ids, f.WatchStarted, f.Stats)
}

// WatchEvents implements runtime.Monitor.
func (f *FakeMonitor) WatchEvents(context.Context, runtime.EventFilter) (<-chan runtime.Event, error) {
	if f.WatchErr != nil {
		return nil, f.WatchErr
	}
	return f.Events, nil
}

// List implements runtime.Inventory.
func (f *FakeInventory) List(context.Context) ([]runtime.Instance, error) {
	return slices.Clone(f.Instances), nil
}

// Metadata implements runtime.Inventory.
func (f *FakeInventory) Metadata(_ context.Context, id runtime.ID, key runtime.MetadataKey) (string, error) {
	return metadataValue(f.Meta, id, key), nil
}

// Inspect implements runtime.Inventory.
func (*FakeInventory) Inspect(context.Context, runtime.ID) (*runtime.InstanceInspect, error) {
	return &runtime.InstanceInspect{}, nil
}

// SudoPassword implements runtime.PrivilegeInfo.
func (f *FakePrivilegeInfo) SudoPassword(context.Context, runtime.ID) (string, error) {
	return f.SudoResult, f.SudoErr
}

func watchStats(ctx context.Context, ids []runtime.ID, started chan []runtime.ID, stats []runtime.StatsSample) (iter.Seq2[runtime.StatsSample, error], error) {
	if started != nil {
		started <- slices.Clone(ids)
	}
	return func(yield func(runtime.StatsSample, error) bool) {
		for i := range stats {
			if !yield(stats[i], nil) {
				return
			}
		}
		<-ctx.Done()
	}, nil
}

func metadataValue(meta map[string]string, id runtime.ID, key runtime.MetadataKey) string {
	if value, ok := meta[string(id)+"\x00"+string(key)]; ok {
		return value
	}
	return meta[string(id.InstanceID())+"\x00"+string(key)]
}
