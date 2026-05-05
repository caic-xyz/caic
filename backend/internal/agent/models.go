// Model list sorting: latest version per family, superseded versions dropped.
package agent

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var modelVersionRegex = regexp.MustCompile(`(\D+)([0-9.]+)`)

// modelEntry holds the parsed information for a single model during sorting.
type modelEntry struct {
	id      string
	key     string
	version float64
	hasVer  bool
}

// SortModels returns models with version deduplication: only the latest
// version per family key is kept. Models without parseable versions are
// preserved as-is. Output is sorted alphabetically.
func SortModels(models []string) []string {
	entries := make([]modelEntry, len(models))
	for i, id := range models {
		_, key, ver, hasVer := parseModelVersion(id)
		entries[i] = modelEntry{id: id, key: key, version: ver, hasVer: hasVer}
	}

	// Find max version per family key.
	maxVer := make(map[string]float64)
	for _, e := range entries {
		if e.hasVer {
			if e.version > maxVer[e.key] {
				maxVer[e.key] = e.version
			}
		}
	}

	// Keep: latest per family, plus everything versionless. Drop superseded.
	var out []string
	for _, e := range entries {
		if !e.hasVer || e.version == maxVer[e.key] {
			out = append(out, e.id)
		}
	}

	slices.Sort(out)
	return out
}

// parseModelVersion extracts the effective provider, family key, and version
// number from a model ID. For aggregator paths like "openrouter/x-ai/grok-4",
// the second segment is treated as the provider.
func parseModelVersion(id string) (provider, key string, version float64, ok bool) {
	provider, name, ok0 := strings.Cut(id, "/")
	if !ok0 {
		return "", id, 0, false
	}
	// Aggregator providers: look through to the real provider.
	if provider == "openrouter" {
		if p, n, ok1 := strings.Cut(name, "/"); ok1 {
			provider, name = p, n
		}
	}

	matches := modelVersionRegex.FindStringSubmatch(name)
	if len(matches) == 3 {
		modelName := matches[1]
		versionStr := matches[2]

		ver, err := strconv.ParseFloat(versionStr, 64)
		if err == nil && ver > 0 {
			return provider, provider + "/" + modelName + "*", ver, true
		}
	}
	return provider, id, 0, false
}
