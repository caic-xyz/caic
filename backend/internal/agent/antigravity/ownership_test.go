// Tests actual Antigravity conversation and step provenance without inventing native lifecycles.

package antigravity

import (
	"fmt"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestConversationProvenance(t *testing.T) {
	t.Parallel()
	// Synthetic DTO cases pin metadata semantics; they are not recorded native
	// lifecycle evidence and cannot enable subagent cards.
	for _, version := range []agent.LogVersion{0, agent.LogVersionV2, agent.LogVersionV3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			t.Parallel()
			wire := New("", nil).NewWire()
			parser, err := agent.NewLogRecordParser(agent.LogVersionV3, wire.ParseMessage)
			if version != 0 {
				parser, err = agent.NewLogRecordParser(version, wire.ParseMessage)
			}
			if err != nil {
				t.Fatal(err)
			}
			feed := func(line string) []agent.Message {
				if version == 0 {
					messages, err := wire.ParseMessage([]byte(line))
					if err != nil {
						t.Fatal(err)
					}
					return messages
				}
				record, err := parser.ParseRecord([]byte(`{"t":"agent","ts":1.000,"msg":` + line + `}`))
				if err != nil {
					t.Fatal(err)
				}
				var messages []agent.Message
				for _, timed := range record.Messages {
					messages = append(messages, timed.Message)
				}
				return messages
			}
			feed(`{"event":"init","conversation_id":"parent"}`)
			messages := feed(`{"event":"step_update","step_update":{"conversation_id":"foreign","step_index":0,"state":"ACTIVE","step_type":"agent_response","text_delta":"unknown owner"}}`)
			if len(messages) != 1 || messages[0].Provenance().ThreadID != "foreign" || messages[0].Provenance().ItemID != "0" || messages[0].Provenance().IsChild() || messages[0].Provenance().HasIdentity() {
				t.Fatalf("unrelated conversation source = %#v", messages)
			}
			feed(`{"event":"step_update","step_update":{"conversation_id":"parent","step_index":1,"state":"DONE","step_type":"tool","tool_name":"invoke_subagent","subagent_info":{"subagents":[{"conversation_id":"child","role":"researcher"}]}}}`)
			for _, line := range []string{
				`{"event":"step_update","step_update":{"conversation_id":"child","step_index":0,"state":"ACTIVE","step_type":"agent_response","text_delta":"answer"}}`,
				`{"event":"step_update","step_update":{"conversation_id":"child","step_index":2,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"output":"child output"},"usage":{"input_tokens":3}}}`,
			} {
				for _, m := range feed(line) {
					s := m.Provenance()
					if s.ThreadID != "child" || s.RootThreadID != "parent" || !s.IsChild() || s.AgentID != "" || s.HasIdentity() {
						t.Fatalf("declared child %T = %+v", m, s)
					}
					if _, ok := m.(*agent.NativeSubagentMessage); ok {
						t.Fatal("fabricated native lifecycle")
					}
				}
			}
			// Foreign completion cannot consume the root's active tool state,
			// suppress response fallback, or contribute unproven usage.
			feed(`{"event":"step_update","step_update":{"conversation_id":"parent","step_index":9,"state":"ACTIVE","step_type":"tool","tool_name":"run_command"}}`)
			for _, line := range []string{
				`{"event":"step_update","step_update":{"conversation_id":"foreign","step_index":9,"state":"DONE","step_type":"tool","tool_info":{"output":"foreign tool output"},"usage":{"input_tokens":9000}}}`,
				`{"event":"result","result":{"conversation_id":"foreign","status":"SUCCESS","response":"foreign answer"}}`,
				`{"event":"result","result":{"conversation_id":"child","status":"SUCCESS","response":"child answer"}}`,
			} {
				for _, m := range feed(line) {
					if _, ok := m.(*agent.ResultMessage); ok {
						t.Fatal("foreign result finished root")
					}
					if text, ok := m.(*agent.TextMessage); ok && !text.Source.IsChild() {
						t.Fatalf("foreign root answer = %+v", text)
					}
				}
			}
			messages = feed(`{"event":"step_update","step_update":{"conversation_id":"parent","step_index":9,"state":"DONE","step_type":"tool","tool_info":{"output":"root output"},"usage":{"input_tokens":5}}}`)
			for _, m := range messages {
				if _, ok := m.(*agent.ToolUseMessage); ok {
					t.Fatal("foreign result cleared running root tool")
				}
			}
			messages = feed(`{"event":"result","result":{"conversation_id":"parent","status":"SUCCESS","response":"root answer"}}`)
			if len(messages) != 2 {
				t.Fatalf("root completion = %#v", messages)
			}
			if text, ok := messages[0].(*agent.TextMessage); !ok || text.Text != "root answer" || text.Source.IsChild() {
				t.Fatalf("root fallback = %#v", messages[0])
			}
			if result, ok := messages[1].(*agent.ResultMessage); !ok || result.Usage.InputTokens != 8 {
				t.Fatalf("inclusive proven usage = %#v", messages[1])
			}
			messages = feed(`{"event":"step_update","step_update":{"state":"ACTIVE","step_type":"agent_response","text_delta":"missing metadata"}}`)
			if len(messages) != 1 || !messages[0].Provenance().IsZero() {
				t.Fatalf("invented missing identity = %#v", messages)
			}
		})
	}
}

func BenchmarkParseConversationSteps(b *testing.B) {
	lines := [][]byte{
		[]byte(`{"event":"init","conversation_id":"parent"}`),
		[]byte(`{"event":"step_update","step_update":{"conversation_id":"parent","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"Parent reply"}}`),
		[]byte(`{"event":"step_update","step_update":{"conversation_id":"foreign","step_index":1,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"output":"output"}}}`),
		[]byte(`{"event":"step_update","step_update":{"conversation_id":"parent","step_index":1,"state":"DONE","step_type":"agent_response","usage":{"input_tokens":10,"output_tokens":5}}}`),
		[]byte(`{"event":"result","result":{"conversation_id":"parent","status":"SUCCESS","response":"Parent reply"}}`),
	}
	b.ReportAllocs()
	for range b.N {
		w := New("", nil).NewWire()
		for _, line := range lines {
			if _, err := w.ParseMessage(line); err != nil {
				b.Fatal(err)
			}
		}
	}
}
