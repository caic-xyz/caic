// Tests agy tool projection for terminal announcements and invoke_subagent cards settled by their prompt.

package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/maruel/genai/acp"

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
	t.Run("SettlePrompt", func(t *testing.T) {
		t.Parallel()
		// Minimized from a task log: the parent spawns several children in one
		// turn, each acknowledged at once, and its prompt response ends them.
		spawn := func(p *tools, id string, status acp.ToolStatus) []agent.Message {
			t.Helper()
			msgs, err := p.Parse(&acp.ToolCallUpdateUpdate{ToolCallID: id, Name: invokeSubagent, Status: status, RawInput: json.RawMessage(`{"Prompt":"joke","Role":"` + id + `","TypeName":"self"}`)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			return msgs
		}
		for _, test := range []struct {
			stop acp.StopReason
			want agent.NativeSubagentStatus
		}{
			{acp.StopReasonEndTurn, agent.NativeSubagentStatusCompleted},
			{acp.StopReasonCancelled, agent.NativeSubagentStatusInterrupted},
			{acp.StopReasonMaxTokens, ""},
		} {
			t.Run(string(test.stop), func(t *testing.T) {
				t.Parallel()
				p := &tools{}
				for _, id := range []string{"a", "b"} {
					if msgs := spawn(p, id, acp.StatusInProgress); len(msgs) != 1 {
						t.Fatalf("announcement = %#v, want one card", msgs)
					}
					if msgs := spawn(p, id, acp.StatusCompleted); len(msgs) != 0 {
						t.Fatalf("spawn acknowledgment = %#v, want nothing", msgs)
					}
				}
				settled := p.SettlePrompt(test.stop)
				if len(settled) != 2 {
					t.Fatalf("settled = %#v, want both cards", settled)
				}
				for i, m := range settled {
					id := "antigravity:tool:" + []string{"a", "b"}[i]
					switch m := m.(type) {
					case *agent.NativeSubagentMessage:
						if m.Subagent.ID != id || m.Subagent.Status != test.want {
							t.Fatalf("settled[%d] = %+v, want %s %s", i, m.Subagent, id, test.want)
						}
					case *agent.BackgroundRetiredMessage:
						if test.want != "" || m.ID != id {
							t.Fatalf("settled[%d] = %+v, want %s", i, m, test.want)
						}
					default:
						t.Fatalf("settled[%d] = %#v", i, m)
					}
				}
				if again := p.SettlePrompt(test.stop); len(again) != 0 {
					t.Fatalf("next prompt settled %#v again", again)
				}
			})
		}
		t.Run("failed spawn", func(t *testing.T) {
			t.Parallel()
			p := &tools{}
			msgs := spawn(p, "a", acp.StatusFailed)
			if m, ok := msgs[0].(*agent.NativeSubagentMessage); len(msgs) != 1 || !ok || m.Subagent.Status != agent.NativeSubagentStatusFailed {
				t.Fatalf("failed spawn = %#v, want a failed card", msgs)
			}
			if settled := p.SettlePrompt(acp.StopReasonEndTurn); len(settled) != 0 {
				t.Fatalf("settled = %#v, want the failed card ended already", settled)
			}
		})
	})
}
