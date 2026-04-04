// Package jsonutil provides forward-compatible JSON unmarshaling with overflow field tracking.

package jsonutil

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// Overflow holds JSON fields that were not mapped to a struct field.
// It is embedded in every record type to ensure forward compatibility.
type Overflow struct {
	// Extra contains any JSON fields not recognized by the current struct definition.
	// These are preserved during unmarshaling so no data is lost.
	Extra map[string]json.RawMessage `json:"-"`
}

// FieldWarner deduplicates unknown-field warnings so each (context, field)
// pair is logged at most once. Create one per session or parse scope.
type FieldWarner struct {
	seen sync.Map // key: "context\x00field"
}

// Warn logs a warning for each previously-unseen (context, field) pair
// in extra. The field value is included (truncated to 128 bytes).
func (fw *FieldWarner) Warn(context string, extra map[string]json.RawMessage) {
	if len(extra) == 0 {
		return
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		dedup := context + "\x00" + k
		if _, loaded := fw.seen.LoadOrStore(dedup, struct{}{}); loaded {
			continue
		}
		val := string(extra[k])
		if len(val) > 128 {
			val = val[:128] + "…"
		}
		slog.Warn("unknown field in record", "context", context, "field", k, "value", val)
	}
}

// overflowType is cached to avoid repeated reflect lookups.
var overflowType = reflect.TypeOf(Overflow{})

// WarnOverflows walks v (a struct or pointer to struct) and calls Warn for
// every embedded Overflow.Extra found at any nesting depth — including inside
// slices and pointer fields. This lets callers do a single post-unmarshal call
// instead of threading the warner into every UnmarshalJSON method.
func (fw *FieldWarner) WarnOverflows(context string, v any) {
	fw.warnValue(context, reflect.ValueOf(v))
}

func (fw *FieldWarner) warnValue(ctx string, v reflect.Value) {
	switch v.Kind() { //nolint:exhaustive // only Ptr and Struct are relevant
	case reflect.Ptr:
		if !v.IsNil() {
			fw.warnValue(ctx, v.Elem())
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			fv := v.Field(i)
			if f.Type == overflowType && f.Anonymous {
				// Found an embedded Overflow — warn its Extra.
				extra := fv.FieldByName("Extra")
				if m, ok := extra.Interface().(map[string]json.RawMessage); ok {
					fw.Warn(ctx, m)
				}
				continue
			}
			// Descend into nested structs, pointers, and slices.
			switch fv.Kind() { //nolint:exhaustive // only Struct, Ptr, and Slice need traversal
			case reflect.Struct:
				// Build a sub-context from the json tag if available.
				fw.warnValue(ctx+"."+jsonFieldName(&f), fv)
			case reflect.Ptr:
				if !fv.IsNil() && fv.Elem().Kind() == reflect.Struct {
					fw.warnValue(ctx+"."+jsonFieldName(&f), fv.Elem())
				}
			case reflect.Slice:
				if fv.Len() > 0 {
					elem := fv.Type().Elem()
					if elem.Kind() == reflect.Struct || (elem.Kind() == reflect.Ptr && elem.Elem().Kind() == reflect.Struct) {
						for j := range fv.Len() {
							fw.warnValue(ctx+"."+jsonFieldName(&f), fv.Index(j))
						}
					}
				}
			}
		}
	}
}

// jsonFieldName returns the JSON tag name for a struct field, or the Go name
// as fallback.
func jsonFieldName(f *reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return f.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name != "" && name != "-" {
		return name
	}
	return f.Name
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
// and warns via fw for each unknown key.
func UnmarshalRecord(data []byte, dest any, overflow *Overflow, known map[string]struct{}, name string, fw *FieldWarner) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	overflow.Extra = CollectUnknown(raw, known)
	fw.Warn(name, overflow.Extra)
	return nil
}
