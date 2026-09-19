// Native-subagent replay state preserves only lifecycle facts reported by a harness.

package agent

// NativeSubagentTimeline folds native-subagent observations into one stable
// card per harness identity.
type NativeSubagentTimeline struct {
	byID  map[string]NativeSubagent
	order []string
}

// Apply records an observation. Empty identities cannot be correlated safely
// and are ignored. Repeated observations enrich missing optional information,
// except that a terminal observation's result upgrades an earlier interim one;
// once terminal, a lifecycle cannot be reopened by replayed history and its result
// is not replaced by a later terminal report.
func (t *NativeSubagentTimeline) Apply(s *NativeSubagent) {
	if s == nil {
		return
	}
	observed := *s
	if observed.ID == "" {
		return
	}
	observed.Status = normalizedNativeSubagentStatus(observed.Status)
	if t.byID == nil {
		t.byID = make(map[string]NativeSubagent)
	}
	old, found := t.byID[observed.ID]
	if !found {
		t.byID[observed.ID] = observed
		t.order = append(t.order, observed.ID)
		return
	}
	if old.ToolUseID == "" {
		old.ToolUseID = observed.ToolUseID
	}
	if old.Scope == "" {
		old.Scope = observed.Scope
	}
	if old.GroupID == "" {
		old.GroupID = observed.GroupID
	}
	if old.Label == "" {
		old.Label = observed.Label
	}
	if old.Prompt == "" {
		old.Prompt = observed.Prompt
	}
	// A terminal observation upgrades an interim result (a paused run's artifact
	// reference, for example), but terminal results do not overwrite each other:
	// the first terminal report stays the outcome.
	if observed.Result != "" && (old.Result == "" || (!old.Status.Terminal() && observed.Status.Terminal())) {
		old.Result = observed.Result
	}
	if !old.Status.Terminal() && observed.Status != NativeSubagentStatusUnknown {
		old.Status = observed.Status
	}
	t.byID[observed.ID] = old
}

// Subagents returns the replayed cards in first-observed order. The returned
// slice is independent of the timeline's internal ordering.
func (t *NativeSubagentTimeline) Subagents() []NativeSubagent {
	out := make([]NativeSubagent, 0, len(t.order))
	for _, id := range t.order {
		out = append(out, t.byID[id])
	}
	return out
}

// Active returns the number of cards whose latest observed status is running.
// Unknown, paused, and terminal cards are not active.
func (t *NativeSubagentTimeline) Active() int {
	active := 0
	for _, id := range t.order {
		if t.byID[id].Status == NativeSubagentStatusRunning {
			active++
		}
	}
	return active
}

func normalizedNativeSubagentStatus(s NativeSubagentStatus) NativeSubagentStatus {
	switch s {
	case NativeSubagentStatusRunning, NativeSubagentStatusPaused, NativeSubagentStatusCompleted, NativeSubagentStatusFailed, NativeSubagentStatusInterrupted:
		return s
	default:
		return NativeSubagentStatusUnknown
	}
}

// Observe folds a parser observation and emits only changed lifecycle facts.
// It is shared by the stateful harness adapters, including during raw-log replay.
func (t *NativeSubagentTimeline) Observe(s *NativeSubagent) []Message {
	old, found := t.byID[s.ID]
	t.Apply(s)
	current, valid := t.byID[s.ID]
	if !valid || (found && old == current) {
		return nil
	}
	return []Message{&NativeSubagentMessage{Subagent: current}}
}
