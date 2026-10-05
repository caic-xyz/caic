// Task metadata conversion helpers.

package task

import (
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func metaCacheMountsFromRuntime(in []runtime.CacheMount) []v3.MetaCacheMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]v3.MetaCacheMount, len(in))
	for i, m := range in {
		out[i] = v3.MetaCacheMount{
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

func metaMountsFromRuntime(in []runtime.Mount) []v3.MetaMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]v3.MetaMount, len(in))
	for i, m := range in {
		out[i] = v3.MetaMount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
		}
	}
	return out
}
