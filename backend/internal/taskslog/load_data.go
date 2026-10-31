// Converts persisted mount configuration into runtime values.

package taskslog

import (
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func runtimeCacheMountsFromMeta(in []v3.MetaCacheMount) []runtime.CacheMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.CacheMount, len(in))
	for i, m := range in {
		out[i] = runtime.CacheMount{Name: m.Name, Description: m.Description, HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly, Shallow: m.Shallow}
	}
	return out
}

func runtimeMountsFromMeta(in []v3.MetaMount) []runtime.Mount {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.Mount, len(in))
	for i, m := range in {
		out[i] = runtime.Mount{HostPath: m.HostPath, ContainerPath: m.ContainerPath, ReadOnly: m.ReadOnly}
	}
	return out
}
