// Shared task event folds reconcile root results, exit diagnostics, and input acceptance.

package task

import (
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// ResultTextWindow accumulates the root assistant text of the current
// turn so a streaming consumer can produce the fallback result text at the
// point a ResultMessage arrives. Any other message type (a boundary, see
// fallbackBoundary) resets the window; finalized TextMessages accumulate
// with consecutive duplicates collapsed, and TextDeltaMessages count only
// until the first finalized text.
type ResultTextWindow struct {
	texts []string
	delta strings.Builder
}

// Update folds one message into the window.
func (w *ResultTextWindow) Update(msg agent.Message) {
	if msg.Provenance().IsChild() {
		return
	}
	if fallbackBoundary(msg) {
		w.reset()
		return
	}
	switch m := msg.(type) {
	case *agent.TextMessage:
		text := strings.TrimSpace(m.Text)
		if text == "" {
			return
		}
		if len(w.texts) == 0 || w.texts[len(w.texts)-1] != text {
			w.texts = append(w.texts, text)
		}
	case *agent.TextDeltaMessage:
		if len(w.texts) == 0 {
			w.delta.WriteString(m.Text)
		}
	}
}

// Value returns the accumulated fallback text: finalized messages joined by
// blank lines, or the trimmed delta stream when none arrived.
func (w *ResultTextWindow) Value() string {
	if len(w.texts) > 0 {
		return strings.Join(w.texts, "\n\n")
	}
	return strings.TrimSpace(w.delta.String())
}

func (w *ResultTextWindow) reset() {
	w.texts = nil
	w.delta.Reset()
}

// ExitStatus reconciles relay termination with root turn and input delivery
// evidence. The zero value is ready for an ordered stream of messages.
type ExitStatus struct {
	// Error is the latest exit diagnostic after reconciliation. Callers must
	// treat it as read-only and update the status through Observe.
	Error string

	clean        bool
	inputPending bool
}

// Observe folds one message and reports whether it should be published.
// A marked input attempt preserves the prior completion until delivery succeeds
// or actual root output arrives. Failed and truncated attempts cannot invalidate
// a completed turn. Unmarked inputs begin a turn immediately.
func (s *ExitStatus) Observe(msg agent.Message) bool {
	if msg.Provenance().IsChild() {
		return true
	}
	switch m := msg.(type) {
	case *agent.ExitMessage:
		if m.ExitCode == 0 || s.clean {
			s.Error = ""
			return m.ExitCode == 0
		}
		s.Error = m.ExitError()
		return true
	case *agent.SystemMessage:
		switch m.Subtype {
		case agent.SystemSubtypeInputDeliveryStarted:
			s.inputPending = true
			return true
		case agent.SystemSubtypeInputDeliveryFailed:
			s.inputPending = false
			return true
		case agent.SystemSubtypeInputDeliveryCompleted:
			if s.inputPending {
				s.inputPending = false
				s.clean = false
				s.Error = ""
			}
			return true
		}
	case *agent.UserInputMessage:
		if s.inputPending {
			return true
		}
	}
	if s.inputPending {
		if !AcceptedInputOutput(msg) {
			return true
		}
		s.inputPending = false
		s.clean = false
		s.Error = ""
	}
	if ClearsExitError(msg) {
		s.Error = ""
		s.clean = false
		s.inputPending = false
	}
	if result, ok := msg.(*agent.ResultMessage); ok {
		s.clean = !result.IsError
		s.inputPending = false
	}
	return true
}

// ClearsExitError reports whether accepted activity clears the last exit error from a
// prior turn. Messages that accompany a turn without starting a new one (exit,
// diff stat, native subagent updates, background command updates, raw relay
// lines, pending user actions, metadata updates, log output, stripped env) never
// clear it; a ResultMessage clears it only when the turn succeeded; every
// other message starts a new turn. Ordered folds use ExitStatus to defer this
// interpretation for input attempts whose delivery is not yet established.
func ClearsExitError(msg agent.Message) bool {
	if msg.Provenance().IsChild() || agent.IsSessionMetadataUpdate(msg) {
		return false
	}
	switch m := msg.(type) {
	case *agent.SystemMessage:
		return m.Subtype != agent.SystemSubtypeExternalActivity && !agent.IsInputDeliveryControl(m)
	case *agent.ExitMessage, *agent.DiffStatMessage, *agent.TurnCommitSnapshotMessage, *agent.NativeSubagentMessage, *agent.RawMessage,
		*agent.BackgroundCommandMessage,
		*agent.PendingUserActionMessage,
		*agent.LogMessage, *agent.StrippedEnvMessage:
		return false
	case *agent.ResultMessage:
		return !m.IsError
	default:
		return true
	}
}

// AcceptedInputOutput reports whether root agent output proves an input was accepted.
func AcceptedInputOutput(m agent.Message) bool {
	if m.Provenance().IsChild() {
		return false
	}
	switch m.(type) {
	case *agent.TextMessage, *agent.TextDeltaMessage,
		*agent.ThinkingMessage, *agent.ThinkingDeltaMessage,
		*agent.ToolUseMessage, *agent.ToolResultMessage, *agent.ToolOutputDeltaMessage,
		*agent.SkillReadMessage, *agent.AskMessage, *agent.PendingUserActionMessage,
		*agent.TodoMessage, *agent.ResultMessage,
		*agent.NativeSubagentMessage, *agent.BackgroundCommandMessage,
		*agent.WidgetMessage, *agent.WidgetDeltaMessage:
		return true
	default:
		return false
	}
}

func fallbackBoundary(msg agent.Message) bool {
	switch msg.(type) {
	case *agent.ResultMessage, *agent.ToolUseMessage, *agent.ToolResultMessage,
		*agent.ThinkingMessage, *agent.ThinkingDeltaMessage:
		return true
	default:
		return false
	}
}
