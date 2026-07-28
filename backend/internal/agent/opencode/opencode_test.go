// Tests OpenCode backend model discovery and ACP capability handling.

package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	genaiopencode "github.com/maruel/genai/providers/opencode"

	"github.com/caic-xyz/caic/backend/internal/agent"
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

		hs, err := handshake(t.Context(), &stdin, stdout, &agent.Options{Dir: "/workspace", Model: selectedModel, Effort: "high"})
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

	t.Run("retains default model when legacy selection fails", func(t *testing.T) {
		t.Parallel()

		const defaultModel = "anthropic/claude-sonnet-4"
		var stdin bytes.Buffer
		stdout := bufio.NewReader(strings.NewReader(strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"result":{}}`,
			`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"session-1","models":{"currentModelId":"anthropic/claude-sonnet-4","availableModels":[{"modelId":"anthropic/claude-sonnet-4"},{"modelId":"openai/gpt-5"}]}}}`,
			`{"jsonrpc":"2.0","id":3,"error":{"code":-32601,"message":"method not found"}}`,
		}, "\n") + "\n"))

		hs, err := handshake(t.Context(), &stdin, stdout, &agent.Options{Dir: "/workspace", Model: "openai/gpt-5"})
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if hs.currentModel != defaultModel {
			t.Fatalf("current model = %q, want default %q after failed selection", hs.currentModel, defaultModel)
		}
	})
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
