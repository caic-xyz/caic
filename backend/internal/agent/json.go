// Borrows native JSON envelope fields for harness dispatch without copying payloads.

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/buger/jsonparser"
)

// JSONRPCEnvelope contains the fields used to dispatch native JSON-RPC records.
// ID and Params borrow the input buffer: callers must copy anything retained
// beyond the current ParseMessage call. Native decoding validates consumed
// fields; this probe does not run an additional whole-document validation pass.
type JSONRPCEnvelope struct {
	Type   string
	Method string
	ID     json.RawMessage
	Params json.RawMessage
}

// ParseJSONRPCEnvelope scans the outer fields once, borrowing payloads rather
// than decoding them into RawMessage copies. Repeated fields use the last value,
// as encoding/json does; null leaves a previously decoded string unchanged.
func ParseJSONRPCEnvelope(line []byte) (JSONRPCEnvelope, error) {
	var env JSONRPCEnvelope
	err := jsonparser.ObjectEach(line, func(key, value []byte, kind jsonparser.ValueType, end int) error {
		switch string(key) {
		case "type", "method":
			if kind == jsonparser.Null {
				return nil
			}
			text, err := decodeJSONString(value, kind)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			if string(key) == "type" {
				env.Type = text
			} else {
				env.Method = text
			}
		case "id":
			env.ID = rawJSONValue(line, value, kind, end)
		case "params":
			env.Params = rawJSONValue(line, value, kind, end)
		}
		return nil
	})
	return env, err
}

// JSONField borrows an optional native JSON field. A missing field returns nil.
// The result, including string quotes, is valid only while data remains intact.
func JSONField(data []byte, keys ...string) (json.RawMessage, error) {
	value, kind, end, err := jsonparser.Get(data, keys...)
	if errors.Is(err, jsonparser.KeyPathNotFoundError) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rawJSONValue(data, value, kind, end), nil
}

// JSONString reads an optional native string field, decoding escapes and
// replacing invalid UTF-8 exactly as encoding/json does. Missing or null fields
// return an empty string; other value types return an error.
func JSONString(data []byte, keys ...string) (string, error) {
	value, kind, _, err := jsonparser.Get(data, keys...)
	if errors.Is(err, jsonparser.KeyPathNotFoundError) || kind == jsonparser.Null {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return decodeJSONString(value, kind)
}

func decodeJSONString(value []byte, kind jsonparser.ValueType) (string, error) {
	if kind != jsonparser.String {
		return "", fmt.Errorf("JSON field is %v, want string", kind)
	}
	text, err := jsonparser.ParseString(value)
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(text) {
		text = string([]rune(text))
	}
	return text, nil
}

func rawJSONValue(data, value []byte, kind jsonparser.ValueType, end int) json.RawMessage {
	if kind == jsonparser.String {
		return data[end-len(value)-2 : end]
	}
	return value
}
