// Tests for shared agent message and quota provider types.

package agent

import (
	"encoding/json"
	"testing"

	taskslogv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func TestUsageMessage(t *testing.T) {
	t.Parallel()
	t.Run("JSONRequestDetail", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			json    string
			perCall bool
		}{
			{`{"model":"gpt-6.1-sol","model_derived":true,"usage":{"input_tokens":300000}}`, false},
			{`{"model":"gpt-6.1-sol","model_derived":true,"per_call":true,"usage":{"input_tokens":300000}}`, true},
		} {
			var msg UsageMessage
			if err := json.Unmarshal([]byte(tc.json), &msg); err != nil {
				t.Fatal(err)
			}
			if msg.PerCall != tc.perCall || !msg.ModelDerived || msg.Usage.InputTokens != 300000 {
				t.Fatalf("decoded request detail = %+v", msg)
			}
			encoded, err := json.Marshal(&msg)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["per_call"]; present != tc.perCall {
				t.Fatalf("per_call presence = %v, JSON = %s", present, encoded)
			}
		}
	})
}

func TestQuotaProvider(t *testing.T) {
	t.Parallel()
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		if !QuotaProviderClaudeCode.Valid() {
			t.Error("QuotaProviderClaudeCode.Valid() = false, want true")
		}
		if QuotaProvider("unknown").Valid() {
			t.Error("QuotaProvider(unknown).Valid() = true, want false")
		}
	})
	t.Run("String", func(t *testing.T) {
		t.Parallel()
		if got := QuotaProviderZai.String(); got != "Z.ai" {
			t.Errorf("QuotaProviderZai.String() = %q, want Z.ai", got)
		}
		if got := QuotaProviderCodex.String(); got != "Codex" {
			t.Errorf("QuotaProviderCodex.String() = %q, want Codex", got)
		}
		if got := QuotaProvider("unknown").String(); got != "unknown" {
			t.Errorf("unknown provider String() = %q, want the raw id", got)
		}
	})
	t.Run("QuotaProviderForModel", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			model string
			want  QuotaProvider
		}{
			{"zai/glm-5.3-flash", QuotaProviderZai},
			{"zai-coding-plan/glm-5.3-flash", QuotaProviderZai},
			{"openai-codex/gpt-5.6-terra", QuotaProviderCodex},
			{"openrouter/z-ai/glm-5.3-flash", QuotaProviderOpenRouter},
			{"deepseek/deepseek-flash", QuotaProviderDeepSeek},
			{"deepseek-official/deepseek-v4-flash", QuotaProviderDeepSeek},
			{"groq/llama-4", QuotaProviderGroq},
			{"claude-sonnet-4-5", ""},
			{"", ""},
		}
		for _, tc := range tests {
			if got := QuotaProviderForModel(tc.model); got != tc.want {
				t.Errorf("QuotaProviderForModel(%q) = %q, want %q", tc.model, got, tc.want)
			}
		}
	})
}

func TestRateLimitStatus(t *testing.T) {
	t.Parallel()
	t.Run("Valid", func(t *testing.T) {
		t.Parallel()
		if !RateLimitStatusAllowedWarning.Valid() {
			t.Error("RateLimitStatusAllowedWarning.Valid() = false, want true")
		}
		if RateLimitStatus("unknown").Valid() {
			t.Error("RateLimitStatus(unknown).Valid() = true, want false")
		}
	})
}

func TestMessageSource(t *testing.T) {
	t.Parallel()
	t.Run("JSON", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			source taskslogv3.MessageSource
			zero   bool
		}{
			{name: "absent", zero: true},
			{name: "root only", source: taskslogv3.MessageSource{RootThreadID: "root"}},
			{name: "thread only", source: taskslogv3.MessageSource{ThreadID: "thread"}},
			{name: "turn only", source: taskslogv3.MessageSource{TurnID: "turn"}},
			{name: "item only", source: taskslogv3.MessageSource{ItemID: "item"}},
			{name: "missing thread", source: taskslogv3.MessageSource{TurnID: "turn", ItemID: "item"}},
			{name: "missing turn", source: taskslogv3.MessageSource{ThreadID: "thread", ItemID: "item"}},
			{name: "missing item", source: taskslogv3.MessageSource{ThreadID: "thread", TurnID: "turn"}},
			{name: "complete rootless", source: taskslogv3.MessageSource{ThreadID: "thread", TurnID: "turn", ItemID: "item"}},
			{name: "complete rooted", source: taskslogv3.MessageSource{ThreadID: "thread", TurnID: "turn", ItemID: "item", RootThreadID: "root"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				for _, message := range []Message{&TextMessage{Source: tc.source}, &TextDeltaMessage{Source: tc.source}, &ThinkingMessage{Source: tc.source}, &ThinkingDeltaMessage{Source: tc.source}} {
					encoded, err := json.Marshal(message)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(encoded, &fields); err != nil {
						t.Fatal(err)
					}
					raw, present := fields["source"]
					if present == tc.zero {
						t.Errorf("%T source presence = %t: %s", message, present, encoded)
					}
					if present {
						var decoded taskslogv3.MessageSource
						if err := json.Unmarshal(raw, &decoded); err != nil {
							t.Fatal(err)
						}
						if decoded != tc.source {
							t.Errorf("%T source = %+v, want %+v", message, decoded, tc.source)
						}
					}
				}
			})
		}
	})
}

func TestMessageProvenance(t *testing.T) {
	t.Parallel()
	source := taskslogv3.MessageSource{AgentID: "native:child", DelegationToolUseID: "delegate", ThreadID: "thread", TurnID: "turn", ItemID: "item", RootThreadID: "root"}
	messages := []Message{&MCPRequestMessage{}, &InitMessage{}, &SessionMetadataMessage{}, &SystemMessage{}, &TextMessage{}, &ToolUseMessage{}, &SkillReadMessage{}, &SkillLoadMessage{}, &AskMessage{}, &PendingUserActionMessage{}, &TodoMessage{}, &UserInputMessage{}, &ToolResultMessage{}, &UsageMessage{}, &TurnCommitSnapshotMessage{}, &ResultMessage{}, &TextDeltaMessage{}, &ThinkingMessage{}, &ThinkingDeltaMessage{}, &ToolOutputDeltaMessage{}, &NativeSubagentMessage{}, &BackgroundCommandMessage{}, &BackgroundRetiredMessage{}, &TurnStartedMessage{}, &WidgetMessage{}, &WidgetDeltaMessage{}, &RateLimitMessage{}, &RawMessage{}, &LogMessage{}, &StrippedEnvMessage{}, &DiffStatMessage{}, &ExitMessage{}, &MetaMessage{}, &MetaSessionMessage{}, &RelayGenerationMessage{}, &ModelInfoMessage{}, &MetaResultMessage{}, &MetaPRMessage{}}
	for _, message := range messages {
		if message.Provenance() == nil {
			t.Fatalf("%T has no owned provenance", message)
		}
		if !message.Provenance().IsZero() {
			t.Errorf("%T unknown provenance is nonzero", message)
		}
		*message.Provenance() = source
		if *message.Provenance() != source {
			t.Errorf("%T did not retain owned provenance", message)
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		var decoded taskslogv3.MessageSource
		if err := json.Unmarshal(fields["source"], &decoded); err != nil {
			t.Fatalf("%T source missing: %s, %v", message, encoded, err)
		}
		if decoded != source {
			t.Errorf("%T serialized source = %+v", message, decoded)
		}
	}
	t.Run("does not promote source zero query", func(t *testing.T) {
		t.Parallel()
		message := &ToolUseMessage{Name: "Bash", ToolUseID: "tool"}
		if _, promoted := any(message).(interface{ IsZero() bool }); promoted {
			t.Fatal("source IsZero promoted onto message")
		}
		encoded, err := json.Marshal(struct {
			Message Message `json:"message,omitzero"`
		}{Message: message})
		if err != nil {
			t.Fatal(err)
		}
		var wrapper struct {
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(encoded, &wrapper); err != nil {
			t.Fatal(err)
		}
		if len(wrapper.Message) == 0 {
			t.Fatalf("zero source hid a nonzero tool message: %s", encoded)
		}
	})
}

func TestToolKeyForSource(t *testing.T) {
	t.Parallel()
	t.Run("owners and turns", func(t *testing.T) {
		t.Parallel()
		root := &ToolUseMessage{Source: taskslogv3.MessageSource{ThreadID: "root", TurnID: "turn"}}
		child := &ToolUseMessage{Source: taskslogv3.MessageSource{ThreadID: "child", TurnID: "turn"}}
		otherTurn := &ToolUseMessage{Source: taskslogv3.MessageSource{ThreadID: "child", TurnID: "other-turn"}}
		keys := map[ToolKey]struct{}{}
		for _, m := range []Message{root, child, otherTurn} {
			keys[ToolKeyForSource(m.Provenance(), "same-tool")] = struct{}{}
		}
		if len(keys) != 3 {
			t.Fatalf("distinct owner/turn keys = %d, want 3", len(keys))
		}
	})
	t.Run("late correlation and sessions", func(t *testing.T) {
		t.Parallel()
		m := &ToolUseMessage{Source: taskslogv3.MessageSource{ThreadID: "session", DelegationToolUseID: "delegation"}}
		before := ToolKeyForSource(m.Provenance(), "local-tool")
		m.Source.AgentID = "canonical-child"
		m.Source.ItemID = "completion-item"
		m.Source.RootThreadID = "root"
		if after := ToolKeyForSource(m.Provenance(), "local-tool"); before != after {
			t.Fatalf("late correlation changed key: %+v -> %+v", before, after)
		}
		m.Source.ThreadID = "next-session"
		if ToolKeyForSource(m.Provenance(), "local-tool") == before {
			t.Fatal("different sessions share a tool key")
		}
	})
}
