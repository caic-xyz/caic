// RuntimeInfoBackend adapts md inventory, monitoring, and metadata operations.

package mdruntime

import (
	"context"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// RuntimeInfoBackend wraps an md client for runtime-neutral inventory, monitoring, and metadata.
type RuntimeInfoBackend struct {
	c *md.Client
}

var (
	_ runtime.Inventory     = RuntimeInfoBackend{}
	_ runtime.Monitor       = RuntimeInfoBackend{}
	_ runtime.PrivilegeInfo = RuntimeInfoBackend{}
)

// NewRuntimeInfoBackend wires an md client into runtime-neutral monitoring operations.
func NewRuntimeInfoBackend(c *md.Client) RuntimeInfoBackend {
	return RuntimeInfoBackend{c: c}
}

// List returns known runtime instances.
func (b RuntimeInfoBackend) List(ctx context.Context) ([]runtime.Instance, error) {
	containers, err := b.c.List(ctx)
	if err != nil {
		return nil, err
	}
	return InstancesFromMD(ctx, containers), nil
}

// StatsAll returns resource stats for the named runtime instances.
func (b RuntimeInfoBackend) StatsAll(ctx context.Context, ids []runtime.InstanceID) (map[runtime.InstanceID]*runtime.Stats, error) {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = string(id)
	}
	stats, err := b.c.StatsAll(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[runtime.InstanceID]*runtime.Stats, len(stats))
	for name, s := range stats {
		out[runtime.InstanceID(name)] = &runtime.Stats{
			CPUPerc:    s.CPUPerc,
			MemUsed:    s.MemUsed,
			MemLimit:   s.MemLimit,
			MemPerc:    s.MemPerc,
			NetRx:      s.NetRx,
			NetTx:      s.NetTx,
			BlockRead:  s.BlockRead,
			BlockWrite: s.BlockWrite,
			DiskUsed:   s.DiskUsed,
		}
	}
	return out, nil
}

// SudoPassword fetches the sudo password for a runtime instance over SSH.
func (b RuntimeInfoBackend) SudoPassword(ctx context.Context, id runtime.InstanceID) (string, error) {
	return (&md.Container{Client: b.c, Name: string(id)}).SudoPassword(ctx)
}

// Metadata reads a single runtime instance metadata value, returning "" when unset.
func (b RuntimeInfoBackend) Metadata(ctx context.Context, id runtime.InstanceID, key runtime.MetadataKey) (string, error) {
	return labelValue(ctx, b.c.Runtime, string(id), string(key))
}

// WatchEvents streams lifecycle events for instances matching filter.
func (b RuntimeInfoBackend) WatchEvents(ctx context.Context, filter runtime.EventFilter) (<-chan runtime.Event, error) {
	return WatchEvents(ctx, b.c.Runtime, filter)
}
