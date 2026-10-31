// Tests backend registration, isolated wires, and directional native history decoding.

package backends

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/antigravity"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestDefault(t *testing.T) {
	t.Parallel()

	b := Default(t.TempDir(), nil)
	if _, ok := b[harness.Antigravity].(*antigravity.Backend); !ok {
		t.Fatalf("Antigravity backend = %T, want native implementation", b[harness.Antigravity])
	}
	first, err := b.ResolveWire(harness.Antigravity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.ResolveWire(harness.Antigravity)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("Antigravity tasks share a wire")
	}

	t.Run("DirectionalParsing", func(t *testing.T) {
		t.Parallel()
		backends := Default(t.TempDir(), nil)
		for _, tc := range []struct {
			harness harness.Name
			input   string
			output  string
			result  string
		}{
			{harness.Claude,
				`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
				`{"type":"system","subtype":"init","session_id":"wrong-session","model":"wrong-model"}`,
				`{"type":"result","usage":{}}`},
			{harness.Codex,
				`{"jsonrpc":"2.0","id":12,"method":"turn/start","params":{"threadId":"session","input":[{"type":"text","text":"hello"}]}}`,
				`{"method":"thread/started","params":{"thread":{"id":"wrong-session","model":"wrong-model"}}}`,
				`{"method":"turn/completed","params":{"threadId":"session","turn":{"id":"turn","status":"completed"}}}`},
			{harness.OpenCode,
				`{"jsonrpc":"2.0","id":12,"method":"session/prompt","params":{"sessionId":"session","prompt":[{"type":"text","text":"hello"}]}}`,
				`{"method":"session/update","params":{"sessionId":"session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"wrong output"}}}}`,
				`{"jsonrpc":"2.0","id":12,"result":{"stopReason":"end_turn"}}`},
			{harness.Pi,
				`{"type":"prompt","message":"hello"}`,
				`{"type":"turn_end","message":{"role":"assistant","model":"wrong-model","usage":{"input":100,"output":20,"totalTokens":120}},"toolResults":[]}`,
				`{"type":"agent_end","messages":[]}`},
			{harness.Antigravity,
				`{"event":"user","message":{"content":[{"type":"text","text":"hello"}]}}`,
				`{"event":"init","conversation_id":"wrong-session","init":{"model":"wrong-model"}}`,
				`{"event":"result","result":{"status":"SUCCESS","conversation_id":"session"}}`},
		} {
			t.Run(string(tc.harness), func(t *testing.T) {
				t.Parallel()
				wire := backends[tc.harness].NewWire()
				parser, err := agent.NewLogRecordParser(wire)
				if err != nil {
					t.Fatal(err)
				}
				input, err := parser.ParseRecord([]byte(`{"t":"input","ts":1.000,"msg":` + tc.input + `}`))
				if err != nil || input.Provenance.Source == agent.RecordSourceRelay {
					t.Fatalf("input = %#v, %v", input, err)
				}
				if tc.harness == harness.Codex {
					if len(input.Messages) != 1 {
						t.Fatalf("client request = %#v", input.Messages)
					}
					raw, ok := input.Messages[0].Message.(*agent.RawMessage)
					if !ok || raw.MessageType != "jsonrpc_request" || !bytes.Equal(raw.Raw, []byte(tc.input)) {
						t.Fatalf("client request = %#v", input.Messages[0].Message)
					}
				} else if len(input.Messages) == 0 {
					t.Fatal("prompt produced no input")
				} else if user, ok := input.Messages[0].Message.(*agent.UserInputMessage); !ok || user.Text != "hello" {
					t.Fatalf("prompt = %#v", input.Messages[0].Message)
				}
				// A native output record arriving under an input envelope must remain
				// raw and cannot establish session identity, models or output usage.
				wrong, err := parser.ParseRecord([]byte(`{"t":"input","ts":2.000,"msg":` + tc.output + `}`))
				if err != nil || len(wrong.Messages) != 1 {
					t.Fatalf("wrong direction = %#v, %v", wrong, err)
				}
				if raw, ok := wrong.Messages[0].Message.(*agent.RawMessage); !ok || string(raw.Raw) != tc.output {
					t.Fatalf("wrong direction = %#v", wrong.Messages[0].Message)
				}
				got, err := wire.ParseOutput([]byte(tc.result))
				if err != nil {
					t.Fatal(err)
				}
				fresh := backends[tc.harness].NewWire()
				if _, err := fresh.ParseInput([]byte(tc.input)); err != nil {
					t.Fatal(err)
				}
				want, err := fresh.ParseOutput([]byte(tc.result))
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("input mutated output state: got %#v, want %#v, err %v", got, want, err)
				}
			})
		}
	})
}
