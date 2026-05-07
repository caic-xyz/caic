// Model list sorting: blacklist filtering, then latest version per family deduplication.

package agent

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// modelBlacklist contains model prefixes that are always excluded from the
// sorted model list (junky or unusable aggregator models).
var modelBlacklist = []string{
	"google/gemini-live",
	"openai/o4",
	"openrouter/ai21/",
	"openrouter/allenai/",
	"openrouter/amazon/",
	"openrouter/arcee-ai/",
	"openrouter/cohere/",
	"openrouter/ibm-granite/",
	"openrouter/inception/",
	"openrouter/kwaipilot/",
	"openrouter/nvidia/",
	"openrouter/poolside/",
	"openrouter/prime-intellect/",
	"openrouter/rekaai/",
	"openrouter/relace/",
	"openrouter/sao10k/",
	"openrouter/stepfun/",
	"openrouter/thedrummer/",
	"openrouter/tngtech/",
	"openrouter/upstage/",
}

var modelVersionRegex = regexp.MustCompile(`(\D+)([0-9.]+)`)

// modelEntry holds the parsed information for a single model during sorting.
type modelEntry struct {
	id      string
	key     string
	version float64
	hasVer  bool
}

// SortModels returns models with version deduplication: only the latest
// version per family key is kept. Models matching a modelBlacklist prefix
// are dropped. Models without parseable versions are preserved as-is.
// Output is sorted alphabetically.
//
// The input slice is copied first so the caller's backing array is not
// modified by slices.DeleteFunc's clear() call.
func SortModels(models []string) []string {
	// Clone to avoid corrupting the caller's slice via DeleteFunc's clear().
	models = slices.Clone(models)

	// Filter out blacklisted models.
	models = slices.DeleteFunc(models, func(id string) bool {
		for _, prefix := range modelBlacklist {
			if strings.HasPrefix(id, prefix) {
				return true
			}
		}
		return false
	})

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
