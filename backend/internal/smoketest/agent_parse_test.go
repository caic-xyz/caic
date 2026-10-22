// Tests fake-agent parsing preserves stream and skill provenance, context boundaries, usage, and timing fields.

package smoketest

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestParseMessage(t *testing.T) {
	t.Parallel()
	t.Run("stream provenance", func(t *testing.T) {
		t.Parallel()
		for _, typ := range []string{"text", "text_delta", "thinking", "thinking_delta"} {
			t.Run(typ, func(t *testing.T) {
				t.Parallel()
				for _, text := range []string{"fragment", ""} {
					t.Run(text, func(t *testing.T) {
						t.Parallel()
						line := `{"type":"` + typ + `","text":"` + text + `","source":{"threadID":"child","turnID":"turn","itemID":"item","rootThreadID":"parent"}}`
						messages, err := parseMessage([]byte(line))
						if err != nil || len(messages) != 1 {
							t.Fatalf("stream parse = %v, %v", messages, err)
						}
						var source agent.MessageSource
						var got string
						switch m := messages[0].(type) {
						case *agent.TextMessage:
							source, got = m.Source, m.Text
						case *agent.TextDeltaMessage:
							source, got = m.Source, m.Text
						case *agent.ThinkingMessage:
							source, got = m.Source, m.Text
						case *agent.ThinkingDeltaMessage:
							source, got = m.Source, m.Text
						default:
							t.Fatalf("stream type = %T", messages[0])
						}
						if got != text || source.ThreadID != "child" || source.TurnID != "turn" || source.ItemID != "item" || source.RootThreadID != "parent" {
							t.Fatalf("stream = %q, %+v", got, source)
						}
					})
				}
			})
		}
	})
	t.Run("tool and usage provenance", func(t *testing.T) {
		t.Parallel()
		for _, record := range []string{
			`{"type":"tool_use","id":"shared","name":"Bash","input":{"command":"echo child"}}`,
			`{"type":"tool_output_delta","tool_use_id":"shared","delta":"child output"}`,
			`{"type":"usage","usage":{"input_tokens":23},"model":"child-model","context_window":1024}`,
		} {
			line := record[:len(record)-1] + `,"source":{"threadID":"child","rootThreadID":"parent","agentID":"native-child"}}`
			messages, err := parseMessage([]byte(line))
			if err != nil || len(messages) != 1 {
				t.Fatalf("parse = %v, %v", messages, err)
			}
			source := messages[0].Provenance()
			if source.ThreadID != "child" || source.RootThreadID != "parent" || source.AgentID != "native-child" {
				t.Fatalf("%T source = %+v", messages[0], source)
			}
			switch m := messages[0].(type) {
			case *agent.ToolUseMessage:
				if m.ToolUseID != "shared" || m.Name != "Bash" {
					t.Fatalf("tool = %+v", m)
				}
			case *agent.ToolOutputDeltaMessage:
				if m.ToolUseID != "shared" || m.Delta != "child output" {
					t.Fatalf("output = %+v", m)
				}
			case *agent.UsageMessage:
				if m.Usage.InputTokens != 23 || m.ReportedModel != "child-model" || m.ContextWindow != 1024 {
					t.Fatalf("usage = %+v", m)
				}
			default:
				t.Fatalf("unexpected message %T", m)
			}
		}
	})
	t.Run("skill provenance", func(t *testing.T) {
		t.Parallel()
		messages, err := parseMessage([]byte(`{"type":"skill_load","skill":"review","path":"/skills/review/SKILL.md"}`))
		if err != nil || len(messages) != 1 {
			t.Fatalf("skill parse = %v, %v", messages, err)
		}
		load, ok := messages[0].(*agent.SkillLoadMessage)
		if !ok || load.Skill != "review" || load.Path != "/skills/review/SKILL.md" {
			t.Fatalf("skill load = %#v", messages[0])
		}
	})
	t.Run("context boundary", func(t *testing.T) {
		t.Parallel()
		messages, err := parseMessage([]byte(`{"type":"system","subtype":"compact_boundary"}`))
		if err != nil || len(messages) != 1 {
			t.Fatalf("boundary parse = %v, %v", messages, err)
		}
		boundary, ok := messages[0].(*agent.SystemMessage)
		if !ok || boundary.Subtype != "compact_boundary" {
			t.Fatalf("context boundary = %#v", messages[0])
		}
	})
	t.Run("result usage and timings", func(t *testing.T) {
		t.Parallel()
		messages, err := parseMessage([]byte(`{"type":"result","subtype":"success","result":"Done","num_turns":1,"total_cost_usd":0.37,"duration_ms":42000,"duration_api_ms":30000,"context_window":200000,"usage":{"input_tokens":2700,"output_tokens":4200,"cache_creation_input_tokens":8400,"cache_read_input_tokens":42000,"reasoning_output_tokens":1200}}`))
		if err != nil || len(messages) != 1 {
			t.Fatalf("result parse = %v, %v", messages, err)
		}
		result, ok := messages[0].(*agent.ResultMessage)
		if !ok {
			t.Fatalf("result type = %T", messages[0])
		}
		if result.Usage.InputTokens != 2700 || result.Usage.CacheReadInputTokens != 42000 || result.Usage.ReasoningOutputTokens != 1200 {
			t.Errorf("result usage = %+v", result.Usage)
		}
		if result.DurationMs != 42000 || result.DurationAPIMs != 30000 || result.ContextWindow != 200000 {
			t.Errorf("result timings/window = %d/%d/%d", result.DurationMs, result.DurationAPIMs, result.ContextWindow)
		}
	})
}
