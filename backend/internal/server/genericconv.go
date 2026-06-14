// Server-local conversion wrappers and replay filtering for task SSE.

package server

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
)

// filterHistoryForReplay removes streaming delta messages that have a
// corresponding final message later in the history. TextDeltaMessage runs
// preceding a TextMessage and ThinkingDeltaMessage runs preceding a
// ThinkingMessage are omitted — the frontend uses only the final message when
// available, so the deltas are pure waste during history replay.
func filterHistoryForReplay(msgs []agent.Message) []agent.Message {
	skip := make([]bool, len(msgs))
	for i, msg := range msgs {
		switch m := msg.(type) {
		case *agent.TextMessage:
			for j := i - 1; j >= 0; j-- {
				if _, ok := msgs[j].(*agent.TextDeltaMessage); ok {
					skip[j] = true
				} else {
					break
				}
			}
		case *agent.ThinkingMessage:
			for j := i - 1; j >= 0; j-- {
				if _, ok := msgs[j].(*agent.ThinkingDeltaMessage); ok {
					skip[j] = true
				} else {
					break
				}
			}
		case *agent.WidgetMessage:
			for j := i - 1; j >= 0; j-- {
				if _, ok := msgs[j].(*agent.WidgetDeltaMessage); ok {
					skip[j] = true
				} else {
					break
				}
			}
		case *agent.ToolResultMessage:
			for j := i - 1; j >= 0; j-- {
				if td, ok := msgs[j].(*agent.ToolOutputDeltaMessage); ok && td.ToolUseID == m.ToolUseID {
					skip[j] = true
				} else {
					break
				}
			}
		}
	}
	out := make([]agent.Message, 0, len(msgs))
	cleanTurnComplete := false
	for i, msg := range msgs {
		if skip[i] {
			continue
		}
		if exit, ok := msg.(*agent.ExitMessage); ok {
			if exit.ExitCode != 0 && cleanTurnComplete {
				continue
			}
		} else if replayClearsExit(msg) {
			cleanTurnComplete = false
		}
		if rm, ok := msg.(*agent.ResultMessage); ok {
			cleanTurnComplete = !rm.IsError
		}
		out = append(out, msg)
	}
	return out
}

func replayClearsExit(msg agent.Message) bool {
	switch m := msg.(type) {
	case *agent.ExitMessage, *agent.DiffStatMessage, *agent.RawMessage,
		*agent.ParseErrorMessage, *agent.LogMessage, *agent.StrippedEnvMessage:
		return false
	case *agent.ResultMessage:
		return !m.IsError
	default:
		return true
	}
}

// newReplayFilter is the streaming equivalent of filterHistoryForReplay: it
// collapses a contiguous run of streaming-delta messages when the matching
// consolidated message immediately follows, emitting surviving messages in
// order via emit. It buffers at most one contiguous delta run (a single
// assistant turn), so memory stays bounded regardless of history length.
//
// It returns push (feed one message) and flush (emit any buffered tail run);
// flush must be called once after the last message.
func newReplayFilter(emit func(agent.Message)) (push func(agent.Message), flush func()) {
	return eventreplay.NewFilter(emit)
}
