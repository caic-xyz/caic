// Tests directional Codex metadata, handshake ownership, model rerouting, and inclusive usage.

package codex

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

func TestMetadataParser(t *testing.T) {
	t.Parallel()
	for _, projection := range []string{"full", "metadata"} {
		t.Run(projection, func(t *testing.T) {
			t.Parallel()
			backend := New("", nil)
			newParser := func() agent.MessageParser {
				if projection == "metadata" {
					return backend.NewMetadataParser()
				}
				return backend.NewWire()
			}
			t.Run("request ownership", func(t *testing.T) {
				t.Parallel()
				p := newParser()
				var metadata agent.SessionMetadata
				feed := func(input bool, line string) {
					parse := p.ParseOutput
					if input {
						parse = p.ParseInput
					}
					messages, err := parse([]byte(line))
					if err != nil {
						t.Fatal(err)
					}
					for _, m := range messages {
						metadata.Apply(m)
					}
				}
				response := `{"id":1,"result":{"thread":{"id":"root","cliVersion":"0.154.0","turns":[{"items":[{"text":"saved transcript"}]}]},"model":"gpt-5.6","reasoningEffort":"high"}}`
				feed(false, response)
				if metadata != (agent.SessionMetadata{}) {
					t.Fatalf("unmatched reply = %+v", metadata)
				}
				feed(true, `{"id":1,"method":"thread/resume","params":{"threadId":"root"}}`)
				feed(true, `{"id":1,"method":"thread/read","params":{"threadId":"other"}}`)
				feed(false, response)
				if metadata != (agent.SessionMetadata{}) {
					t.Fatalf("reused unrelated request = %+v", metadata)
				}
				feed(true, `{"id":1,"method":"thread/resume","params":{"threadId":"root"}}`)
				feed(false, response)
				want := agent.SessionMetadata{SessionID: "root", ReportedModel: "gpt-5.6", ReportedEffort: "high", AgentVersion: "0.154.0"}
				if metadata != want {
					t.Fatalf("native resume = %+v, want %+v", metadata, want)
				}
				feed(false, `{"id":1,"result":{"thread":{"id":"stale"},"model":"stale"}}`)
				feed(true, `{"id":2,"method":"thread/start","params":{}}`)
				feed(false, `{"id":2,"error":{"code":-1,"message":"failed"}}`)
				feed(false, `{"id":2,"result":{"thread":{"id":"stale"},"model":"stale"}}`)
				if metadata != want {
					t.Fatalf("retired/failed request = %+v", metadata)
				}
				feed(true, `{"id":3,"method":"thread/start","params":{}}`)
				feed(true, `{"id":1,"method":"initialize","params":{}}`)
				feed(false, `{"id":3,"result":{"thread":{"id":"stale"},"model":"stale"}}`)
				if metadata != want {
					t.Fatalf("prior process request = %+v", metadata)
				}
			})
			t.Run("ordered models and inclusive usage", func(t *testing.T) {
				t.Parallel()
				p := newParser()
				var metadata agent.SessionMetadata
				var results []agent.Usage
				var models []string
				if _, err := p.ParseInput([]byte(`{"id":7,"method":"thread/start","params":{}}`)); err != nil {
					t.Fatal(err)
				}
				for _, line := range []string{
					`{"id":7,"result":{"thread":{"id":"root","cliVersion":"0.154.0"},"model":"gpt-5.6","reasoningEffort":"high"}}`,
					`{"method":"thread/tokenUsage/updated","params":{"threadId":"root","tokenUsage":{"total":{"totalTokens":1000,"inputTokens":800,"outputTokens":200},"last":{"totalTokens":100,"inputTokens":80,"outputTokens":20}}}}`,
					`{"method":"item/started","params":{"threadId":"root","item":{"id":"spawn","type":"subAgentActivity","kind":"started","agentThreadId":"child","agentPath":"/root/worker","model":"child-model"}}}`,
					`{"method":"thread/started","params":{"thread":{"id":"child","model":"child-model"}}}`,
					`{"method":"turn/started","params":{"threadId":"root","turn":{"id":"turn"}}}`,
					`{"method":"model/rerouted","params":{"threadId":"root","fromModel":"gpt-5.6","toModel":"gpt-5.6-codex"}}`,
					`{"method":"model/rerouted","params":{"threadId":"child","fromModel":"child-model","toModel":"child-rerouted"}}`,
					`{"method":"thread/tokenUsage/updated","params":{"threadId":"root","tokenUsage":{"total":{"totalTokens":1100,"inputTokens":890,"outputTokens":210},"last":{"totalTokens":100,"inputTokens":90,"outputTokens":10}}}}`,
					`{"method":"thread/tokenUsage/updated","params":{"threadId":"child","tokenUsage":{"total":{"totalTokens":35,"inputTokens":30,"outputTokens":5},"last":{"totalTokens":35,"inputTokens":30,"outputTokens":5}}}}`,
					`{"method":"turn/completed","params":{"threadId":"child","turn":{"id":"ct","status":"completed"}}}`,
					`{"method":"turn/completed","params":{"threadId":"root","turn":{"id":"turn","status":"completed"}}}`,
					`{"method":"turn/completed","params":{"threadId":"root","turn":{"id":"empty","status":"completed"}}}`,
				} {
					messages, err := p.ParseOutput([]byte(line))
					if err != nil {
						t.Fatal(err)
					}
					for _, m := range messages {
						metadata.Apply(m)
						if u, ok := m.(*agent.UsageMessage); ok && u.Usage.InputTokens > 0 {
							models = append(models, u.ReportedModel)
						}
						if r, ok := m.(*agent.ResultMessage); ok {
							results = append(results, r.Usage)
						}
					}
				}
				want := agent.SessionMetadata{SessionID: "root", ReportedModel: "gpt-5.6-codex", ReportedEffort: "high", AgentVersion: "0.154.0"}
				if metadata != want {
					t.Fatalf("metadata = %+v, want %+v", metadata, want)
				}
				if len(models) != 2 || models[0] != "gpt-5.6-codex" || models[1] != "child-rerouted" {
					t.Fatalf("usage models = %v", models)
				}
				if len(results) != 2 || !results[0].Equal(agent.Usage{InputTokens: 120, OutputTokens: 15}) || !results[1].Equal(agent.Usage{}) {
					t.Fatalf("root turn usage = %+v", results)
				}
			})
			t.Run("recorded handshake without session control", func(t *testing.T) {
				t.Parallel()
				var metadata agent.SessionMetadata
				for _, m := range agenttest.ParseJSONL(t, "testdata/read-edit-bash.jsonl", newParser()) {
					metadata.Apply(m)
				}
				if metadata != (agent.SessionMetadata{SessionID: "019e7427-b074-7033-9074-7e87794ca5bd", ReportedModel: "gpt-5.4-mini", AgentVersion: "0.133.0"}) {
					t.Fatalf("recorded native metadata = %+v", metadata)
				}
			})
		})
	}
}
