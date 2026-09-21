// Native-subagent and background-command replay state preserve only the lifecycle facts a harness reported.

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
	// Detachment is monotonic: once a harness launched a delegation detached from
	// the parent turn, a later observation cannot make it foreground again.
	if observed.Background {
		old.Background = true
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

// ActiveBackground returns the number of detached cards whose latest observed
// status is running. Only a delegation the harness launched independently of
// the parent turn can outlive that turn, so only these justify keeping a task
// running after its trailing result. A foreground card that is still reported
// running then is stale or unread lifecycle evidence, not detached work.
func (t *NativeSubagentTimeline) ActiveBackground() int {
	active := 0
	for _, id := range t.order {
		if s := t.byID[id]; s.Background && s.Status == NativeSubagentStatusRunning {
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

// BackgroundCommandTimeline folds background-command observations into one
// stable card per harness identity. It deliberately mirrors NativeSubagentTimeline
// instead of generalizing it: a shell command has no resumable semantics, no
// scope, and never influences task state, so the two invariants stay separate.
type BackgroundCommandTimeline struct {
	byID  map[string]BackgroundCommand
	order []string
}

// Apply records an observation. Empty identities cannot be correlated safely
// and are ignored. Repeated observations enrich missing optional information,
// except that a terminal observation's result upgrades an earlier interim one;
// once terminal, a lifecycle cannot be reopened by replayed history and its
// result is not replaced by a later terminal report.
func (t *BackgroundCommandTimeline) Apply(s *BackgroundCommand) {
	observed := *s
	if observed.ID == "" {
		return
	}
	if observed.Status != BackgroundCommandStatusRunning &&
		observed.Status != BackgroundCommandStatusCompleted &&
		observed.Status != BackgroundCommandStatusFailed &&
		observed.Status != BackgroundCommandStatusInterrupted {
		observed.Status = BackgroundCommandStatusRunning
	}
	if t.byID == nil {
		t.byID = make(map[string]BackgroundCommand)
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
	if old.Label == "" {
		old.Label = observed.Label
	}
	if old.OutputRef == "" {
		old.OutputRef = observed.OutputRef
	}
	if old.ExitCode == nil && observed.ExitCode != nil {
		code := *observed.ExitCode
		old.ExitCode = &code
	}
	// A terminal observation upgrades an interim result, but terminal results do
	// not overwrite each other: the first terminal report stays the outcome.
	if observed.Result != "" && (old.Result == "" || (!old.Status.Terminal() && observed.Status.Terminal())) {
		old.Result = observed.Result
	}
	if !old.Status.Terminal() {
		old.Status = observed.Status
	}
	t.byID[observed.ID] = old
}

// Commands returns the replayed cards in first-observed order. The returned
// slice is independent of the timeline's internal ordering.
func (t *BackgroundCommandTimeline) Commands() []BackgroundCommand {
	out := make([]BackgroundCommand, 0, len(t.order))
	for _, id := range t.order {
		out = append(out, t.byID[id])
	}
	return out
}

// Observe folds a parser observation and emits only changed lifecycle facts.
// It is shared by the stateful harness adapters, including during raw-log replay.
func (t *BackgroundCommandTimeline) Observe(s *BackgroundCommand) []Message {
	old, found := t.byID[s.ID]
	t.Apply(s)
	current, valid := t.byID[s.ID]
	if !valid || (found && old == current) {
		return nil
	}
	return []Message{&BackgroundCommandMessage{Command: current}}
}
