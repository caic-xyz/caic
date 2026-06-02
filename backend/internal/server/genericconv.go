// Server-local conversion wrappers and replay filtering for task SSE.

package server

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
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
	for i, msg := range msgs {
		if !skip[i] {
			out = append(out, msg)
		}
	}
	return out
}

// replayDeltaKind classifies a streaming-delta message: 1=text, 2=thinking,
// 3=widget, 0=not a delta.
func replayDeltaKind(m agent.Message) int {
	switch m.(type) {
	case *agent.TextDeltaMessage:
		return 1
	case *agent.ThinkingDeltaMessage:
		return 2
	case *agent.WidgetDeltaMessage:
		return 3
	case *agent.ToolOutputDeltaMessage:
		return 4
	}
	return 0
}

// replayFinalKind classifies a consolidated message that supersedes a delta run
// of the same kind: 1=text, 2=thinking, 3=widget, 0=neither.
func replayFinalKind(m agent.Message) int {
	switch m.(type) {
	case *agent.TextMessage:
		return 1
	case *agent.ThinkingMessage:
		return 2
	case *agent.WidgetMessage:
		return 3
	case *agent.ToolResultMessage:
		return 4
	}
	return 0
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
	var pending []agent.Message // contiguous run of one delta kind, not yet emitted
	flush = func() {
		for _, m := range pending {
			emit(m)
		}
		pending = pending[:0]
	}
	push = func(m agent.Message) {
		if k := replayDeltaKind(m); k != 0 {
			if len(pending) > 0 && replayDeltaKind(pending[0]) != k {
				flush() // a different delta kind ends the current run
			}
			pending = append(pending, m)
			return
		}
		if replayFinalKind(m) != 0 && len(pending) > 0 && replayDeltaKind(pending[0]) == replayFinalKind(m) {
			pending = pending[:0] // consolidated message supersedes its delta run
		} else {
			flush()
		}
		emit(m)
	}
	return push, flush
}
