// Tests OpenCode backend ACP capability handling.

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
		if len(hs.capabilities) != 2 || !slices.Equal(hs.capabilities[0].EffortOptions, []string{"low", "high"}) {
			t.Fatalf("capabilities = %#v, want selected model effort options", hs.capabilities)
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

func TestCapabilitiesForModels(t *testing.T) {
	t.Parallel()

	capabilities := capabilitiesForModels([]string{"openai/gpt-5", "anthropic/claude-sonnet-4"}, "openai/gpt-5", []string{"low", "high"}, []string{"build", "plan"})
	if len(capabilities) != 2 {
		t.Fatalf("len(capabilities) = %d, want 2", len(capabilities))
	}
	if got, want := capabilities[0].EffortOptions, []string{"low", "high"}; !slices.Equal(got, want) {
		t.Fatalf("selected effort options = %v, want %v", got, want)
	}
	if got, want := capabilities[0].Modes, []string{"build", "plan"}; !slices.Equal(got, want) {
		t.Fatalf("selected modes = %v, want %v", got, want)
	}
	if len(capabilities[1].EffortOptions) != 0 || len(capabilities[1].Modes) != 0 {
		t.Fatalf("unselected capability = %#v, want no model-specific options", capabilities[1])
	}
}

func TestHandshakeResultSetConfigOptions(t *testing.T) {
	t.Parallel()

	res := &handshakeResult{models: []string{"fallback/model"}, currentModel: "fallback/model"}
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
	if got, want := res.models, []string{"openai/gpt-5", "anthropic/claude-sonnet-4"}; !slices.Equal(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	if got, want := res.capabilities[0].EffortOptions, []string{"minimal", "high"}; !slices.Equal(got, want) {
		t.Fatalf("effort options = %v, want %v", got, want)
	}
}
