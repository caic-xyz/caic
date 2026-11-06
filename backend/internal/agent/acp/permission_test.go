// Tests correlated ACP permission replies and advertised approval options.

package acp

import (
	"encoding/json"
	"testing"

	"github.com/maruel/genai/acp"
)

func TestPermissionResponse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, options, outcome, optionID string }{
		{"allow_once", `[{"kind":"reject_once","optionId":"no"},{"kind":"allow_once","optionId":"yes"}]`, "selected", "yes"},
		{"allow_always", `[{"kind":"allow_always","optionId":"always"}]`, "selected", "always"},
		{"unavailable", `[{"kind":"reject_once","optionId":"no"}]`, "cancelled", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := permissionResponse([]byte(`{"jsonrpc":"2.0","id":"permission-id","method":"session/request_permission","params":{"sessionId":"s","options":` + tc.options + `}}`))
			if err != nil {
				t.Fatal(err)
			}
			var response acp.JSONRPCResponse
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			var result acp.PermissionResponseResult
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatal(err)
			}
			if string(response.ID) != `"permission-id"` || string(result.Outcome.Outcome) != tc.outcome || result.Outcome.OptionID != tc.optionID {
				t.Fatalf("permission response = %s", data)
			}
		})
	}
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{`{`, `{"method":"session/request_permission"}`, `{"id":null,"method":"session/request_permission"}`, `{"id":1,"method":"session/request_permission","params":{"options":true}}`} {
			if _, err := permissionResponse([]byte(raw)); err == nil {
				t.Fatalf("accepted invalid request %s", raw)
			}
		}
	})
}
