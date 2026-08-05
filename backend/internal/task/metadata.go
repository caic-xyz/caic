// Task metadata conversion helpers.

package task

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

func metaCacheMountsFromRuntime(in []runtime.CacheMount) []agent.MetaCacheMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]agent.MetaCacheMount, len(in))
	for i, m := range in {
		out[i] = agent.MetaCacheMount{
			Name:          m.Name,
			Description:   m.Description,
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
			Shallow:       m.Shallow,
		}
	}
	return out
}

func runtimeCacheMountsFromMeta(in []agent.MetaCacheMount) []runtime.CacheMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.CacheMount, len(in))
	for i, m := range in {
		out[i] = runtime.CacheMount{
			Name:          m.Name,
			Description:   m.Description,
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
			Shallow:       m.Shallow,
		}
	}
	return out
}

func metaMountsFromRuntime(in []runtime.Mount) []agent.MetaMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]agent.MetaMount, len(in))
	for i, m := range in {
		out[i] = agent.MetaMount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
		}
	}
	return out
}

func runtimeMountsFromMeta(in []agent.MetaMount) []runtime.Mount {
	if len(in) == 0 {
		return nil
	}
	out := make([]runtime.Mount, len(in))
	for i, m := range in {
		out[i] = runtime.Mount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
		}
	}
	return out
}
