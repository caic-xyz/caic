// Root turn metrics preserve native accounting and bound fallback clocks to root invocations.

package task

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// TurnMetrics accumulates completed root turns. Native invocation durations and
// turn counts take precedence; absent values use the elapsed producer time from
// the first root input or assistant activity and one completed root result.
// Idle time between results and the next activity does not contribute. A root
// context clear abandons unfinished timing; automatic compaction continues the
// current invocation. Reported skill calls are activity; inferred reads alone
// do not establish a clock independently of their owning tool.
type TurnMetrics struct {
	NumTurns int
	Duration time.Duration

	startedAt time.Time
	accepted  bool // Delivery completed or independent root output proves the turn survived a failed attempt.
}

// Observe folds an ordered message without changing its native evidence.
func (s *TurnMetrics) Observe(msg agent.Message, at time.Time) {
	if msg.Provenance().IsChild() {
		return
	}
	if !s.startedAt.IsZero() && AcceptedInputOutput(msg) {
		s.accepted = true
	}
	switch m := msg.(type) {
	case *agent.ResultMessage:
		s.NumTurns += max(m.NumTurns, 1)
		if duration, ok := agent.NativeDuration(m); ok {
			s.Duration += duration
		} else if !s.startedAt.IsZero() && at.After(s.startedAt) {
			s.Duration += at.Sub(s.startedAt)
		}
		s.startedAt = time.Time{}
		s.accepted = false
	case *agent.SystemMessage:
		switch m.Subtype {
		case "context_cleared":
			s.startedAt = time.Time{}
			s.accepted = false
		case agent.SystemSubtypeInputDeliveryCompleted:
			s.accepted = !s.startedAt.IsZero()
		case agent.SystemSubtypeInputDeliveryFailed:
			if !s.accepted {
				s.startedAt = time.Time{}
			}
		}
	case *agent.SkillReadMessage:
		if !m.Inferred && m.Skill != "" {
			if s.startedAt.IsZero() {
				s.startedAt = at
			}
			s.accepted = true
		}
	case *agent.UserInputMessage:
		if s.startedAt.IsZero() {
			s.startedAt = at
		}
	case *agent.TextMessage, *agent.TextDeltaMessage, *agent.ThinkingMessage,
		*agent.ThinkingDeltaMessage, *agent.ToolUseMessage, *agent.ToolResultMessage,
		*agent.ToolOutputDeltaMessage, *agent.AskMessage, *agent.TodoMessage,
		*agent.WidgetMessage, *agent.WidgetDeltaMessage:
		if s.startedAt.IsZero() {
			s.startedAt = at
		}
		s.accepted = true
	}
}
