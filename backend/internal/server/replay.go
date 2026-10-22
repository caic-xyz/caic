// History replay filtering and backward-loading boundaries for task SSE streams, preserving provider message identity.

package server

import (
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// taskEventBackwardProjector finds the latest-response projection and event ID
// independently from canonical event conversion. Canonical conversion remains
// strictly ordered because its tracker carries cost, timing, and turn state.
type taskEventBackwardProjector struct {
	message  *v1.EventMessage
	sequence uint64
}

func newTaskEventBackwardProjector(history replayHistory) *taskEventBackwardProjector {
	p := &taskEventBackwardProjector{}
	var deltaParts []string
	var source agent.MessageSource
	for i := history.Len() - 1; i >= 0; i-- {
		msg := history.At(i)
		if msg.Provenance().IsChild() {
			continue
		}
		switch m := msg.(type) {
		case *agent.TextDeltaMessage:
			if m.Source.IsChild() {
				continue
			}
			if len(deltaParts) > 0 && source != m.Source {
				p.setDeltaParts(deltaParts, &source)
				return p
			}
			if m.Text != "" {
				source = m.Source
				deltaParts = append(deltaParts, m.Text)
				if p.sequence == 0 {
					p.sequence = uint64(i + 1)
				}
			}
		case *agent.TextMessage:
			if m.Source.IsChild() {
				continue
			}
			if len(deltaParts) > 0 {
				p.setDeltaParts(deltaParts, &source)
				return p
			}
			if m.Text != "" {
				p.sequence = uint64(i + 1)
				p.setText(m.Text, &m.Source, time.Time{})
				return p
			}
		case *agent.AskMessage:
			if len(deltaParts) > 0 {
				p.setDeltaParts(deltaParts, &source)
				return p
			}
			if len(m.Questions) > 0 {
				p.sequence = uint64(i + 1)
				p.setAsk(m, time.Time{})
				return p
			}
		case *agent.ResultMessage:
			if len(deltaParts) > 0 {
				p.setDeltaParts(deltaParts, &source)
				return p
			}
			if m.Result != "" {
				p.sequence = uint64(i + 1)
				p.setText(m.Result, &agent.MessageSource{}, time.Time{})
				return p
			}
		case *agent.UserInputMessage, *agent.InitMessage:
			if len(deltaParts) > 0 {
				p.setDeltaParts(deltaParts, &source)
			}
			return p
		}
	}
	if len(deltaParts) > 0 {
		p.setDeltaParts(deltaParts, &source)
	}
	return p
}

func (p *taskEventBackwardProjector) setText(text string, source *agent.MessageSource, at time.Time) {
	p.message = &v1.EventMessage{Kind: v1.EventKindText, Ts: backwardTimestamp(at), Text: &v1.EventText{Text: text}, Source: v1.EventMessageSource(*source)}
}

func (p *taskEventBackwardProjector) setDeltaParts(parts []string, source *agent.MessageSource) {
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	p.setText(strings.Join(parts, ""), source, time.Time{})
}

func (p *taskEventBackwardProjector) setAsk(msg *agent.AskMessage, at time.Time) {
	p.message = &v1.EventMessage{
		Source: v1.EventMessageSource(msg.Source),
		Kind:   v1.EventKindAsk,
		Ts:     backwardTimestamp(at),
		Ask:    &v1.EventAsk{ToolUseID: msg.ToolUseID, Questions: apiconv.AskQuestions(msg.Questions)},
	}
}

func backwardTimestamp(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixMilli()
}

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
	if result, ok := msg.(*agent.ResultMessage); ok && !result.Source.IsChild() {
		f.cleanTurnComplete = !result.IsError
	}
	return false
}

func deltaRunEnd[H replayHistory](history H, start int) (int, bool) {
	switch first := history.At(start).(type) {
	case *agent.TextDeltaMessage:
		end := start + 1
		for end < history.Len() {
			if next, ok := history.At(end).(*agent.TextDeltaMessage); !ok || first.Source != next.Source {
				break
			}
			end++
		}
		if end < history.Len() {
			if final, ok := history.At(end).(*agent.TextMessage); ok && first.Source == final.Source {
				return end, true
			}
		}
		return end, false
	case *agent.ThinkingDeltaMessage:
		end := start + 1
		for end < history.Len() {
			if next, ok := history.At(end).(*agent.ThinkingDeltaMessage); !ok || first.Source != next.Source {
				break
			}
			end++
		}
		if end < history.Len() {
			if final, ok := history.At(end).(*agent.ThinkingMessage); ok && first.Source == final.Source {
				return end, true
			}
		}
		return end, false
	case *agent.WidgetDeltaMessage:
		end := start + 1
		for end < history.Len() {
			if delta, ok := history.At(end).(*agent.WidgetDeltaMessage); !ok || agent.ToolKeyForSource(delta.Provenance(), delta.ToolUseID) != agent.ToolKeyForSource(first.Provenance(), first.ToolUseID) {
				break
			}
			end++
		}
		if end < history.Len() {
			if final, ok := history.At(end).(*agent.WidgetMessage); ok && agent.ToolKeyForSource(final.Provenance(), final.ToolUseID) == agent.ToolKeyForSource(first.Provenance(), first.ToolUseID) {
				return end, true
			}
		}
		return end, false
	case *agent.ToolOutputDeltaMessage:
		end := start + 1
		for end < history.Len() {
			delta, ok := history.At(end).(*agent.ToolOutputDeltaMessage)
			if !ok || agent.ToolKeyForSource(delta.Provenance(), delta.ToolUseID) != agent.ToolKeyForSource(first.Provenance(), first.ToolUseID) {
				break
			}
			end++
		}
		if end < history.Len() {
			if result, ok := history.At(end).(*agent.ToolResultMessage); ok && agent.ToolKeyForSource(result.Provenance(), result.ToolUseID) == agent.ToolKeyForSource(first.Provenance(), first.ToolUseID) {
				return end, true
			}
		}
		return end, false
	}
	return start, false
}
