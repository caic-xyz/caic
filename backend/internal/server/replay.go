// History replay filtering for task SSE: drops streaming deltas superseded by a final message.

package server

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// filterHistoryForReplay removes streaming delta messages that have a
// corresponding final message later in the history. TextDeltaMessage runs
// preceding a TextMessage and ThinkingDeltaMessage runs preceding a
// ThinkingMessage are omitted — the frontend uses only the final message when
// available, so the deltas are pure waste during history replay.
func filterHistoryForReplay(msgs []agent.Message) []agent.Message {
	filter := newHistoryReplayFilter(agentReplayHistory(msgs))
	out := make([]agent.Message, 0, len(msgs))
	for i, msg := range msgs {
		if !filter.Skip(i) {
			out = append(out, msg)
		}
	}
	return out
}

type replayHistory interface {
	Len() int
	At(index int) agent.Message
}

type agentReplayHistory []agent.Message

func (h agentReplayHistory) Len() int               { return len(h) }
func (h agentReplayHistory) At(i int) agent.Message { return h[i] }

type timelineReplayHistory struct {
	snapshot task.TimelineSnapshot
}

func (h timelineReplayHistory) Len() int               { return h.snapshot.Len() }
func (h timelineReplayHistory) At(i int) agent.Message { return h.snapshot.At(i).Message }

type historyReplayFilter[H replayHistory] struct {
	history           H
	skipUntil         int
	keepUntil         int
	cleanTurnComplete bool
}

func newHistoryReplayFilter[H replayHistory](history H) historyReplayFilter[H] {
	return historyReplayFilter[H]{history: history}
}

func (f *historyReplayFilter[H]) Skip(i int) bool {
	if i < f.skipUntil {
		return true
	}
	if i >= f.keepUntil {
		end, superseded := deltaRunEnd(f.history, i)
		if superseded {
			f.skipUntil = end
			return true
		}
		f.keepUntil = end
	} else {
		return false
	}
	msg := f.history.At(i)
	if exit, ok := msg.(*agent.ExitMessage); ok {
		if exit.ExitCode != 0 && f.cleanTurnComplete {
			return true
		}
	} else if task.ClearsExitError(msg) {
		f.cleanTurnComplete = false
	}
	if result, ok := msg.(*agent.ResultMessage); ok {
		f.cleanTurnComplete = !result.IsError
	}
	return false
}

func deltaRunEnd[H replayHistory](history H, start int) (int, bool) {
	switch first := history.At(start).(type) {
	case *agent.TextDeltaMessage:
		end := start + 1
		for end < history.Len() {
			if _, ok := history.At(end).(*agent.TextDeltaMessage); !ok {
				break
			}
			end++
		}
		if end < history.Len() {
			if _, ok := history.At(end).(*agent.TextMessage); ok {
				return end, true
			}
		}
		return end, false
	case *agent.ThinkingDeltaMessage:
		end := start + 1
		for end < history.Len() {
			if _, ok := history.At(end).(*agent.ThinkingDeltaMessage); !ok {
				break
			}
			end++
		}
		if end < history.Len() {
			if _, ok := history.At(end).(*agent.ThinkingMessage); ok {
				return end, true
			}
		}
		return end, false
	case *agent.WidgetDeltaMessage:
		end := start + 1
		for end < history.Len() {
			if _, ok := history.At(end).(*agent.WidgetDeltaMessage); !ok {
				break
			}
			end++
		}
		if end < history.Len() {
			if _, ok := history.At(end).(*agent.WidgetMessage); ok {
				return end, true
			}
		}
		return end, false
	case *agent.ToolOutputDeltaMessage:
		end := start + 1
		for end < history.Len() {
			delta, ok := history.At(end).(*agent.ToolOutputDeltaMessage)
			if !ok || delta.ToolUseID != first.ToolUseID {
				break
			}
			end++
		}
		if end < history.Len() {
			if result, ok := history.At(end).(*agent.ToolResultMessage); ok && result.ToolUseID == first.ToolUseID {
				return end, true
			}
		}
		return end, false
	}
	return start, false
}
