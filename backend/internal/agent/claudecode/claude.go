// Package claudecode implements Claude Code sessions with delegation-scoped content, request-correlated answers, controls, and accounting.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"

	"github.com/buger/jsonparser"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/genai/providers/anthropic"
	"github.com/maruel/genai/providers/claudecode"

	"github.com/caic-xyz/caic/backend/internal/agent"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// Backend implements agent.Backend for Claude Code. It is registered once per
// process (see backends.Default) and shared across every concurrent Claude
// Code task, so it must hold no per-session mutable state. Start, AttachRelay,
// and NewWire each construct a fresh wireFormat instead.
type Backend struct {
	agent.Base
}

// New creates a Claude Code backend descriptor.
func New() *Backend {
	b := &Backend{
		HarnessID:       harness.Claude,
		QuotaProviderID: agent.QuotaProviderClaudeCode,
		Images:          true,
		Compact:         true,
	}
	b.SetModelInventory(claudeModelInventory())
	return b
}

// Start launches a Claude Code process via the relay daemon. It deploys the
// widget plugin to the container before starting the relay so Claude Code
// picks up the show_widget MCP tool and the widget design skill.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	pluginFS, err := fs.Sub(WidgetPlugin, "widget-plugin")
	if err != nil {
		return nil, fmt.Errorf("widget plugin fs: %w", err)
	}
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	if err := agent.DeployEmbeddedDir(ctx, sshHost, pluginFS, agent.WidgetPluginDir); err != nil {
		return nil, err
	}
	// When Claude Code has an OAuth session, strip ANTHROPIC_API_KEY from the
	// agent subprocess so it authenticates via the subscription instead of
	// silently billing API credits. The key is deliberately NOT re-injected
	// afterward: re-injecting it made Claude Code switch authentication to API
	// billing mid-session (observed in traces). The tradeoff is that
	// in-container tools and MCP servers do not receive ANTHROPIC_API_KEY.
	if hasOAuth() {
		opts.StripEnv = []string{"ANTHROPIC_API_KEY"}
	}
	args := b.AgentArgs(agent.HarnessArgs{Model: opts.Model, Effort: opts.Effort, ResumeSessionID: opts.ResumeSessionID})
	relayArgs := []string{"--harness", "claude"}
	if opts.MCP != nil {
		args = append(args, "--mcp-config", agent.ClaudeCodeCaicMCPConfigPath)
		relayArgs = append(relayArgs, "--caic-mcp")
	}
	rp, err := agent.PrepareRelay(ctx, opts, relayArgs, args)
	if err != nil {
		return nil, err
	}
	c := agent.Conn(&controlConn{Conn: agent.NewConn(ctx, opts.Logger, rp.Stdin, opts.Records, opts.MCP)})
	return agent.StartSession(ctx, rp, c, opts)
}

// AgentArgs implements agent.Backend.
func (*Backend) AgentArgs(a agent.HarnessArgs) []string {
	args := []string{
		"claude", "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "acceptEdits",
		"--permission-prompt-tool", "stdio",
		"--include-partial-messages",
		"--plugin-dir", agent.WidgetPluginDir,
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.Effort != "" {
		args = append(args, "--effort", a.Effort)
	}
	if a.ResumeSessionID != "" {
		// Resume reads local JSONL history under ~/.claude/projects/ by default.
		// The ID alone cannot recover missing history. md mounts ~/.claude into
		// new containers; a custom CLAUDE_CONFIG_DIR needs accessible storage too.
		// Sessions are directory-scoped, so workspace paths affect discovery.
		// https://code.claude.com/docs/en/how-claude-code-works#work-with-sessions
		args = append(args, "--resume", a.ResumeSessionID)
	}
	return args
}

// AttachRelay implements agent.Backend.
func (*Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, func(c agent.Conn) (agent.Conn, error) {
		cc := &controlConn{Conn: c}
		if err := cc.restorePendingActions(opts.PendingUserActions); err != nil {
			return nil, err
		}
		return cc, nil
	})
}

// NewWire implements agent.Backend.
func (*Backend) NewWire() agent.WireFormat {
	return newWireFormat()
}

// NewMetadataParser creates a session/accounting parser with bounded native stream state.
func (*Backend) NewMetadataParser() agent.MessageParser {
	return &metadataParser{childModels: make(map[agent.ToolKey]string)}
}

var _ agent.Backend = (*Backend)(nil)

// recordedQuestion is input interpretation context, not a delivery outcome.
// An observed request ID with no questions stays ineligible after conflicting
// reuse, cancellation, or completion, until a new root session initializes.
type recordedQuestion struct {
	source    agent.MessageSource
	toolUseID string
	questions []v3.AskQuestion
}

// wireFormat holds per-session Claude Code parsing state: widget tracking,
// reasoning-token accounting, question requests, native-subagent correlation, the active model
// reported by the assistant so result records can resolve the model's context
// window and usage records without a model stay attributed, and the Skill
// tool calls whose results must stay suppressed or confirm a load. A fresh
// instance is created for every session so this state can't leak between
// concurrent Claude Code tasks sharing the registered Backend singleton.
type wireFormat struct {
	metadataState

	nativeSubagents    nativeSubagents
	backgroundCommands backgroundCommands
	widgetTracker      *WidgetTracker
	// pendingSkillUses maps a Skill tool use ID to the skill its success
	// loads into the main context, until the paired tool result arrives. The
	// value is empty for a call naming no skill and for a subagent's call.
	pendingSkillUses map[agent.ToolKey]string
	// askSources retains Ask ownership across controls and retries until completion.
	askSources       map[agent.ToolKey]agent.MessageSource
	questionRequests map[string]recordedQuestion
	// loadedSkill is the skill whose Skill call succeeded. Its load waits one
	// record for the synthetic injection that names the skill directory.
	loadedSkill *agent.SkillLoadMessage
}

// newWireFormat builds a live wireFormat with fresh per-session state for use by
// Start and AttachRelay. Schema drift is checked offline by check-agent-logs.
func newWireFormat() *wireFormat {
	return &wireFormat{
		nativeSubagents: nativeSubagents{
			tasks:  make(map[string]agent.NativeSubagent),
			byTool: make(map[delegationKey]string),
			tools:  make(map[delegationKey]agent.NativeSubagent),
		},
		backgroundCommands: backgroundCommands{known: make(map[string]bool)},
		widgetTracker:      newWidgetTracker(),
		pendingSkillUses:   make(map[agent.ToolKey]string),
		askSources:         make(map[agent.ToolKey]agent.MessageSource),
		questionRequests:   make(map[string]recordedQuestion),
		childModels:        make(map[agent.ToolKey]string),
	}
}

// ParseOutput wraps the package-level parser with per-session state: widget
// tracking for streaming deltas, session-model attribution for model-less
// usage records, Skill tool-result suppression, and skill loads.
//
// The analytics SkillReadMessage stays at the Skill invocation. A main-agent
// call that succeeds also yields a SkillLoadMessage on the record that follows
// its tool result: Claude Code names the skill directory only in that
// synthetic injection, and another record leaves the load without a path. A
// subagent record carries parent_tool_use_id; its loads are not in the main
// context, so the wire layer drops them.
func (w *wireFormat) ParseOutput(line []byte) ([]agent.Message, error) {
	msgs, record, err := parseMessageWithTracker(line, w.widgetTracker)
	if err != nil {
		return nil, err
	}
	loaded := w.loadedSkill
	w.loadedSkill = nil
	for _, message := range msgs {
		if init, ok := message.(*agent.InitMessage); ok && init.SessionID != "" && !record.source.IsChild() {
			if init.SessionID != w.sessionID {
				clear(w.askSources)
				clear(w.questionRequests)
			}
		}
	}
	if record.source.ThreadID == "" && record.harnessRecord && !record.askControl {
		record.source.ThreadID = w.sessionID
	}
	reportedAgentID := record.controlAgentID
	if record.user != nil && record.source.DelegationToolUseID != "" {
		// On root Agent-tool results, agent_id describes the tool's target.
		// Only a child stream can report it as the record's own native owner.
		reportedAgentID = record.user.AgentID
	}
	recordOwner := w.nativeSubagents.resolveOwner(&record.source, reportedAgentID)
	subagent := record.source.DelegationToolUseID != ""
	native, err := w.nativeSubagents.parse(&record, msgs)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, native...)
	msgs = append(msgs, w.backgroundCommands.parse(&record)...)
	for i, message := range msgs {
		if message.Provenance().IsZero() || record.harnessRecord {
			*message.Provenance() = record.source
		}
		source := message.Provenance()
		owner := recordOwner
		if owner != nativeOwnerInvalid {
			owner = w.nativeSubagents.resolveOwner(source, "")
		}
		switch m := message.(type) {
		case *agent.NativeSubagentMessage:
			// Native identity proves which existing requests gain newly
			// reported delegation metadata. Retain known metadata so later
			// contradictions cannot rewrite the canonical request owner.
			if m.Subagent.ToolUseID != "" {
				for id := range w.questionRequests {
					question := w.questionRequests[id]
					if len(question.questions) > 0 && w.nativeSubagents.resolveOwner(&question.source, "") == nativeOwnerResolved {
						w.questionRequests[id] = question
					}
				}
			}
		case *agent.AskMessage:
			// Keep unresolved Ask evidence too: it prevents a later source-free
			// control from falling back to root after ambiguous child ownership.
			w.askSources[agent.ToolKeyForSource(source, m.ToolUseID)] = *source
		case *agent.PendingUserActionMessage:
			requested := *source
			var matched agent.MessageSource
			var found, ambiguous bool
			if question, recorded := w.questionRequests[m.Action.RequestID]; recorded {
				// Request identity owns duplicates even when other agents later
				// use the same tool ID. Explicit conflicting provenance still
				// invalidates reuse; rememberQuestion checks tool and questions.
				matched = question.source
				found = len(question.questions) > 0
				ambiguous = !found ||
					requested.ThreadID != "" && requested.ThreadID != matched.ThreadID ||
					requested.DelegationToolUseID != "" && requested.DelegationToolUseID != matched.DelegationToolUseID ||
					requested.AgentID != "" && matched.AgentID != "" && requested.AgentID != matched.AgentID ||
					requested.IsChild() && !matched.IsChild()
			} else if owner != nativeOwnerAmbiguous && owner != nativeOwnerInvalid {
				for candidate, askSource := range w.askSources {
					// Explicit scope excludes unrelated evidence before its
					// ambiguous native resolution can affect this request.
					if candidate.ToolUseID != m.Action.ToolUseID ||
						requested.ThreadID != "" && askSource.ThreadID != "" && requested.ThreadID != askSource.ThreadID ||
						requested.DelegationToolUseID != "" && requested.DelegationToolUseID != askSource.DelegationToolUseID ||
						requested.AgentID != "" && askSource.AgentID != "" && requested.AgentID != askSource.AgentID ||
						requested.IsChild() && !askSource.IsChild() {
						continue
					}
					candidateOwner := w.nativeSubagents.resolveOwner(&askSource, "")
					if candidateOwner == nativeOwnerAmbiguous || candidateOwner == nativeOwnerInvalid {
						if requested.AgentID == "" {
							ambiguous = true
							break
						}
						continue
					}
					// Resolution can supply a previously missing native identity.
					if requested.AgentID != "" && askSource.AgentID != "" && requested.AgentID != askSource.AgentID {
						continue
					}
					if found {
						ambiguous = true
						break
					}
					found = true
					matched = askSource
				}
			}
			if found && !ambiguous {
				*source = matched
				if source.AgentID == "" {
					source.AgentID = requested.AgentID
				}
				if owner != nativeOwnerInvalid {
					owner = w.nativeSubagents.resolveOwner(source, "")
				}
			}
			if ambiguous || owner == nativeOwnerAmbiguous || owner == nativeOwnerInvalid {
				// An unresolved control cannot be actionable: even an empty
				// source can match a source-free root Ask. Keep the actual
				// record informationally instead of assigning root ownership,
				// including unknown native IDs and contradictory delegations.
				msgs[i] = &agent.RawMessage{Source: requested, MessageType: "control_request", Raw: append([]byte(nil), line...)}
			} else if source.ThreadID == "" && record.askControl {
				source.ThreadID = w.sessionID
			}
		case *agent.ToolResultMessage:
			if m.Error == "" && owner != nativeOwnerAmbiguous && owner != nativeOwnerInvalid {
				key := agent.ToolKeyForSource(source, m.ToolUseID)
				if askSource, ok := w.askSources[key]; ok {
					askOwner := w.nativeSubagents.resolveOwner(&askSource, "")
					if askOwner != nativeOwnerAmbiguous && askOwner != nativeOwnerInvalid && sameQuestionOwner(&askSource, source) {
						delete(w.askSources, key)
					}
				}
				for id := range w.questionRequests {
					question := w.questionRequests[id]
					questionOwner := w.nativeSubagents.resolveOwner(&question.source, "")
					if len(question.questions) > 0 && questionOwner != nativeOwnerAmbiguous && questionOwner != nativeOwnerInvalid && question.toolUseID == m.ToolUseID && sameQuestionOwner(&question.source, source) {
						w.retireQuestion(id)
					}
				}
			}
		}
	}
	if record.controlRequest != nil {
		w.rememberQuestion(record.controlRequest.RequestID, msgs, line)
	}
	if record.cancelRequestID != "" {
		w.retireQuestion(record.cancelRequestID)
	}
	w.apply(&record.source, record.reasoningEstimate, msgs, line)
	if loaded != nil && agent.ToolKeyForSource(loaded.Provenance(), "") == agent.ToolKeyForSource(&record.source, "") {
		loaded.Path = skillPath(record.user)
	}
	out := msgs[:0]
	for _, msg := range msgs {
		keep := true
		switch m := msg.(type) {
		case *agent.SkillReadMessage:
			// Track the read so its paired tool result stays suppressed, even
			// when the read itself is dropped below for lacking a skill name.
			if m.ToolUseID != "" {
				load := m.Skill
				if subagent {
					load = ""
				}
				w.pendingSkillUses[agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)] = load
			}
			if m.Skill == "" {
				// Parse-level placeholder for an unexpected Skill input shape;
				// keep it out of the timeline like the pre-skill-read behavior.
				keep = false
			}
		case *agent.SkillLoadMessage:
			// An inferred load from a file read; a subagent's read is not in
			// the main context.
			keep = !subagent
		case *agent.ToolResultMessage:
			if load, ok := w.pendingSkillUses[agent.ToolKeyForSource(m.Provenance(), m.ToolUseID)]; ok {
				delete(w.pendingSkillUses, agent.ToolKeyForSource(m.Provenance(), m.ToolUseID))
				// The Skill tool's result is internal machinery; the read
				// itself is already recorded.
				keep = false
				if load != "" && m.Error == "" {
					if w.loadedSkill != nil {
						// Another success before the next record; no
						// injection can name both directories.
						out = append(out, w.loadedSkill)
					}
					w.loadedSkill = &agent.SkillLoadMessage{Source: *m.Provenance(), Skill: load}
				}
			}
		case *agent.ResultMessage:
			if m.Source.IsChild() {
				break
			}
			// A turn boundary drops skill reads whose result never arrived.
			clear(w.pendingSkillUses)
		}
		if keep {
			out = append(out, msg)
		}
	}
	if loaded != nil {
		out = append([]agent.Message{loaded}, out...)
	}
	return out, nil
}

// resultContextWindow returns the context window a Claude result record reports
// for model, or 0 when the record carries none. A record that names a single
// model is unambiguous even when the session model is still unknown.
func resultContextWindow(line []byte, model string) int {
	var w claudecode.OutputResultMsg
	raw, err := agent.JSONField(line, "modelUsage")
	if err != nil || len(raw) == 0 || json.Unmarshal(raw, &w.ModelUsage) != nil {
		return 0
	}
	if e, ok := w.ModelUsage[model]; ok && e.ContextWindow > 0 {
		return int(e.ContextWindow)
	}
	if len(w.ModelUsage) == 1 {
		for _, e := range w.ModelUsage {
			if e.ContextWindow > 0 {
				return int(e.ContextWindow)
			}
		}
	}
	return 0
}

// EncodePrompt encodes a single user message in Claude Code's stdin format.
// When images are provided, content is emitted as an array of content blocks.
func (*wireFormat) EncodePrompt(p agent.Prompt) ([]byte, error) {
	var blocks []claudecode.InputContentBlock
	for _, img := range p.Images {
		blocks = append(blocks, claudecode.InputContentBlock{
			Type: "image",
			Source: anthropic.Source{
				Type:      "base64",
				MediaType: img.MediaType,
				Data:      img.Data,
			},
		})
	}
	if p.Text != "" {
		blocks = append(blocks, claudecode.InputContentBlock{Type: "text", Text: p.Text})
	}
	msg := claudecode.InputUserMsg{
		Type:    claudecode.InputUser,
		Message: claudecode.InputUserContent{Role: "user", Content: blocks},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// EncodeCompact implements agent.CompactCommand by sending /compact as a user
// message. Claude Code recognizes this as a slash command in -p mode.
func (w *wireFormat) EncodeCompact(instructions string) ([]byte, error) {
	text := "/compact"
	if instructions != "" {
		text = "/compact " + instructions
	}
	return w.EncodePrompt(agent.Prompt{Text: text})
}

// ParseInput decodes client prompts and request-correlated question answers.
// Attempts do not consume requests or imply successful delivery.
func (w *wireFormat) ParseInput(line []byte) ([]agent.Message, error) {
	typ, source, err := parseRecordHeader(line)
	if err != nil {
		return nil, err
	}
	switch claudecode.InputType(typ) {
	case claudecode.InputUser:
		raw, err := agent.JSONField(line, "message")
		if err != nil {
			return nil, err
		}
		if source.ThreadID == "" {
			source.ThreadID = w.sessionID
		}
		input := &agent.UserInputMessage{Source: source}
		var text claudecode.OutputUserText
		if err := json.Unmarshal(raw, &text); err == nil && text.Role == "user" {
			input.Text = text.Content
			return []agent.Message{input}, nil
		}
		var prompt claudecode.InputUserMsg
		if err := json.Unmarshal(line, &prompt); err != nil {
			return nil, err
		}
		for i := range prompt.Message.Content {
			content := &prompt.Message.Content[i]
			switch content.Type {
			case "text":
				input.Text = content.Text
			case "image":
				if content.Source.Type != "" {
					input.Images = append(input.Images, v3.ImageData{MediaType: content.Source.MediaType, Data: content.Source.Data})
				}
			}
		}
		return []agent.Message{input}, nil
	case claudecode.InputControlResponse:
		var response claudecode.InputControlResponseMsg
		if err := json.Unmarshal(line, &response); err != nil {
			return nil, err
		}
		if answer := w.questionAnswer(&response.Response, &source); answer != nil {
			return []agent.Message{answer}, nil
		}
		return []agent.Message{&agent.RawMessage{MessageType: typ, Raw: append([]byte(nil), line...)}}, nil

	default:
		return []agent.Message{&agent.RawMessage{MessageType: typ, Raw: append([]byte(nil), line...)}}, nil
	}
}

// rememberQuestion runs after control ownership has been resolved. Recording
// non-question IDs too prevents a later colliding request from turning a
// permission response into a user answer.
func (w *wireFormat) rememberQuestion(id string, messages []agent.Message, line []byte) {
	var next recordedQuestion
	index := -1
	for i, message := range messages {
		if pending, ok := message.(*agent.PendingUserActionMessage); ok {
			index = i
			next.source = *pending.Provenance()
			next.toolUseID = pending.Action.ToolUseID
			next.questions = pending.Action.Ask.Questions
		}
	}
	// Empty or repeated question names cannot be recovered from an answer map.
	seen := make(map[string]struct{}, len(next.questions))
	for _, q := range next.questions {
		if _, duplicate := seen[q.Question]; q.Question == "" || duplicate {
			next.questions = nil
			break
		}
		seen[q.Question] = struct{}{}
	}
	if previous, exists := w.questionRequests[id]; exists {
		if previous.toolUseID != next.toolUseID || !sameQuestionOwner(&previous.source, &next.source) ||
			!reflect.DeepEqual(previous.questions, next.questions) {
			next.questions = nil
		} else if next.source.AgentID == "" {
			next.source.AgentID = previous.source.AgentID
		}
	}
	if id == "" || next.toolUseID == "" || len(next.questions) == 0 {
		if id != "" {
			w.questionRequests[id] = recordedQuestion{}
		}
		if index >= 0 {
			messages[index] = &agent.RawMessage{Source: next.source, MessageType: string(claudecode.OutputControlRequest), Raw: append([]byte(nil), line...)}
		}
		return
	}
	w.questionRequests[id] = next
}

func (w *wireFormat) retireQuestion(id string) {
	question, exists := w.questionRequests[id]
	if !exists {
		return
	}
	w.questionRequests[id] = recordedQuestion{}
	if len(question.questions) == 0 {
		return
	}
	owner := w.nativeSubagents.resolveOwner(&question.source, "")
	if owner == nativeOwnerAmbiguous || owner == nativeOwnerInvalid {
		return
	}
	key := agent.ToolKeyForSource(&question.source, question.toolUseID)
	// Canceling one request must not discard ownership needed by a different
	// outstanding request for the same tool, including its repeated controls.
	for otherID := range w.questionRequests {
		other := w.questionRequests[otherID]
		if len(other.questions) > 0 && other.toolUseID == question.toolUseID && sameQuestionOwner(&other.source, &question.source) {
			return
		}
	}
	if askSource, ok := w.askSources[key]; ok {
		askOwner := w.nativeSubagents.resolveOwner(&askSource, "")
		if askOwner != nativeOwnerAmbiguous && askOwner != nativeOwnerInvalid && sameQuestionOwner(&askSource, &question.source) {
			delete(w.askSources, key)
		}
	}
}

func (w *wireFormat) questionAnswer(response *claudecode.ControlResponse, source *agent.MessageSource) *agent.UserInputMessage {
	question := w.questionRequests[response.RequestID]
	if len(question.questions) == 0 || response.Subtype != claudecode.ControlResponseSuccess || response.Response.Behavior != claudecode.ControlCanUseToolBehaviorAllow {
		return nil
	}
	owner := w.nativeSubagents.resolveOwner(&question.source, "")
	if owner == nativeOwnerAmbiguous || owner == nativeOwnerInvalid {
		return nil
	}
	if source.ThreadID != "" && source.ThreadID != question.source.ThreadID ||
		source.DelegationToolUseID != "" && source.DelegationToolUseID != question.source.DelegationToolUseID ||
		response.Response.ToolUseID != "" && response.Response.ToolUseID != question.toolUseID {
		return nil
	}
	var input claudecode.AskUserQuestionUpdatedInput
	if err := json.Unmarshal(response.Response.UpdatedInput, &input); err != nil || len(input.Answers) != len(question.questions) {
		return nil
	}
	// The DTO's map[string]string accepts null as an empty string. Require
	// actual string values on the wire so missing answers stay informational.
	answers, err := agent.JSONField(response.Response.UpdatedInput, "answers")
	if err != nil {
		return nil
	}
	if err := jsonparser.ObjectEach(answers, func(_, _ []byte, kind jsonparser.ValueType, _ int) error {
		if kind != jsonparser.String {
			return errors.New("question answer is not a string")
		}
		return nil
	}); err != nil {
		return nil
	}
	// An echoed question list may confirm the request, never replace it.
	if len(input.Questions) > 0 && !reflect.DeepEqual(askQuestionsFromClaude(input.Questions), question.questions) {
		return nil
	}
	answer, ok := input.Answers[question.questions[0].Question]
	if !ok {
		return nil
	}
	for _, q := range question.questions {
		if text, ok := input.Answers[q.Question]; !ok || text != answer {
			return nil
		}
	}
	message := &agent.UserInputMessage{Text: answer}
	*message.Provenance() = question.source
	return message
}

// sameQuestionOwner compares resolved owners. A ToolKey alone cannot distinguish
// a root from a native child sharing its thread without delegation metadata.
// Known identities must agree. Matching native IDs prove compatible missing
// delegation metadata; matching delegation identity can gain a native ID later.
func sameQuestionOwner(a, b *agent.MessageSource) bool {
	if a.IsChild() != b.IsChild() ||
		a.AgentID != "" && b.AgentID != "" && a.AgentID != b.AgentID ||
		a.ThreadID != "" && b.ThreadID != "" && a.ThreadID != b.ThreadID ||
		a.DelegationToolUseID != "" && b.DelegationToolUseID != "" && a.DelegationToolUseID != b.DelegationToolUseID ||
		a.TurnID != b.TurnID {
		return false
	}
	return agent.ToolKeyForSource(a, "") == agent.ToolKeyForSource(b, "") ||
		a.AgentID != "" && a.AgentID == b.AgentID
}

var _ agent.WireFormat = (*wireFormat)(nil)
var _ agent.CompactCommand = (*wireFormat)(nil)

var claudeEffortOptions = []string{
	string(claudecode.EffortLow),
	string(claudecode.EffortMedium),
	string(claudecode.EffortHigh),
	string(claudecode.EffortXHigh),
	string(claudecode.EffortMax),
}

func claudeModelInventory() agent.ModelInventory {
	models := [...]string{"fable", "opus", "sonnet", "haiku"}
	inventory := make([]agent.Model, 0, len(models))
	for _, model := range models {
		inventory = append(inventory, agent.Model{
			ID:            model,
			EffortOptions: append([]string(nil), claudeEffortOptions...),
		})
	}
	return agent.ModelInventory{Models: inventory}
}

// hasOAuth reports whether Claude Code has an OAuth session configured
// in ~/.claude/claude.json. When true, ANTHROPIC_API_KEY should be
// stripped so Claude Code authenticates via OAuth instead of API key.
func hasOAuth() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "claude.json")) //nolint:gosec // Path components are hardcoded.
	if err != nil {
		return false
	}
	// Quick key check avoids unmarshaling the full config.
	var m map[string]json.RawMessage
	return json.Unmarshal(data, &m) == nil && m["oauthAccount"] != nil
}
