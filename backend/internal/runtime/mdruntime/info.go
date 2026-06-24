// RuntimeInfoBackend adapts md inventory, monitoring, and metadata operations.

package mdruntime

import (
	"context"
	"log/slog"
	"maps"
	"sync"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// RuntimeInfoBackend wraps an md client for runtime-neutral inventory, monitoring, and metadata.
type RuntimeInfoBackend struct {
	c       *md.Client
	backend *Backend

	mu     sync.Mutex
	labels map[runtime.InstanceID]map[string]string
}

var (
	_ runtime.Inventory     = (*RuntimeInfoBackend)(nil)
	_ runtime.Monitor       = (*RuntimeInfoBackend)(nil)
	_ runtime.PrivilegeInfo = (*RuntimeInfoBackend)(nil)
)

// NewRuntimeInfoBackend wires an md client into runtime-neutral monitoring operations.
func NewRuntimeInfoBackend(c *md.Client, backend *Backend) *RuntimeInfoBackend {
	return &RuntimeInfoBackend{c: c, backend: backend}
}

// List returns known runtime instances.
func (b *RuntimeInfoBackend) List(ctx context.Context) ([]runtime.Instance, error) {
	containers, err := b.c.List(ctx)
	if err != nil {
		return nil, err
	}
	b.rememberLabels(containers)
	if b.backend != nil {
		b.backend.rememberMDContainers(containers)
	}
	return InstancesFromMD(ctx, containers), nil
}

// Inspect returns observed runtime configuration for a runtime instance.
func (b *RuntimeInfoBackend) Inspect(ctx context.Context, id runtime.InstanceID) (*runtime.InstanceInspect, error) {
	info, err := (&md.Container{Client: b.c, Name: string(id)}).Inspect(ctx)
	if err != nil {
		return nil, err
	}
	mounts := make([]runtime.Mount, len(info.Mounts))
	for i, m := range info.Mounts {
		mounts[i] = runtime.Mount{HostPath: m.HostPath, MountPath: m.ContainerPath, ReadOnly: m.ReadOnly}
	}
	caches := make([]runtime.CacheMount, len(info.Caches))
	for i, c := range info.Caches {
		caches[i] = runtime.CacheMount{
			Name:        c.Name,
			Description: c.Description,
			HostPath:    c.HostPath,
			MountPath:   c.ContainerPath,
			ReadOnly:    c.ReadOnly,
			Shallow:     c.Shallow,
		}
	}
	inspectID := runtime.InstanceID(info.ID)
	if inspectID == "" {
		inspectID = id
	}
	return &runtime.InstanceInspect{
		Runtime:         info.Runtime,
		ID:              inspectID,
		State:           info.State,
		ImageRef:        info.ImageRef,
		ImageID:         info.ImageID,
		OS:              info.OS,
		CPUArchitecture: info.Architecture,
		CPULimit:        info.CPULimit,
		Mounts:          mounts,
		Caches:          caches,
	}, nil
}

// StatsAll returns resource stats for the named runtime instances.
func (b *RuntimeInfoBackend) StatsAll(ctx context.Context, ids []runtime.InstanceID) (map[runtime.InstanceID]*runtime.Stats, error) {
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
func (b *RuntimeInfoBackend) SudoPassword(ctx context.Context, id runtime.InstanceID) (string, error) {
	return (&md.Container{Client: b.c, Name: string(id)}).SudoPassword(ctx)
}

// Metadata reads a single runtime instance metadata value, returning "" when unset.
func (b *RuntimeInfoBackend) Metadata(ctx context.Context, id runtime.InstanceID, key runtime.MetadataKey) (string, error) {
	if value, ok := b.cachedLabel(id, string(key)); ok {
		return value, nil
	}
	ct, err := b.c.Get(ctx, string(id))
	if err != nil {
		return "", err
	}
	b.rememberLabelMap(id, ct.Labels)
	return ct.Labels[string(key)], nil
}

// WatchEvents streams lifecycle events for instances matching filter.
func (b *RuntimeInfoBackend) WatchEvents(ctx context.Context, filter runtime.EventFilter) (<-chan runtime.Event, error) {
	events, err := b.c.WatchDieEvents(ctx, string(filter.MetadataKey))
	if err != nil {
		return nil, err
	}
	out := make(chan runtime.Event, 16)
	go func() {
		defer close(out)
		for ev, err := range events {
			if err != nil {
				if ctx.Err() == nil {
					slog.WarnContext(ctx, "runtime events stream failed", "err", err)
				}
				return
			}
			select {
			case out <- runtime.Event{InstanceID: runtime.InstanceID(ev.Name)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (b *RuntimeInfoBackend) rememberLabels(containers []*md.Container) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.labels == nil {
		b.labels = make(map[runtime.InstanceID]map[string]string, len(containers))
	}
	for _, c := range containers {
		b.labels[runtime.InstanceID(c.Name)] = cloneLabelMap(c.Labels)
	}
}

func (b *RuntimeInfoBackend) rememberLabelMap(id runtime.InstanceID, labels map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.labels == nil {
		b.labels = make(map[runtime.InstanceID]map[string]string)
	}
	b.labels[id] = cloneLabelMap(labels)
}

func (b *RuntimeInfoBackend) cachedLabel(id runtime.InstanceID, key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	labels, ok := b.labels[id]
	if !ok {
		return "", false
	}
	return labels[key], true
}

func cloneLabelMap(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	maps.Copy(out, labels)
	return out
}
