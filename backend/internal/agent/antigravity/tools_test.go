// Tests agy tool projection for announcements that already carry a terminal status.

package antigravity

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestTools(t *testing.T) {
	t.Parallel()
	t.Run("Parse", func(t *testing.T) {
		t.Parallel()
		t.Run("completed announcement", func(t *testing.T) {
			t.Parallel()
			line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c","status":"completed","kind":"read","title":"File a.txt","rawInput":{"Path":"/w/a.txt"},"rawOutput":{"output":"hi"}}}}`)
			msgs, err := New("", nil).NewWire().ParseOutput(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 3 {
				t.Fatalf("msgs = %#v, want tool use, output, result", msgs)
			}
			if m, ok := msgs[0].(*agent.ToolUseMessage); !ok || m.Name != "Read" || string(m.Input) != `{"filePath":"/w/a.txt"}` {
				t.Fatalf("msgs[0] = %#v, want Read of /w/a.txt", msgs[0])
			}
			if m, ok := msgs[1].(*agent.ToolOutputDeltaMessage); !ok || m.ToolUseID != "c" || m.Delta != "hi" {
				t.Fatalf("msgs[1] = %#v, want output hi", msgs[1])
			}
			if m, ok := msgs[2].(*agent.ToolResultMessage); !ok || m.ToolUseID != "c" || m.Error != "" {
				t.Fatalf("msgs[2] = %#v, want completed result", msgs[2])
			}
		})
	})
}
