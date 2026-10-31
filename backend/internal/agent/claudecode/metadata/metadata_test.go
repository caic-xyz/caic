// Tests Claude token-accounting normalization shared by inventory and live parsing.

package metadata

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	genclaudecode "github.com/maruel/genai/providers/claudecode"
)

func TestUsage(t *testing.T) {
	t.Parallel()
	t.Run("inconsistent breakdown", func(t *testing.T) {
		t.Parallel()
		u := Usage(&genclaudecode.MsgUsage{CacheCreationInputTokens: 20, CacheCreation: genclaudecode.CacheCreation{Ephemeral5mInputTokens: 10, Ephemeral1hInputTokens: 100}})
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
			usage := Usage(&tt.in)
			if usage.CacheWriteBuckets().Count(time.Hour) != int64(tt.in.CacheCreation.Ephemeral1hInputTokens) {
				t.Fatalf("one-hour tokens lost: %+v", usage)
			}
			if got := usage.CacheTTLSeconds; got != tt.want {
				t.Errorf("CacheTTLSeconds = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParser(t *testing.T) {
	t.Parallel()
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
					var parser Parser
					if _, err := parser.Parse([]byte(tc.line)); err != nil {
						t.Fatal(err)
					}
					messages, err := parser.Parse([]byte(`{"type":"result","usage":{}}`))
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
					var parser Parser
					if _, err := parser.Parse([]byte(line)); err == nil {
						t.Fatal("invalid accounting accepted")
					}
				})
			}
		})
	})
}
