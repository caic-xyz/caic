// Tests native DeepSeek ACP configuration, history reconstruction, permissions, and model discovery.

package dsh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/maruel/genai/acp"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agentacp "github.com/caic-xyz/caic/backend/internal/agent/acp"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

// The retained handshake comes from genai providers/dsh's native
// GenSync_session_turn1.ndjson recording, through the third config response.
func dshHandshakeFixture(t *testing.T) string {
	data, err := os.ReadFile("testdata/dsh-handshake.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("RecordHandshake", func(t *testing.T) {
		t.Parallel()
		for _, resume := range []string{"", "144d4205-70bc-4389-b9c6-5ee98d26a34a"} {
			t.Run("resume="+resume, func(t *testing.T) {
				t.Parallel()
				b := New("", nil)
				wire := b.NewWire()
				log := &agenttest.LogSink{}
				handler := &agenttest.RecordHandler{}
				records, err := agent.NewRecords(t.Context(), wire, log, handler)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := records.Close(); err != nil {
						t.Error(err)
					}
				})
				var stdin bytes.Buffer
				_, err = b.RecordHandshake(t.Context(), &stdin, io.NopCloser(strings.NewReader(agenttest.ACPRecords(dshHandshakeFixture(t)))), &agent.Options{
					Dir: "/workspace", Model: "deepseek-official/deepseek-v4-flash", Effort: "high", ResumeSessionID: resume, Records: records,
				})
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(stdin.String()), "\n")
				if len(lines) != 3 {
					t.Fatalf("requests = %s", stdin.String())
				}
				var session, effort acp.JSONRPCRequest
				if err := json.Unmarshal([]byte(lines[1]), &session); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(lines[2]), &effort); err != nil {
					t.Fatal(err)
				}
				want := acp.MethodSessionNew
				if resume != "" {
					want = acp.MethodSessionResume
				}
				if session.Method != want {
					t.Fatalf("session method = %s, want %s", session.Method, want)
				}
				var selected acp.SetSessionConfigOptionParams
				if err := json.Unmarshal(effort.Params, &selected); err != nil {
					t.Fatal(err)
				}
				if selected.ConfigID != "reasoning_effort" || selected.Value != "high" {
					t.Fatalf("effort command = %+v", selected)
				}
				var found bool
				for _, event := range handler.Events() {
					if init, ok := event.Message.(*agent.InitMessage); ok {
						found = true
						if init.ReportedModel != "deepseek-official/deepseek-v4-flash" || init.ReportedEffort != "high" || init.Version != "0.0.1" {
							t.Fatalf("init = %+v", init)
						}
					}
				}
				if !found {
					t.Fatal("no session init")
				}
				prompt, err := wire.EncodePrompt(agent.Prompt{Text: "second turn"})
				if err != nil {
					t.Fatal(err)
				}
				var command acp.JSONRPCRequest
				if err := json.Unmarshal(prompt, &command); err != nil {
					t.Fatal(err)
				}
				var params acp.SessionPromptParams
				if err := json.Unmarshal(command.Params, &params); err != nil {
					t.Fatal(err)
				}
				if command.ID != 4 || params.SessionID != "144d4205-70bc-4389-b9c6-5ee98d26a34a" || len(params.Prompt) != 1 || params.Prompt[0].Text != "second turn" {
					t.Fatalf("prompt = %s", prompt)
				}
				metadata := b.NewMetadataParser()
				parser, err := agent.NewLogRecordParser(metadata)
				if err != nil {
					t.Fatal(err)
				}
				var init *agent.InitMessage
				for line := range bytes.SplitSeq(bytes.TrimSpace(log.Bytes()), []byte{'\n'}) {
					record, err := parser.ParseRecord(line)
					if err != nil {
						t.Fatal(err)
					}
					for _, m := range record.Messages {
						if v, ok := m.Message.(*agent.InitMessage); ok {
							init = v
						}
					}
				}
				if init == nil || init.ReportedModel != "deepseek-official/deepseek-v4-flash" || init.ReportedEffort != "high" {
					t.Fatalf("metadata replay init = %+v", init)
				}
			})
		}
		t.Run("Cancellation", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			r, w := io.Pipe()
			t.Cleanup(func() { _ = w.Close() })
			records, err := agent.NewRecords(t.Context(), New("", nil).NewWire(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = records.Close() })
			_, err = New("", nil).RecordHandshake(ctx, io.Discard, r, &agent.Options{Dir: "/workspace", Records: records})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
		})
	})

	t.Run("NativeTaskDoesNotInferOpenCodeDelegation", func(t *testing.T) {
		t.Parallel()
		wire := New("", nil).NewWire()
		messages, err := wire.ParseOutput([]byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"task1","title":"task","kind":"think","status":"in_progress","rawInput":{"prompt":"p","description":"d","subagent_type":"general"}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 {
			t.Fatalf("messages = %#v", messages)
		}
		if _, ok := messages[0].(*agent.ToolUseMessage); !ok {
			t.Fatalf("message = %T", messages[0])
		}
	})

	t.Run("UsageDoesNotBillContextOccupancy", func(t *testing.T) {
		t.Parallel()
		for _, parser := range []agent.MessageParser{New("", nil).NewWire(), New("", nil).NewMetadataParser()} {
			messages, err := parser.ParseOutput([]byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":1234,"size":1000000}}}`))
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 {
				t.Fatalf("messages = %#v", messages)
			}
			usage, ok := messages[0].(*agent.UsageMessage)
			if !ok || usage.ContextWindow != 1000000 || !usage.Usage.Equal(agent.Usage{}) || usage.Additive || usage.PerCall {
				t.Fatalf("usage = %+v", messages[0])
			}
		}
	})
}

func TestDiscoverDSHModels(t *testing.T) {
	t.Parallel()
	var stdin bytes.Buffer
	fixture := strings.Split(strings.TrimSpace(dshHandshakeFixture(t)), "\n")
	// Each model selection reports its own effort catalog; keep this test's
	// synthetic replies small while retaining native grouped route values.
	fixture[1] = `{"id":2,"result":{"sessionId":"s","configOptions":[{"id":"model","options":[{"group":"DeepSeek","options":[{"value":"[\"deepseek-official\",\"deepseek-v4-flash\"]"},{"value":"[\"deepseek-official\",\"deepseek-v4-pro\"]"}]}]}]}}`
	fixture[2] = `{"id":3,"result":{"configOptions":[{"id":"reasoning_effort","options":[{"value":"off"},{"value":"high"},{"value":""}]}]}}`
	fixture = append(fixture, `{"id":4,"result":{"configOptions":[{"id":"reasoning_effort","options":[{"value":"max"}]}]}}`)
	inventory, err := discoverDSHModels(&stdin, strings.NewReader(strings.Join(fixture, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Models) != 2 {
		t.Fatalf("inventory = %+v", inventory)
	}
	byID := map[string][]string{}
	for _, m := range inventory.Models {
		byID[m.ID] = m.EffortOptions
	}
	if !slices.Equal(byID["deepseek-official/deepseek-v4-flash"], []string{"off", "high"}) || !slices.Equal(byID["deepseek-official/deepseek-v4-pro"], []string{"max"}) {
		t.Fatalf("model efforts = %#v", byID)
	}
	sc := bufio.NewScanner(&stdin)
	var selections []string
	for sc.Scan() {
		var request acp.JSONRPCRequest
		if err := json.Unmarshal(sc.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		if request.Method == acp.MethodSessionSetConfigOption {
			var params acp.SetSessionConfigOptionParams
			if err := json.Unmarshal(request.Params, &params); err != nil {
				t.Fatal(err)
			}
			selections = append(selections, params.Value)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selections, []string{`["deepseek-official","deepseek-v4-flash"]`, `["deepseek-official","deepseek-v4-pro"]`}) {
		t.Fatalf("model selections = %v", selections)
	}
}

func TestDSHModelSelection(t *testing.T) {
	t.Parallel()
	b := New("", nil)
	wire := b.NewWire()
	records, err := agent.NewRecords(t.Context(), wire, &agenttest.LogSink{}, &agenttest.RecordHandler{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := records.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture := strings.Split(strings.TrimSpace(dshHandshakeFixture(t)), "\n")
	fixture[2] = strings.ReplaceAll(fixture[2], "deepseek-v4-flash", "deepseek-v4-pro")
	// A native permission may arrive during session creation or restoration.
	permission := `{"jsonrpc":"2.0","id":"permission-1","method":"session/request_permission","params":{"options":[{"kind":"allow_once","optionId":"approve-native"}]}}`
	fixture = append(fixture[:1], append([]string{permission}, fixture[1:]...)...)
	var stdin bytes.Buffer
	_, err = b.RecordHandshake(t.Context(), &stdin, io.NopCloser(strings.NewReader(agenttest.ACPRecords(strings.Join(fixture, "\n")))), &agent.Options{
		Dir: "/workspace", Model: "deepseek-official/deepseek-v4-pro", Records: records,
	})
	if err != nil {
		t.Fatal(err)
	}
	var selection acp.SetSessionConfigOptionParams
	var approved bool
	for line := range strings.SplitSeq(strings.TrimSpace(stdin.String()), "\n") {
		var message acp.JSONRPCMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatal(err)
		}
		if string(message.ID) == `"permission-1"` {
			var result acp.PermissionResponseResult
			if err := json.Unmarshal(message.Result, &result); err != nil {
				t.Fatal(err)
			}
			approved = result.Outcome.OptionID == "approve-native"
		}
		if message.Method == acp.MethodSessionSetConfigOption {
			if err := json.Unmarshal(message.Params, &selection); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !approved || selection.ConfigID != "model" || selection.Value != `["deepseek-official","deepseek-v4-pro"]` {
		t.Fatalf("startup commands = %s", stdin.String())
	}
}

func TestDSHModelRouteErrors(t *testing.T) {
	t.Parallel()
	for _, route := range []string{`not json`, `["one"]`, `["","model"]`, `["provider/invalid","model"]`, `["provider","model","extra"]`} {
		if _, err := decodeDSHModel(route); err == nil {
			t.Fatalf("accepted route %q", route)
		}
	}
	for _, id := range []string{"", "model", "/model", "provider/"} {
		if _, err := encodeDSHModel(id); err == nil {
			t.Fatalf("accepted model ID %q", id)
		}
	}
}

func TestDSHCompletedToolOutput(t *testing.T) {
	t.Parallel()
	// Native genai dsh GenStream-Tools-SquareRoot-1 records output only at completion.
	data, err := os.ReadFile("testdata/dsh-tool-completed.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := New("", nil).NewWire().ParseOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("completed tool messages = %#v, want output followed by completion", msgs)
	}
	output, ok := msgs[0].(*agent.ToolOutputDeltaMessage)
	if !ok || output.Delta != "363.88597115030416\n[stderr]\nlandlock-run: partial enforcement (older Landlock ABI)\n" {
		t.Fatalf("completed tool output = %#v", msgs[0])
	}
	result, ok := msgs[1].(*agent.ToolResultMessage)
	if !ok || result.ToolUseID != output.ToolUseID || result.Error != "" {
		t.Fatalf("completed tool result = %#v", msgs[1])
	}
}

func TestSkillToolRead(t *testing.T) {
	t.Parallel()
	wire := New("", nil).NewWire()
	line := []byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"skill-call","title":"skill","kind":"other","status":"in_progress","rawInput":{"name":"code-quality"}}}}`)
	msgs, err := wire.ParseOutput(line)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("native skill messages=%#v, want use/read/load", msgs)
	}
	if use, ok := msgs[0].(*agent.ToolUseMessage); !ok || use.Name != "skill" {
		t.Fatalf("skill tool use=%#v", msgs[0])
	}
	if read, ok := msgs[1].(*agent.SkillReadMessage); !ok || read.Skill != "code-quality" || read.ToolUseID != "skill-call" || read.Inferred {
		t.Fatalf("skill read=%#v", msgs[1])
	}
	if load, ok := msgs[2].(*agent.SkillLoadMessage); !ok || load.Skill != "code-quality" || load.SourceToolUseID != "skill-call" {
		t.Fatalf("skill load=%#v", msgs[2])
	}
}

func TestSubagentTool(t *testing.T) {
	t.Parallel()
	wire := New("", nil).NewWire()
	initial := []byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"subagent-call","title":"subagent","kind":"other","status":"in_progress","rawInput":{"description":"Silly duck task","cwd":"/tmp","prompt":"act as a rubber duck"}}}}`)
	msgs, err := wire.ParseOutput(initial)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("subagent spawn messages=%#v, want generic tool call suppressed", msgs)
	}
	completed := []byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"subagent-call","status":"completed","content":[{"type":"content","content":{"type":"text","text":"started subagent 77c5125c-db9e-4bb0-8bfe-97ac042d25c5"}}]}}}`)
	msgs, err = wire.ParseOutput(completed)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("subagent completion messages=%#v, want one native card", msgs)
	}
	m, ok := msgs[0].(*agent.NativeSubagentMessage)
	if !ok {
		t.Fatalf("subagent completion=%T, want *agent.NativeSubagentMessage", msgs[0])
	}
	got := m.Subagent
	if got.ID != "77c5125c-db9e-4bb0-8bfe-97ac042d25c5" || got.ToolUseID != "subagent-call" || got.Label != "Silly duck task" || got.Prompt != "act as a rubber duck" || got.Status != agent.NativeSubagentStatusUnknown || !got.Background || got.Scope != agent.NativeSubagentScopeAgent {
		t.Fatalf("native subagent=%+v", got)
	}
}

// TestDSHNativeRecordings checks the complete production-relay journals, including
// directional request ownership and terminal-only tool output, without a CLI or key.
func TestDSHNativeRecordings(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"read-edit-bash", "session-resume", "session-turns", "tool-error"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", name+".jsonl")))
			if err != nil {
				t.Fatal(err)
			}
			parser, err := agent.NewLogRecordParser(New("", nil).NewWire())
			if err != nil {
				t.Fatal(err)
			}
			var inputs, text []string
			var metadata agent.SessionMetadata
			var results, failedTools, readOutputs, commandOutputs, edits, skillReads, skillLoads int
			for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
				record, err := parser.ParseRecord(line)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range record.Messages {
					metadata.Apply(event.Message)
					switch m := event.Message.(type) {
					case *agent.UserInputMessage:
						inputs = append(inputs, m.Text)
					case *agent.TextDeltaMessage:
						text = append(text, m.Text)
					case *agent.ResultMessage:
						results++
					case *agent.ToolResultMessage:
						if m.Error != "" {
							failedTools++
							if !strings.Contains(m.Error, "nonexistent_file.txt") {
								t.Fatalf("unexpected tool error %q", m.Error)
							}
						}
					case *agent.ToolOutputDeltaMessage:
						if strings.Contains(m.Delta, "Hello, World!") {
							readOutputs++
						}
						if strings.Contains(m.Delta, "Hi, World!") {
							commandOutputs++
						}
					case *agent.ToolUseMessage:
						if m.Name == "Edit" {
							edits++
						}
					case *agent.SkillReadMessage:
						if m.Skill == "code-quality" {
							skillReads++
						}
					case *agent.SkillLoadMessage:
						if m.Skill == "code-quality" {
							skillLoads++
						}
					case *agent.UsageMessage:
						if !m.Usage.Equal(agent.Usage{}) || m.PerCall || m.Additive || m.ContextWindow != 1000000 {
							t.Fatalf("occupancy became billed usage: %+v", m)
						}
					}
				}
			}
			wantResults := 1
			wantEffort := "high"
			if name == "session-turns" {
				wantResults = 2
				wantEffort = "off"
				if strings.Join(text, "") != "FIRST-TURNcobalt-otter" {
					t.Fatalf("turn answers = %q", text)
				}
			}
			if name == "session-resume" {
				wantEffort = "low"
				if metadata.SessionID != "bf93755b-3901-4cdb-bab4-069387853d86" || strings.Join(text, "") != "cobalt-otter" {
					t.Fatalf("resumed metadata = %+v, answer = %q", metadata, text)
				}
			}
			if results != wantResults || len(inputs) != wantResults || metadata.ReportedModel != "deepseek-official/deepseek-v4-flash" || metadata.ReportedEffort != wantEffort {
				t.Fatalf("results=%d inputs=%q metadata=%+v", results, inputs, metadata)
			}
			if name == "read-edit-bash" && (readOutputs != 1 || commandOutputs != 1 || edits != 1 || skillReads != 1 || skillLoads != 1) {
				t.Fatalf("read outputs=%d command outputs=%d edits=%d skill reads=%d loads=%d", readOutputs, commandOutputs, edits, skillReads, skillLoads)
			}
			if name == "tool-error" && failedTools != 1 {
				t.Fatalf("failed tools = %d", failedTools)
			}
		})
	}
}

func TestDSHNativePermissionRecording(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/native-permission.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var request *acp.JSONRPCMessage
	var response *acp.JSONRPCMessage
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
		var frame struct {
			Type    string             `json:"t"`
			Message acp.JSONRPCMessage `json:"msg"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Type == "agent" && frame.Message.Method == acp.MethodSessionRequestPermission {
			if request != nil {
				t.Fatal("duplicate permission request")
			}
			request = &frame.Message
		}
		if frame.Type == "input" && len(frame.Message.Result) != 0 {
			if request == nil {
				t.Fatal("permission reply preceded its request")
			}
			response = &frame.Message
		}
	}
	if request == nil || response == nil || string(request.ID) != "0" || string(response.ID) != "0" {
		t.Fatalf("native permission correlation: request=%+v response=%+v", request, response)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := New("", nil).HandleMessage(&agent.RawMessage{MessageType: string(acp.MethodSessionRequestPermission), Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	var replay acp.JSONRPCMessage
	if err := json.Unmarshal(got, &replay); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replay.Result, response.Result) {
		t.Fatalf("permission replay = %s, recorded = %s", replay.Result, response.Result)
	}
}

func TestDSHNativeModelCatalog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ file, model, effort string }{
		{"model-flash", "deepseek-official/deepseek-flash", "low"},
		{"model-pro", "deepseek-official/deepseek-v4-pro", "max"},
		{"session-turns", "deepseek-official/deepseek-v4-flash", "off"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile("testdata/" + tc.file + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			parser, err := agent.NewLogRecordParser(New("", nil).NewMetadataParser())
			if err != nil {
				t.Fatal(err)
			}
			var metadata agent.SessionMetadata
			selections := map[string]acp.SetSessionConfigOptionParams{}
			var modelSelections, configurations int
			for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
				record, err := parser.ParseRecord(line)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range record.Messages {
					metadata.Apply(event.Message)
				}
				var frame struct {
					Type    string             `json:"t"`
					Message acp.JSONRPCMessage `json:"msg"`
				}
				if err := json.Unmarshal(line, &frame); err != nil {
					t.Fatal(err)
				}
				message := &frame.Message
				if frame.Type == "input" && message.Method == acp.MethodSessionSetConfigOption {
					var params acp.SetSessionConfigOptionParams
					if err := json.Unmarshal(message.Params, &params); err != nil {
						t.Fatal(err)
					}
					selections[string(message.ID)] = params
				}
				if frame.Type != "agent" || len(message.Result) == 0 {
					continue
				}
				var result acp.SessionNewResult
				if err := json.Unmarshal(message.Result, &result); err != nil {
					t.Fatal(err)
				}
				if len(result.ConfigOptions) == 0 {
					continue
				}
				configurations++
				var current string
				for i := range result.ConfigOptions {
					option := &result.ConfigOptions[i]
					values := agentacp.ConfigValues(option.Options)
					switch option.ID {
					case acp.ConfigOptionModel:
						if err := json.Unmarshal(option.CurrentValue, &current); err != nil {
							t.Fatal(err)
						}
						routes := make([]string, 0, len(values))
						for _, value := range values {
							route, err := decodeDSHModel(value)
							if err != nil {
								t.Fatal(err)
							}
							routes = append(routes, route)
						}
						want := []string{"deepseek-official/deepseek-flash", "deepseek-official/deepseek-v4-pro"}
						if current == `["deepseek-official","deepseek-v4-flash"]` {
							want = append(want, "deepseek-official/deepseek-v4-flash")
						}
						slices.Sort(routes)
						slices.Sort(want)
						if !slices.Equal(routes, want) {
							t.Fatalf("native model choices = %v, want %v", routes, want)
						}
					case dshEffortID:
						if !slices.Equal(values, []string{"off", "low", "high", "max"}) {
							t.Fatalf("native effort choices = %v", values)
						}
					}
				}
				if selection, ok := selections[string(message.ID)]; ok && selection.ConfigID == acp.ConfigOptionModel {
					modelSelections++
					model, err := decodeDSHModel(current)
					if err != nil || selection.Value != current || model != tc.model {
						t.Fatalf("selected %s; response current=%s, normalized=%s, err=%v", selection.Value, current, model, err)
					}
				}
			}
			wantSelections, wantConfigurations := 1, 3
			if tc.file == "session-turns" {
				wantSelections, wantConfigurations = 0, 2
			}
			if modelSelections != wantSelections || configurations != wantConfigurations || metadata.ReportedModel != tc.model || metadata.ReportedEffort != tc.effort {
				t.Fatalf("native model selections=%d configurations=%d metadata=%+v", modelSelections, configurations, metadata)
			}
		})
	}
}

func TestBashSkillRead(t *testing.T) {
	t.Parallel()
	wire := New("", nil).NewWire()
	line := []byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"bash-skill","title":"bash","kind":"execute","status":"in_progress","rawInput":{"command":"cat ~/.agents/skills/review/SKILL.md"}}}}`)
	first, err := wire.ParseOutput(line)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("bash skill messages=%#v, want use/read/load", first)
	}
	if read, ok := first[1].(*agent.SkillReadMessage); !ok || read.Skill != "review" || read.SourceToolUseID != "bash-skill" {
		t.Fatalf("skill read=%#v", first[1])
	}
	if load, ok := first[2].(*agent.SkillLoadMessage); !ok || load.SourceToolUseID != "bash-skill" {
		t.Fatalf("skill load=%#v", first[2])
	}
	repeated, err := wire.ParseOutput(line)
	if err != nil || len(repeated) != 1 {
		t.Fatalf("repeated skill update=%#v, err=%v", repeated, err)
	}
}
