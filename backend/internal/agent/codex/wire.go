// JSON-RPC probe types used by ParseMessage and handshake.
package codex

import "encoding/json"

// messageProbe extracts routing fields from a codex app-server line to
// distinguish caic-injected JSON (has "type") from JSON-RPC (has "method"/"id").
type messageProbe struct {
	Type   string           `json:"type,omitempty"`
	Method string           `json:"method,omitempty"`
	ID     *json.RawMessage `json:"id,omitempty"`
}

// methodProbe extracts the method field from a JSON-RPC message.
type methodProbe struct {
	Method string `json:"method,omitempty"`
}

// threadStartResult is the result object from a thread/start JSON-RPC response.
type threadStartResult struct {
	Thread threadStartThread `json:"thread"`
}

// threadStartThread is the thread object inside a threadStartResult.
type threadStartThread struct {
	ID string `json:"id"`
}
