// Metadata helper functions for mdruntime backends.

package mdruntime

import "maps"

func cloneLabelMap(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	maps.Copy(out, labels)
	return out
}
