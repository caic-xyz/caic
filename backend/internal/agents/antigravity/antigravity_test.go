// Tests agy command construction, prompt provenance, stream parsing, and model discovery.

package antigravity

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/maruel/gomode/mcp/mcptest"
)

func TestBackend(t *testing.T) {
	t.Parallel()
	b := New("", nil)
	t.Run("AgentArgs", func(t *testing.T) {
		t.Parallel()
		want := []string{"agy", "-p=", "--input-format", "stream-json", "--output-format", "stream-json", "--dangerously-skip-permissions", "--model", "gemini-3.8-flash-low", "--effort", "high", "--conversation", "session"}
		got := b.AgentArgs(agent.HarnessArgs{Model: "gemini-3.8-flash-low", Effort: "high", ResumeSessionID: "session"})
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("args = %q, want %q", got, want)
		}
	})
	t.Run("capabilities", func(t *testing.T) {
		t.Parallel()
		if b.Harness() != harness.Antigravity || b.SupportsImages() || b.SupportsCompact() || b.QuotaProvider() != "" {
			t.Fatal("unexpected backend capabilities")
		}
	})
	t.Run("Start", func(t *testing.T) {
		t.Parallel()
		if _, err := b.Start(t.Context(), &agent.Options{MCP: mcptest.FakeRegistry{}, Logger: slog.Default(), Dir: "/tmp"}); err == nil || !strings.Contains(err.Error(), "missing SSH host") {
			t.Fatalf("task MCP start target validation = %v", err)
		}
		// An empty target would fail relay launch. The image error must precede
		// any launch so rejection cannot leave an orphaned remote process.
		if _, err := b.Start(t.Context(), &agent.Options{InitialPrompt: agent.Prompt{Images: []v3.ImageData{{Data: "image"}}}}); err == nil || !strings.Contains(err.Error(), "image input is not supported") {
			t.Fatalf("initial image rejection = %v", err)
		}
	})
	t.Run("AttachRelay", func(t *testing.T) {
		t.Parallel()
		if _, err := b.AttachRelay(t.Context(), &agent.Options{MCP: mcptest.FakeRegistry{}}); err == nil || !strings.Contains(err.Error(), "missing SSH host") {
			t.Fatalf("task MCP attach target validation = %v", err)
		}
	})
	t.Run("FetchModelInventory", func(t *testing.T) {
		t.Parallel()
		if _, err := b.FetchModelInventory(t.Context(), agent.Options{}.Target, nil); err == nil {
			t.Fatal("empty SSH target must fail")
		}
	})
}

func TestWireFormat(t *testing.T) {
	t.Parallel()
	t.Run("WritePrompt", func(t *testing.T) {
		t.Parallel()
		for _, version := range []agent.LogVersion{agent.LogVersionV2, agent.LogVersionV3} {
			t.Run(fmt.Sprint(version), func(t *testing.T) {
				t.Parallel()
				w := New("", nil).NewWire()
				var out bytes.Buffer
				log := &agenttest.LogSink{Version: version}
				if err := w.WritePrompt(&out, agent.Prompt{Text: "hello\nworld"}, log); err != nil {
					t.Fatal(err)
				}
				want := `{"event":"user","message":{"content":[{"type":"text","text":"hello\nworld"}]}}` + "\n"
				if out.String() != want {
					t.Fatalf("input = %q", out.String())
				}
				msgs, err := w.ParseMessage(out.Bytes())
				if err != nil || len(msgs) != 1 {
					t.Fatalf("parse input = %v, %v", msgs, err)
				}
				if m, ok := msgs[0].(*agent.UserInputMessage); !ok || m.Text != "hello\nworld" {
					t.Fatalf("input = %+v", msgs[0])
				}
				if version == agent.LogVersionV3 {
					if !strings.Contains(log.String(), `"t":"input"`) || !strings.Contains(log.String(), `"event":"user"`) {
						t.Fatalf("missing input provenance: %s", log.String())
					}
				} else if log.Len() != 0 {
					t.Fatal("v2 prompt logging belongs to relay")
				}
			})
		}
		t.Run("errors", func(t *testing.T) {
			t.Parallel()
			w := New("", nil).NewWire()
			log := &agenttest.LogSink{Version: agent.LogVersionV3}
			var out bytes.Buffer
			if err := w.WritePrompt(&out, agent.Prompt{Images: []v3.ImageData{{Data: "image"}}}, log); err == nil || out.Len() != 0 {
				t.Fatal("images must fail without writing")
			}
			if err := w.WritePrompt(shortWriter{}, agent.Prompt{Text: "hello"}, log); !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("short write = %v", err)
			}
			if log.Len() != 0 {
				t.Fatal("failed write must not be logged")
			}
		})
	})
	t.Run("ParseMessage", func(t *testing.T) {
		t.Parallel()
		t.Run("recorded turn", func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile("testdata/turn.ndjson")
			if err != nil {
				t.Fatal(err)
			}
			w := New("", nil).NewWire()
			sc := bufio.NewScanner(bytes.NewReader(data))
			var text string
			var result *agent.ResultMessage
			for sc.Scan() {
				msgs, err := w.ParseMessage(sc.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				for _, msg := range msgs {
					switch m := msg.(type) {
					case *agent.InitMessage:
						if m.SessionID != "session" || m.ReportedModel != "gemini-3.8-flash-low" {
							t.Fatalf("init = %+v", m)
						}
					case *agent.TextDeltaMessage:
						text += m.Text
					case *agent.ResultMessage:
						result = m
					}
				}
			}
			if err := sc.Err(); err != nil {
				t.Fatal(err)
			}
			if text != "OK.\n" || result == nil || result.IsError || result.Result != text || result.Usage.InputTokens != 12260 || result.Usage.ReasoningOutputTokens != 38 {
				t.Fatalf("text = %q, result = %+v", text, result)
			}
		})
		t.Run("recorded cached usage and tool output", func(t *testing.T) {
			t.Parallel()
			// Session IDs and workspace paths are sanitized in this recording.
			// Retain native cached usage, command output, task
			// status, scheduling, streamed text, and conversation-wide result.
			f, err := os.Open("testdata/cached-tools.ndjson")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := f.Close(); err != nil {
					t.Error(err)
				}
			})
			w := New("", nil).NewWire()
			sc := bufio.NewScanner(f)
			uses := make(map[string]string)
			outputs := make(map[string]string)
			var calls []agent.Usage
			var result *agent.ResultMessage
			var text strings.Builder
			for sc.Scan() {
				msgs, err := w.ParseMessage(sc.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				for _, msg := range msgs {
					switch m := msg.(type) {
					case *agent.InitMessage:
						if m.ReportedModel != "" || m.SessionID != "session" {
							t.Fatalf("init = %+v, want unknown model", m)
						}
					case *agent.UsageMessage:
						if m.ReportedModel != "" {
							t.Fatalf("invented model: %q", m.ReportedModel)
						}
						calls = append(calls, m.Usage)
					case *agent.ToolUseMessage:
						uses[m.ToolUseID] = m.Name
					case *agent.ToolOutputDeltaMessage:
						if uses[m.ToolUseID] == "" {
							t.Fatalf("output before invocation: %+v", m)
						}
						outputs[m.ToolUseID] += m.Delta
					case *agent.ToolResultMessage:
						if m.Error != "" {
							t.Fatalf("output text invented a native error: %+v", m)
						}
						if outputs[m.ToolUseID] == "" {
							t.Fatalf("completion lost output: %+v", m)
						}
					case *agent.TextDeltaMessage:
						text.WriteString(m.Text)
					case *agent.ResultMessage:
						result = m
					}
				}
			}
			if err := sc.Err(); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 2 || calls[0].InputTokens != 3157 || calls[0].CacheReadInputTokens != 16275 {
				t.Fatalf("cached usage = %+v", calls)
			}
			if result == nil || result.IsError || result.Usage.InputTokens != 7897 || result.Usage.CacheReadInputTokens != 191461 || result.Usage.OutputTokens != 1127 || result.Usage.ReasoningOutputTokens != 452 {
				t.Fatalf("turn result = %+v", result)
			}
			if len(uses) != 4 || len(outputs) != 4 || !strings.Contains(outputs["session:12"], "undefined: typesafe.Questions") || !strings.Contains(outputs["session:160"], "Status: RUNNING") || !strings.Contains(outputs["session:162"], "Timer cancelled early") {
				t.Fatalf("tool output lost: uses=%v outputs=%v", uses, outputs)
			}
			if !strings.Contains(text.String(), "Updated `github.com/maruel/genai`") || !strings.Contains(text.String(), "make verify") {
				t.Fatalf("streamed text lost: %q", text.String())
			}
		})
		t.Run("turn usage and tool correlation", func(t *testing.T) {
			t.Parallel()
			w := New("", nil).NewWire()
			lines := []string{
				`{"event":"step_update","step_update":{"conversation_id":"s","step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"view_file","tool_info":{"parameters":{"path":"README.md"}}}}`,
				`{"event":"step_update","step_update":{"conversation_id":"s","step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"view_file"}}`,
				`{"event":"step_update","step_update":{"conversation_id":"s","step_index":2,"state":"DONE","step_type":"tool","duration_seconds":0.25,"tool_info":{"error":{"type":"error","message":"not found"}}}}`,
				`{"event":"step_update","step_update":{"conversation_id":"s","step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"answer","usage":{"input_tokens":100,"cache_read_tokens":60,"output_tokens":20,"thinking_tokens":10}}}`,
				`{"event":"step_update","step_update":{"conversation_id":"s","step_index":3,"state":"DONE","step_type":"agent_response","usage":{"input_tokens":100,"output_tokens":20}}}`,
				`{"event":"result","result":{"conversation_id":"s","status":"SUCCESS","usage":{"input_tokens":5000,"output_tokens":4000},"duration_seconds":30,"num_turns":5}}`,
				`{"event":"result","result":{"conversation_id":"s","status":"ERROR","error":"quota exhausted","usage":{"input_tokens":5000,"output_tokens":4000}}}`,
			}
			var uses []*agent.ToolUseMessage
			var results []*agent.ToolResultMessage
			var turns []*agent.ResultMessage
			for _, line := range lines {
				msgs, err := w.ParseMessage([]byte(line))
				if err != nil {
					t.Fatal(err)
				}
				for _, msg := range msgs {
					switch m := msg.(type) {
					case *agent.ToolUseMessage:
						uses = append(uses, m)
					case *agent.ToolResultMessage:
						results = append(results, m)
					case *agent.ResultMessage:
						turns = append(turns, m)
					}
				}
			}
			if len(uses) != 1 || len(results) != 1 || uses[0].ToolUseID != results[0].ToolUseID || results[0].DurationMs != 250 || results[0].Error != "not found" {
				t.Fatalf("tools = %+v, results = %+v", uses, results)
			}
			if len(turns) != 2 || turns[0].Usage.InputTokens != 100 || turns[0].Usage.CacheReadInputTokens != 60 || turns[0].Usage.OutputTokens != 20 || turns[0].DurationMs != 0 || turns[0].NumTurns != 0 {
				t.Fatalf("turns = %+v", turns)
			}
			if !turns[1].IsError || !strings.Contains(turns[1].Result, "quota exhausted") || turns[1].Usage != (agent.Usage{}) {
				t.Fatalf("second result = %+v", turns[1])
			}
		})
		t.Run("tool output completes once", func(t *testing.T) {
			t.Parallel()
			w := New("", nil).NewWire()
			line := []byte(`{"event":"step_update","step_update":{"conversation_id":"s","step_index":2,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"output":"build failed\n","error":{"type":"error","message":"command failed"}}}}`)
			msgs, err := w.ParseMessage(line)
			if err != nil || len(msgs) != 3 {
				t.Fatalf("completion = %v, %v", msgs, err)
			}
			use, ok := msgs[0].(*agent.ToolUseMessage)
			if !ok {
				t.Fatalf("first message = %T, want invocation", msgs[0])
			}
			out, ok := msgs[1].(*agent.ToolOutputDeltaMessage)
			if !ok || out.ToolUseID != use.ToolUseID || out.Delta != "build failed\n" {
				t.Fatalf("output = %+v", msgs[1])
			}
			result, ok := msgs[2].(*agent.ToolResultMessage)
			if !ok || result.ToolUseID != use.ToolUseID || result.Error != "command failed" {
				t.Fatalf("result = %+v", msgs[2])
			}
			if msgs, err := w.ParseMessage(line); err != nil || len(msgs) != 0 {
				t.Fatalf("duplicate completion = %v, %v", msgs, err)
			}
		})
		t.Run("response finalization preserves tool order", func(t *testing.T) {
			t.Parallel()
			w := New("", nil).NewWire()
			lines := []string{
				`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"Before."}}`,
				`{"event":"step_update","step_update":{"step_index":1,"state":"DONE","step_type":"agent_response","usage":{"input_tokens":10}}}`,
				`{"event":"step_update","step_update":{"step_index":2,"state":"DONE","step_type":"tool","tool_name":"view_file"}}`,
				`{"event":"step_update","step_update":{"step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"After."}}`,
				`{"event":"result","result":{"status":"SUCCESS","response":"Before.After."}}`,
			}
			var types, texts []string
			for _, line := range lines {
				msgs, err := w.ParseMessage([]byte(line))
				if err != nil {
					t.Fatal(err)
				}
				for _, msg := range msgs {
					types = append(types, msg.Type())
					if m, ok := msg.(*agent.TextMessage); ok {
						texts = append(texts, m.Text)
					}
				}
			}
			want := []string{"text_delta", "text", "usage", "tool_use", "tool_result", "text_delta", "text", "result"}
			if !reflect.DeepEqual(types, want) || !reflect.DeepEqual(texts, []string{"Before.", "After."}) {
				t.Fatalf("types=%q texts=%q", types, texts)
			}
		})
		t.Run("response fallback", func(t *testing.T) {
			t.Parallel()
			msgs, err := New("", nil).NewWire().ParseMessage([]byte(`{"event":"result","result":{"status":"SUCCESS","response":"answer"}}`))
			if err != nil || len(msgs) != 2 || msgs[0].Type() != "text" || msgs[1].Type() != "result" {
				t.Fatalf("fallback = %v, %v", msgs, err)
			}
		})
		t.Run("independent state", func(t *testing.T) {
			t.Parallel()
			b := New("", nil)
			first, second := b.NewWire(), b.NewWire()
			if _, err := first.ParseMessage([]byte(`{"event":"step_update","step_update":{"state":"DONE","step_type":"agent_response","usage":{"input_tokens":123}}}`)); err != nil {
				t.Fatal(err)
			}
			msgs, err := second.ParseMessage([]byte(`{"event":"result","result":{"status":"SUCCESS"}}`))
			if err != nil || len(msgs) != 1 {
				t.Fatalf("fresh result = %v, %v", msgs, err)
			}
			if m, ok := msgs[0].(*agent.ResultMessage); !ok || m.Usage != (agent.Usage{}) {
				t.Fatalf("fresh result = %v", msgs)
			}
		})
		t.Run("errors and forward fields", func(t *testing.T) {
			t.Parallel()
			w := New("", nil).NewWire()
			for _, line := range []string{`{`, `{"event":"user","message":{"content":[{"type":"image"}]}}`} {
				if _, err := w.ParseMessage([]byte(line)); err == nil {
					t.Fatalf("accepted %s", line)
				}
			}
			if _, err := w.ParseMessage([]byte(`{"event":"init","future":true}`)); err != nil {
				t.Fatal(err)
			}
		})
	})
}

func TestParseModels(t *testing.T) {
	t.Parallel()
	result := "\n" + `{"event":"result","result":{"status":"SUCCESS"}}`
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		out := `{"event":"command_result","command":{"name":"models","data":{"models":[{"id":"gemini-3.8-flash-low","label":"Flash"}]}}}` + result
		inv, err := parseModels([]byte(out))
		if err != nil || len(inv.Models) != 1 || inv.Models[0].ID != "gemini-3.8-flash-low" || len(inv.Models[0].EffortOptions) != 0 {
			t.Fatalf("models = %+v, %v", inv, err)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, out := range []string{
			`{`,
			`{"event":"result","result":{"status":"ERROR","error":"login required"}}`,
			`{"event":"result","result":{"status":"SUCCESS"}}`,
			`{"event":"command_result","command":{"name":"models","data":{"models":[]}}}`,
			`{"event":"command_result","command":{"name":"other"}}`,
			`{"event":"command_result","command":{"name":"models","data":{"models":[{}]}}}` + result,
			`{"event":"command_result","command":{"name":"models","data":{"models":"invalid"}}}`,
		} {
			if _, err := parseModels([]byte(out)); err == nil {
				t.Fatalf("accepted %s", out)
			}
		}
	})
}

func TestShellCommand(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "spaces and 'quotes'", "$(printf injected); $HOME `printf injected`\n*"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", shellCommand([]string{"env", "AGY_TEST=" + value, "/bin/sh", "-c", `printf '%s' "$AGY_TEST"`})) //nolint:gosec // Exercises shell quoting with fixed hostile test values.
			out, err := cmd.Output()
			if err != nil || string(out) != value {
				t.Fatalf("shell argv preservation = %q, %v; want %q", out, err, value)
			}
		})
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
