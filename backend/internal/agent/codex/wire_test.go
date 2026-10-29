// Tests for Codex CLI wire protocol message types.

package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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
		records, err := agent.NewRelayRecordReader(strings.NewReader(responses), agent.LogVersionV1, agent.DiscardLogSink{Version: agent.LogVersionV1})
		if err != nil {
			t.Fatal(err)
		}
		var nextID atomic.Int64
		models, err := fetchModelsFromAppServer(t.Context(), &stdin, records, &nextID, agent.DiscardLogSink{Version: agent.LogVersionV1})
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
		records, err := agent.NewRelayRecordReader(strings.NewReader(responses), agent.LogVersionV1, agent.DiscardLogSink{Version: agent.LogVersionV1})
		if err != nil {
			t.Fatal(err)
		}
		var nextID atomic.Int64
		models, err := fetchModelsFromAppServer(t.Context(), &stdin, records, &nextID, agent.DiscardLogSink{Version: agent.LogVersionV1})
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
		records, err := agent.NewRelayRecordReader(strings.NewReader(responses), agent.LogVersionV1, agent.DiscardLogSink{Version: agent.LogVersionV1})
		if err != nil {
			t.Fatal(err)
		}
		var nextID atomic.Int64
		if _, err := fetchModelsFromAppServer(t.Context(), io.Discard, records, &nextID, agent.DiscardLogSink{Version: agent.LogVersionV1}); err == nil || !strings.Contains(err.Error(), "repeated cursor") {
			t.Fatalf("fetchModelsFromAppServer error = %v, want repeated cursor", err)
		}
	})
}

func v2Records(native string) string {
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
{"id":3,"result":{"thread":{"id":"saved-session"},"model":"gpt-5.4"}}
`
		var stdin bytes.Buffer
		_, _, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(responses)), io.NopCloser(strings.NewReader("")), &agent.Options{
			Dir: "/repo", Model: "gpt-5.4", ResumeSessionID: "saved-session", Log: &agenttest.LogSink{Version: agent.LogVersionV1},
		})
		if err != nil {
			t.Fatal(err)
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
		w, models, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(responses)), io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/repo", Log: &agenttest.LogSink{Version: agent.LogVersionV1}})
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
	t.Run("v2_agent_envelopes", func(t *testing.T) {
		t.Parallel()
		const responses = `{"id":1,"result":{"userAgent":"caic/0.1"}}
{"id":2,"result":{"data":[{"id":"gpt-5.4"}],"nextCursor":null}}
{"id":3,"result":{"thread":{"id":"thread_1","cliVersion":"0.133.0"}}}
`
		var stdin bytes.Buffer
		log := &agenttest.LogSink{Version: agent.LogVersionV2}
		w, models, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(v2Records(responses))), io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/repo", Model: "gpt-5.4", Log: log})
		if err != nil {
			t.Fatal(err)
		}
		if w.threadID != "thread_1" || w.agentVersion != "0.133.0" || w.reportedModel != "" || w.reportedEffort != "" || len(models) != 1 {
			t.Fatalf("v2 handshake = thread=%q model=%q effort=%q version=%q models=%v", w.threadID, w.reportedModel, w.reportedEffort, w.agentVersion, models)
		}
		if log.Len() != 0 {
			t.Fatalf("v2 handshake log = %s, want no legacy stdin persistence", log.Bytes())
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
		log := &agenttest.LogSink{Version: agent.LogVersionV3}
		if _, _, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(generation+v2Records(responses))), io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/repo", Model: "gpt-5.4", Log: log}); err != nil {
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
	var stdin bytes.Buffer
	var compact agent.CompactCommand = wire
	if err := compact.WriteCompact(&stdin, "", agent.DiscardLogSink{Version: agent.LogVersionV1}); err != nil {
		t.Fatal(err)
	}
	var request codex.JSONRPCRequest
	if err := json.Unmarshal(bytes.TrimSpace(stdin.Bytes()), &request); err != nil {
		t.Fatal(err)
	}
	if request.Method != "thread/compact/start" {
		t.Fatalf("compact method = %q, want thread/compact/start", request.Method)
	}
}

func TestWritePromptInputPersistence(t *testing.T) {
	t.Parallel()
	for _, version := range []agent.LogVersion{agent.LogVersionV1, agent.LogVersionV2, agent.LogVersionV3} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			t.Parallel()
			var stdin bytes.Buffer
			log := &agenttest.LogSink{Version: version}
			wire := &wireFormat{threadID: "thread-1"}
			if err := wire.WritePrompt(&stdin, agent.Prompt{Text: "hello"}, log); err != nil {
				t.Fatal(err)
			}
			if version == agent.LogVersionV3 && !bytes.HasPrefix(log.Bytes(), []byte(`{"t":"input","ts":`)) {
				t.Fatalf("v3 log = %s, want input envelope", log.Bytes())
			}
			if version != agent.LogVersionV3 && log.Len() != 0 {
				t.Fatalf("v%d log = %s, want no legacy stdin persistence", version, log.Bytes())
			}
		})
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
		name    string
		version agent.LogVersion
		input   string
		want    string
	}{
		{name: "v1", version: agent.LogVersionV1, input: native + notification + "\n", want: notification + "\n"},
		{name: "v2", version: agent.LogVersionV2, input: v2Records(native + notification), want: `{"t":"agent","ts":1.000,"msg":` + notification + "}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdin bytes.Buffer
			_, _, continuation, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(tc.input)), io.NopCloser(strings.NewReader("")), &agent.Options{Dir: "/repo", Log: &agenttest.LogSink{Version: tc.version}})
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
