// Tests native OpenCode inferred skill reads, per-call deduplication, and edit fixture projection.

package opencode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
)

func TestTools(t *testing.T) {
	t.Parallel()
	t.Run("ToolCallUpdateInProgressInfersSkillRead", func(t *testing.T) {
		t.Parallel()
		// OpenCode has no Skill tool. Its read tool names the file in
		// filePath, and the arguments arrive only on the in-progress update.
		input := mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]any{
				"sessionId": "ses_1",
				"update": map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call_2",
					"status":        "in_progress",
					"title":         "read",
					"rawInput":      map[string]any{"filePath": "/home/user/.config/opencode/skill/customize-opencode/SKILL.md"},
				},
			},
		})
		msgs, err := parseTestMessage(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Fatalf("msgs = %d, want 3", len(msgs))
		}
		if _, ok := msgs[0].(*agent.ToolUseMessage); !ok {
			t.Fatalf("msgs[0] = %T, want *agent.ToolUseMessage", msgs[0])
		}
		read, ok := msgs[1].(*agent.SkillReadMessage)
		if !ok {
			t.Fatalf("msgs[1] = %T, want *agent.SkillReadMessage", msgs[1])
		}
		if read.Skill != "customize-opencode" || !read.Inferred || read.ToolUseID != "" {
			t.Errorf("SkillReadMessage = %+v", read)
		}
		wantLoad := agent.SkillLoadMessage{Skill: "customize-opencode", Path: "/home/user/.config/opencode/skill/customize-opencode/SKILL.md", SourceToolUseID: "call_2"}
		if load, ok := msgs[2].(*agent.SkillLoadMessage); !ok || *load != wantLoad {
			t.Errorf("msgs[2] = %#v, want %#v", msgs[2], wantLoad)
		}
	})
	t.Run("read edit bash fixture normalizes edit", func(t *testing.T) {
		t.Parallel()

		msgs := agenttest.ParseJSONL(t, "testdata/read-edit-bash.jsonl", New("", nil).NewWire())
		var use *agent.ToolUseMessage
		for _, msg := range msgs {
			candidate, ok := msg.(*agent.ToolUseMessage)
			if ok && candidate.Name == "Edit" && candidate.InputView.Kind == agent.ToolInputFileChanges {
				use = candidate
				break
			}
		}
		if use == nil {
			t.Fatal("no normalized Edit tool use found")
		}
		if use.Detail != "main.go" {
			t.Fatalf("detail = %q, want main.go", use.Detail)
		}
		if use.InputView.Kind != agent.ToolInputFileChanges {
			t.Fatalf("input view kind = %q, want fileChanges", use.InputView.Kind)
		}
		if len(use.InputView.Files) != 1 {
			t.Fatalf("files = %#v, want one file", use.InputView.Files)
		}
		file := use.InputView.Files[0]
		if file.Path != "/workspace/main.go" || !strings.Contains(file.Patch, "-Hello") || !strings.Contains(file.Patch, "+Hi") {
			t.Fatalf("file change = %#v", file)
		}
	})
}
func TestWireFormatSkillReadOncePerToolCall(t *testing.T) {
	t.Parallel()
	w := New("", nil).NewWire()
	line := mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": "ses_1", "update": map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": "call_1",
			"title": "bash", "kind": "execute", "status": "in_progress",
			"rawInput": map[string]any{"command": "cat ~/.agents/skills/code-quality/SKILL.md"},
		}},
	})
	reads := 0
	for range 3 {
		msgs, err := w.ParseOutput(line)
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range msgs {
			if _, ok := msg.(*agent.SkillReadMessage); ok {
				reads++
			}
		}
	}
	if reads != 1 {
		t.Errorf("skill reads = %d, want 1", reads)
	}
}
func parseTestMessage(line []byte) ([]agent.Message, error) {
	return New("", nil).NewWire().ParseOutput(line)
}
func mustJSON(t *testing.T, v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
