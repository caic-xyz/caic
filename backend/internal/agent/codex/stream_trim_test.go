// Tests which streamed Codex deltas a settled log drops.

package codex

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestStreamTrim(t *testing.T) {
	t.Parallel()
	notification := func(method, params string) string {
		return `{"jsonrpc":"2.0","method":"` + method + `","params":` + params + `}`
	}
	completed := func(thread, item string) string {
		return notification("item/completed", `{"threadId":"`+thread+`","turnId":"u","item":`+item+`}`)
	}
	text := func(method, thread, item, text string) string {
		return notification(method, `{"threadId":"`+thread+`","turnId":"u","itemId":"`+item+`","delta":"`+text+`"}`)
	}
	delta := func(method, thread, item string) string { return text(method, thread, item, "x") }
	command := func(item, output string) string {
		return completed("t", `{"type":"commandExecution","id":"`+item+`","command":"ls","status":"completed","aggregatedOutput":"`+output+`"}`)
	}
	const output = "item/commandExecution/outputDelta"
	observed := []string{
		completed("t", `{"type":"agentMessage","id":"msg","text":"x"}`),
		completed("t", `{"type":"reasoning","id":"rs","summary":["x"],"content":[]}`),
		`{"jsonrpc":"2.0","id":3,"result":{}}`,
		// The deltas of "cmd" add up to its aggregatedOutput; "partial"
		// lacks the output's prefix, and "running" never completes.
		text(output, "t", "cmd", "a"),
		text(output, "t", "partial", "b"),
		text(output, "t", "cmd", "b"),
		text(output, "t", "running", "a"),
		command("cmd", "ab"),
		command("partial", "ab"),
	}
	for _, test := range []struct {
		name       string
		line       string
		superseded bool
	}{
		{"completed agent message", delta("item/agentMessage/delta", "t", "msg"), true},
		{"completed reasoning summary", delta("item/reasoning/summaryTextDelta", "t", "rs"), true},
		{"uncompleted item", delta("item/agentMessage/delta", "t", "other"), false},
		{"other thread with the same item", delta("item/agentMessage/delta", "child", "msg"), false},
		{"command output repeated by its completion", text(output, "t", "cmd", "a"), true},
		{"command output its completion extends", text(output, "t", "partial", "b"), false},
		{"running command output", text(output, "t", "running", "a"), false},
		{"completed record", observed[0], false},
		{"turn completed", notification("turn/completed", `{"threadId":"t","turn":{"id":"u","status":"completed"}}`), false},
		{"response", observed[2], false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			trimmer, ok := New("", nil).NewWire().(agent.StreamTrimmer)
			if !ok {
				t.Fatal("Codex wire has no stream trim rule")
			}
			trim := trimmer.NewStreamTrim()
			for _, line := range observed {
				if err := trim.Observe([]byte(line)); err != nil {
					t.Fatal(err)
				}
			}
			superseded, err := trim.Superseded([]byte(test.line))
			if err != nil {
				t.Fatal(err)
			}
			if superseded != test.superseded {
				t.Fatalf("Superseded = %t, want %t", superseded, test.superseded)
			}
		})
	}
}
