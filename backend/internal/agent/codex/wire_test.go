// Tests for Codex CLI wire protocol message types.

package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/maruel/genai/providers/codex"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("empty_without_cache", func(t *testing.T) {
		t.Parallel()
		b := New("", nil)
		if len(b.ModelInventory().Models) != 0 {
			t.Fatalf("ModelInventory() = %#v, want empty", b.ModelInventory())
		}
	})
	t.Run("loads_cached_models", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		envVars := []string{"OPENAI_API_KEY=secret"}
		cache := agent.OpenHarnessCache(filepath.Join(dir, "harnesses.json"))
		cache.SetModelInventory(harness.Codex, agent.ModelInventory{Models: []agent.Model{{ID: "z-model"}, {ID: "a-model"}}}, agent.APIKeyHash(envVars))

		b := New(dir, envVars)
		if got, want := b.ModelInventory().IDs(), []string{"z-model", "a-model"}; !slices.Equal(got, want) {
			t.Fatalf("ModelInventory().IDs() = %v, want %v", got, want)
		}
	})
	t.Run("loads cached model effort options", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		envVars := []string{"OPENAI_API_KEY=secret"}
		cache := agent.OpenHarnessCache(filepath.Join(dir, "harnesses.json"))
		cache.SetModelInventory(harness.Codex, agent.ModelInventory{Models: []agent.Model{{ID: "gpt-5", EffortOptions: []string{"minimal", "high"}}}}, agent.APIKeyHash(envVars))

		b := New(dir, envVars)
		models := b.ModelInventory().Models
		if got, want := models[0].EffortOptions, []string{"minimal", "high"}; !slices.Equal(got, want) {
			t.Fatalf("cached effort options = %v, want %v", got, want)
		}
	})
	t.Run("records discovered models and their effort options", func(t *testing.T) {
		t.Parallel()
		b := New("", nil)

		b.SetModelInventory(newModelInventory([]codex.ModelInfo{
			{ID: "z-model", SupportedReasoningEfforts: []codex.ModelReasoningOpt{{ReasoningEffort: codex.ReasoningEffortMinimal}, {ReasoningEffort: codex.ReasoningEffortHigh}}},
			{ID: "a-model", SupportedReasoningEfforts: []codex.ModelReasoningOpt{{ReasoningEffort: codex.ReasoningEffortNone}}},
		}))

		models := b.ModelInventory().Models
		if got, want := models[0].EffortOptions, []string{"minimal", "high"}; !slices.Equal(got, want) {
			t.Fatalf("z-model effort options = %v, want %v", got, want)
		}
		if got, want := models[1].EffortOptions, []string{"none"}; !slices.Equal(got, want) {
			t.Fatalf("a-model effort options = %v, want %v", got, want)
		}
	})
}

func TestFetchModels(t *testing.T) {
	t.Parallel()
	t.Run("protocol", func(t *testing.T) {
		t.Parallel()
		const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"gpt-5.4","supportedReasoningEfforts":[{"reasoningEffort":"minimal"},{"reasoningEffort":"high"}]},{"id":"gpt-5.3-codex","supportedReasoningEfforts":[{"reasoningEffort":"low"}]}],"nextCursor":null}}
`
		var stdin bytes.Buffer
		records := &appServerReader{scanner: bufio.NewScanner(strings.NewReader(responses))}
		var nextID atomic.Int64
		models, err := fetchModelsFromAppServer(t.Context(), records, &nextID, func(v any) error {
			data, err := marshalInput(v)
			if err != nil {
				return err
			}
			_, err = stdin.Write(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		wantModels := []string{"gpt-5.4", "gpt-5.3-codex"}
		if got := modelIDs(models); !slices.Equal(got, wantModels) {
			t.Errorf("models = %v, want %v", got, wantModels)
		}
		if got, want := models[0].SupportedReasoningEfforts[1].ReasoningEffort, codex.ReasoningEffortHigh; got != want {
			t.Errorf("gpt-5.4 second effort = %q, want %q", got, want)
		}

		lines := bytes.Split(bytes.TrimSpace(stdin.Bytes()), []byte{'\n'})
		if len(lines) != 3 {
			t.Fatalf("wrote %d requests, want 3:\n%s", len(lines), stdin.String())
		}
		var req codex.JSONRPCRequest
		if err := json.Unmarshal(lines[2], &req); err != nil {
			t.Fatal(err)
		}
		if req.Method != "model/list" {
			t.Fatalf("method = %q, want model/list", req.Method)
		}
	})
	t.Run("pagination", func(t *testing.T) {
		t.Parallel()
		const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"first"}],"nextCursor":"page-2"}}
{"id":3,"result":{"data":[{"id":"second"}],"nextCursor":null}}
`
		var stdin bytes.Buffer
		records := &appServerReader{scanner: bufio.NewScanner(strings.NewReader(responses))}
		var nextID atomic.Int64
		models, err := fetchModelsFromAppServer(t.Context(), records, &nextID, func(v any) error {
			data, err := marshalInput(v)
			if err != nil {
				return err
			}
			_, err = stdin.Write(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := modelIDs(models), []string{"first", "second"}; !slices.Equal(got, want) {
			t.Fatalf("models = %v, want %v", got, want)
		}
		lines := bytes.Split(bytes.TrimSpace(stdin.Bytes()), []byte{'\n'})
		if len(lines) != 4 {
			t.Fatalf("wrote %d requests, want 4:\n%s", len(lines), stdin.String())
		}
		var params codex.ModelListParams
		var request codex.JSONRPCRequest
		if err := json.Unmarshal(lines[3], &request); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.Cursor != "page-2" {
			t.Fatalf("second model/list cursor = %q, want page-2", params.Cursor)
		}
	})
	t.Run("repeated_cursor", func(t *testing.T) {
		t.Parallel()
		const responses = `{"id":1,"result":{}}
{"id":2,"result":{"data":[],"nextCursor":"same"}}
{"id":3,"result":{"data":[],"nextCursor":"same"}}
`
		records := &appServerReader{scanner: bufio.NewScanner(strings.NewReader(responses))}
		var nextID atomic.Int64
		if _, err := fetchModelsFromAppServer(t.Context(), records, &nextID, func(any) error { return nil }); err == nil || !strings.Contains(err.Error(), "repeated cursor") {
			t.Fatalf("fetchModelsFromAppServer error = %v, want repeated cursor", err)
		}
	})
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
	t.Run("resume_with_selected_model", func(t *testing.T) {
		t.Parallel()
		const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[],"nextCursor":null}}
{"id":98,"error":{"code":-1,"message":"unrelated failed request"}}
{"id":99,"result":{"thread":{"id":"other-session"},"model":"wrong-model"}}
{"id":3,"result":{"thread":{"id":"saved-session"},"model":"gpt-5.4"}}
`
		var stdin bytes.Buffer
		wire, records := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
		_, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(v3Records(responses))), io.NopCloser(strings.NewReader("")), &agent.Options{
			Dir: "/repo", Model: "gpt-5.4", ResumeSessionID: "saved-session", Records: records,
		})
		if err != nil {
			t.Fatal(err)
		}
		if wire.threadID != "saved-session" || wire.reportedModel != "gpt-5.4" {
			t.Fatalf("matched resume metadata = session:%q model:%q", wire.threadID, wire.reportedModel)
		}
		var req struct {
			Method string                   `json:"method"`
			Params codex.ThreadResumeParams `json:"params"`
		}
		lines := bytes.Split(bytes.TrimSpace(stdin.Bytes()), []byte{'\n'})
		if err := json.Unmarshal(lines[len(lines)-1], &req); err != nil {
			t.Fatal(err)
		}
		if req.Method != "thread/resume" || req.Params.ThreadID != "saved-session" || req.Params.Model != "gpt-5.4" {
			t.Fatalf("resume request = %#v, want saved-session with gpt-5.4", req)
		}
	})
	t.Run("model_list_params", func(t *testing.T) {
		t.Parallel()
		const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"gpt-5.4"},{"id":"gpt-5.3-codex"}],"nextCursor":null}}
{"id":3,"result":{"thread":{"id":"thread_1","cliVersion":"0.133.0"},"model":"gpt-5.6","reasoningEffort":"medium"}}
`
		var stdin bytes.Buffer
		w, records := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
		models, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(v3Records(responses))), io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/repo", Records: records})
		if err != nil {
			t.Fatal(err)
		}
		if w.threadID != "thread_1" {
			t.Errorf("threadID = %q, want thread_1", w.threadID)
		}
		if w.agentVersion != "0.133.0" {
			t.Errorf("agentVersion = %q, want 0.133.0", w.agentVersion)
		}
		if w.reportedModel != "gpt-5.6" {
			t.Errorf("model = %q, want gpt-5.6", w.reportedModel)
		}
		if w.reportedEffort != "medium" {
			t.Errorf("reportedEffort = %q, want medium", w.reportedEffort)
		}
		wantModels := []string{"gpt-5.4", "gpt-5.3-codex"}
		if got := modelIDs(models); !slices.Equal(got, wantModels) {
			t.Errorf("models = %v, want %v", got, wantModels)
		}

		lines := bytes.Split(bytes.TrimSpace(stdin.Bytes()), []byte{'\n'})
		if len(lines) != 4 {
			t.Fatalf("wrote %d requests, want 4:\n%s", len(lines), stdin.String())
		}
		var req codex.JSONRPCRequest
		if err := json.Unmarshal(lines[2], &req); err != nil {
			t.Fatal(err)
		}
		if req.Method != "model/list" {
			t.Fatalf("method = %q, want model/list", req.Method)
		}
		if req.Params == nil {
			t.Fatal("model/list params = nil, want empty object")
		}
		data, err := json.Marshal(req.Params)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "{}" {
			t.Fatalf("model/list params = %s, want {}", data)
		}
	})

	t.Run("v3_persists_every_handshake_input", func(t *testing.T) {
		t.Parallel()
		const generation = `{"t":"relay_generation","generation":"generation-1"}` + "\n"
		const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"gpt-5.4"}],"nextCursor":null}}
{"id":3,"result":{"thread":{"id":"thread_1","cliVersion":"0.133.0"}}}
`
		var stdin bytes.Buffer
		log := &agenttest.LogSink{}
		_, records := protocolRecords(t, t.Context(), log, &agenttest.RecordHandler{})
		if _, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(generation+v3Records(responses))), io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/repo", Model: "gpt-5.4", Records: records}); err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(log.Bytes()), []byte{'\n'})
		if len(lines) != 8 {
			t.Fatalf("persisted handshake records = %d, want 4 inputs, generation, and 3 responses:\n%s", len(lines), log.String())
		}
		inputs := 0
		agentRecords := 0
		generations := 0
		for _, line := range lines {
			switch {
			case bytes.HasPrefix(line, []byte(`{"t":"input","ts":`)):
				inputs++
			case bytes.HasPrefix(line, []byte(`{"t":"agent","ts":`)):
				agentRecords++
			case bytes.Equal(line, bytes.TrimSpace([]byte(generation))):
				generations++
			default:
				t.Fatalf("unexpected handshake record: %s", line)
			}
		}
		if inputs != 4 || agentRecords != 3 || generations != 1 {
			t.Fatalf("persisted handshake records = inputs:%d agent:%d generations:%d", inputs, agentRecords, generations)
		}
	})
}

func TestWireFormatCompactCommand(t *testing.T) {
	t.Parallel()

	wire := &wireFormat{threadID: "thread-1"}
	var compact agent.CompactCommand = wire
	data, err := compact.EncodeCompact("")
	if err != nil {
		t.Fatal(err)
	}
	var request codex.JSONRPCRequest
	if err := json.Unmarshal(bytes.TrimSpace(data), &request); err != nil {
		t.Fatal(err)
	}
	if request.Method != "thread/compact/start" {
		t.Fatalf("compact method = %q, want thread/compact/start", request.Method)
	}
}

func TestEncodePromptCommittedCorrelation(t *testing.T) {
	t.Parallel()
	wire := &wireFormat{threadID: "thread-1"}
	data, err := wire.EncodePrompt(agent.Prompt{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.requests) != 0 || len(wire.inputTurns) != 0 {
		t.Fatal("encoding changed input correlation")
	}
	messages, err := wire.ParseInput(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || len(wire.requests) != 1 {
		t.Fatalf("committed prompt = %#v, requests = %#v", messages, wire.requests)
	}
	if input, ok := messages[0].(*agent.UserInputMessage); !ok || input.Text != "hello" {
		t.Fatalf("attempted input = %#v", messages)
	}
}

func TestHandshakeContinuation(t *testing.T) {
	t.Parallel()
	const native = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"gpt-5.4"}],"nextCursor":null}}
{"id":3,"result":{"thread":{"id":"thread_1","cliVersion":"0.133.0"}}}
`
	const notification = `{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"next","cliVersion":"1.0"}}}`
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
			_, records := protocolRecords(t, t.Context(), &agenttest.LogSink{}, &agenttest.RecordHandler{})
			_, continuation, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(tc.input)), io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/repo", Records: records})
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
