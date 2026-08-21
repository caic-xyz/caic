// Tests for strict Pi content-block validation in check-agent-logs.

package main

import (
	"strings"
	"testing"

	pidto "github.com/maruel/genai/providers/pi"
)

func TestValidatePiStrictContentBlocks(t *testing.T) {
	t.Parallel()
	t.Run("agent message", func(t *testing.T) {
		t.Parallel()
		valid := []byte(`{"type":"message_start","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`)
		if err := validatePiStrictContentBlocks(pidto.EventMessageStart, valid); err != nil {
			t.Fatal(err)
		}
		invalid := []byte(`{"type":"message_start","message":{"role":"assistant","content":[{"type":"text","text":"hello","unexpected":true}]}}`)
		if err := validatePiStrictContentBlocks(pidto.EventMessageStart, invalid); err == nil || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
			t.Fatalf("validatePiStrictContentBlocks error = %v, want unknown-field error", err)
		}
	})
	t.Run("tool call content fields", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"type":"message_start","message":{"role":"assistant","content":[{"type":"toolCall","id":"call","name":"read","arguments":{},"partialArgs":"{\"path\":","streamIndex":0}]}}`)
		if err := validatePiStrictContentBlocks(pidto.EventMessageStart, data); err != nil {
			t.Fatalf("validatePiStrictContentBlocks error = %v, want valid tool call", err)
		}
	})
	t.Run("opaque arguments", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"type":"tool_execution_start","toolCallId":"call","toolName":"tool","args":{"content":[{"unexpected":true}]}}`)
		if err := validatePiStrictContentBlocks(pidto.EventToolExecStart, data); err != nil {
			t.Fatalf("validatePiStrictContentBlocks error = %v, want opaque arguments accepted", err)
		}
	})
	t.Run("response history", func(t *testing.T) {
		t.Parallel()
		valid := []byte(`{"type":"response","command":"get_messages","success":true,"data":{"messages":[{"role":"assistant","content":[{"type":"text","text":"hello"}]}]}}`)
		if err := validatePiStrictContentBlocks(pidto.EventResponse, valid); err != nil {
			t.Fatal(err)
		}
		invalid := []byte(`{"type":"response","command":"get_entries","success":true,"data":{"entries":[{"content":[{"type":"text","text":"hello","unexpected":true}]}]}}`)
		if err := validatePiStrictContentBlocks(pidto.EventResponse, invalid); err == nil || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
			t.Fatalf("validatePiStrictContentBlocks error = %v, want unknown-field error", err)
		}
	})
}
