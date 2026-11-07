// Tests Claude token-accounting normalization shared by inventory and live parsing.

package claudecode

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	genclaudecode "github.com/maruel/genai/providers/claudecode"
)

func TestNormalizeUsage(t *testing.T) {
	t.Parallel()
	t.Run("inconsistent breakdown", func(t *testing.T) {
		t.Parallel()
		u := normalizeUsage(&genclaudecode.MsgUsage{CacheCreationInputTokens: 20, CacheCreation: genclaudecode.CacheCreation{Ephemeral5mInputTokens: 10, Ephemeral1hInputTokens: 100}})
		if u.CacheWriteBuckets().Total() != 20 {
			t.Fatalf("write buckets exceed total: %+v", u)
		}
	})

	for _, tt := range []struct {
		name string
		in   genclaudecode.MsgUsage
		want int
	}{
		{name: "unknown", in: genclaudecode.MsgUsage{CacheReadInputTokens: 100}},
		{name: "five minute", in: genclaudecode.MsgUsage{CacheCreationInputTokens: 100, CacheCreation: genclaudecode.CacheCreation{Ephemeral5mInputTokens: 100}}, want: 300},
		{name: "one hour", in: genclaudecode.MsgUsage{CacheCreationInputTokens: 100, CacheCreation: genclaudecode.CacheCreation{Ephemeral1hInputTokens: 100}}, want: 3600},
		{name: "mixed uses first expiry", in: genclaudecode.MsgUsage{CacheCreationInputTokens: 110, CacheCreation: genclaudecode.CacheCreation{Ephemeral5mInputTokens: 10, Ephemeral1hInputTokens: 100}}, want: 300},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			usage := normalizeUsage(&tt.in)
			if usage.CacheWriteBuckets().Count(time.Hour) != int64(tt.in.CacheCreation.Ephemeral1hInputTokens) {
				t.Fatalf("one-hour tokens lost: %+v", usage)
			}
			if got := usage.CacheTTLSeconds; got != tt.want {
				t.Errorf("CacheTTLSeconds = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMetadataParser(t *testing.T) {
	t.Parallel()
	t.Run("ParseOutput", func(t *testing.T) {
		t.Parallel()
		t.Run("native zero and absent cost", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				native string
				known  bool
			}{{`{"type":"result","total_cost_usd":0}`, true}, {`{"type":"result"}`, false}, {`{"type":"result","total_cost_usd":null}`, false}} {
				result, err := parseResult([]byte(tc.native))
				if err != nil {
					t.Fatal(err)
				}
				if result.CostReported != tc.known {
					t.Fatalf("cost presence=%v, want %v", result.CostReported, tc.known)
				}
			}
		})

		t.Run("WidgetBufferRelease", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct{ name, end string }{
				{"block stop", `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`},
				{"cancelled result", `{"type":"result","subtype":"error","is_error":true}`},
				{"session reset", `{"type":"system","subtype":"init","session_id":"next"}`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					parser, ok := New().NewMetadataParser().(*metadataParser)
					if !ok {
						t.Fatal("wrong metadata parser type")
					}
					start := `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"widget","name":"show_widget"}}}`
					delta := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"widget_code\":\"<h1>Hi"}}}`
					if msgs, err := parser.ParseOutput([]byte(start)); err != nil || len(msgs) != 0 {
						t.Fatalf("start = %+v, %v", msgs, err)
					}
					msgs, err := parser.ParseOutput([]byte(delta))
					if err != nil || len(msgs) != 1 {
						t.Fatalf("delta = %+v, %v", msgs, err)
					}
					if msg, ok := msgs[0].(*agent.TextDeltaMessage); !ok || msg.Text != "" {
						t.Fatalf("metadata retained payload: %+v", msgs[0])
					}
					if parser.widgets == nil || parser.widgets.bufferedJSONBytes == 0 {
						t.Fatal("missing partial native widget state")
					}
					if _, err := parser.ParseOutput([]byte(tc.end)); err != nil {
						t.Fatal(err)
					}
					if parser.widgets != nil {
						t.Fatal("widget buffers survive native end boundary")
					}
					if msgs, err := parser.ParseOutput([]byte(delta)); err != nil || len(msgs) != 0 {
						t.Fatalf("stale delta = %+v, %v", msgs, err)
					}
				})
			}
		})
	})
	t.Run("Parse", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name string
				line string
				want int
			}{
				{"null estimate", `{"type":"system","subtype":"thinking_tokens","estimated_tokens_delta":null}`, 0},
				{"missing estimate", `{"type":"system","subtype":"thinking_tokens"}`, 0},
				{"null usage", `{"type":"assistant","message":{"usage":null}}`, 0},
				{"assistant thinking without published usage", `{"type":"assistant","message":{"usage":{"output_tokens_details":{"thinking_tokens":50}}}}`, 0},
				{"assistant borrowed usage", `{"type":"assistant","message":{"usage":{"cache_read_input_tokens":1,"output_tokens_details":{"thinking_tokens":9},"iterations":[{"output_tokens":100}]}}}`, 9},
				{"stream borrowed usage", `{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens_details":{"thinking_tokens":7}}}}`, 7},
				{"escaped scalar names", `{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens_details":{"thinking\u005ftokens":8}}}}`, 8},
				{"escaped discriminators", `{"type":"stream\u005fevent","event":{"type":"message\u005fdelta","usage":{"output_tokens_details":{"thinking_tokens":4}}}}`, 4},
				{"repeated scalars and null", `{"type":"assistant","message":{"usage":{"input_tokens":0,"input_tokens":1,"input_tokens":null,"output_tokens_details":{"thinking_tokens":1,"thinking_tokens":6,"thinking_tokens":null}}}}`, 6},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					var parser metadataParser
					if _, err := parser.ParseOutput([]byte(tc.line)); err != nil {
						t.Fatal(err)
					}
					messages, err := parser.ParseOutput([]byte(`{"type":"result","usage":{}}`))
					if err != nil {
						t.Fatal(err)
					}
					if len(messages) != 1 {
						t.Fatalf("messages = %v, want one result", messages)
					}
					result, ok := messages[0].(*agent.ResultMessage)
					if !ok {
						t.Fatalf("message = %T, want completed result", messages[0])
					}
					if got := result.Usage.ReasoningOutputTokens; got != tc.want {
						t.Errorf("reasoning = %d, want %d", got, tc.want)
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, line := range []string{
				`{"type":3}`,
				`{"type":"stream_event","event":{"type":3}}`,
				`{"type":"system","subtype":3}`,
				`{"type":"system","subtype":"thinking_tokens","estimated_tokens_delta":"bad"}`,
				`{"type":"system","subtype":"thinking_tokens","estimated_tokens_delta":1.5}`,
				`{"type":"assistant","message":{"usage":{"input_tokens":"bad"}}}`,
				`{"type":"assistant","message":{"usage":[]}}`,
				`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens_details":{"thinking_tokens":"5"}}}}`,
				`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens_details":{"thinking_tokens":9223372036854775808}}}}`,
				`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens_details":[]}}}`,
			} {
				t.Run(line, func(t *testing.T) {
					t.Parallel()
					var parser metadataParser
					if _, err := parser.ParseOutput([]byte(line)); err == nil {
						t.Fatal("invalid accounting accepted")
					}
				})
			}
		})
	})
	t.Run("ordered models and child accounting", func(t *testing.T) {
		t.Parallel()
		for _, projection := range []string{"full", "metadata"} {
			t.Run(projection, func(t *testing.T) {
				t.Parallel()
				var parser agent.MessageParser = New().NewWire()
				if projection == "metadata" {
					parser = New().NewMetadataParser()
				}
				var metadata agent.SessionMetadata
				var results []*agent.ResultMessage
				var call *agent.UsageMessage
				for _, line := range []string{
					`{"type":"system","subtype":"init","session_id":"root","model":"claude-sonnet-4-6","effort":"high","claude_code_version":"2.1.0"}`,
					`{"type":"assistant","parent_tool_use_id":"child","message":{"model":"child-model","content":[],"usage":{"input_tokens":99,"output_tokens":1,"output_tokens_details":{"thinking_tokens":500}}}}`,
					`{"type":"assistant","message":{"model":"claude-opus-4-6","content":[],"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":5},"output_tokens_details":{"thinking_tokens":7}}}}`,
					`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":1,"output_tokens_details":{"thinking_tokens":3}}}}`,
					`{"type":"result","parent_tool_use_id":"child","usage":{"output_tokens_details":{"thinking_tokens":900}}}`,
					`{"type":"result","usage":{"input_tokens":100,"output_tokens":20},"modelUsage":{"claude-opus-4-6":{"contextWindow":200000}}}`,
					`{"type":"system","subtype":"init","effort":"medium"}`,
				} {
					messages, err := parser.ParseOutput([]byte(line))
					if err != nil {
						t.Fatal(err)
					}
					for _, m := range messages {
						metadata.Apply(m)
						if usage, ok := m.(*agent.UsageMessage); ok && !usage.ModelDerived && !usage.Source.IsChild() {
							call = usage
						}
						if result, ok := m.(*agent.ResultMessage); ok && !result.Source.IsChild() {
							results = append(results, result)
						}
					}
				}
				want := agent.SessionMetadata{SessionID: "root", ReportedModel: "claude-opus-4-6", ReportedEffort: "medium", AgentVersion: "2.1.0"}
				if metadata != want {
					t.Fatalf("metadata = %+v, want %+v", metadata, want)
				}
				if len(results) != 1 || results[0].Usage.ReasoningOutputTokens != 10 || results[0].ContextWindow != 200000 {
					t.Fatalf("root reasoning/window = %+v", results)
				}
				if call == nil || call.ReportedModel != "claude-opus-4-6" || call.Usage.CacheTTLSeconds != 300 || call.Usage.CacheWriteBuckets().Count(5*time.Minute) != 5 {
					t.Fatalf("native call accounting = %+v", call)
				}
			})
		}
	})
}
