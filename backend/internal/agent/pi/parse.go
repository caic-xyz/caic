// Pi event parser. Converts Pi's type-dispatched JSONL events into normalized
// agent.Message types.

package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/maruel/genai/providers/pi"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func decodeEventType(line []byte) (pi.EventType, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := consumeObjectStart(dec); err != nil {
		return "", err
	}
	var typ pi.EventType
	var foundType bool
	for dec.More() {
		key, err := nextObjectKey(dec)
		if err != nil {
			return "", err
		}
		if key == "type" {
			var decoded pi.EventType
			if err := dec.Decode(&decoded); err != nil {
				return "", err
			}
			if !foundType {
				typ = decoded
				foundType = true
			}
			continue
		}
		if err := discardValue(dec); err != nil {
			return "", err
		}
	}
	if err := validateUnknownEventRemainder(dec); err != nil {
		return "", err
	}
	return typ, nil
}

// decodeMessageUpdateEvent decodes only the assistantMessageEvent field,
// deliberately skipping the line's "message" field: Pi resends the full
// accumulated assistant message on every delta, and parsing it here would
// cost O(n²) over a turn's output.
func decodeMessageUpdateEvent(line []byte) (pi.MessageUpdateEvent, error) {
	var ev pi.MessageUpdateEvent
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := consumeObjectStart(dec); err != nil {
		return ev, err
	}
	for dec.More() {
		key, err := nextObjectKey(dec)
		if err != nil {
			return ev, err
		}
		if key == "assistantMessageEvent" {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return ev, err
			}
			if err := json.Unmarshal(raw, &ev.AssistantMessageEvent); err != nil {
				return ev, err
			}
			return ev, nil
		}
		if err := discardValue(dec); err != nil {
			return ev, err
		}
	}
	return ev, nil
}

func consumeObjectStart(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("JSON root is %T, want object", tok)
	}
	return nil
}

func nextObjectKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	key, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("JSON object key is %T, want string", tok)
	}
	return key, nil
}

func discardValue(dec *json.Decoder) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil && err != io.EOF {
		return err
	}
	return nil
}

func validateUnknownEventRemainder(dec *json.Decoder) error {
	for dec.More() {
		if _, err := nextObjectKey(dec); err != nil {
			return err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
	}

	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '}' {
		return fmt.Errorf("JSON object ends with %T, want closing brace", tok)
	}

	tok, err = dec.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("unexpected JSON token %v after object", tok)
}

// parseMessageTyped decodes a single JSONL line from Pi's stdout into one or
// more typed agent.Messages, dispatching on typ (the line's "type" field,
// already extracted by the caller via decodeEventType).
//
// The line is one of:
//   - A caic-injected JSON object with a "type" field (caic_diff_stat, etc.)
//   - A Pi event dispatched by the "type" field (message_update, tool_execution_end, etc.)
//   - A Pi response envelope (type:"response")
//   - A Pi command sent on stdin (prompt, compact, etc.) — logged by the relay
//
// Emitted agent.Message types:
//   - TextDeltaMessage     — message_update (text_delta)
//   - ThinkingDeltaMessage — message_update (thinking_delta)
//   - ToolUseMessage       — tool_execution_start
//   - ToolResultMessage    — tool_execution_end
//   - ToolOutputDeltaMessage — tool_execution_update
//   - DiffStatMessage      — caic_diff_stat injection
//   - UserInputMessage     — prompt command (stdin logged by relay)
//   - SystemMessage        — compaction start/boundary/error
//   - RawMessage           — unrecognised event types
func parseMessageTyped(typ pi.EventType, line []byte) ([]agent.Message, decodedRecord, error) {
	// caic-injected lines and stdin commands.
	switch typ {
	case pi.EventType("caic_model_info"):
		// Handled by wireFormat.ParseMessage; skip in stateless replay.
		return nil, decodedRecord{}, nil

	case pi.CmdPrompt:
		// Stdin prompt command logged by relay; convert to UserInputMessage.
		var cmd pi.PromptCmd
		if err := json.Unmarshal(line, &cmd); err != nil {
			return nil, decodedRecord{}, fmt.Errorf("unmarshal prompt cmd: %w", err)
		}
		ui := &agent.UserInputMessage{Text: cmd.Message}
		for _, img := range cmd.Images {
			ui.Images = append(ui.Images, agent.ImageData{
				MediaType: img.MimeType,
				Data:      img.Data,
			})
		}
		return []agent.Message{ui}, decodedRecord{}, nil

	case pi.CmdCompact:
		// Stdin compact command logged by relay; skip during replay.
		return nil, decodedRecord{}, nil

	case pi.EventType("caic_diff_stat"):
		var m agent.DiffStatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, decodedRecord{}, err
		}
		return []agent.Message{&m}, decodedRecord{}, nil

	case pi.EventType("caic_exit"):
		var m agent.ExitMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, decodedRecord{}, err
		}
		return []agent.Message{&m}, decodedRecord{}, nil

	case pi.EventMessageUpdate:
		msgs, err := parseMessageUpdate(line)
		return msgs, decodedRecord{}, err

	case pi.EventToolExecStart:
		msgs, ev, err := parseToolExecStart(line)
		return msgs, decodedRecord{start: ev}, err

	case pi.EventToolExecUpdate:
		msgs, err := parseToolExecUpdate(line)
		return msgs, decodedRecord{}, err

	case pi.EventToolExecEnd:
		msgs, ev, err := parseToolExecEnd(line)
		return msgs, decodedRecord{end: ev}, err

	case pi.EventAgentStart, pi.EventMessageStart, pi.EventMessageEnd, pi.EventTurnStart:
		// Lifecycle events with no semantic content; skip.
		return nil, decodedRecord{}, nil

	case pi.EventResponse:
		// Command responses (e.g. set_model ack); skip unless error.
		msgs, err := parseResponse(line)
		return msgs, decodedRecord{}, err

	case pi.EventCompactionStart:
		var ev pi.CompactionStartEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, decodedRecord{}, fmt.Errorf("unmarshal compaction_start: %w", err)
		}
		return []agent.Message{&agent.SystemMessage{
			MessageType: "system",
			Subtype:     agent.SystemSubtypeCompactStart,
			Detail:      string(ev.Reason),
		}}, decodedRecord{}, nil

	case pi.EventCompactionEnd:
		var ev pi.CompactionEndEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, decodedRecord{}, fmt.Errorf("unmarshal compaction_end: %w", err)
		}
		// A retry reschedules summarization, so the retried attempt owns the
		// final outcome and emits the boundary or error.
		if ev.WillRetry {
			return nil, decodedRecord{}, nil
		}
		m := &agent.SystemMessage{MessageType: "system"}
		switch {
		case ev.ErrorMessage != "":
			m.Subtype = agent.SystemSubtypeCompactError
			m.Detail = ev.ErrorMessage
		case ev.Aborted:
			m.Subtype = agent.SystemSubtypeCompactError
			m.Detail = "cancelled"
		default:
			m.Subtype = agent.SystemSubtypeCompactBoundary
			if ev.Result != nil {
				m.ContextTokensBefore = ev.Result.TokensBefore
				m.ContextTokensAfter = ev.Result.EstimatedTokensAfter
			}
		}
		return []agent.Message{m}, decodedRecord{}, nil

	case pi.EventExtensionUI:
		// Extension UI requests are passed through as RawMessage, and the
		// wireFormat handles auto-responses. The decoded request is also handed to
		// the native-subagent adapter, which reads the pi-subagents async-status
		// widget the extension publishes here.
		var ev pi.ExtensionUIRequest
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, decodedRecord{}, fmt.Errorf("unmarshal extension_ui_request: %w", err)
		}
		return []agent.Message{&agent.RawMessage{
			MessageType: string(pi.EventExtensionUI),
			Raw:         append([]byte(nil), line...),
		}}, decodedRecord{extensionUI: &ev}, nil

	case pi.EventAgentEnd, pi.EventTurnEnd,
		pi.EventAgentSettled, pi.EventAutoRetryStart, pi.EventAutoRetryEnd,
		pi.EventEntryAppended, pi.EventQueueUpdate,
		pi.EventSummarizationRetryScheduled, pi.EventSummarizationRetryAttemptStart, pi.EventSummarizationRetryFinished,
		pi.EventThinkingLevelChanged:
		// These events have no normalized agent.Message representation. Preserve
		// the raw record, as the default path did before Pi named the event types.
		return []agent.Message{&agent.RawMessage{
			MessageType: string(typ),
			Raw:         append([]byte(nil), line...),
		}}, decodedRecord{}, nil

	default:
		if typ == "" {
			return nil, decodedRecord{}, nil
		}
		// Preserve unrecognized events and logged stdin commands.
		return []agent.Message{&agent.RawMessage{
			MessageType: string(typ),
			Raw:         append([]byte(nil), line...),
		}}, decodedRecord{}, nil
	}
}

// parseMessageUpdate dispatches on the assistantMessageEvent delta type.
func parseMessageUpdate(line []byte) ([]agent.Message, error) {
	ev, err := decodeMessageUpdateEvent(line)
	if err != nil {
		return nil, fmt.Errorf("unmarshal message_update: %w", err)
	}

	return messagesFromAssistantMessageEvent(&ev.AssistantMessageEvent, line)
}

func messagesFromAssistantMessageEvent(delta *pi.AssistantMessageEvent, line []byte) ([]agent.Message, error) {
	switch delta.Type {
	case pi.DeltaTextDelta:
		return []agent.Message{&agent.TextDeltaMessage{Text: delta.Delta}}, nil

	case pi.DeltaThinkDelta:
		return []agent.Message{&agent.ThinkingDeltaMessage{Text: delta.Delta}}, nil

	case pi.DeltaTextStart, pi.DeltaTextEnd, pi.DeltaThinkStart, pi.DeltaThinkEnd,
		pi.DeltaToolStart, pi.DeltaToolDelta, pi.DeltaToolEnd, pi.DeltaStart:
		// Boundary markers; skip. DeltaToolStart is deliberately not a ToolUse
		// source: it precedes message_end (which emits the consolidated text
		// and thinking) and carries no arguments, so emitting it here would
		// split the message's content across UI groups (duplicated assistant
		// text) and duplicate the tool card that tool_execution_start — the
		// authoritative source, with full arguments — already provides.
		return nil, nil

	case pi.DeltaDone, pi.DeltaError:
		// Handled by wireFormat.ParseMessage; in stateless mode, pass through.
		return []agent.Message{&agent.RawMessage{
			MessageType: "message_update:" + string(delta.Type),
			Raw:         append([]byte(nil), line...),
		}}, nil
	}

	return nil, nil
}

// parseToolExecStart renders a tool start and returns the decoded record for the
// native-subagent adapter.
func parseToolExecStart(line []byte) ([]agent.Message, *pi.ToolExecStartEvent, error) {
	var ev pi.ToolExecStartEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, nil, fmt.Errorf("unmarshal tool_execution_start: %w", err)
	}
	name := normalizeToolName(ev.ToolName)

	var input json.RawMessage
	if ev.Args != nil {
		var err error
		input, err = json.Marshal(ev.Args)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal tool exec args: %w", err)
		}
	}

	if _, ok := agent.WidgetToolNames[name]; ok {
		return []agent.Message{agent.NewWidgetMessage(ev.ToolCallID, input)}, &ev, nil
	}
	use := newToolUseMessage(ev.ToolCallID, ev.ToolName, name, input)
	return []agent.Message{use}, &ev, nil
}

// parseToolExecUpdate converts a tool_execution_update event to a streaming
// delta. Pi's PartialResult carries the tool's full accumulated output on
// every update, so decoding it normally would rescan an ever-growing blob on
// every chunk.
func parseToolExecUpdate(line []byte) ([]agent.Message, error) {
	var ev pi.ToolExecUpdateEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal tool_execution_update: %w", err)
	}
	s := ev.PartialResult.Text()
	if s == "" || s == runningPlaceholder {
		return nil, nil
	}
	return []agent.Message{&agent.ToolOutputDeltaMessage{
		ToolUseID: ev.ToolCallID,
		Delta:     s,
	}}, nil
}

// parseToolExecEnd converts tool completion and exposes delegation output, and
// returns the decoded record for the native-subagent adapter, which correlates
// the native run lifecycle.
func parseToolExecEnd(line []byte) ([]agent.Message, *pi.ToolExecEndEvent, error) {
	var ev pi.ToolExecEndEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, nil, fmt.Errorf("unmarshal tool_execution_end: %w", err)
	}
	resultText := ev.Result.Text()
	res := &agent.ToolResultMessage{ToolUseID: ev.ToolCallID}
	if ev.IsError {
		if resultText != "" {
			res.Error = resultText
		} else {
			res.Error = "tool execution failed"
		}
	}
	if !strings.EqualFold(ev.ToolName, subagentToolName) {
		return []agent.Message{res}, &ev, nil
	}
	var msgs []agent.Message
	// On success the result body (review findings, plan, etc.) is the subagent's
	// output; surface it in the tool card. Failures already render via res.Error.
	// Relies on running-placeholder updates being suppressed so the output-length
	// accounting in piWireFormat starts at zero for this tool call.
	if !ev.IsError && resultText != "" {
		msgs = append(msgs, &agent.ToolOutputDeltaMessage{ToolUseID: ev.ToolCallID, Delta: resultText})
	}
	return append(msgs, res), &ev, nil
}

// parseResponse handles response envelopes. A failed prompt is terminal since
// Pi will never emit agent events; other failures are passed through as raw.
func parseResponse(line []byte) ([]agent.Message, error) {
	var resp pi.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if !resp.Success && resp.Error != "" {
		if resp.Command == pi.CmdPrompt {
			return []agent.Message{&agent.ResultMessage{
				MessageType: "result",
				Subtype:     "error",
				IsError:     true,
				Result:      resp.Error,
			}}, nil
		}
		return []agent.Message{&agent.RawMessage{
			MessageType: "response:" + string(resp.Command),
			Raw:         append([]byte(nil), line...),
		}}, nil
	}
	return []agent.Message{&agent.RawMessage{
		MessageType: "response:" + string(resp.Command),
		Raw:         append([]byte(nil), line...),
	}}, nil
}

func newToolUseMessage(id, rawName, name string, input json.RawMessage) *agent.ToolUseMessage {
	use := &agent.ToolUseMessage{
		ToolUseID: id,
		Name:      name,
		Input:     input,
	}
	if strings.EqualFold(name, "Edit") {
		if p, replacements, ok := parseEditArgs(input); ok {
			use.Detail = path.Base(p)
			use.InputView = agent.FileChangesInputViewFromReplacements(p, replacements)
		}
		return use
	}
	if !strings.EqualFold(rawName, subagentToolName) {
		return use
	}
	info := parseSubagentArgs(input)
	if len(info.Spawns) > 0 {
		use.Detail = subagentDescription(info.Kind, info.Spawns)
		use.InputView = agent.ToolInputView{
			Kind:      agent.ToolInputSubagents,
			Subagents: info.Spawns,
		}
		return use
	}
	use.Detail = info.Action
	return use
}

func parseEditArgs(raw json.RawMessage) (string, []agent.TextReplacement, bool) {
	var args pi.EditToolArgs
	if len(raw) == 0 || json.Unmarshal(raw, &args) != nil || args.Path == "" {
		return "", nil, false
	}
	replacements := make([]agent.TextReplacement, 0, len(args.Edits)+1)
	for _, edit := range args.Edits {
		if edit.OldText == "" {
			return "", nil, false
		}
		replacements = append(replacements, agent.TextReplacement{OldText: edit.OldText, NewText: edit.NewText})
	}
	if args.OldText != "" || args.NewText != "" {
		if args.OldText == "" {
			return "", nil, false
		}
		replacements = append(replacements, agent.TextReplacement{OldText: args.OldText, NewText: args.NewText})
	}
	if len(replacements) == 0 {
		return "", nil, false
	}
	return args.Path, replacements, true
}

// subagentToolName is Pi's raw tool name for spawning and orchestrating
// subagents. It is normalized to the canonical "Agent" name for display (see
// normalizeToolName) but matched on the raw name to detect spawns.
const subagentToolName = "subagent"

// runningPlaceholder is Pi's sentinel progress text for a tool that is executing
// but has produced no output yet (notably the subagent tool). It carries no
// information and is suppressed from the tool-output stream.
const runningPlaceholder = "(running...)"

// subagentInfo is the parsed view of a subagent tool call's arguments.
type subagentInfo struct {
	Kind   string
	Action string
	Spawns []agent.SubagentSpawn
}

// parseSubagentArgs decodes a subagent tool call's arguments into a structured
// view. It recognises the single, parallel-batch, chain, and workflow-script
// orchestration shapes, and the action-based introspection calls (list/status)
// which spawn no subagents.
func parseSubagentArgs(raw json.RawMessage) subagentInfo {
	var args pi.SubagentToolArgs
	if len(raw) == 0 || json.Unmarshal(raw, &args) != nil {
		return subagentInfo{}
	}
	if args.Action != "" {
		return subagentInfo{Kind: "action", Action: args.Action}
	}
	// workflowScript is the installed extension's current orchestration shape.
	// Legacy parallel and chain arguments were removed from the public tool, but
	// historical logs still replay through the shapes below. The byte probe keeps
	// ordinary single spawns from paying for a second unmarshal; a false positive
	// cannot fabricate a workflow because the decoded field stays empty.
	if bytes.Contains(raw, []byte(`"workflowScript"`)) {
		var workflow struct {
			WorkflowScript string `json:"workflowScript"`
		}
		if json.Unmarshal(raw, &workflow) == nil && workflow.WorkflowScript != "" {
			return subagentInfo{Kind: "workflow"}
		}
	}
	spawns := subagentSpawns(&args)
	switch {
	case len(args.Chain) > 0 && len(spawns) > 0:
		return subagentInfo{Kind: "chain", Spawns: spawns}
	case len(args.Tasks) > 0 && len(spawns) > 0:
		return subagentInfo{Kind: "parallel", Spawns: spawns}
	case len(spawns) > 0:
		return subagentInfo{Kind: "single", Spawns: spawns}
	default:
		return subagentInfo{}
	}
}

// subagentSpawns flattens the orchestration shapes into an ordered list of
// subagent invocations. Steps with no agent (e.g. the introspection action) are
// dropped.
func subagentSpawns(a *pi.SubagentToolArgs) []agent.SubagentSpawn {
	var out []agent.SubagentSpawn
	add := func(s pi.SubagentToolStep) {
		if s.Agent == "" {
			return
		}
		out = append(out, agent.SubagentSpawn{
			Agent: s.Agent,
			Task:  s.Task,
			Label: s.Label,
			Phase: s.Phase,
		})
	}
	switch {
	case len(a.Chain) > 0:
		for _, step := range a.Chain {
			if len(step.Parallel) > 0 {
				for _, s := range step.Parallel {
					add(s)
				}
				continue
			}
			add(step.SubagentToolStep)
		}
	case len(a.Tasks) > 0:
		for _, s := range a.Tasks {
			add(s)
		}
	default:
		add(a.SubagentToolStep)
	}
	return out
}

// subagentDescription summarises a subagent spawn for its canonical card,
// e.g. "reviewer — Review the last commit" for a single spawn,
// "chain · reviewer ×3, worker" for an orchestration of known steps, or the
// orchestration kind when the extension exposed no per-step detail.
func subagentDescription(kind string, spawns []agent.SubagentSpawn) string {
	if len(spawns) == 0 {
		return kind
	}
	if len(spawns) == 1 {
		s := spawns[0]
		detail := s.Label
		if detail == "" {
			detail = firstLine(s.Task)
		}
		if detail == "" {
			return s.Agent
		}
		return s.Agent + " — " + detail
	}

	order := make([]string, 0, len(spawns))
	counts := make(map[string]int, len(spawns))
	for _, s := range spawns {
		if _, ok := counts[s.Agent]; !ok {
			order = append(order, s.Agent)
		}
		counts[s.Agent]++
	}
	parts := make([]string, 0, len(order))
	for _, a := range order {
		if n := counts[a]; n > 1 {
			parts = append(parts, a+" ×"+strconv.Itoa(n))
		} else {
			parts = append(parts, a)
		}
	}
	return kind + " · " + strings.Join(parts, ", ")
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// normalizeToolName maps Pi tool names to caic canonical names.
func normalizeToolName(name string) string {
	lower := strings.ToLower(name)
	switch lower {
	case "bash", "shell", "terminal", "run_shell_command":
		return "Bash"
	case "edit", "replace", "edit_file":
		return "Edit"
	case "write", "write_file":
		return "Write"
	case "read", "read_file":
		return "Read"
	case "glob", "find_files":
		return "Glob"
	case "grep", "search", "grep_search":
		return "Grep"
	case "list", "list_directory", "ls":
		return "ListDirectory"
	case "webfetch", "web_fetch":
		return "WebFetch"
	case "websearch", "web_search", "google_web_search":
		return "WebSearch"
	case "todowrite", "todo_write":
		return "TodoWrite"
	case "task", "agent", "subagent":
		return "Agent"
	case "patch":
		return "Edit"
	case "notebook_edit":
		return "NotebookEdit"
	}
	// Check widget tools before returning original.
	if _, ok := agent.WidgetToolNames[name]; ok {
		return name
	}
	return name
}
