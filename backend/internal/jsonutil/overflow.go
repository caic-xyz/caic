// Package jsonutil provides forward-compatible JSON unmarshaling with overflow field tracking.

package jsonutil

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
)

// Overflow holds JSON fields that were not mapped to a struct field.
// It is embedded in every record type to ensure forward compatibility.
type Overflow struct {
	// Extra contains any JSON fields not recognized by the current struct definition.
	// These are preserved during unmarshaling so no data is lost.
	Extra map[string]json.RawMessage `json:"-"`
}

// WarnUnknown logs a warning for each key in extra, identified by context.
func WarnUnknown(context string, extra map[string]json.RawMessage) {
	if len(extra) == 0 {
		return
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	slog.Warn("unknown fields in record", "context", context, "fields", keys)
}

// KnownFields builds a set of JSON field names by reflecting on v's struct
// tags (including embedded structs).
func KnownFields(v any) map[string]struct{} {
	t := reflect.TypeOf(v)
	s := make(map[string]struct{})
	addJSONFields(t, s)
	return s
}

// addJSONFields recursively collects JSON tag names from t's fields into s.
func addJSONFields(t reflect.Type, s map[string]struct{}) {
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			addJSONFields(f.Type, s)
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			s[name] = struct{}{}
		}
	}
}

// CollectUnknown returns entries from raw whose keys are not in known.
func CollectUnknown(raw map[string]json.RawMessage, known map[string]struct{}) map[string]json.RawMessage {
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

// UnmarshalRecord decodes data into dest (which must be a type-alias pointer
// to break recursive UnmarshalJSON), collects unknown fields into overflow,
// and logs a warning for each unknown key.
func UnmarshalRecord(data []byte, dest any, overflow *Overflow, known map[string]struct{}, name string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	overflow.Extra = CollectUnknown(raw, known)
	WarnUnknown(name, overflow.Extra)
	return nil
}
