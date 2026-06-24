// RuntimeInfoBackend adapts md inventory, monitoring, and metadata operations.

package mdruntime

import (
	"context"
	"os/exec"
	"strings"

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

// Inspect returns observed runtime configuration for a runtime instance.
func (b RuntimeInfoBackend) Inspect(ctx context.Context, id runtime.InstanceID) (*runtime.InstanceInspect, error) {
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
	osName, cpuArchitecture := inspectOSArch(ctx, b.c.Runtime, string(id), info.ImageID, info.ImageRef, info.Platform)
	return &runtime.InstanceInspect{
		Runtime:         info.Runtime,
		ID:              inspectID,
		State:           info.State,
		ImageRef:        info.ImageRef,
		ImageID:         info.ImageID,
		OS:              osName,
		CPUArchitecture: cpuArchitecture,
		CPULimit:        info.CPULimit,
		Mounts:          mounts,
		Caches:          caches,
	}, nil
}

func inspectOSArch(ctx context.Context, runtimeName, containerName, imageID, imageRef, platform string) (osName, cpuArchitecture string) {
	if osName, cpuArchitecture, ok := splitOSArch(platform, ""); ok {
		return osName, cpuArchitecture
	}
	osName = cleanInspectValue(platform)
	if runtimeName == "" || containerName == "" {
		return osName, ""
	}
	if observedOS, observedCPUArchitecture, ok := inspectTargetOSArch(ctx, runtimeName, []string{"inspect", containerName, "--format", "{{.Os}}/{{.Architecture}}"}, osName); ok {
		return observedOS, observedCPUArchitecture
	}
	for _, image := range []string{imageID, imageRef} {
		if image == "" {
			continue
		}
		observedOS, observedCPUArchitecture, ok := inspectTargetOSArch(ctx, runtimeName, []string{"image", "inspect", image, "--format", "{{.Os}}/{{.Architecture}}"}, osName)
		if ok {
			return observedOS, observedCPUArchitecture
		}
	}
	return osName, ""
}

func inspectTargetOSArch(ctx context.Context, runtimeName string, args []string, fallbackOS string) (osName, cpuArchitecture string, ok bool) {
	out, err := exec.CommandContext(ctx, runtimeName, args...).Output() //nolint:gosec // runtime command and inspect targets are internal adapter values.
	if err != nil {
		return "", "", false
	}
	osName, cpuArchitecture, ok = splitOSArch(strings.TrimSpace(string(out)), fallbackOS)
	if !ok || fallbackOS != "" && osName != fallbackOS {
		return "", "", false
	}
	return osName, cpuArchitecture, true
}

func splitOSArch(platform, fallbackOS string) (osName, cpuArchitecture string, ok bool) {
	osName, cpuArchitecture, ok = strings.Cut(platform, "/")
	if !ok {
		return "", "", false
	}
	osName = cleanInspectValue(osName)
	if osName == "" {
		osName = fallbackOS
	}
	cpuArchitecture = cleanInspectValue(cpuArchitecture)
	if osName == "" || cpuArchitecture == "" {
		return "", "", false
	}
	return osName, cpuArchitecture, true
}

func cleanInspectValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "<no value>" {
		return ""
	}
	return v
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
