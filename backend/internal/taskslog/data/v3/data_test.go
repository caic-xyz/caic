// Tests for v3 provenance field queries and additive message-control encoding.

package v3

import (
	"encoding/json"
	"testing"
)

func TestMessageSource(t *testing.T) {
	t.Parallel()
	t.Run("HasIdentity", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			source   MessageSource
			complete bool
		}{
			{name: "absent"},
			{name: "agent only", source: MessageSource{AgentID: "native:child"}},
			{name: "delegation only", source: MessageSource{DelegationToolUseID: "delegate"}},
			{name: "root only", source: MessageSource{RootThreadID: "root"}},
			{name: "thread only", source: MessageSource{ThreadID: "thread"}},
			{name: "turn only", source: MessageSource{TurnID: "turn"}},
			{name: "item only", source: MessageSource{ItemID: "item"}},
			{name: "missing thread", source: MessageSource{TurnID: "turn", ItemID: "item"}},
			{name: "missing turn", source: MessageSource{ThreadID: "thread", ItemID: "item"}},
			{name: "missing item", source: MessageSource{ThreadID: "thread", TurnID: "turn"}},
			{name: "complete rootless", source: MessageSource{ThreadID: "thread", TurnID: "turn", ItemID: "item"}, complete: true},
			{name: "complete rooted", source: MessageSource{ThreadID: "thread", TurnID: "turn", ItemID: "item", RootThreadID: "root"}, complete: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := tc.source.HasIdentity(); got != tc.complete {
					t.Errorf("HasIdentity() = %t, want %t", got, tc.complete)
				}
			})
		}
	})

	t.Run("IsZero", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			source MessageSource
			zero   bool
		}{
			{name: "absent", zero: true},
			{name: "agent only", source: MessageSource{AgentID: "native:child"}},
			{name: "delegation only", source: MessageSource{DelegationToolUseID: "delegate"}},
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
				if got := tc.source.IsZero(); got != tc.zero {
					t.Errorf("IsZero() = %t, want %t", got, tc.zero)
				}
				encoded, err := json.Marshal(TextMessage{Source: tc.source})
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &fields); err != nil {
					t.Fatal(err)
				}
				raw, present := fields["source"]
				if present == tc.zero {
					t.Errorf("source presence = %t: %s", present, encoded)
				}
				if present {
					var source MessageSource
					if err := json.Unmarshal(raw, &source); err != nil {
						t.Fatal(err)
					}
					if source != tc.source {
						t.Errorf("serialized source = %+v, want %+v", source, tc.source)
					}
				}
			})
		}
	})
	t.Run("IsChild", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			source MessageSource
			child  bool
		}{
			{name: "absent"},
			{name: "canonical child", source: MessageSource{AgentID: "native:child"}, child: true},
			{name: "early delegation", source: MessageSource{DelegationToolUseID: "delegate"}, child: true},
			{name: "unknown owner", source: MessageSource{ThreadID: "child"}},
			{name: "unknown thread", source: MessageSource{RootThreadID: "root"}},
			{name: "root", source: MessageSource{ThreadID: "root", RootThreadID: "root"}},
			{name: "partial child", source: MessageSource{ThreadID: "child", RootThreadID: "root"}, child: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if got := tc.source.IsChild(); got != tc.child {
					t.Errorf("IsChild() = %t, want %t", got, tc.child)
				}
			})
		}
	})
}

func TestMCPRequestMessage(t *testing.T) {
	t.Parallel()
	t.Run("provenance", func(t *testing.T) {
		t.Parallel()
		source := MessageSource{DelegationToolUseID: "delegate"}
		original := MCPRequestMessage{Source: source, ID: "request", Method: "tools/list"}
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var restored MCPRequestMessage
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.Source != source {
			t.Errorf("MCP control source lost: %s", encoded)
		}
	})
}
