// Tests recent previews for bounded semantic content, streaming identity, and tool context.

package server

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
)

func previewEvents(messages []agent.Message, projected bool) []v1.EventMessage {
	preview := newTaskPreview()
	tracker := apiconv.NewToolTimingTracker(harness.Claude, "", nil, nil)
	if projected {
		events := make([]v1.EventMessage, 0, len(messages))
		for i, message := range messages {
			events = append(events, tracker.ConvertMessage(message, time.Unix(int64(i+1), 0))...)
		}
		for i := len(events) - 1; i >= 0; i-- {
			record := projectedPreviewRecord(&events[i])
			if !preview.addRecord(&record) {
				break
			}
		}
	} else {
		for i, message := range slices.Backward(messages) {
			if !preview.add(agent.TimedMessage{Message: message, ProducerTime: time.Unix(int64(i+1), 0)}) {
				break
			}
		}
	}
	return preview.events(tracker)
}

func TestTaskPreview(t *testing.T) {
	t.Parallel()
	for _, projected := range []bool{false, true} {
		mode := "semantic"
		if projected {
			mode = "projected"
		}
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			t.Run("repeated session metadata stays bounded", func(t *testing.T) {
				t.Parallel()
				messages := make([]agent.Message, 1000)
				for i := range messages {
					messages[i] = &agent.InitMessage{SessionID: "session"}
				}
				events := previewEvents(messages, projected)
				if len(events) != 1 || events[0].Init == nil {
					t.Fatalf("repeated session metadata retained: %d events", len(events))
				}
			})
			t.Run("omitted boundaries cannot supply result text", func(t *testing.T) {
				t.Parallel()
				messages := make([]agent.Message, 0, 64)
				messages = append(messages, &agent.TextMessage{Text: "Previous turn"}, &agent.ResultMessage{}, &agent.UserInputMessage{Text: "New turn"})
				for i := range 60 {
					messages = append(messages, &agent.ToolUseMessage{ToolUseID: strconv.Itoa(i), Name: "Read"})
				}
				messages = append(messages, &agent.ResultMessage{})
				events := previewEvents(messages, projected)
				last := events[len(events)-1]
				if last.Result == nil || last.Result.Result != "" {
					t.Fatalf("invented result from omitted turn context: %+v", last)
				}
			})
			t.Run("recent window ignores metadata", func(t *testing.T) {
				t.Parallel()
				messages := make([]agent.Message, 0, 320)
				for i := range 80 {
					messages = append(messages, &agent.TextMessage{Text: fmt.Sprintf("message %d", i)}, &agent.UsageMessage{}, &agent.DiffStatMessage{}, &agent.LogMessage{Line: "metadata"})
				}
				events := previewEvents(messages, projected)
				if len(events) != 50 || events[0].Text.Text != "message 30" || events[49].Text.Text != "message 79" {
					t.Fatalf("wrong recent window: %+v", events)
				}
				if events[0].Ts != 121000 {
					t.Errorf("timestamp=%d", events[0].Ts)
				}
			})
			t.Run("latest response and a tool start before session init extend window", func(t *testing.T) {
				t.Parallel()
				messages := make([]agent.Message, 0, 79)
				messages = append(messages, &agent.ToolUseMessage{ToolUseID: "old", Name: "Bash"}, &agent.InitMessage{SessionID: "new-session"}, &agent.TextMessage{Text: "Latest explanation"})
				for i := range 75 {
					messages = append(messages, &agent.ToolUseMessage{ToolUseID: strconv.Itoa(i), Name: "Read"})
				}
				messages = append(messages, &agent.ToolResultMessage{ToolUseID: "old"})
				events := previewEvents(messages, projected)
				if len(events) != 52 || events[0].ToolUse == nil || events[0].ToolUse.ToolUseID != "old" || events[1].Text == nil || events[1].Text.Text != "Latest explanation" || events[51].ToolResult == nil {
					t.Fatalf("missing extra context: %+v", events)
				}
			})
			t.Run("coalesces interleaved streams without child output", func(t *testing.T) {
				t.Parallel()
				a := agent.MessageSource{ThreadID: "parent", RootThreadID: "parent", TurnID: "turn", ItemID: "a"}
				b := a
				b.ItemID = "b"
				earlyA := a
				earlyA.RootThreadID = ""
				child := agent.MessageSource{ThreadID: "child", RootThreadID: "parent", TurnID: "turn", ItemID: "a"}
				events := previewEvents([]agent.Message{
					&agent.UserInputMessage{Text: "Start"},
					&agent.TextDeltaMessage{Source: earlyA, Text: "one "},
					&agent.TextDeltaMessage{Source: b, Text: "two "},
					&agent.TextMessage{Source: child, Text: "child"},
					&agent.TextDeltaMessage{Source: a, Text: "answer"},
					&agent.TextDeltaMessage{Source: b, Text: "answer"},
				}, projected)
				if len(events) != 3 || events[1].TextDelta.Text != "one answer" || events[2].TextDelta.Text != "two answer" || events[1].Source.ItemID != "a" || events[2].Source.ItemID != "b" {
					t.Fatalf("mixed streams: %+v", events)
				}
			})
			t.Run("completed messages replace fragments including empty finals", func(t *testing.T) {
				t.Parallel()
				source := agent.MessageSource{ThreadID: "root", TurnID: "turn", ItemID: "message"}
				events := previewEvents([]agent.Message{
					&agent.TextDeltaMessage{Text: strings.Repeat("fragment", 100)},
					&agent.TextMessage{Text: "Complete"},
					&agent.TextDeltaMessage{Source: source, Text: "Discarded"},
					&agent.TextMessage{Source: source},
				}, projected)
				if len(events) != 2 || events[0].Text.Text != "Complete" || events[1].Text.Text != "" {
					t.Fatalf("superseded fragments survived: %+v", events)
				}
			})
			t.Run("widget snapshots replace streamed HTML and retain tool context", func(t *testing.T) {
				t.Parallel()
				messages := make([]agent.Message, 0, 78)
				messages = append(messages, &agent.TextMessage{Text: "Widget explanation"}, &agent.WidgetDeltaMessage{ToolUseID: "widget", Delta: "old HTML"}, &agent.WidgetMessage{ToolUseID: "widget", Title: "Chart", HTML: "<p>Chart</p>"})
				for i := range 74 {
					messages = append(messages, &agent.ToolUseMessage{ToolUseID: strconv.Itoa(i), Name: "Read"})
				}
				messages = append(messages, &agent.ToolResultMessage{ToolUseID: "widget"})
				events := previewEvents(messages, projected)
				if len(events) != 52 || events[0].Text == nil || events[1].Widget == nil || events[1].Widget.HTML != "<p>Chart</p>" || events[51].ToolResult == nil {
					t.Fatalf("widget context missing: %+v", events)
				}
			})
			t.Run("unfinished text cannot merge across turns", func(t *testing.T) {
				t.Parallel()
				events := previewEvents([]agent.Message{
					&agent.TextDeltaMessage{Text: "older"}, &agent.UserInputMessage{Text: "next"}, &agent.TextDeltaMessage{Text: "new"},
				}, projected)
				if len(events) != 3 || events[0].TextDelta.Text != "older" || events[2].TextDelta.Text != "new" {
					t.Fatalf("merged turns: %+v", events)
				}
			})
			t.Run("tool fragments collapse and matching starts retain provenance", func(t *testing.T) {
				t.Parallel()
				source := agent.MessageSource{ThreadID: "root", TurnID: "turn", ItemID: "call"}
				other := source
				other.TurnID = "other-turn"
				events := previewEvents([]agent.Message{
					&agent.TextMessage{Text: "Running"},
					&agent.ToolUseMessage{Source: other, ToolUseID: "shared", Name: "Other"},
					&agent.ToolUseMessage{Source: source, ToolUseID: "shared", Name: "Bash"},
					&agent.ToolOutputDeltaMessage{Source: source, ToolUseID: "shared", Delta: "one"},
					&agent.ToolOutputDeltaMessage{Source: source, ToolUseID: "shared", Delta: "two"},
				}, projected)
				if len(events) != 4 || events[3].ToolOutputDelta.Delta != "onetwo" || events[2].Source.TurnID != "turn" {
					t.Fatalf("lost tool identity: %+v", events)
				}
			})
		})
	}
}
