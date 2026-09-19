// Tests OpenCode backend model discovery and ACP capability handling.

package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	genaiopencode "github.com/maruel/genai/providers/opencode"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

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

func TestSetModelInventory(t *testing.T) {
	t.Parallel()

	b := New("", nil)
	b.SetModelInventory(agent.ModelInventory{Models: []agent.Model{{ID: "openai/gpt-5", EffortOptions: []string{"low", "high"}}}})

	got := b.ModelInventory()
	if len(got.Models) != 1 || !slices.Equal(got.Models[0].EffortOptions, []string{"high", "low"}) {
		t.Fatalf("ModelInventory() = %#v, want normalized inventory", got)
	}
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

	t.Run("selects model and effort through ACP configuration", func(t *testing.T) {
		t.Parallel()

		const selectedModel = "openai/gpt-5"
		var stdin bytes.Buffer
		stdout := bufio.NewReader(strings.NewReader(v2Records(strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{"agentCapabilities":{"promptCapabilities":{"image":true}}}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","configOptions":[{"id":"model","type":"select","currentValue":"anthropic/claude-sonnet-4","options":[{"value":"anthropic/claude-sonnet-4"},{"value":"openai/gpt-5"}]},{"id":"effort","type":"select","currentValue":"low","options":[{"value":"low"},{"value":"high"}]},{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"},{"value":"plan"}]}]}}`,
			`{"jsonrpc":"2.0","id":3,"result":{"configOptions":[{"id":"model","type":"select","currentValue":"openai/gpt-5","options":[{"value":"anthropic/claude-sonnet-4"},{"value":"openai/gpt-5"}]},{"id":"effort","type":"select","currentValue":"low","options":[{"value":"low"},{"value":"high"}]},{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"},{"value":"plan"}]}]}}`,
			`{"jsonrpc":"2.0","id":4,"result":{"configOptions":[{"id":"model","type":"select","currentValue":"openai/gpt-5","options":[{"value":"anthropic/claude-sonnet-4"},{"value":"openai/gpt-5"}]},{"id":"effort","type":"select","currentValue":"high","options":[{"value":"low"},{"value":"high"}]},{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"},{"value":"plan"}]}]}}`,
		}, "\n") + "\n")))

		log := &agenttest.LogSink{Version: agent.LogVersionV3}
		hs, _, err := handshake(t.Context(), &stdin, stdout, &agent.Options{Dir: "/workspace", Model: selectedModel, Effort: "high", Log: log})
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if hs.currentModel != selectedModel || hs.currentEffort != "high" {
			t.Fatalf("reported settings = %q/%q, want %q/high", hs.currentModel, hs.currentEffort, selectedModel)
		}
		lines := strings.Fields(stdin.String())
		if len(lines) != 4 {
			t.Fatalf("request count = %d, want 4; requests = %s", len(lines), stdin.String())
		}
		var modelRequest, effortRequest genaiopencode.JSONRPCRequest
		if err := json.Unmarshal([]byte(lines[2]), &modelRequest); err != nil {
			t.Fatalf("unmarshal model request: %v", err)
		}
		if err := json.Unmarshal([]byte(lines[3]), &effortRequest); err != nil {
			t.Fatalf("unmarshal effort request: %v", err)
		}
		if modelRequest.Method != genaiopencode.MethodSessionSetConfigOption || effortRequest.Method != genaiopencode.MethodSessionSetConfigOption {
			t.Fatalf("configuration request methods = %q, %q", modelRequest.Method, effortRequest.Method)
		}
		var modelParams, effortParams genaiopencode.SetSessionConfigOptionParams
		if err := json.Unmarshal(modelRequest.Params, &modelParams); err != nil {
			t.Fatalf("unmarshal model params: %v", err)
		}
		if err := json.Unmarshal(effortRequest.Params, &effortParams); err != nil {
			t.Fatalf("unmarshal effort params: %v", err)
		}
		if modelParams.ConfigID != genaiopencode.ConfigOptionModel || modelParams.Value != selectedModel {
			t.Fatalf("model params = %#v", modelParams)
		}
		if effortParams.ConfigID != genaiopencode.ConfigOptionEffort || effortParams.Value != "high" {
			t.Fatalf("effort params = %#v", effortParams)
		}
		persisted := bytes.Split(bytes.TrimSpace(log.Bytes()), []byte{'\n'})
		if len(persisted) != 4 {
			t.Fatalf("persisted inputs = %d, want initialize, session/new, model, effort:\n%s", len(persisted), log.String())
		}
		for _, record := range persisted {
			if !bytes.HasPrefix(record, []byte(`{"t":"input","ts":`)) {
				t.Fatalf("handshake record = %s, want v3 input envelope", record)
			}
		}
	})

	t.Run("v2_agent_envelopes", func(t *testing.T) {
		t.Parallel()
		const responses = `{"jsonrpc":"2.0","id":1,"result":{}}
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","models":{"currentModelId":"openai/gpt-5"}}}
`
		var stdin bytes.Buffer
		log := &agenttest.LogSink{Version: agent.LogVersionV2}
		hs, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(v2Records(responses))), &agent.Options{Dir: "/workspace", Log: log})
		if err != nil {
			t.Fatal(err)
		}
		if hs.wire.sessionID != "session-1" || hs.currentModel != "openai/gpt-5" {
			t.Fatalf("v2 handshake = session=%q model=%q", hs.wire.sessionID, hs.currentModel)
		}
		if log.Len() != 0 {
			t.Fatalf("v2 handshake log = %s, want no legacy stdin persistence", log.Bytes())
		}
	})
}

func TestWritePromptInputPersistence(t *testing.T) {
	t.Parallel()
	for _, version := range []agent.LogVersion{agent.LogVersionV1, agent.LogVersionV2, agent.LogVersionV3} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			t.Parallel()
			var stdin bytes.Buffer
			log := &agenttest.LogSink{Version: version}
			wire := &wireFormat{sessionID: "session-1"}
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
	const native = `{"jsonrpc":"2.0","id":1,"result":{}}
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","models":{"currentModelId":"openai/gpt-5"}}}
`
	const notification = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":{}}}`
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
			_, continuation, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(tc.input)), &agent.Options{Dir: "/workspace", Log: &agenttest.LogSink{Version: tc.version}})
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
	err := res.setConfigOptions([]genaiopencode.SessionConfigOption{
		{
			ID:           genaiopencode.ConfigOptionModel,
			Type:         genaiopencode.ConfigOptionTypeSelect,
			CurrentValue: json.RawMessage(`"openai/gpt-5"`),
			Options:      []genaiopencode.ConfigOptionValue{{Value: "openai/gpt-5"}, {Value: "anthropic/claude-sonnet-4"}},
		},
		{
			ID:           genaiopencode.ConfigOptionEffort,
			Type:         genaiopencode.ConfigOptionTypeSelect,
			CurrentValue: json.RawMessage(`"high"`),
			Options:      []genaiopencode.ConfigOptionValue{{Value: "minimal"}, {Value: "high"}},
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
	if err := res.setConfigOptions([]genaiopencode.SessionConfigOption{{
		ID:           genaiopencode.ConfigOptionModel,
		CurrentValue: json.RawMessage(`true`),
	}}); err == nil || !strings.Contains(err.Error(), "decode current model configuration") {
		t.Fatalf("setConfigOptions error = %v, want model decoding error", err)
	}
}

type failingLogSink struct{}

func (failingLogSink) LogVersion() agent.LogVersion { return agent.LogVersionV1 }
func (failingLogSink) AppendNative([]byte) error    { return nil }
func (failingLogSink) AppendMessage(agent.Message) error {
	return errors.New("persist session metadata")
}
func (failingLogSink) Close() error { return nil }

func TestStartReapsRelayOnMetadataFailure(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "relay.pid")
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
case "$2" in
  mkdir*) cat >/dev/null ; exit 0 ;;
esac
for arg in "$@"; do
  case "$arg" in
  serve-attach)
    /bin/sleep 600 </dev/null >/dev/null 2>&1 &
    relay=$!
    echo "$relay" > "$CAIC_OPENCODE_RELAY_PID"
    IFS= read -r _
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"agentInfo":{"version":"1.0"}}}'
    IFS= read -r _
    printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1"}}'
    while IFS= read -r _; do :; done
    kill "$relay" 2>/dev/null
    wait "$relay" 2>/dev/null
    echo reaped > "$CAIC_OPENCODE_RELAY_PID.reaped"
    exit 0
    ;;
  attach)
    IFS= read -r _
    kill "$(cat "$CAIC_OPENCODE_RELAY_PID")"
    exit 0
    ;;
  esac
done
exit 1
`
	if err := os.WriteFile(sshPath, []byte(script), 0o700); err != nil { //nolint:gosec // test helper must be executable.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CAIC_OPENCODE_RELAY_PID", pidPath)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	backend := New("", nil)
	_, err := backend.Start(ctx, &agent.Options{
		Logger: slog.New(slog.DiscardHandler),
		Target: runtime.ConnectionTarget{SSHHost: "task"},
		Dir:    "/workspace",
		MsgCh:  make(chan agent.TimedMessage, 1),
		Log:    failingLogSink{},
	})
	if err == nil || !strings.Contains(err.Error(), "write session metadata") {
		t.Fatalf("Start error = %v, want metadata failure", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		data, err := os.ReadFile(pidPath + ".reaped") //nolint:gosec // test-controlled helper path.
		if err == nil && strings.TrimSpace(string(data)) == "reaped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay daemon was not reaped after metadata failure: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}
