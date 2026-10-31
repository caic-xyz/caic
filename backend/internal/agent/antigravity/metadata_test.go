// Tests native Antigravity metadata and conversation-scoped accounting in both projections.

package antigravity

import (
	"bufio"
	"os"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestMetadataParser(t *testing.T) {
	t.Parallel()
	for _, projection := range []string{"full", "metadata"} {
		t.Run(projection, func(t *testing.T) {
			t.Parallel()
			newParser := func() agent.MessageParser {
				if projection == "metadata" {
					return New("", nil).NewMetadataParser()
				}
				return New("", nil).NewWire()
			}
			for _, tc := range []struct {
				file, model string
				usage       agent.Usage
			}{
				{"turn.ndjson", "gemini-3.8-flash-low", agent.Usage{InputTokens: 12260, OutputTokens: 40, ReasoningOutputTokens: 38}},
				{"cached-tools.ndjson", "", agent.Usage{InputTokens: 7897, OutputTokens: 1127, ReasoningOutputTokens: 452, CacheReadInputTokens: 191461}},
			} {
				t.Run("recorded "+tc.file, func(t *testing.T) {
					t.Parallel()
					p := newParser()
					f, err := os.Open("testdata/" + tc.file)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := f.Close(); err != nil {
							t.Error(err)
						}
					})
					var metadata agent.SessionMetadata
					var results []agent.Usage
					scanner := bufio.NewScanner(f)
					for scanner.Scan() {
						messages, err := p.ParseOutput(scanner.Bytes())
						if err != nil {
							t.Fatal(err)
						}
						for _, m := range messages {
							metadata.Apply(m)
							if r, ok := m.(*agent.ResultMessage); ok {
								results = append(results, r.Usage)
							}
						}
					}
					if err := scanner.Err(); err != nil {
						t.Fatal(err)
					}
					want := agent.SessionMetadata{SessionID: "session", ReportedModel: tc.model}
					if metadata != want {
						t.Fatalf("metadata = %+v, want %+v", metadata, want)
					}
					if len(results) != 1 || !results[0].Equal(tc.usage) {
						t.Fatalf("DONE usage = %+v", results)
					}
				})
			}
			t.Run("child ownership dedup and lifetime totals", func(t *testing.T) {
				t.Parallel()
				p := newParser()
				var results []agent.Usage
				var metadata agent.SessionMetadata
				for _, line := range []string{
					`{"event":"init","conversation_id":"root","init":{"model":"root-model"}}`,
					`{"event":"step_update","step_update":{"conversation_id":"root","step_index":1,"state":"DONE","subagent_info":{"subagents":[{"conversation_id":"child","initial_prompt":"large transcript ignored"}]}}}`,
					`{"event":"step_update","step_update":{"conversation_id":"child","step_index":2,"state":"DONE","usage":{"input_tokens":3,"cache_read_tokens":10}}}`,
					`{"event":"step_update","step_update":{"conversation_id":"child","step_index":2,"state":"DONE","usage":{"input_tokens":3,"cache_read_tokens":10}}}`,
					`{"event":"result","result":{"conversation_id":"child","status":"SUCCESS","usage":{"input_tokens":99999}}}`,
					`{"event":"step_update","step_update":{"conversation_id":"foreign","step_index":2,"state":"DONE","usage":{"input_tokens":99999}}}`,
					`{"event":"result","result":{"conversation_id":"foreign","status":"SUCCESS","usage":{"input_tokens":99999}}}`,
					`{"event":"step_update","step_update":{"conversation_id":"root","step_index":2,"state":"DONE","usage":{"input_tokens":5,"output_tokens":2}}}`,
					`{"event":"step_update","step_update":{"conversation_id":"root","step_index":2,"state":"DONE","usage":{"input_tokens":5,"output_tokens":2}}}`,
					`{"event":"result","result":{"conversation_id":"root","status":"SUCCESS","usage":{"input_tokens":99999},"num_turns":80,"duration_seconds":99999}}`,
					`{"event":"result","result":{"conversation_id":"root","status":"SUCCESS","usage":{"input_tokens":99999}}}`,
				} {
					messages, err := p.ParseOutput([]byte(line))
					if err != nil {
						t.Fatal(err)
					}
					for _, m := range messages {
						metadata.Apply(m)
						if r, ok := m.(*agent.ResultMessage); ok {
							results = append(results, r.Usage)
							if r.NumTurns != 0 || r.DurationMs != 0 {
								t.Fatalf("conversation totals became turn totals: %+v", r)
							}
						}
					}
				}
				if metadata != (agent.SessionMetadata{SessionID: "root", ReportedModel: "root-model"}) {
					t.Fatalf("root metadata = %+v", metadata)
				}
				if len(results) != 2 || !results[0].Equal(agent.Usage{InputTokens: 8, OutputTokens: 2, CacheReadInputTokens: 10}) || !results[1].Equal(agent.Usage{}) {
					t.Fatalf("turn results = %+v", results)
				}
			})
		})
	}
}
