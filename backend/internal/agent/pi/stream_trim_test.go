// Tests which streamed Pi deltas a settled log drops.

package pi

import (
	"slices"
	"strconv"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestStreamTrim(t *testing.T) {
	t.Parallel()
	update := func(typ string, index int) string {
		return `{"type":"message_update","assistantMessageEvent":{"type":"` + typ + `","contentIndex":` + strconv.Itoa(index) + `,"delta":"x"},"usage":{"output":1}}`
	}
	end := func(role, stop string) string {
		return `{"type":"message_end","message":{"role":"` + role + `","content":[],"stopReason":"` + stop + `"}}`
	}
	const start = `{"type":"message_start","message":{"role":"assistant","content":[]}}`
	const uiRequest = `{"type":"extension_ui_request","id":"u","method":"setStatus"}`
	for _, test := range []struct {
		name string
		log  []string
		want []int
	}{
		{
			"settled blocks",
			[]string{
				start,
				update("thinking_start", 0), update("thinking_delta", 0), update("thinking_delta", 0), update("thinking_end", 0),
				update("text_start", 1), update("text_delta", 1), update("text_delta", 1), update("text_end", 1),
				update("toolcall_start", 2), update("toolcall_delta", 2), update("toolcall_end", 2),
				end("assistant", "toolUse"),
			},
			[]int{3, 6, 7, 10},
		},
		{
			"first delta after another record",
			[]string{
				start, update("text_delta", 0), update("text_delta", 0), uiRequest, update("text_delta", 0), update("text_delta", 0),
				update("text_end", 0), end("assistant", "stop"),
			},
			[]int{2, 5},
		},
		{
			"unfinished message",
			[]string{start, update("text_delta", 0), update("text_delta", 0), update("text_end", 0)},
			nil,
		},
		{
			"block without end",
			[]string{
				start, update("text_delta", 0), update("text_delta", 0), update("text_end", 0), update("text_delta", 1),
				end("assistant", "aborted"),
			},
			[]int{2},
		},
		{
			"error stop",
			[]string{start, update("text_delta", 0), update("text_delta", 0), update("text_end", 0), end("assistant", "error")},
			nil,
		},
		{
			"multiple messages reuse indexes",
			[]string{
				start, update("text_delta", 0), update("text_delta", 0), update("text_end", 0), end("assistant", "toolUse"),
				`{"type":"tool_execution_end","toolCallId":"t","toolName":"bash","result":{"content":[]}}`,
				`{"type":"message_start","message":{"role":"toolResult"}}`, end("toolResult", ""),
				start, update("text_delta", 0), update("text_delta", 0), update("text_end", 0),
			},
			[]int{2},
		},
		{
			"update after message_end",
			[]string{start, update("text_end", 0), end("assistant", "stop"), update("toolcall_delta", 0)},
			nil,
		},
		{
			"done and error updates",
			[]string{
				start, update("text_delta", 0), update("text_delta", 0), update("text_end", 0), update("done", 0), update("error", 0),
				end("assistant", "stop"),
			},
			[]int{2},
		},
		{
			"no content index",
			[]string{
				start, update("text_delta", 0), `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"x"}}`,
				`{"type":"message_update","assistantMessageEvent":{"type":"text_end","content":"x"}}`, end("assistant", "stop"),
			},
			nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			trimmer, ok := New("", nil).NewWire().(agent.StreamTrimmer)
			if !ok {
				t.Fatal("Pi wire has no stream trim rule")
			}
			trim := trimmer.NewStreamTrim()
			for _, line := range test.log {
				if err := trim.Observe([]byte(line)); err != nil {
					t.Fatal(err)
				}
			}
			var got []int
			for i, line := range test.log {
				superseded, err := trim.Superseded([]byte(line))
				if err != nil {
					t.Fatal(err)
				}
				if superseded {
					got = append(got, i)
				}
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("superseded records = %v, want %v", got, test.want)
			}
		})
	}
}
