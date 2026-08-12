// Tests OpenCode backend model discovery and ACP capability handling.

package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

		models, err := parseModels([]byte("\x1b[92mModels cache refreshed\x1b[0m\nopencode/alpha\n{\n  \"variants\": {\n    \"high\": {},\n    \"low\": {}\n  }\n}\nanthropic/bravo\n{\"variants\": {}}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := (agent.ModelInventory{Models: models}).IDs(), []string{"anthropic/bravo", "opencode/alpha"}; !slices.Equal(got, want) {
			t.Fatalf("models = %v, want %v", got, want)
		}
		if got, want := models[1].EffortOptions, []string{"high", "low"}; !slices.Equal(got, want) {
			t.Fatalf("effort options = %v, want %v", got, want)
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
		stdout := bufio.NewReader(strings.NewReader(strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{"agentCapabilities":{"promptCapabilities":{"image":true}}}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","configOptions":[{"id":"model","type":"select","currentValue":"anthropic/claude-sonnet-4","options":[{"value":"anthropic/claude-sonnet-4"},{"value":"openai/gpt-5"}]},{"id":"effort","type":"select","currentValue":"low","options":[{"value":"low"},{"value":"high"}]},{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"},{"value":"plan"}]}]}}`,
			`{"jsonrpc":"2.0","id":3,"result":{"configOptions":[{"id":"model","type":"select","currentValue":"openai/gpt-5","options":[{"value":"anthropic/claude-sonnet-4"},{"value":"openai/gpt-5"}]},{"id":"effort","type":"select","currentValue":"low","options":[{"value":"low"},{"value":"high"}]},{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"},{"value":"plan"}]}]}}`,
			`{"jsonrpc":"2.0","id":4,"result":{"configOptions":[{"id":"model","type":"select","currentValue":"openai/gpt-5","options":[{"value":"anthropic/claude-sonnet-4"},{"value":"openai/gpt-5"}]},{"id":"effort","type":"select","currentValue":"high","options":[{"value":"low"},{"value":"high"}]},{"id":"mode","type":"select","currentValue":"build","options":[{"value":"build"},{"value":"plan"}]}]}}`,
		}, "\n") + "\n"))

		hs, _, err := handshake(t.Context(), &stdin, stdout, &agent.Options{Dir: "/workspace", Model: selectedModel, Effort: "high", Log: &agenttest.LogSink{Version: agent.LogVersionV1}})
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if hs.currentModel != selectedModel {
			t.Fatalf("current model = %q, want %q", hs.currentModel, selectedModel)
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
	})

	t.Run("v2_agent_envelopes", func(t *testing.T) {
		t.Parallel()
		const responses = `{"jsonrpc":"2.0","id":1,"result":{}}
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","models":{"currentModelId":"openai/gpt-5"}}}
`
		var stdin bytes.Buffer
		hs, _, err := handshake(t.Context(), &stdin, bufio.NewReader(strings.NewReader(v2Records(responses))), &agent.Options{Dir: "/workspace", Log: &agenttest.LogSink{Version: agent.LogVersionV2}})
		if err != nil {
			t.Fatal(err)
		}
		if hs.wire.sessionID != "session-1" || hs.currentModel != "openai/gpt-5" {
			t.Fatalf("v2 handshake = session=%q model=%q", hs.wire.sessionID, hs.currentModel)
		}
	})

	t.Run("retains default model when legacy selection fails", func(t *testing.T) {
		t.Parallel()

		const defaultModel = "anthropic/claude-sonnet-4"
		var stdin bytes.Buffer
		stdout := bufio.NewReader(strings.NewReader(strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","models":{"currentModelId":"anthropic/claude-sonnet-4","availableModels":[{"modelId":"anthropic/claude-sonnet-4"},{"modelId":"openai/gpt-5"}]}}}`,
			`{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"method not found"}}`,
		}, "\n") + "\n"))

		hs, _, err := handshake(t.Context(), &stdin, stdout, &agent.Options{Dir: "/workspace", Model: "openai/gpt-5", Log: &agenttest.LogSink{Version: agent.LogVersionV1}})
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if hs.currentModel != defaultModel {
			t.Fatalf("current model = %q, want default %q after failed selection", hs.currentModel, defaultModel)
		}
	})
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

func TestLegacyModelSelectionCancellation(t *testing.T) {
	t.Parallel()
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	records, err := agent.NewRelayRecordReader(reader, agent.LogVersionV1, agent.DiscardLogSink{Version: agent.LogVersionV1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res := &handshakeResult{wire: &wireFormat{sessionID: "s"}}
	if _, err := res.setSessionModel(ctx, io.Discard, records, "openai/gpt-5"); !errors.Is(err, context.Canceled) {
		t.Fatalf("setSessionModel error = %v, want context canceled", err)
	}
}

func TestHandshakeResultSetConfigOptions(t *testing.T) {
	t.Parallel()

	res := &handshakeResult{currentModel: "fallback/model"}
	res.setConfigOptions([]genaiopencode.SessionConfigOption{
		{
			ID:           genaiopencode.ConfigOptionModel,
			Type:         genaiopencode.ConfigOptionTypeSelect,
			CurrentValue: "openai/gpt-5",
			Options:      []genaiopencode.ConfigOptionValue{{Value: "openai/gpt-5"}, {Value: "anthropic/claude-sonnet-4"}},
		},
		{
			ID:      genaiopencode.ConfigOptionEffort,
			Type:    genaiopencode.ConfigOptionTypeSelect,
			Options: []genaiopencode.ConfigOptionValue{{Value: "minimal"}, {Value: "high"}},
		},
	})
	if res.currentModel != "openai/gpt-5" {
		t.Fatalf("current model = %q, want openai/gpt-5", res.currentModel)
	}
	if got := res.configOption(genaiopencode.ConfigOptionEffort); got == nil || len(got.Options) != 2 || got.Options[0].Value != "minimal" || got.Options[1].Value != "high" {
		t.Fatalf("effort option = %#v, want minimal and high", got)
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
		Target: runtime.ConnectionTarget{SSHHost: "task"},
		Dir:    "/workspace",
		MsgCh:  make(chan agent.ParsedMessage, 1),
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
