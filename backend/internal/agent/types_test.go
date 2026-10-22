// Tests for shared agent message and quota provider types.

package agent

import (
	"encoding/json"
	"testing"
)

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
			source MessageSource
			zero   bool
		}{
			{name: "absent", zero: true},
			{name: "root only", source: MessageSource{RootThreadID: "root"}},
			{name: "thread only", source: MessageSource{ThreadID: "thread"}},
			{name: "turn only", source: MessageSource{TurnID: "turn"}},
			{name: "item only", source: MessageSource{ItemID: "item"}},
			{name: "missing thread", source: MessageSource{TurnID: "turn", ItemID: "item"}},
			{name: "missing turn", source: MessageSource{ThreadID: "thread", ItemID: "item"}},
			{name: "missing item", source: MessageSource{ThreadID: "thread", TurnID: "turn"}},
			{name: "complete rootless", source: MessageSource{ThreadID: "thread", TurnID: "turn", ItemID: "item"}},
			{name: "complete rooted", source: MessageSource{ThreadID: "thread", TurnID: "turn", ItemID: "item", RootThreadID: "root"}},
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
						var decoded MessageSource
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
	source := MessageSource{AgentID: "native:child", DelegationToolUseID: "delegate", ThreadID: "thread", TurnID: "turn", ItemID: "item", RootThreadID: "root"}
	messages := []Message{&MCPRequestMessage{}, &InitMessage{}, &SystemMessage{}, &TextMessage{}, &ToolUseMessage{}, &SkillReadMessage{}, &SkillLoadMessage{}, &AskMessage{}, &PendingUserActionMessage{}, &TodoMessage{}, &UserInputMessage{}, &ToolResultMessage{}, &UsageMessage{}, &TurnCommitSnapshotMessage{}, &ResultMessage{}, &TextDeltaMessage{}, &ThinkingMessage{}, &ThinkingDeltaMessage{}, &ToolOutputDeltaMessage{}, &NativeSubagentMessage{}, &BackgroundCommandMessage{}, &WidgetMessage{}, &WidgetDeltaMessage{}, &RateLimitMessage{}, &RawMessage{}, &ParseErrorMessage{}, &LogMessage{}, &StrippedEnvMessage{}, &DiffStatMessage{}, &ExitMessage{}, &MetaMessage{}, &MetaSessionMessage{}, &RelayGenerationMessage{}, &ModelInfoMessage{}, &MetaResultMessage{}, &MetaPRMessage{}}
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
		var decoded MessageSource
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
		root := &ToolUseMessage{Source: MessageSource{ThreadID: "root", TurnID: "turn"}}
		child := &ToolUseMessage{Source: MessageSource{ThreadID: "child", TurnID: "turn"}}
		otherTurn := &ToolUseMessage{Source: MessageSource{ThreadID: "child", TurnID: "other-turn"}}
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
		m := &ToolUseMessage{Source: MessageSource{ThreadID: "session", DelegationToolUseID: "delegation"}}
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
