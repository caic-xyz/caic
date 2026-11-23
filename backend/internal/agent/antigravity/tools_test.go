// Tests agy tool projection for terminal announcements, invoke_subagent cards settled by their prompt, and detached commands that wake agy.

package antigravity

import (
	"encoding/json"
	"fmt"
	"strings"
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
			w := New("", nil).NewWire()
			if _, err := w.ParseInput([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)); err != nil {
				t.Fatal(err)
			}
			msgs, err := w.ParseOutput(line)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 3 {
				t.Fatalf("msgs = %#v, want tool use, output, result", msgs)
			}
			for _, m := range msgs {
				if m.Provenance().ThreadID != "s" {
					t.Fatalf("%s source = %+v, want session s", m.Type(), m.Provenance())
				}
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
			msgs, err := p.Parse("", true, &acp.ToolCallUpdateUpdate{ToolCallID: id, Name: invokeSubagent, Status: status, RawInput: json.RawMessage(`{"Prompt":"joke","Role":"` + id + `","TypeName":"self"}`)}, nil)
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
				settled := p.SettlePrompt("", test.stop, &agent.ResultMessage{})
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
				if again := p.SettlePrompt("", test.stop, &agent.ResultMessage{}); len(again) != 0 {
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
			if settled := p.SettlePrompt("", acp.StopReasonEndTurn, &agent.ResultMessage{}); len(settled) != 0 {
				t.Fatalf("settled = %#v, want the failed card ended already", settled)
			}
		})
	})
	t.Run("DetachedCommand", func(t *testing.T) {
		t.Parallel()
		// Minimized from a task log: the model ends its turn while a command it
		// started runs past WaitMsBeforeAsync. When the command ends, agy runs
		// a model call that checks the result with a second command, and a
		// final call that reports in text. No prompt response follows.
		update := func(u string) string {
			return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":` + u + `}}`
		}
		command := func(id, kind, status, line, output string) string {
			u := `{"sessionUpdate":"` + kind + `","toolCallId":"` + id + `","name":"run_command","args":{"CommandLine":"` + line + `","WaitMsBeforeAsync":10000},"status":"` + status + `","kind":"execute","title":"` + line + `","rawInput":{"CommandLine":"` + line + `","WaitMsBeforeAsync":10000}`
			if output != "" {
				u += `,"rawOutput":{"output":"` + output + `"}`
			}
			return update(u + `}`)
		}
		chunk := func(text string) string {
			return update(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + text + `"}}`)
		}
		usage := update(`{"sessionUpdate":"usage_update","used":1,"size":10}`)
		w := New("", nil).NewWire()
		for i, step := range []struct {
			input bool
			line  string
			want  string
		}{
			{true, `{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"test it"}]}}`, "user_input jsonrpc_request"},
			{false, command("c1", "tool_call", "in_progress", "make test", ""), "tool_use"},
			{false, chunk("Testing; I will report back."), "text_delta"},
			{false, usage, "text usage"},
			{false, `{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}`, "background_command(running c1 wakes) result"},
			{false, command("c1", "tool_call_update", "completed", "make test", `Command exited with code 0.\n\nCommand output:\nok`), "turn_started tool_output_delta tool_result background_command(completed exit 0)"},
			{false, usage, "usage"},
			{false, command("c2", "tool_call", "in_progress", "git status", ""), "tool_use"},
			{false, command("c2", "tool_call_update", "completed", "git status", `Command exited with code 0.`), "tool_output_delta tool_result"},
			{false, chunk("Tests passed."), "text_delta"},
			{false, usage, "text usage turn_ended"},
		} {
			parse := w.ParseOutput
			if step.input {
				parse = w.ParseInput
			}
			msgs, err := parse([]byte(step.line))
			if err != nil {
				t.Fatal(err)
			}
			if got := summarize(msgs); got != step.want {
				t.Fatalf("record %d = %q, want %q", i, got, step.want)
			}
		}
	})
}

// summarize names each message's type, with a background command's lifecycle.
func summarize(msgs []agent.Message) string {
	names := make([]string, 0, len(msgs))
	for _, m := range msgs {
		name := m.Type()
		if c, ok := m.(*agent.BackgroundCommandMessage); ok {
			detail := string(c.Command.Status)
			if c.Command.ToolUseID != "" {
				detail += " " + c.Command.ToolUseID
			}
			if c.Command.WakesAgent {
				detail += " wakes"
			}
			if c.Command.ExitCode != nil {
				detail += fmt.Sprintf(" exit %d", *c.Command.ExitCode)
			}
			name += "(" + detail + ")"
		}
		names = append(names, name)
	}
	return strings.Join(names, " ")
}
