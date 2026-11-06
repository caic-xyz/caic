// ACP permission approval preserves native request IDs and selects advertised options.

package acp

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/acp"
)

// PermissionApprover selects an advertised allow option, or cancels when none exists.
type PermissionApprover struct{}

// HandleMessage implements the native permission interceptor for opted-in harnesses.
func (PermissionApprover) HandleMessage(m agent.Message) (response []byte, handled bool, err error) {
	raw, ok := m.(*agent.RawMessage)
	if !ok || raw.MessageType != string(acp.MethodSessionRequestPermission) {
		return nil, false, nil
	}
	data, err := permissionResponse(raw.Raw)
	return data, true, err
}

// permissionConn lets the shared bounded reply owner deliver native permissions.
// It has no mutable state and works after either startup or relay adoption.
type permissionConn struct {
	agent.Conn
	agent.MessageInterceptor
}

func permissionResponse(raw []byte) ([]byte, error) {
	var req acp.JSONRPCMessage
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode ACP permission request: %w", err)
	}
	if !req.IsAgentRequest() || string(req.ID) == "null" || req.Method != acp.MethodSessionRequestPermission {
		return nil, errors.New("ACP permission request missing method or request ID")
	}
	var params acp.PermissionRequestParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("decode ACP permission options: %w", err)
	}
	outcome := acp.PermissionOutcome{Outcome: acp.PermissionOutcomeCancelled}
	for _, option := range params.Options {
		if option.OptionID != "" && (option.Kind == acp.PermissionAllowOnce || option.Kind == acp.PermissionAllowAlways) {
			outcome = acp.PermissionOutcome{Outcome: acp.PermissionOutcomeSelected, OptionID: option.OptionID}
			break
		}
	}
	result, err := json.Marshal(acp.PermissionResponseResult{Outcome: outcome})
	if err != nil {
		return nil, err
	}
	return MarshalInput(acp.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}
