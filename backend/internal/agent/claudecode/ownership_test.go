// Tests Claude delegation ownership, child tool pairing, controls, and isolated widget streams.

package claudecode

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestClaudeOwnership(t *testing.T) {
	t.Parallel()
	for _, version := range []agent.LogVersion{0, agent.LogVersionV2, agent.LogVersionV3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			t.Parallel()
			var history []string
			parse := ownershipParser(t, version, New().NewWire())
			feed := func(line string) []agent.Message {
				history = append(history, line)
				return parse(line)
			}
			feed(`{"type":"system","subtype":"init","session_id":"session"}`)
			prompt := feed(`{"type":"user","session_id":"session","parent_tool_use_id":"spawn","message":{"role":"user","content":[{"type":"text","text":"Research polygons"}]}}`)
			if len(prompt) != 1 {
				t.Fatalf("prompt = %#v", prompt)
			}
			if p, ok := prompt[0].(*agent.UserInputMessage); !ok || p.Text != "Research polygons" || p.Source.DelegationToolUseID != "spawn" || !p.Source.IsChild() || p.Source.HasIdentity() {
				t.Fatalf("child prompt = %#v", prompt[0])
			}
			result := feed(`{"type":"user","session_id":"session","parent_tool_use_id":"spawn","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"inner","content":"failed","is_error":true}]}}`)
			if len(result) != 1 {
				t.Fatalf("result = %#v", result)
			}
			if r, ok := result[0].(*agent.ToolResultMessage); !ok || r.ToolUseID != "inner" || r.Error != "failed" || r.Source.DelegationToolUseID != "spawn" {
				t.Fatalf("child result = %#v", result[0])
			}
			feed(`{"type":"system","subtype":"task_started","session_id":"session","task_type":"local_agent","task_id":"child","tool_use_id":"spawn"}`)
			for _, line := range []string{
				`{"type":"assistant","session_id":"session","parent_tool_use_id":"spawn","message":{"role":"assistant","content":[{"type":"thinking","thinking":"checking"},{"type":"text","text":"answer"},{"type":"tool_use","id":"inner","name":"Bash","input":{"command":"pwd"}}],"usage":{"input_tokens":3}}}`,
				`{"type":"stream_event","session_id":"session","parent_tool_use_id":"spawn","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fragment"}}}`,
				`{"type":"stream_event","session_id":"session","parent_tool_use_id":"spawn","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"fragment"}}}`,
				`{"type":"stream_event","session_id":"session","parent_tool_use_id":"spawn","event":{"type":"message_delta","usage":{"output_tokens":5}}}`,
				`{"type":"future_child_record","session_id":"session","parent_tool_use_id":"spawn"}`,
			} {
				messages := feed(line)
				if len(messages) == 0 {
					t.Fatalf("record emitted nothing: %s", line)
				}
				for _, m := range messages {
					s := m.Provenance()
					if s.ThreadID != "session" || s.DelegationToolUseID != "spawn" || s.AgentID != "claude:task:child" || !s.IsChild() || s.HasIdentity() {
						t.Fatalf("%T source = %+v", m, s)
					}
				}
			}

			// Adoption warms the same wire from retained records before reading
			// the live tail, without re-emitting that prefix to the task.
			warm := ownershipParser(t, agent.LogVersionV3, New().NewWire())
			for _, line := range history {
				warm(line)
			}
			messages := warm(`{"type":"user","session_id":"session","parent_tool_use_id":"spawn","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"inner","content":"ok"}]}}`)
			if len(messages) != 1 || messages[0].Provenance().AgentID != "claude:task:child" {
				t.Fatalf("warmed result = %#v", messages)
			}
		})
	}
}

func ownershipParser(t *testing.T, version agent.LogVersion, wire agent.WireFormat) func(string) []agent.Message {
	parser, err := agent.NewLogRecordParser(agent.LogVersionV3, wire.ParseMessage)
	if version != 0 {
		parser, err = agent.NewLogRecordParser(version, wire.ParseMessage)
	}
	if err != nil {
		t.Fatal(err)
	}
	return func(line string) []agent.Message {
		if version == 0 {
			messages, err := wire.ParseMessage([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			return messages
		}
		record, err := parser.ParseRecord([]byte(`{"t":"agent","ts":1.000,"msg":` + line + `}`))
		if err != nil {
			t.Fatal(err)
		}
		var messages []agent.Message
		for _, timed := range record.Messages {
			messages = append(messages, timed.Message)
		}
		return messages
	}
}

func TestClaudeRecordedChildPromptOwnership(t *testing.T) {
	t.Parallel()
	evidence := agenttest.LoadNativeSubagentEvidence(t, "testdata/evidence/native-subagent-joke-v3.jsonl", agent.LogVersionV3, func() agent.WireFormat { return New().NewWire() })
	for _, version := range []agent.LogVersion{0, agent.LogVersionV2, agent.LogVersionV3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			t.Parallel()
			feed := ownershipParser(t, version, New().NewWire())
			var prompts int
			for _, line := range evidence.Payloads {
				for _, message := range feed(string(line)) {
					if prompt, ok := message.(*agent.UserInputMessage); ok {
						prompts++
						if prompt.Source.AgentID != "claude:task:test-agent" || prompt.Source.DelegationToolUseID != "test-spawn" {
							t.Fatalf("recorded child prompt source = %+v", prompt.Source)
						}
					}
				}
			}
			if prompts != 1 {
				t.Fatalf("child prompts = %d", prompts)
			}
		})
	}
}

func TestClaudeWidgetOwnership(t *testing.T) {
	t.Parallel()
	feed := ownershipParser(t, 0, New().NewWire())
	stream := func(owner string, event string) []agent.Message {
		return feed(`{"type":"stream_event","session_id":"session","parent_tool_use_id":"` + owner + `","event":` + event + `}`)
	}
	for _, owner := range []string{"a", "b"} {
		stream(owner, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"widget-`+owner+`","name":"show_widget"}}`)
	}
	for _, owner := range []string{"a", "b"} {
		partial, err := json.Marshal(`{"widget_code":"<p>` + owner)
		if err != nil {
			t.Fatal(err)
		}
		messages := stream(owner, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(partial)+`}}`)
		if len(messages) != 1 {
			t.Fatalf("widget %s = %#v", owner, messages)
		}
		m, ok := messages[0].(*agent.WidgetDeltaMessage)
		if !ok || m.ToolUseID != "widget-"+owner || m.Delta != "<p>"+owner || m.Source.DelegationToolUseID != owner {
			t.Fatalf("widget = %#v", messages[0])
		}
	}
	stream("a", `{"type":"content_block_stop","index":0}`)
	messages := stream("b", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" survives"}}`)
	if len(messages) != 1 {
		t.Fatalf("other widget stopped: %#v", messages)
	}
	if widget, ok := messages[0].(*agent.WidgetDeltaMessage); !ok || widget.Delta != " survives" {
		t.Fatalf("other widget update = %#v", messages[0])
	}
}

func TestClaudeAskControlOwnership(t *testing.T) {
	t.Parallel()
	for _, version := range []agent.LogVersion{0, agent.LogVersionV2, agent.LogVersionV3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			t.Parallel()
			feed := ownershipParser(t, version, New().NewWire())
			feed(`{"type":"system","subtype":"init","session_id":"session"}`)
			feed(`{"type":"system","subtype":"task_started","session_id":"session","task_type":"local_agent","task_id":"child","tool_use_id":"spawn"}`)
			input := `{"questions":[{"question":"Proceed?","header":"Choice","options":[{"label":"Yes","description":"Go"},{"label":"No","description":"Stop"}]}]}`
			ask := feed(`{"type":"assistant","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":` + input + `}]}}`)
			pending := feed(`{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":` + input + `}}`)
			if len(ask) != 1 || len(pending) != 1 {
				t.Fatalf("ask=%#v pending=%#v", ask, pending)
			}
			if _, ok := pending[0].(*agent.PendingUserActionMessage); !ok {
				t.Fatalf("pending = %T", pending[0])
			}
			if *ask[0].Provenance() != *pending[0].Provenance() || pending[0].Provenance().DelegationToolUseID != "spawn" {
				t.Fatalf("ask=%+v pending=%+v", ask[0].Provenance(), pending[0].Provenance())
			}
		})
	}
}

func TestClaudeRootModelOwnership(t *testing.T) {
	t.Parallel()
	w := newWireFormat()
	feed := ownershipParser(t, 0, w)
	feed(`{"type":"system","subtype":"init","session_id":"session","model":"root-model"}`)
	feed(`{"type":"assistant","session_id":"session","parent_tool_use_id":"spawn","message":{"model":"child-model","content":[{"type":"text","text":"child answer"}],"usage":{"input_tokens":3}}}`)
	if w.reportedModel != "root-model" {
		t.Fatalf("child replaced root model: %q", w.reportedModel)
	}
	for _, delegation := range []string{"spawn", "unknown-child"} {
		messages := feed(`{"type":"stream_event","session_id":"session","parent_tool_use_id":"` + delegation + `","event":{"type":"message_delta","usage":{"output_tokens":5}}}`)
		if len(messages) != 1 {
			t.Fatalf("usage = %#v", messages)
		}
		usage, ok := messages[0].(*agent.UsageMessage)
		if !ok {
			t.Fatalf("usage = %T", messages[0])
		}
		if delegation == "spawn" && (usage.ReportedModel != "child-model" || !usage.ModelDerived) {
			t.Fatalf("known child model = %+v", usage)
		}
		if delegation == "unknown-child" && usage.ReportedModel != "" {
			t.Fatalf("guessed child model = %+v", usage)
		}
	}
	unknown := feed(`{"type":"future_record","message":"no ownership metadata"}`)
	if len(unknown) != 1 || !unknown[0].Provenance().IsZero() {
		t.Fatalf("invented unknown ownership = %#v", unknown)
	}
	legacy := feed(`{"type":"user","parent_tool_use_id":"direct-tool","message":{"content":"old error","is_error":true}}`)
	if len(legacy) != 1 {
		t.Fatalf("direct result = %#v", legacy)
	}
	if result, ok := legacy[0].(*agent.ToolResultMessage); !ok || result.Source.IsChild() || result.ToolUseID != "direct-tool" {
		t.Fatalf("direct result treated as delegation = %#v", legacy[0])
	}
}

func TestClaudeControlReportedAgent(t *testing.T) {
	t.Parallel()
	feed := ownershipParser(t, 0, New().NewWire())
	feed(`{"type":"system","subtype":"init","session_id":"session"}`)
	feed(`{"type":"system","subtype":"task_started","session_id":"session","task_type":"local_agent","task_id":"known-child"}`)
	input := `{"questions":[{"question":"Proceed?","header":"Choice","options":[{"label":"Yes","description":"Go"},{"label":"No","description":"Stop"}]}]}`
	// The task record omitted its delegating tool, but the actual Ask still
	// supplies it. Matching control ownership must retain both pieces of evidence.
	ask := feed(`{"type":"assistant","session_id":"session","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"correlated-ask","name":"AskUserQuestion","input":` + input + `}]}}`)
	pending := feed(`{"type":"control_request","request_id":"partial-owner","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"correlated-ask","agent_id":"known-child","input":` + input + `}}`)
	if len(ask) != 1 || len(pending) != 1 {
		t.Fatalf("ask=%#v pending=%#v", ask, pending)
	}
	if pending[0].Provenance().AgentID != "claude:task:known-child" || agent.ToolKeyForSource(ask[0].Provenance(), "correlated-ask") != agent.ToolKeyForSource(pending[0].Provenance(), "correlated-ask") {
		t.Fatalf("partial control ownership lost: %+v", pending[0].Provenance())
	}
	for _, id := range []string{"known-child", "unknown-child"} {
		messages := feed(`{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","agent_id":"` + id + `","input":` + input + `}}`)
		if len(messages) != 1 {
			t.Fatalf("control = %#v", messages)
		}
		source := messages[0].Provenance()
		if id == "known-child" && (source.AgentID != "claude:task:known-child" || !source.IsChild()) {
			t.Fatalf("known native owner lost: %+v", source)
		}
		if id == "unknown-child" && source.IsChild() {
			t.Fatalf("unknown native owner invented: %+v", source)
		}
		if source.DelegationToolUseID != "" || source.HasIdentity() {
			t.Fatalf("invented missing delegation/item: %+v", source)
		}
	}
}

func TestParseRecordHeader(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, line, thread, delegation string }{
			{"escaping", `{"type":"assistant","session_id":"sess\u0069on","parent_tool_use_id":"sp\u0061wn","extra":{"unknown":true}}`, "session", "spawn"},
			{"first fields", `{"type":"assistant","session_id":"first","session_id":"second","parent_tool_use_id":null,"parent_tool_use_id":42}`, "first", ""},
			{"missing metadata", `{"type":"assistant","extra":[1,{"arbitrary":false}]}`, "", ""},
			{"invalid utf8", "{\"type\":\"assistant\",\"session_id\":\"\xff\"}", "\ufffd", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				typ, source, err := parseRecordHeader([]byte(tc.line))
				if err != nil || typ != "assistant" || source.ThreadID != tc.thread || source.DelegationToolUseID != tc.delegation {
					t.Fatalf("header = %q %+v %v", typ, source, err)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, line := range []string{`{"type":12}`, `{"type":"assistant","session_id":[]}`, `{"type":"assistant","parent_tool_use_id":false}`} {
			if _, _, err := parseRecordHeader([]byte(line)); err == nil {
				t.Fatalf("invalid header accepted: %s", line)
			}
		}
	})
}

func TestClaudeReviewOwnershipRegressions(t *testing.T) {
	t.Parallel()
	for _, version := range []agent.LogVersion{0, agent.LogVersionV2, agent.LogVersionV3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			t.Parallel()
			t.Run("delegation namespaces", func(t *testing.T) {
				t.Parallel()
				feed := ownershipParser(t, version, New().NewWire())
				for _, session := range []string{"A", "B"} {
					background := "false"
					if session == "A" {
						background = "true"
					}
					feed(`{"type":"assistant","session_id":"` + session + `","message":{"content":[{"type":"tool_use","id":"spawn","name":"Agent","input":{"prompt":"prompt-` + session + `","run_in_background":` + background + `}}]}}`)
				}
				for _, session := range []string{"A", "B"} {
					messages := feed(`{"type":"system","subtype":"task_started","session_id":"` + session + `","task_type":"local_agent","task_id":"child-` + session + `","tool_use_id":"spawn"}`)
					for _, m := range messages {
						if native, ok := m.(*agent.NativeSubagentMessage); ok && native.Subagent.Prompt != "prompt-"+session {
							t.Errorf("session %s got other delegation metadata: %+v", session, native.Subagent)
						}
					}
				}
				for _, session := range []string{"A", "B"} {
					messages := feed(`{"type":"assistant","session_id":"` + session + `","parent_tool_use_id":"spawn","message":{"content":[{"type":"text","text":"answer"}]}}`)
					if len(messages) != 1 || messages[0].Provenance().AgentID != "claude:task:child-"+session {
						t.Errorf("session %s attached other agent: %#v", session, messages)
					}
				}
				messages := feed(`{"type":"user","session_id":"A","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"spawn","content":"async dispatch"}]}}`)
				for _, m := range messages {
					if native, ok := m.(*agent.NativeSubagentMessage); ok {
						t.Errorf("background A settled another lifecycle: %+v", native.Subagent)
					}
				}
				messages = feed(`{"type":"user","session_id":"B","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"spawn","content":"finished B"}]}}`)
				var completed bool
				for _, m := range messages {
					if native, ok := m.(*agent.NativeSubagentMessage); ok {
						completed = native.Subagent.ID == "claude:task:child-B" && native.Subagent.Status == agent.NativeSubagentStatusCompleted
					}
				}
				if !completed {
					t.Errorf("foreground B lost lifecycle: %#v", messages)
				}
			})
			t.Run("control matches reported child thread", func(t *testing.T) {
				t.Parallel()
				feed := ownershipParser(t, version, New().NewWire())
				feed(`{"type":"system","subtype":"init","session_id":"root"}`)
				feed(`{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"child","tool_use_id":"spawn"}`)
				input := `{"questions":[{"question":"Proceed?","header":"Choice","options":[{"label":"Yes","description":"Go"},{"label":"No","description":"Stop"}]}]}`
				ask := feed(`{"type":"assistant","session_id":"child-thread","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":` + input + `}]}}`)
				pending := feed(`{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","agent_id":"child","input":` + input + `}}`)
				if len(ask) != 1 || len(pending) != 1 {
					t.Fatalf("ask=%#v control=%#v", ask, pending)
				}
				if agent.ToolKeyForSource(ask[0].Provenance(), "ask") != agent.ToolKeyForSource(pending[0].Provenance(), "ask") {
					t.Fatalf("control fallback rejected actual child source: ask=%+v pending=%+v", ask[0].Provenance(), pending[0].Provenance())
				}
			})
			t.Run("ambiguous control remains informational", func(t *testing.T) {
				t.Parallel()
				for _, initialized := range []bool{false, true} {
					feed := ownershipParser(t, version, New().NewWire())
					if initialized {
						feed(`{"type":"system","subtype":"init","session_id":"root"}`)
					}
					input := `{"questions":[{"question":"Proceed?","header":"Choice","options":[{"label":"Yes","description":"Go"},{"label":"No","description":"Stop"}]}]}`
					feed(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":` + input + `}]}}`)
					feed(`{"type":"assistant","session_id":"child-thread","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":` + input + `}]}}`)
					line := `{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":` + input + `}}`
					messages := feed(line)
					if len(messages) != 1 {
						t.Fatalf("initialized=%t: control=%#v", initialized, messages)
					}
					raw, ok := messages[0].(*agent.RawMessage)
					if !ok || string(raw.Raw) != line || !raw.Source.IsZero() {
						t.Errorf("initialized=%t: ambiguous control became actionable or lost actual data: %#v", initialized, messages[0])
					}
				}
			})
			t.Run("unknown session does not borrow delegation", func(t *testing.T) {
				t.Parallel()
				feed := ownershipParser(t, version, New().NewWire())
				for _, session := range []string{"A", "B"} {
					feed(`{"type":"system","subtype":"task_started","session_id":"` + session + `","task_type":"local_agent","task_id":"child-` + session + `","tool_use_id":"spawn"}`)
				}
				messages := feed(`{"type":"assistant","parent_tool_use_id":"spawn","message":{"content":[{"type":"text","text":"unknown session"}]}}`)
				if len(messages) != 1 || messages[0].Provenance().AgentID != "" || messages[0].Provenance().ThreadID != "" {
					t.Fatalf("borrowed unknown session owner: %#v", messages)
				}
				for _, id := range []string{"unscoped-A", "unscoped-B"} {
					feed(`{"type":"system","subtype":"task_started","task_type":"local_agent","task_id":"` + id + `","tool_use_id":"ambiguous"}`)
				}
				messages = feed(`{"type":"assistant","parent_tool_use_id":"ambiguous","message":{"content":[{"type":"text","text":"ambiguous native owner"}]}}`)
				if len(messages) != 1 || messages[0].Provenance().AgentID != "" {
					t.Fatalf("guessed ambiguous native owner: %#v", messages)
				}
			})
			t.Run("deferred loads retain successful source", func(t *testing.T) {
				t.Parallel()
				feed := ownershipParser(t, version, New().NewWire())
				feed(`{"type":"assistant","session_id":"A","message":{"content":[{"type":"tool_use","id":"skill-1","name":"Skill","input":{"skill":"review"}},{"type":"tool_use","id":"skill-2","name":"Skill","input":{"skill":"quality"}}]}}`)
				messages := feed(`{"type":"user","session_id":"A","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"skill-1","content":"ok"},{"type":"tool_result","tool_use_id":"skill-2","content":"ok"}]}}`)
				messages = append(messages, feed(`{"type":"assistant","session_id":"B","message":{"content":[{"type":"text","text":"other session"}]}}`)...)
				var loads int
				for _, m := range messages {
					if load, ok := m.(*agent.SkillLoadMessage); ok {
						loads++
						if load.Source.ThreadID != "A" {
							t.Errorf("load lost success source: %+v", load)
						}
					}
				}
				if loads != 2 {
					t.Fatalf("successful loads=%d", loads)
				}
			})
		})
	}
}

func TestControlConnOwnership(t *testing.T) {
	t.Parallel()
	for _, version := range []agent.LogVersion{0, agent.LogVersionV2, agent.LogVersionV3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			t.Parallel()
			for _, kind := range []string{"ambiguous initialized", "ambiguous source-free", "child", "root"} {
				t.Run(kind, func(t *testing.T) {
					t.Parallel()
					feed := ownershipParser(t, version, New().NewWire())
					inner := &fakeConn{}
					if kind != "ambiguous source-free" {
						inner.messages = append(inner.messages, feed(`{"type":"system","subtype":"init","session_id":"root"}`)...)
					}
					input := `{"questions":[{"question":"Proceed?","header":"Choice","options":[{"label":"Yes","description":"Go"},{"label":"No","description":"Stop"}]}]}`
					if kind != "child" {
						inner.messages = append(inner.messages, feed(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":`+input+`}]}}`)...)
					}
					if kind != "root" {
						inner.messages = append(inner.messages, feed(`{"type":"assistant","session_id":"child-thread","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":`+input+`}]}}`)...)
					}
					owner := ""
					if kind == "child" {
						owner = `,"session_id":"child-thread","parent_tool_use_id":"spawn"`
					}
					control := feed(`{"type":"control_request"` + owner + `,"request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":` + input + `}}`)
					inner.messages = append(inner.messages, control...)
					conn := &controlConn{Conn: inner}
					messages := make(chan agent.TimedMessage, len(inner.messages))
					if err := conn.ReadMessages(io.NopCloser(strings.NewReader("")), messages, conn); err != nil {
						t.Fatal(err)
					}
					close(messages)
					var forwarded agent.Message
					for m := range messages {
						forwarded = m.Message
					}
					if len(control) != 1 || forwarded != control[0] {
						t.Fatalf("connection lost normalized control: %#v", forwarded)
					}
					if err := conn.SendPrompt(agent.Prompt{Text: "Yes"}, nil); err != nil {
						t.Fatal(err)
					}
					if kind == "root" {
						if len(inner.sent) != 1 || len(inner.prompts) != 0 || decodeControlResponse(t, inner.sent[0]).Response.RequestID != "permission" {
							t.Fatalf("root control not answered: raw=%q prompts=%#v", inner.sent, inner.prompts)
						}
					} else if len(inner.sent) != 0 || len(inner.prompts) != 1 || inner.prompts[0].Text != "Yes" {
						t.Fatalf("root prompt answered unresolved/child control: raw=%q prompts=%#v", inner.sent, inner.prompts)
					}
				})
			}
		})
	}
}
