// Model list sorting: recent versions first, superseded versions last.
package kilo

import (
	"sort"
	"strconv"
	"strings"
)

// topProviders lists provider prefixes (before the "/") considered high quality.
// Models from other providers are pushed to the end of the list.
var topProviders = map[string]bool{
	"anthropic": true,
	"deepseek":  true,
	"google":    true,
	"minimax":   true,
	"openai":    true,
	"qwen":      true,
	"x-ai":      true,
	"z-ai":      true,
}

// SortModels returns models in three tiers, each sorted alphabetically:
//  1. Recent — latest version per family from top providers
//  2. Old — superseded versions and provider-stale models from top providers
//  3. Other — all models from non-top providers
//
// Provider-specific rules:
//   - Anthropic: claude-<digit>* (old naming) is stale
//   - DeepSeek: only bare deepseek-v<N>; latest version only
//   - Google: only gemini-<digit>*; no customtools; grouped by pro/flash/flash-lite
//   - Minimax: only bare minimax-m<N>; latest version only
//   - OpenAI: only "codex" models; latest version only
//   - Qwen: only versioned models (qwen<N>* or qwen-<N>*); latest version only
//   - x-ai: only models containing "code"
//   - z-ai: latest version only across all variants
func SortModels(models []string) []string {
	type entry struct {
		id      string
		key     string
		version float64
		hasVer  bool
		top     bool
		stale   bool
	}

	entries := make([]entry, len(models))
	for i, id := range models {
		key, ver, hasVer := parseModelVersion(id)
		provider, name := splitProvider(id)
		top := topProviders[provider]
		stale := false
		if top {
			stale = isProviderStale(provider, name)
			if !stale {
				key, ver, hasVer = normalizeEntry(provider, name, key, ver, hasVer)
			}
		}
		entries[i] = entry{id: id, key: key, version: ver, hasVer: hasVer, top: top, stale: stale}
	}

	// Find max version per family key (top, non-stale only).
	maxVer := make(map[string]float64)
	for _, e := range entries {
		if e.hasVer && e.top && !e.stale {
			if e.version > maxVer[e.key] {
				maxVer[e.key] = e.version
			}
		}
	}

	// Partition into three tiers.
	var recent, old, other []string
	for _, e := range entries {
		switch {
		case !e.top:
			other = append(other, e.id)
		case e.stale:
			old = append(old, e.id)
		case !e.hasVer || e.version == maxVer[e.key]:
			recent = append(recent, e.id)
		default:
			old = append(old, e.id)
		}
	}

	sort.Strings(recent)
	sort.Strings(old)
	sort.Strings(other)

	out := make([]string, 0, len(recent)+len(old)+len(other))
	out = append(out, recent...)
	out = append(out, old...)
	out = append(out, other...)
	return out
}

// splitProvider splits "provider/name" into its two parts.
func splitProvider(id string) (provider, name string) {
	if before, after, ok := strings.Cut(id, "/"); ok {
		return before, after
	}
	return "", id
}

// isProviderStale applies provider-specific rules to detect unconditionally
// superseded models.
func isProviderStale(provider, name string) bool {
	switch provider {
	case "anthropic":
		// Old naming: claude-3-haiku, claude-3.5-sonnet.
		// New naming: claude-opus-4.6, claude-sonnet-4.6.
		after, ok := strings.CutPrefix(name, "claude-")
		return ok && after != "" && after[0] >= '0' && after[0] <= '9'
	case "deepseek":
		// Only bare deepseek-v<N> (no suffix like -terminus, -exp).
		after, ok := strings.CutPrefix(name, "deepseek-v")
		if !ok || after == "" || after[0] < '0' || after[0] > '9' {
			return true
		}
		return strings.ContainsRune(after, '-')
	case "google":
		// Only gemini-<digit> models; exclude customtools variants.
		after, ok := strings.CutPrefix(name, "gemini-")
		if !ok || after == "" || after[0] < '0' || after[0] > '9' {
			return true
		}
		return strings.Contains(name, "customtools") || strings.Contains(name, "image")
	case "minimax":
		// Only bare minimax-m<N> (no suffix like -her).
		after, ok := strings.CutPrefix(name, "minimax-m")
		if !ok || after == "" || after[0] < '0' || after[0] > '9' {
			return true
		}
		return strings.ContainsRune(after, '-')
	case "openai":
		return !strings.Contains(name, "codex")
	case "qwen":
		_, ok := qwenVersion(name)
		return !ok
	case "x-ai":
		return !strings.Contains(name, "code")
	}
	return false
}

// normalizeEntry adjusts the family key and/or version for provider-specific
// grouping. Returns the (possibly updated) key, version, and hasVer.
func normalizeEntry(provider, name, defaultKey string, defaultVer float64, defaultHasVer bool) (key string, ver float64, hasVer bool) {
	switch provider {
	case "deepseek":
		// Extract version from deepseek-v<N> (parseModelVersion can't parse "v3").
		after, _ := strings.CutPrefix(name, "deepseek-v")
		if v, err := strconv.ParseFloat(after, 64); err == nil {
			return "deepseek/deepseek", v, true
		}
	case "google":
		// Group gemini models by variant (pro/flash/flash-lite).
		// Check flash-lite before flash since it contains "flash".
		switch {
		case strings.Contains(name, "flash-lite"):
			return "google/gemini-flash-lite", defaultVer, defaultHasVer
		case strings.Contains(name, "flash"):
			return "google/gemini-flash", defaultVer, defaultHasVer
		case strings.Contains(name, "pro"):
			return "google/gemini-pro", defaultVer, defaultHasVer
		}
	case "minimax":
		// Extract version from minimax-m<N> (parseModelVersion can't parse "m2.5").
		after, _ := strings.CutPrefix(name, "minimax-m")
		if v, err := strconv.ParseFloat(after, 64); err == nil {
			return "minimax/minimax", v, true
		}
	case "openai":
		// Group all codex models together; keep latest version only.
		return "openai/codex", defaultVer, defaultHasVer
	case "qwen":
		// Extract version from qwen<N>* or qwen-<N>* naming.
		if v, ok := qwenVersion(name); ok {
			return "qwen/qwen", v, true
		}
	case "z-ai":
		// Group all z-ai models together; keep latest version only.
		// Manually extract version for names like "glm-4.5v" where trailing
		// "v" blocks parseModelVersion.
		if !defaultHasVer {
			after, _ := strings.CutPrefix(name, "glm-")
			if i := strings.IndexByte(after, '-'); i >= 0 {
				after = after[:i]
			}
			for after != "" && (after[len(after)-1] < '0' || after[len(after)-1] > '9') {
				after = after[:len(after)-1]
			}
			if v, err := strconv.ParseFloat(after, 64); err == nil && v > 0 {
				return "z-ai/glm", v, true
			}
		}
		return "z-ai/glm", defaultVer, defaultHasVer
	}
	return defaultKey, defaultVer, defaultHasVer
}

// qwenVersion extracts the version number from a Qwen model name.
// Handles both "qwen-2.5-*" (dash before version) and "qwen3.5-*" (no dash).
func qwenVersion(name string) (float64, bool) {
	var after string
	switch {
	case strings.HasPrefix(name, "qwen-"):
		after = name[len("qwen-"):]
	case strings.HasPrefix(name, "qwen"):
		after = name[len("qwen"):]
	default:
		return 0, false
	}
	if after == "" || after[0] < '0' || after[0] > '9' {
		return 0, false
	}
	// Take up to the first dash as the version string.
	verStr := after
	if before, _, ok := strings.Cut(after, "-"); ok {
		verStr = before
	}
	v, err := strconv.ParseFloat(verStr, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseModelVersion extracts a family key and version number from a model ID.
// It finds the first dash-separated segment that parses as a positive number
// and replaces it with "*" to form the family key.
//
// Examples:
//
//	"openai/gpt-5.3-codex"       → ("openai/gpt-*-codex", 5.3, true)
//	"anthropic/claude-opus-4.6"  → ("anthropic/claude-opus-*", 4.6, true)
//	"mistralai/devstral-medium"  → ("mistralai/devstral-medium", 0, false)
func parseModelVersion(id string) (key string, version float64, ok bool) {
	before, after, ok0 := strings.Cut(id, "/")
	if !ok0 {
		return id, 0, false
	}
	provider := before
	name := after

	parts := strings.Split(name, "-")
	for i, part := range parts {
		// Strip colon suffix (e.g. ":free", ":thinking") before parsing.
		clean := part
		if before, _, ok0 := strings.Cut(part, ":"); ok0 {
			clean = before
		}
		ver, err := strconv.ParseFloat(clean, 64)
		if err == nil && ver > 0 {
			keyParts := make([]string, len(parts))
			copy(keyParts, parts)
			keyParts[i] = "*"
			return provider + "/" + strings.Join(keyParts, "-"), ver, true
		}
	}
	return id, 0, false
}
