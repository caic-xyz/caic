// Tests Pi native metadata and accounting parity without decoding transcript payloads.

package pi

import (
	"reflect"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

func TestMetadataParser(t *testing.T) {
	t.Parallel()
	t.Run("recorded served model", func(t *testing.T) {
		t.Parallel()
		for name, parser := range map[string]agent.MessageParser{"full": New("", nil).NewWire(), "metadata": New("", nil).NewMetadataParser()} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var got agent.SessionMetadata
				for _, msg := range agenttest.ParseJSONL(t, "testdata/read-edit-bash.jsonl", parser) {
					got.Apply(msg)
				}
				want := agent.SessionMetadata{ReportedModel: "xiaomi/mimo-v2.5-pro"}
				if got != want {
					t.Fatalf("session = %+v, want %+v", got, want)
				}
			})
		}
	})
	t.Run("native boundaries and partial reports", func(t *testing.T) {
		t.Parallel()
		for name, parser := range map[string]agent.MessageParser{"full": New("", nil).NewWire(), "metadata": New("", nil).NewMetadataParser()} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var got agent.SessionMetadata
				for _, tc := range []struct {
					name     string
					raw      string
					boundary bool
					want     agent.SessionMetadata
				}{
					{"selected model", `{"type":"response","command":"set_model","success":true,"data":{"provider":"openrouter","id":"auto"}}`, false, agent.SessionMetadata{ReportedModel: "openrouter/auto"}},
					{"partial state", `{"type":"response","command":"get_state","success":true,"data":{"thinkingLevel":"high"}}`, false, agent.SessionMetadata{ReportedModel: "openrouter/auto", ReportedEffort: "high"}},
					{"proven session", `{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1"}}`, true, agent.SessionMetadata{SessionID: "ses-1", ReportedModel: "openrouter/auto", ReportedEffort: "high"}},
					{"same session state", `{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1","thinkingLevel":"low"}}`, false, agent.SessionMetadata{SessionID: "ses-1", ReportedModel: "openrouter/auto", ReportedEffort: "low"}},
					{"first served model", `{"type":"message_start","message":{"role":"assistant","provider":"openrouter","model":"auto","responseModel":"anthropic/claude-opus-4.7"}}`, false, agent.SessionMetadata{SessionID: "ses-1", ReportedModel: "openrouter/anthropic/claude-opus-4.7", ReportedEffort: "low"}},
					{"next served model", `{"type":"message_start","message":{"role":"assistant","provider":"openrouter","model":"auto","responseModel":"openai/gpt-5.6"}}`, false, agent.SessionMetadata{SessionID: "ses-1", ReportedModel: "openrouter/openai/gpt-5.6", ReportedEffort: "low"}},
					{"new session settings", `{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-2","model":{"provider":"anthropic","id":"claude-sonnet-4-7"},"thinkingLevel":"medium"}}`, true, agent.SessionMetadata{SessionID: "ses-2", ReportedModel: "anthropic/claude-sonnet-4-7", ReportedEffort: "medium"}},
					{"new session retains known settings", `{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-3"}}`, true, agent.SessionMetadata{SessionID: "ses-3", ReportedModel: "anthropic/claude-sonnet-4-7", ReportedEffort: "medium"}},
				} {
					msgs, err := parser.ParseOutput([]byte(tc.raw))
					if err != nil {
						t.Fatalf("%s: %v", tc.name, err)
					}
					if len(msgs) != 1 {
						t.Fatalf("%s: messages = %#v, want one native metadata report", tc.name, msgs)
					}
					if tc.boundary {
						init, ok := msgs[0].(*agent.InitMessage)
						if !ok || init.SessionID != tc.want.SessionID || init.ReportedModel != tc.want.ReportedModel || init.ReportedEffort != tc.want.ReportedEffort || init.Version != "" {
							t.Fatalf("%s: session boundary = %#v, want known metadata %+v", tc.name, msgs[0], tc.want)
						}
					} else if _, ok := msgs[0].(*agent.SessionMetadataMessage); !ok {
						t.Fatalf("%s: partial report = %T, want *agent.SessionMetadataMessage", tc.name, msgs[0])
					}
					got.Apply(msgs[0])
					if got.ReportedModel != "" && got.ReportedEffort != "" {
						got.Apply(&agent.MetaSessionMessage{ReportedModel: "stale/model", ReportedEffort: "stale-effort"})
					}
					if got != tc.want {
						t.Fatalf("%s: metadata = %+v, want %+v", tc.name, got, tc.want)
					}
				}
			})
		}
	})
	t.Run("native startup and served model accounting", func(t *testing.T) {
		t.Parallel()
		for name, parser := range map[string]agent.MessageParser{"full": New("", nil).NewWire(), "metadata": New("", nil).NewMetadataParser()} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var got agent.SessionMetadata
				parse := func(raw string) []agent.Message {
					msgs, err := parser.ParseOutput([]byte(raw))
					if err != nil {
						t.Fatal(err)
					}
					for _, msg := range msgs {
						got.Apply(msg)
					}
					return msgs
				}
				parse(`{"type":"response","command":"set_model","success":false,"error":"unknown model"}`)
				if got != (agent.SessionMetadata{}) {
					t.Fatalf("failed selection became metadata: %+v", got)
				}
				selected := parse(`{"type":"response","command":"set_model","success":true,"data":{"provider":"openrouter","id":"selected","contextWindow":262144}}`)
				if len(selected) != 2 {
					t.Fatalf("selected model reports = %#v", selected)
				}
				window, ok := selected[1].(*agent.ModelInfoMessage)
				if !ok || window.ContextWindow != 262144 {
					t.Fatalf("selected model context = %#v", selected[1])
				}
				if got.ReportedModel != "openrouter/selected" {
					t.Fatalf("selected actual model = %+v", got)
				}
				parse(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1","model":{"provider":"openrouter","id":"auto"},"thinkingLevel":"high"}}`)
				want := agent.SessionMetadata{SessionID: "ses-1", ReportedModel: "openrouter/auto", ReportedEffort: "high"}
				if got != want {
					t.Fatalf("state = %+v, want %+v", got, want)
				}
				// An unrelated successful response must not be shape-classified as get_state.
				parse(`{"type":"response","command":"get_available_models","success":true,"data":{"sessionId":"foreign","model":{"provider":"wrong","id":"wrong"}}}`)
				if got != want {
					t.Fatalf("unrelated response replaced state: %+v", got)
				}
				parse(`{"type":"agent_start"}`)
				parse(`{"type":"message_start","message":{"role":"assistant","provider":"openrouter","model":"auto","responseModel":"anthropic/claude-opus-4.7","content":[]}}`)
				want.ReportedModel = "openrouter/anthropic/claude-opus-4.7"
				if got != want {
					t.Fatalf("served model = %+v, want %+v", got, want)
				}
				usage := parse(`{"type":"turn_end","message":{"role":"assistant","provider":"openrouter","model":"auto","responseModel":"anthropic/claude-opus-4.7","usage":{"input":10,"output":4,"cacheRead":3,"cacheWrite":2,"cacheWrite1h":2,"totalTokens":19,"cost":{"total":0.25}}}}`)
				if len(usage) != 1 {
					t.Fatalf("turn messages = %#v", usage)
				}
				u, ok := usage[0].(*agent.UsageMessage)
				if !ok || !u.Additive || !u.PerCall || u.ContextWindow != 262144 || u.Usage.InputTokens != 10 || u.Usage.OutputTokens != 4 || u.Usage.CacheTTLSeconds != 3600 || u.ReportedCostUSD == nil || *u.ReportedCostUSD != 0.25 {
					t.Fatalf("turn usage = %#v", usage[0])
				}
				result := parse(`{"type":"agent_end","messages":[{"role":"user","content":[{"type":"text","text":"not decoded"}]},{"role":"assistant","usage":{"input":10,"output":4,"cacheRead":3,"cacheWrite":2,"cacheWrite1h":2,"totalTokens":19,"cost":{"total":0.25}},"content":[{"type":"text","text":"not decoded"}]}]}`)
				r, ok := result[0].(*agent.ResultMessage)
				if !ok || !r.UsageIsSnapshot || r.TotalCostUSD != 0.25 || r.NumTurns != 1 || !reflect.DeepEqual(r.Usage, u.Usage) {
					t.Fatalf("result = %#v", result[0])
				}
				// A repeated state report and a later served-model report keep their native order.
				parse(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"ses-1","model":{"provider":"openrouter","id":"auto"},"thinkingLevel":"high"}}`)
				parse(`{"type":"message_start","message":{"role":"assistant","provider":"openrouter","model":"auto","responseModel":"openai/gpt-5.6"}}`)
				if got.ReportedModel != "openrouter/openai/gpt-5.6" {
					t.Fatalf("restarted served model = %+v", got)
				}
				parse(`{"type":"agent_start"}`)
				result = parse(`{"type":"agent_end","messages":[{"role":"assistant","usage":{"input":2,"output":1,"totalTokens":3,"cost":{"total":0.05}}}]}`)
				r, ok = result[0].(*agent.ResultMessage)
				if !ok || r.UsageIsSnapshot || r.TotalCostUSD != 0.05 || r.Usage.InputTokens != 2 || r.NumTurns != 0 {
					t.Fatalf("new invocation = %#v", result[0])
				}
			})
		}
	})
	t.Run("compaction and retries", func(t *testing.T) {
		t.Parallel()
		for name, parser := range map[string]agent.MessageParser{"full": New("", nil).NewWire(), "metadata": New("", nil).NewMetadataParser()} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				for _, raw := range []string{`{"type":"compaction_end","willRetry":true}`, `{"type":"agent_end","willRetry":true}`} {
					msgs, err := parser.ParseOutput([]byte(raw))
					if err != nil || len(msgs) != 0 {
						t.Fatalf("retry = %#v, %v", msgs, err)
					}
				}
				msgs, err := parser.ParseOutput([]byte(`{"type":"compaction_end","result":{"summary":"unretained summary","tokensBefore":100000,"estimatedTokensAfter":1000,"usage":{"input":10,"output":2,"totalTokens":12,"cost":{"total":0.1}}}}`))
				if err != nil {
					t.Fatal(err)
				}
				if len(msgs) != 2 {
					t.Fatalf("compaction = %#v", msgs)
				}
				u, ok := msgs[0].(*agent.UsageMessage)
				if !ok || !u.Additive || u.PerCall || u.Usage.InputTokens != 10 || u.ReportedCostUSD == nil || *u.ReportedCostUSD != 0.1 {
					t.Fatalf("summary usage = %#v", msgs[0])
				}
				boundary, ok := msgs[1].(*agent.SystemMessage)
				if !ok || boundary.Subtype != agent.SystemSubtypeCompactBoundary || boundary.ContextTokensBefore != 100000 || boundary.ContextTokensAfter != 1000 {
					t.Fatalf("boundary = %#v", msgs[1])
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()

		for _, raw := range []string{
			`{"type":"message_start","message":5}`,
			`{"type":"response","command":"get_state","success":true,"data":{"model":false}}`,
			`{"type":"compaction_end","result":5}`,
		} {
			for _, parser := range []agent.MessageParser{New("", nil).NewWire(), New("", nil).NewMetadataParser()} {
				if _, err := parser.ParseOutput([]byte(raw)); err == nil {
					t.Fatalf("accepted malformed native metadata: %s", raw)
				}
			}
		}
	})
}
