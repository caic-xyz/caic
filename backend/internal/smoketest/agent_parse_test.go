// Tests fake-agent result parsing preserves measured usage and timing fields.

package smoketest

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestParseMessage(t *testing.T) {
	t.Parallel()
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
