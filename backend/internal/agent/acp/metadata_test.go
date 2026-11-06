// Tests ordered ACP metadata and accounting parity without persisted session controls.

package acp

import (
	"reflect"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestMetadataParser(t *testing.T) {
	t.Parallel()
	t.Run("partial reports preserve session boundaries", func(t *testing.T) {
		t.Parallel()
		for name, parser := range map[string]agent.MessageParser{"full": testBackend().NewWire(), "metadata": testBackend().NewMetadataParser()} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var metadata agent.SessionMetadata
				var starts []agent.SessionMetadata
				for _, record := range []struct{ input, output, kind string }{
					{`{"id":1,"method":"initialize","params":{}}`, `{"id":1,"result":{"agentInfo":{"version":"1.18.31"}}}`, "session_metadata"},
					{`{"id":2,"method":"session/new","params":{}}`, `{"id":2,"result":{"sessionId":"root","models":{"currentModelId":"native-default"}}}`, "init"},
					{`{"id":3,"method":"session/set_config_option","params":{"sessionId":"root","configId":"model","value":"requested"}}`, `{"id":3,"result":{"configOptions":[{"id":"model","currentValue":"served-model"},{"id":"effort","currentValue":"high"}]}}`, "session_metadata"},
					{"", `{"method":"session/update","params":{"sessionId":"root","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"effort","currentValue":"low"}]}}}`, "session_metadata"},
					{`{"id":1,"method":"initialize","params":{}}`, `{"id":1,"result":{"agentInfo":{"version":"1.18.32"}}}`, "session_metadata"},
					{`{"id":2,"method":"session/load","params":{"sessionId":"root"}}`, `{"id":2,"result":{}}`, "init"},
				} {
					if record.input != "" {
						if _, err := parser.ParseInput([]byte(record.input)); err != nil {
							t.Fatal(err)
						}
					}
					messages, err := parser.ParseOutput([]byte(record.output))
					if err != nil {
						t.Fatal(err)
					}
					if len(messages) != 1 || messages[0].Type() != record.kind {
						t.Fatalf("reply %s = %#v, want %s", record.output, messages, record.kind)
					}
					for _, message := range messages {
						metadata.Apply(message)
						if init, ok := message.(*agent.InitMessage); ok {
							starts = append(starts, agent.SessionMetadata{SessionID: init.SessionID, ReportedModel: init.ReportedModel, ReportedEffort: init.ReportedEffort, AgentVersion: init.Version})
						}
					}
				}
				metadata.Apply(&agent.MetaSessionMessage{SessionID: "root", ReportedModel: "stale-default", ReportedEffort: "stale-effort"})
				want := agent.SessionMetadata{SessionID: "root", ReportedModel: "served-model", ReportedEffort: "low", AgentVersion: "1.18.32"}
				if metadata != want {
					t.Fatalf("metadata = %+v, want %+v", metadata, want)
				}
				wantStarts := []agent.SessionMetadata{
					{SessionID: "root", ReportedModel: "native-default", AgentVersion: "1.18.31"},
					want,
				}
				if !reflect.DeepEqual(starts, wantStarts) {
					t.Fatalf("proven session starts = %+v, want %+v", starts, wantStarts)
				}
			})
		}
	})

	t.Run("ordered replies and native updates", func(t *testing.T) {
		t.Parallel()
		for name, parser := range map[string]agent.MessageParser{"full": testBackend().NewWire(), "metadata": testBackend().NewMetadataParser()} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				records := []struct {
					input bool
					raw   string
					want  agent.SessionMetadata
				}{
					{true, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, agent.SessionMetadata{}},
					{false, `{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"version":"1.18.31"}}}`, agent.SessionMetadata{AgentVersion: "1.18.31"}},
					{true, `{"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"root"}}`, agent.SessionMetadata{AgentVersion: "1.18.31"}},
					{false, `{"jsonrpc":"2.0","id":2,"result":{"models":{"currentModelId":"default"},"configOptions":[{"id":"model","currentValue":"anthropic/claude-opus-4-7"},{"id":"effort","currentValue":"high"}]}}`, agent.SessionMetadata{SessionID: "root", ReportedModel: "anthropic/claude-opus-4-7", ReportedEffort: "high", AgentVersion: "1.18.31"}},
					{true, `{"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"sessionId":"root","configId":"model","value":"requested"}}`, agent.SessionMetadata{SessionID: "root", ReportedModel: "anthropic/claude-opus-4-7", ReportedEffort: "high", AgentVersion: "1.18.31"}},
					{false, `{"jsonrpc":"2.0","id":3,"result":{"configOptions":[{"id":"model","currentValue":"openai/gpt-5.6"}]}}`, agent.SessionMetadata{SessionID: "root", ReportedModel: "openai/gpt-5.6", ReportedEffort: "high", AgentVersion: "1.18.31"}},
					// Unmatched native-looking replies and known foreign sessions cannot replace the root.
					{false, `{"jsonrpc":"2.0","id":99,"result":{"sessionId":"foreign","models":{"currentModelId":"foreign"}}}`, agent.SessionMetadata{SessionID: "root", ReportedModel: "openai/gpt-5.6", ReportedEffort: "high", AgentVersion: "1.18.31"}},
					{false, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"foreign","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"model","currentValue":"foreign"}]}}}`, agent.SessionMetadata{SessionID: "root", ReportedModel: "openai/gpt-5.6", ReportedEffort: "high", AgentVersion: "1.18.31"}},
					{false, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"root","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"effort","currentValue":"low"}]}}}`, agent.SessionMetadata{SessionID: "root", ReportedModel: "openai/gpt-5.6", ReportedEffort: "low", AgentVersion: "1.18.31"}},
				}
				var got agent.SessionMetadata
				for i, record := range records {
					var msgs []agent.Message
					var err error
					if record.input {
						msgs, err = parser.ParseInput([]byte(record.raw))
					} else {
						msgs, err = parser.ParseOutput([]byte(record.raw))
					}
					if err != nil {
						t.Fatalf("record %d: %v", i, err)
					}
					for _, msg := range msgs {
						got.Apply(msg)
					}
					if got != record.want {
						t.Fatalf("record %d metadata = %+v, want %+v", i, got, record.want)
					}
				}
			})
		}
	})
	t.Run("request ownership and usage", func(t *testing.T) {
		t.Parallel()
		for name, parser := range map[string]agent.MessageParser{"full": testBackend().NewWire(), "metadata": testBackend().NewMetadataParser()} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				input := func(raw string) {
					if _, err := parser.ParseInput([]byte(raw)); err != nil {
						t.Fatal(err)
					}
				}
				output := func(raw string) []agent.Message {
					msgs, err := parser.ParseOutput([]byte(raw))
					if err != nil {
						t.Fatal(err)
					}
					return msgs
				}
				// New process initialization retires old requests; id reuse by unrelated requests also retires ownership.
				input(`{"id":7,"method":"session/new","params":{}}`)
				input(`{"id":1,"method":"initialize","params":{}}`)
				output(`{"id":7,"result":{"sessionId":"stale"}}`)
				input(`{"id":2,"method":"session/new","params":{}}`)
				input(`{"id":2,"method":"session/list","params":{}}`)
				msgs := output(`{"id":2,"result":{"sessionId":"unrelated"}}`)
				for _, msg := range msgs {
					switch msg.(type) {
					case *agent.InitMessage, *agent.SessionMetadataMessage:
						t.Fatalf("unowned reply became session metadata: %+v", msg)
					}
				}
				input(`{"id":0,"method":"session/prompt","params":{"sessionId":"root","prompt":[{"type":"image","data":"ignored"}]}}`)
				msgs = output(`{"id":0,"result":{"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":4,"cachedReadTokens":3,"cachedWriteTokens":2,"thoughtTokens":1}}}`)
				if len(msgs) != 1 {
					t.Fatalf("prompt messages = %#v", msgs)
				}
				result, ok := msgs[0].(*agent.ResultMessage)
				want := agent.Usage{InputTokens: 10, OutputTokens: 4, CacheReadInputTokens: 3, CacheCreationInputTokens: 2, ReasoningOutputTokens: 1}
				if !ok || !reflect.DeepEqual(result.Usage, want) || result.UsageIsSnapshot || result.TotalCostUSD != 0 {
					t.Fatalf("prompt result = %#v, want usage %+v", msgs[0], want)
				}
				input(`{"id":8,"method":"session/prompt","params":{}}`)
				msgs = output(`{"id":8,"error":{"code":-1,"message":"prompt failed"}}`)
				if len(msgs) != 1 {
					t.Fatalf("failure messages = %#v", msgs)
				}
				failed, ok := msgs[0].(*agent.ResultMessage)
				if !ok || !failed.IsError || failed.Result != "prompt failed" {
					t.Fatalf("failed prompt = %#v", msgs[0])
				}
				// The same failed response is no longer owned and cannot finish another turn.
				for _, msg := range output(`{"id":8,"error":{"code":-1,"message":"prompt failed"}}`) {
					if _, ok := msg.(*agent.ResultMessage); ok {
						t.Fatal("retired response emitted another result")
					}
				}
				input(`{"id":9,"method":"session/load","params":{"sessionId":"root"}}`)
				output(`{"id":9,"result":{}}`)
				input(`{"id":10,"method":"session/set_config_option","params":{"sessionId":"foreign","configId":"model","value":"foreign"}}`)
				for _, msg := range output(`{"id":10,"result":{"configOptions":[{"id":"model","currentValue":"foreign"}]}}`) {
					switch msg.(type) {
					case *agent.InitMessage, *agent.SessionMetadataMessage:
						t.Fatal("foreign session configuration became root metadata")
					}
				}
				msgs = output(`{"method":"session/update","params":{"sessionId":"root","update":{"sessionUpdate":"usage_update","size":200000,"cost":{"currency":"USD","amount":0.42}}}}`)
				usage, ok := msgs[0].(*agent.UsageMessage)
				if !ok || usage.ContextWindow != 200000 || usage.CumulativeCostUSD == nil || *usage.CumulativeCostUSD != 0.42 {
					t.Fatalf("usage = %#v", msgs)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct{ input, output string }{
			{`{"id":1,"method":"session/new","params":{}}`, `{"id":1,"result":5}`},
			{`{"id":1,"method":"session/new","params":{}}`, `{"id":1,"result":{}}`},
			{`{"id":1,"method":"session/load","params":{}}`, `{"id":1,"result":{}}`},
			{`{"id":1,"method":"initialize","params":{}}`, `{"id":1,"result":{"agentInfo":false}}`},
			{`{"id":1,"method":"session/set_config_option","params":{}}`, `{"id":1,"result":{"configOptions":[{"id":"model","currentValue":5}]}}`},
			{`{"id":1,"method":"session/prompt","params":{}}`, `{"id":1,"result":{"stopReason":"end_turn","usage":{"inputTokens":"invalid"}}}`},
		} {
			for _, parser := range []agent.MessageParser{testBackend().NewWire(), testBackend().NewMetadataParser()} {
				if _, err := parser.ParseInput([]byte(tc.input)); err != nil {
					t.Fatal(err)
				}
				if _, err := parser.ParseOutput([]byte(tc.output)); err == nil {
					t.Fatalf("accepted malformed native metadata: %s", tc.output)
				}
			}
		}
	})
}
