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
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/maruel/genai/providers/pi"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// eventKnownFields caches the known field sets for Pi wire event types, built
// on first use. Uses sync.Map: few writes (once per type), many reads.
var eventKnownFields sync.Map

// unmarshalEvent unmarshals data into v and warns via fw for any unknown JSON
// fields, so protocol drift in the Pi harness (fields our structs don't know
// about) surfaces as a log warning instead of silently dropping data. The
// name identifies the type for logging. Skip call sites whose payload can
// grow unboundedly across repeated events (message_update's full-history
// message field, tool_execution_update's accumulated output) — those would
// turn this scan into a per-event rescan of an ever-growing blob.
func unmarshalEvent(data []byte, v any, name string, fw *jsonutil.FieldWarner) error {
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	val, ok := eventKnownFields.Load(name)
	if !ok {
		val, _ = eventKnownFields.LoadOrStore(name, jsonutil.KnownFields(reflect.ValueOf(v).Elem().Interface()))
	}
	known, ok2 := val.(map[string]struct{})
	if !ok2 {
		return fmt.Errorf("eventKnownFields stored unexpected type %T", val)
	}
	if fw != nil {
		var raw map[string]json.RawMessage
		if json.Unmarshal(data, &raw) == nil {
			fw.Warn(name, jsonutil.CollectUnknown(raw, known))
		}
	}
	return nil
}

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
// cost O(n²) over a turn's output. fw only scans the small, bounded
// assistantMessageEvent payload — never the accumulated message.
func decodeMessageUpdateEvent(line []byte, fw *jsonutil.FieldWarner) (pi.MessageUpdateDeltaEvent, error) {
	var ev pi.MessageUpdateDeltaEvent
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
			if err := unmarshalEvent(raw, &ev.AssistantMessageEvent, "MessageUpdateDelta", fw); err != nil {
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
//   - RawMessage           — unrecognised event types
func parseMessageTyped(fw *jsonutil.FieldWarner, typ pi.EventType, line []byte) ([]agent.Message, error) {
	// caic-injected lines and stdin commands.
	switch typ {
	case "caic_model_info":
		// Handled by wireFormat.ParseMessage; skip in stateless replay.
		return nil, nil

	case pi.EventType(pi.CmdPrompt):
		// Stdin prompt command logged by relay; convert to UserInputMessage.
		var cmd pi.PromptCmd
		if err := unmarshalEvent(line, &cmd, "PromptCmd", fw); err != nil {
			return nil, fmt.Errorf("unmarshal prompt cmd: %w", err)
		}
		ui := &agent.UserInputMessage{Text: cmd.Message}
		for _, img := range cmd.Images {
			ui.Images = append(ui.Images, agent.ImageData{
				MediaType: img.MimeType,
				Data:      img.Data,
			})
		}
		return []agent.Message{ui}, nil

	case pi.EventType(pi.CmdCompact):
		// Stdin compact command logged by relay; skip during replay.
		return nil, nil

	case "caic_diff_stat":
		var m agent.DiffStatMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil

	case "caic_exit":
		var m agent.ExitMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []agent.Message{&m}, nil

	case pi.EventMessageUpdate:
		return parseMessageUpdate(fw, line)

	case pi.EventToolExecStart:
		return parseToolExecStart(fw, line)

	case pi.EventToolExecUpdate:
		return parseToolExecUpdate(line)

	case pi.EventToolExecEnd:
		return parseToolExecEnd(fw, line)

	case pi.EventAgentStart, pi.EventMessageStart, pi.EventMessageEnd, pi.EventTurnStart:
		// Lifecycle events with no semantic content; skip.
		return nil, nil

	case pi.EventResponse:
		// Command responses (e.g. set_model ack); skip unless error.
		return parseResponse(fw, line)

	case pi.EventExtensionUI:
		// Extension UI requests are passed through as RawMessage.
		// The wireFormat handles auto-responses.
		return []agent.Message{&agent.RawMessage{
			MessageType: string(pi.EventExtensionUI),
			Raw:         append([]byte(nil), line...),
		}}, nil

	case pi.EventAgentEnd, pi.EventTurnEnd:
		// Handled by wireFormat; if we reach here (stateless replay), pass through.
		return []agent.Message{&agent.RawMessage{
			MessageType: string(typ),
			Raw:         append([]byte(nil), line...),
		}}, nil
	}

	// Unknown event type.
	if typ != "" {
		return []agent.Message{&agent.RawMessage{
			MessageType: string(typ),
			Raw:         append([]byte(nil), line...),
		}}, nil
	}
	return nil, nil
}

// parseMessageUpdate dispatches on the assistantMessageEvent delta type.
func parseMessageUpdate(fw *jsonutil.FieldWarner, line []byte) ([]agent.Message, error) {
	ev, err := decodeMessageUpdateEvent(line, fw)
	if err != nil {
		return nil, fmt.Errorf("unmarshal message_update: %w", err)
	}

	return messagesFromMessageUpdateDelta(&ev.AssistantMessageEvent, line)
}

func messagesFromMessageUpdateDelta(delta *pi.MessageUpdateDelta, line []byte) ([]agent.Message, error) {
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

// parseToolExecStart converts a tool_execution_start event.
func parseToolExecStart(fw *jsonutil.FieldWarner, line []byte) ([]agent.Message, error) {
	var ev pi.ToolExecStartEvent
	if err := unmarshalEvent(line, &ev, "ToolExecStartEvent", fw); err != nil {
		return nil, fmt.Errorf("unmarshal tool_execution_start: %w", err)
	}
	name := normalizeToolName(ev.ToolName)

	var input json.RawMessage
	if ev.Args != nil {
		var err error
		input, err = json.Marshal(ev.Args)
		if err != nil {
			return nil, fmt.Errorf("marshal tool exec args: %w", err)
		}
	}

	if _, ok := agent.WidgetToolNames[name]; ok {
		return []agent.Message{agent.NewWidgetMessage(ev.ToolCallID, input)}, nil
	}
	use := newToolUseMessage(ev.ToolCallID, ev.ToolName, name, input)
	// Spawning subagents emits a SubagentStartMessage so the live progress panel
	// surfaces the orchestration; introspection calls (list/status) spawn none.
	if strings.EqualFold(ev.ToolName, subagentToolName) {
		if info := parseSubagentArgs(input); len(info.Spawns) > 0 {
			return []agent.Message{
				&agent.SubagentStartMessage{
					TaskID:      ev.ToolCallID,
					Description: subagentDescription(info.Kind, info.Spawns),
				},
				use,
			}, nil
		}
	}
	return []agent.Message{use}, nil
}

// parseToolExecUpdate converts a tool_execution_update event to a streaming
// delta. Unlike the other event parsers, this skips unmarshalEvent: Pi's
// PartialResult carries the tool's full accumulated output on every update
// (see the incremental-delta comment in piWireFormat.ParseMessage), so an
// unknown-field scan here would rescan an ever-growing blob on every chunk.
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

// parseToolExecEnd converts a tool_execution_end event. Subagent tool calls also
// emit a SubagentEndMessage to close out the progress panel, and surface their
// aggregated result text as tool output so the orchestration outcome is visible.
func parseToolExecEnd(fw *jsonutil.FieldWarner, line []byte) ([]agent.Message, error) {
	var ev pi.ToolExecEndEvent
	if err := unmarshalEvent(line, &ev, "ToolExecEndEvent", fw); err != nil {
		return nil, fmt.Errorf("unmarshal tool_execution_end: %w", err)
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
		return []agent.Message{res}, nil
	}
	msgs := []agent.Message{&agent.SubagentEndMessage{
		TaskID: ev.ToolCallID,
		Status: subagentStatus(ev.IsError, resultText),
	}}
	// On success the result body (review findings, plan, etc.) is the subagent's
	// output; surface it in the tool card. Failures already render via res.Error.
	// Relies on running-placeholder updates being suppressed so the output-length
	// accounting in piWireFormat starts at zero for this tool call.
	if !ev.IsError && resultText != "" {
		msgs = append(msgs, &agent.ToolOutputDeltaMessage{ToolUseID: ev.ToolCallID, Delta: resultText})
	}
	return append(msgs, res), nil
}

// parseResponse handles response envelopes. A failed prompt is terminal since
// Pi will never emit agent events; other failures are passed through as raw.
func parseResponse(fw *jsonutil.FieldWarner, line []byte) ([]agent.Message, error) {
	var resp pi.Response
	if err := unmarshalEvent(line, &resp, "Response", fw); err != nil {
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
// view. It recognises the single, parallel-batch, and chain orchestration
// shapes, and the action-based introspection calls (list/status) which spawn
// no subagents.
func parseSubagentArgs(raw json.RawMessage) subagentInfo {
	var args pi.SubagentToolArgs
	if len(raw) == 0 || json.Unmarshal(raw, &args) != nil {
		return subagentInfo{}
	}
	spawns := subagentSpawns(&args)
	switch {
	case len(args.Chain) > 0 && len(spawns) > 0:
		return subagentInfo{Kind: "chain", Spawns: spawns}
	case len(args.Tasks) > 0 && len(spawns) > 0:
		return subagentInfo{Kind: "parallel", Spawns: spawns}
	case len(spawns) > 0:
		return subagentInfo{Kind: "single", Spawns: spawns}
	case args.Action != "":
		return subagentInfo{Kind: "action", Action: args.Action}
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

// subagentDescription summarises a subagent spawn for the live progress panel,
// e.g. "reviewer — Review the last commit" for a single spawn or
// "chain · reviewer ×3, worker" for an orchestration.
func subagentDescription(kind string, spawns []agent.SubagentSpawn) string {
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

// subagentStatus derives a terminal status ("completed"/"failed") from a
// subagent tool result, matching the SubagentEndMessage status vocabulary.
func subagentStatus(isError bool, resultText string) string {
	if isError || strings.HasPrefix(strings.TrimSpace(resultText), "❌") {
		return "failed"
	}
	return "completed"
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
