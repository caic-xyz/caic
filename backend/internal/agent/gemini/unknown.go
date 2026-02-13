// Package gemini provides Go types for Gemini CLI stream-json session logs.
//
// Gemini CLI writes NDJSON when invoked with --output-format stream-json.
// New fields may appear at any version, so all types preserve unknown fields
// in an Overflow map and log a warning when they are encountered.
package gemini

import (
	"encoding/json"
	"log/slog"
	"sort"
)

// Overflow holds JSON fields that were not mapped to a struct field.
// It is embedded in every record type to ensure forward compatibility.
type Overflow struct {
	// Extra contains any JSON fields not recognized by the current struct definition.
	// These are preserved during unmarshaling so no data is lost.
	Extra map[string]json.RawMessage `json:"-"`
}

// warnUnknown logs a warning for each key in extra, identified by context.
func warnUnknown(context string, extra map[string]json.RawMessage) {
	if len(extra) == 0 {
		return
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	slog.Warn("unknown fields in Gemini CLI record", "context", context, "fields", keys)
}

// makeSet builds a map[string]struct{} from keys for O(1) lookup.
func makeSet(keys ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		s[k] = struct{}{}
	}
	return s
}

// collectUnknown returns entries from raw whose keys are not in known.
func collectUnknown(raw map[string]json.RawMessage, known map[string]struct{}) map[string]json.RawMessage {
	var extra map[string]json.RawMessage
	for k, v := range raw {
		if _, ok := known[k]; !ok {
			if extra == nil {
				extra = make(map[string]json.RawMessage)
			}
			extra[k] = v
		}
	}
	return extra
}
