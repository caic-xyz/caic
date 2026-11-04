// Tests OpenCode handshakes, persisted metadata parity, ACP capabilities, and task MCP registration.

package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/maruel/genai/acp"

	genaiopencode "github.com/maruel/genai/providers/opencode"
	"github.com/maruel/gomode/mcp/mcptest"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

func TestBackend(t *testing.T) {
	t.Parallel()
	t.Run("RecordHandshake", func(t *testing.T) {
		t.Parallel()
		t.Run("Cancellation", func(t *testing.T) {
			t.Parallel()
			r, w := io.Pipe()
			t.Cleanup(func() { _ = w.Close() })
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, owner := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
			_, err := New("", nil).RecordHandshake(ctx, io.Discard, r, &agent.Options{Dir: "/workspace", Records: owner})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("handshake error = %v, want cancellation", err)
			}
		})
	})

	t.Run("SetModelInventory", func(t *testing.T) {
		t.Parallel()

		b := New("", nil)
		b.SetModelInventory(agent.ModelInventory{Models: []agent.Model{{ID: "openai/gpt-5", EffortOptions: []string{"low", "high"}}}})

		got := b.ModelInventory()
		if len(got.Models) != 1 || !slices.Equal(got.Models[0].EffortOptions, []string{"high", "low"}) {
			t.Fatalf("ModelInventory() = %#v, want normalized inventory", got)
		}
	})
}

func TestParseModels(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		models, err := parseModels([]byte("\x1b[92mModels cache refreshed\x1b[0m\nopencode/alpha\n{\n  \"variants\": {\n    \"high\": {},\n    \"low\": {}\n  },\n  \"limit\": {\n    \"context\": 200000,\n    \"output\": 32000\n  }\n}\nanthropic/bravo\n{\"variants\": {}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := (agent.ModelInventory{Models: models}).IDs(), []string{"anthropic/bravo", "opencode/alpha"}; !slices.Equal(got, want) {
			t.Fatalf("models = %v, want %v", got, want)
		}
		if got, want := models[1].EffortOptions, []string{"high", "low"}; !slices.Equal(got, want) {
			t.Fatalf("effort options = %v, want %v", got, want)
		}
		if got, want := models[1].ContextWindow, 200_000; got != want {
			t.Fatalf("context window = %d, want %d", got, want)
		}
		if got := models[0].ContextWindow; got != 0 {
			t.Fatalf("unpublished context window = %d, want 0", got)
		}
	})

	t.Run("missing_metadata", func(t *testing.T) {
		t.Parallel()

		if _, err := parseModels([]byte("opencode/alpha\n")); err == nil {
			t.Fatal("parseModels() succeeded, want error")
		}
	})
}

func TestNew(t *testing.T) {
	t.Parallel()

	if got := New("", nil).ModelInventory(); len(got.Models) != 0 {
		t.Fatalf("ModelInventory() = %#v, want an empty inventory before discovery", got)
	}
}

func v3Records(native string) string {
	var records strings.Builder
	for line := range strings.SplitSeq(strings.TrimSpace(native), "\n") {
		records.WriteString(`{"t":"agent","ts":1.000,"msg":`)
		records.WriteString(line)
		records.WriteString("}\n")
	}
	return records.String()
}

func TestHandshake(t *testing.T) {
	t.Parallel()

	for _, resume := range []string{"", "session-1"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("task_mcp/resume=%s/enabled=%t", resume, enabled), func(t *testing.T) {
				t.Parallel()
				var stdin bytes.Buffer
				stdout := bufio.NewReader(strings.NewReader(v3Records(
					`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n" +
						`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1"}}`)))
				_, owner := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
				opts := &agent.Options{Dir: "/workspace", ResumeSessionID: resume, Records: owner}
				if enabled {
					opts.MCP = &mcptest.FakeRegistry{}
				}
				if _, _, err := handshake(t.Context(), &stdin, stdout, io.NopCloser(strings.NewReader("")), opts); err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(stdin.String()), "\n")
				var request acp.JSONRPCRequest
				if err := json.Unmarshal([]byte(lines[1]), &request); err != nil {
					t.Fatal(err)
				}
				wantMethod := acp.MethodSessionNew
				if resume != "" {
					wantMethod = acp.MethodSessionLoad
				}
				if request.Method != wantMethod {
					t.Fatalf("method = %s, want %s", request.Method, wantMethod)
				}
				var params acp.SessionLoadParams
				if err := json.Unmarshal(request.Params, &params); err != nil {
					t.Fatal(err)
				}
				if !enabled {
					if len(params.McpServers) != 0 {
						t.Fatalf("disabled task has servers: %v", params.McpServers)
					}
					return
				}
				if len(params.McpServers) != 1 {
					t.Fatalf("servers = %v, want one", params.McpServers)
				}
				server := params.McpServers[0]
				if server.Name != "caic" || server.Command != "python3" || !slices.Equal(server.Args, []string{"/tmp/caic-relay/relay.py", "caic-mcp"}) {
					t.Fatalf("invalid bridge command: %v", server)
				}
				if len(server.Env) != 1 || server.Env[0].Name != "CAIC_RELAY_DIR" || server.Env[0].Value != "/tmp/caic-relay" {
					t.Fatalf("invalid bridge environment: %v", server.Env)
				}
			})
		}
	}

	t.Run("selects model and effort through ACP configuration", func(t *testing.T) {
		t.Parallel()

		const selectedModel = "openai/gpt-5"
		const generation = `{"t":"relay_generation","generation":"generation-1"}` + "\n"
		var stdin bytes.Buffer
		stdout := bufio.NewReader(strings.NewReader(generation + v3Records(strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"version":"1.18.31"},"agentCapabilities":{"promptCapabilities":{"image":true}}}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","configOptions":[{"id":"model","type":"select","currentValue":"anthropic/claude-sonnet-4","options":[{"value":"anthropic/claude-sonnet-4"},{"value":"openai/gpt-5"}]},{"id":"effort","type":"select","currentValue":"low","options":[{"value":"low"},{"value":"high"}]},{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"},{"value":"plan"}]}]}}`,
			`{"jsonrpc":"2.0","id":3,"result":{"configOptions":[{"id":"model","type":"select","currentValue":"openai/gpt-5","options":[{"value":"anthropic/claude-sonnet-4"},{"value":"openai/gpt-5"}]},{"id":"effort","type":"select","currentValue":"low","options":[{"value":"low"},{"value":"high"}]},{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"},{"value":"plan"}]}]}}`,
			`{"jsonrpc":"2.0","id":4,"result":{"configOptions":[{"id":"model","type":"select","currentValue":"openai/gpt-5","options":[{"value":"anthropic/claude-sonnet-4"},{"value":"openai/gpt-5"}]},{"id":"effort","type":"select","currentValue":"high","options":[{"value":"low"},{"value":"high"}]},{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"},{"value":"plan"}]}]}}`,
		}, "\n")+"\n")))

		log := &agenttest.LogSink{}
		observer := &agenttest.RecordHandler{}
		wire, owner := protocolRecords(t, t.Context(), log, observer)
		hs, _, err := handshake(t.Context(), &stdin, stdout, io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/workspace", Model: selectedModel, Effort: "high", Records: owner})
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if hs.currentModel != selectedModel || hs.currentEffort != "high" {
			t.Fatalf("reported settings = %q/%q, want %q/high", hs.currentModel, hs.currentEffort, selectedModel)
		}
		lines := strings.Split(strings.TrimSpace(stdin.String()), "\n")
		if len(lines) != 4 {
			t.Fatalf("request count = %d, want 4; requests = %s", len(lines), stdin.String())
		}
		var modelRequest, effortRequest acp.JSONRPCRequest
		if err := json.Unmarshal([]byte(lines[2]), &modelRequest); err != nil {
			t.Fatalf("unmarshal model request: %v", err)
		}
		if err := json.Unmarshal([]byte(lines[3]), &effortRequest); err != nil {
			t.Fatalf("unmarshal effort request: %v", err)
		}
		if modelRequest.Method != acp.MethodSessionSetConfigOption || effortRequest.Method != acp.MethodSessionSetConfigOption {
			t.Fatalf("configuration request methods = %q, %q", modelRequest.Method, effortRequest.Method)
		}
		var modelParams, effortParams acp.SetSessionConfigOptionParams
		if err := json.Unmarshal(modelRequest.Params, &modelParams); err != nil {
			t.Fatalf("unmarshal model params: %v", err)
		}
		if err := json.Unmarshal(effortRequest.Params, &effortParams); err != nil {
			t.Fatalf("unmarshal effort params: %v", err)
		}
		if modelParams.ConfigID != acp.ConfigOptionModel || modelParams.Value != selectedModel {
			t.Fatalf("model params = %#v", modelParams)
		}
		if effortParams.ConfigID != genaiopencode.ConfigOptionEffort || effortParams.Value != "high" {
			t.Fatalf("effort params = %#v", effortParams)
		}
		persisted := bytes.Split(bytes.TrimSpace(log.Bytes()), []byte{'\n'})
		if len(persisted) != 9 {
			t.Fatalf("persisted handshake records = %d, want 4 inputs, generation, and 4 responses:\n%s", len(persisted), log.String())
		}
		inputs := 0
		agentRecords := 0
		generations := 0
		for _, record := range persisted {
			switch {
			case bytes.HasPrefix(record, []byte(`{"t":"input","ts":`)):
				inputs++
			case bytes.HasPrefix(record, []byte(`{"t":"agent","ts":`)):
				agentRecords++
			case bytes.Equal(record, bytes.TrimSpace([]byte(generation))):
				generations++
			default:
				t.Fatalf("unexpected handshake record: %s", record)
			}
		}
		if inputs != 4 || agentRecords != 4 || generations != 1 {
			t.Fatalf("persisted handshake records = inputs:%d agent:%d generations:%d", inputs, agentRecords, generations)
		}
		want := agent.SessionMetadata{SessionID: "session-1", ReportedModel: selectedModel, ReportedEffort: "high", AgentVersion: "1.18.31"}
		var live agent.SessionMetadata
		starts := 0
		for _, event := range observer.Events() {
			live.Apply(event.Message)
			if _, ok := event.Message.(*agent.InitMessage); ok {
				starts++
			}
		}
		if starts != 1 || !wire.supportsImage {
			t.Fatalf("native starts = %d, supportsImage = %t", starts, wire.supportsImage)
		}
		if live != want {
			t.Fatalf("live metadata = %+v, want %+v", live, want)
		}
		for name, native := range map[string]agent.MessageParser{"full": New("", nil).NewWire(), "metadata": New("", nil).NewMetadataParser()} {
			t.Run(name+" replay", func(t *testing.T) {
				t.Parallel()
				parser, err := agent.NewLogRecordParser(native)
				if err != nil {
					t.Fatal(err)
				}
				var got agent.SessionMetadata
				starts, reports := 0, 0
				for _, physical := range persisted {
					record, err := parser.ParseRecord(physical)
					if err != nil {
						t.Fatal(err)
					}
					for _, timed := range record.Messages {
						got.Apply(timed.Message)
						switch message := timed.Message.(type) {
						case *agent.InitMessage:
							starts++
							if message.SessionID != "session-1" || message.ReportedModel != "anthropic/claude-sonnet-4" || message.ReportedEffort != "low" || message.Version != "1.18.31" {
								t.Fatalf("native session start = %+v", message)
							}
						case *agent.SessionMetadataMessage:
							reports++
						}
					}
				}
				if got != want || starts != 1 || reports != 3 {
					t.Fatalf("replay metadata = %+v, starts = %d, partial reports = %d; want %+v, 1, 3", got, starts, reports, want)
				}
			})
		}
	})

	t.Run("services task scoped MCP during session creation", func(t *testing.T) {
		t.Parallel()

		const initializeResponse = `{"jsonrpc":"2.0","id":1,"result":{}}`
		const sessionResponse = `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1"}}`
		records := v3Records(initializeResponse) +
			`{"t":"mcp_request","id":"tools-1","method":"tools/list"}` + "\n" +
			v3Records(sessionResponse)
		var stdin bytes.Buffer
		_, owner := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
		_, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(records)), io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/workspace", Records: owner})
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(stdin.String()), "\n")
		if len(lines) != 3 {
			t.Fatalf("request count = %d, want initialize, session/new, MCP response: %s", len(lines), stdin.String())
		}
		var response agent.MCPResponseEnvelope
		if err := json.Unmarshal([]byte(lines[2]), &response); err != nil {
			t.Fatalf("unmarshal MCP response: %v", err)
		}
		if response.ID != "tools-1" || response.Error != "task-scoped MCP is unavailable" {
			t.Fatalf("MCP response = %#v", response)
		}
	})
}

func TestEncodePromptCommittedCorrelation(t *testing.T) {
	t.Parallel()
	wire := &wireFormat{}
	wire.session.SessionID = "session-1"
	wire.textAccum.WriteString("prior text")
	data, err := wire.EncodePrompt(agent.Prompt{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.requests) != 0 || wire.textAccum.String() != "prior text" {
		t.Fatal("encoding changed decoder state")
	}
	messages, err := wire.ParseInput(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || len(wire.requests) != 1 || wire.textAccum.Len() != 0 {
		t.Fatalf("committed prompt = %#v, requests = %#v, prior text = %q", messages, wire.requests, wire.textAccum.String())
	}
	if input, ok := messages[0].(*agent.UserInputMessage); !ok || input.Text != "hello" {
		t.Fatalf("attempted input = %#v", messages)
	}
}

func TestHandshakeContinuation(t *testing.T) {
	t.Parallel()
	const native = `{"jsonrpc":"2.0","id":1,"result":{}}
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","models":{"currentModelId":"openai/gpt-5"}}}
`
	const notification = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":{}}}`
	for _, tc := range []struct {
		name string

		input string
		want  string
	}{

		{name: "v3", input: v3Records(native + notification), want: `{"t":"agent","ts":1.000,"msg":` + notification + "}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdin bytes.Buffer
			_, owner := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
			_, continuation, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(tc.input)), io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/workspace", Records: owner})
			if err != nil {
				t.Fatal(err)
			}
			got, err := continuation.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("continuation = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandshakeResultSetConfigOptions(t *testing.T) {
	t.Parallel()

	res := &handshakeResult{currentModel: "fallback/model"}
	err := res.setConfigOptions([]acp.SessionConfigOption{
		{
			ID:           acp.ConfigOptionModel,
			Type:         acp.ConfigOptionTypeSelect,
			CurrentValue: json.RawMessage(`"openai/gpt-5"`),
			Options:      []acp.ConfigOptionValue{{Value: "openai/gpt-5"}, {Value: "anthropic/claude-sonnet-4"}},
		},
		{
			ID:           genaiopencode.ConfigOptionEffort,
			Type:         acp.ConfigOptionTypeSelect,
			CurrentValue: json.RawMessage(`"high"`),
			Options:      []acp.ConfigOptionValue{{Value: "minimal"}, {Value: "high"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.currentModel != "openai/gpt-5" || res.currentEffort != "high" {
		t.Fatalf("reported settings = %q/%q, want openai/gpt-5/high", res.currentModel, res.currentEffort)
	}
	if got := res.configOption(genaiopencode.ConfigOptionEffort); got == nil || len(got.Options) != 2 || got.Options[0].Value != "minimal" || got.Options[1].Value != "high" {
		t.Fatalf("effort option = %#v, want minimal and high", got)
	}
	if err := res.setConfigOptions([]acp.SessionConfigOption{{
		ID:           acp.ConfigOptionModel,
		CurrentValue: json.RawMessage(`true`),
	}}); err == nil || !strings.Contains(err.Error(), "decode current model configuration") {
		t.Fatalf("setConfigOptions error = %v, want model decoding error", err)
	}
}

// protocolRecords keeps the actual startup decoder and its ordered observer in
// the same fixture; the caller joins every transport before cleanup closes it.
func protocolRecords(t testing.TB, ctx context.Context, sink agent.LogSink, handler *agenttest.RecordHandler) (*wireFormat, *agent.Records) {
	wire, ok := New("", nil).NewWire().(*wireFormat)
	if !ok {
		t.Fatal("unexpected protocol wire")
	}
	records, err := agent.NewRecords(ctx, wire, sink, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := records.Close(); err != nil {
			t.Error(err)
		}
	})
	return wire, records
}
