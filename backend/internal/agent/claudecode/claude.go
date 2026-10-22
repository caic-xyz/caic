// Package claudecode implements Claude Code sessions with delegation-scoped content, tools, controls, and accounting.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

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
	c := agent.Conn(&controlConn{Conn: agent.NewMCPConn(ctx, opts.Logger, rp.Stdin, opts.Log, newWireFormat(), opts.MCP)})
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
	return agent.AttachRelaySession(ctx, opts, newWireFormat(), func(c agent.Conn) (agent.Conn, error) {
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

var _ agent.Backend = (*Backend)(nil)

// wireFormat holds per-session Claude Code parsing state: widget tracking,
// reasoning-token accounting, native-subagent correlation, the active model
// reported by the assistant so result records can resolve the model's context
// window and usage records without a model stay attributed, and the Skill
// tool calls whose results must stay suppressed or confirm a load. A fresh
// instance is created for every session so this state can't leak between
// concurrent Claude Code tasks sharing the registered Backend singleton.
type wireFormat struct {
	nativeSubagents              nativeSubagents
	backgroundCommands           backgroundCommands
	widgetTracker                *WidgetTracker
	pendingReasoningOutputTokens int
	pendingReasoningEstimate     int
	reportedModel                string
	// pendingSkillUses maps a Skill tool use ID to the skill its success
	// loads into the main context, until the paired tool result arrives. The
	// value is empty for a call naming no skill and for a subagent's call.
	pendingSkillUses map[agent.ToolKey]string
	// askSources retains actual Ask ownership until its matching control arrives.
	askSources  map[agent.ToolKey]agent.MessageSource
	sessionID   string
	childModels map[agent.ToolKey]string
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
		childModels:        make(map[agent.ToolKey]string),
	}
}

// ParseMessage wraps the package-level parser with per-session state: widget
// tracking for streaming deltas, session-model attribution for model-less
// usage records, Skill tool-result suppression, and skill loads.
//
// The analytics SkillReadMessage stays at the Skill invocation. A main-agent
// call that succeeds also yields a SkillLoadMessage on the record that follows
// its tool result: Claude Code names the skill directory only in that
// synthetic injection, and another record leaves the load without a path. A
// subagent record carries parent_tool_use_id; its loads are not in the main
// context, so the wire layer drops them.
func (w *wireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	msgs, record, err := parseMessageWithTracker(line, w.widgetTracker)
	if err != nil {
		return nil, err
	}
	loaded := w.loadedSkill
	w.loadedSkill = nil
	for _, message := range msgs {
		if init, ok := message.(*agent.InitMessage); ok && init.SessionID != "" && !record.source.IsChild() {
			w.sessionID = init.SessionID
		}
	}
	if record.source.ThreadID == "" && record.harnessRecord && !record.askControl {
		record.source.ThreadID = w.sessionID
	}
	if record.controlAgentID != "" {
		if card, ok := w.nativeSubagents.tasks["claude:task:"+record.controlAgentID]; ok {
			record.source.AgentID = card.ID
			record.source.DelegationToolUseID = card.ToolUseID
		}
	}
	subagent := record.source.DelegationToolUseID != ""
	w.pendingReasoningEstimate += record.reasoningEstimate
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
		if source.AgentID == "" && source.DelegationToolUseID != "" {
			key := delegationKey{sessionID: source.ThreadID, toolUseID: source.DelegationToolUseID}
			if id := w.nativeSubagents.byTool[key]; id != "" {
				source.AgentID = id
			}
		}
		switch m := message.(type) {
		case *agent.AskMessage:
			w.askSources[agent.ToolKeyForSource(source, m.ToolUseID)] = *source
		case *agent.PendingUserActionMessage:
			requested := *source
			var matched agent.MessageSource
			var key agent.ToolKey
			var found, ambiguous bool
			for candidate, askSource := range w.askSources {
				if candidate.ToolUseID != m.Action.ToolUseID ||
					requested.ThreadID != "" && askSource.ThreadID != "" && requested.ThreadID != askSource.ThreadID ||
					requested.DelegationToolUseID != "" && requested.DelegationToolUseID != askSource.DelegationToolUseID ||
					requested.AgentID != "" && askSource.AgentID != "" && requested.AgentID != askSource.AgentID ||
					requested.IsChild() && !askSource.IsChild() {
					continue
				}
				if found {
					ambiguous = true
					break
				}
				key, found = candidate, true
				matched = askSource
			}
			if found && !ambiguous {
				*source = matched
				if source.AgentID == "" {
					source.AgentID = requested.AgentID
				}
				delete(w.askSources, key)
			}
			if ambiguous {
				// An unresolved control cannot be actionable: even an empty
				// source can match a source-free root Ask. Keep the actual
				// record informationally instead of assigning root ownership.
				msgs[i] = &agent.RawMessage{Source: requested, MessageType: "control_request", Raw: append([]byte(nil), line...)}
			} else if source.ThreadID == "" && record.askControl {
				source.ThreadID = w.sessionID
			}
		case *agent.ToolResultMessage:
			delete(w.askSources, agent.ToolKeyForSource(source, m.ToolUseID))
		}
	}
	if loaded != nil && agent.ToolKeyForSource(loaded.Provenance(), "") == agent.ToolKeyForSource(&record.source, "") {
		loaded.Path = skillPath(record.user)
	}
	out := msgs[:0]
	for _, msg := range msgs {
		keep := true
		switch m := msg.(type) {
		case *agent.InitMessage:
			if m.ReportedModel != "" && !m.Source.IsChild() {
				w.reportedModel = m.ReportedModel
			}
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
		case *agent.UsageMessage:
			switch {
			case m.Source.IsChild():
				key := agent.ToolKeyForSource(m.Provenance(), "")
				if m.ReportedModel != "" {
					w.childModels[key] = m.ReportedModel
				} else if model := w.childModels[key]; model != "" {
					m.ReportedModel, m.ModelDerived = model, true
				}
			case m.ReportedModel != "":
				w.reportedModel = m.ReportedModel
			case w.reportedModel != "":
				// message_delta usage events carry no model; attribute them
				// to the session's active model. The stamp is derived, so the
				// task pricing fold must not price this record per call: the
				// assistant record already covers the same API call.
				m.ReportedModel = w.reportedModel
				m.ModelDerived = true
			}
			w.pendingReasoningOutputTokens += m.Usage.ReasoningOutputTokens
		case *agent.ResultMessage:
			if m.Source.IsChild() {
				break
			}
			// Claude Code reports the active model's context window only in the
			// turn result, so this is the earliest point caic can learn it.
			m.ContextWindow = resultContextWindow(line, w.reportedModel)
			// Claude result records can omit output_tokens_details even when
			// preceding events reported thinking tokens. Prefer actual
			// message_delta usage over system/thinking_tokens estimates.
			if m.Usage.ReasoningOutputTokens == 0 {
				m.Usage.ReasoningOutputTokens = w.pendingReasoningOutputTokens
				if m.Usage.ReasoningOutputTokens == 0 {
					m.Usage.ReasoningOutputTokens = w.pendingReasoningEstimate
				}
			}
			w.pendingReasoningOutputTokens = 0
			w.pendingReasoningEstimate = 0
			// A turn boundary drops skill reads whose result never arrived.
			clear(w.pendingSkillUses)
			clear(w.askSources)
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
	if json.Unmarshal(line, &w) != nil {
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

// WritePrompt writes a single user message in Claude Code's stdin format.
// When images are provided, content is emitted as an array of content blocks.
func (*wireFormat) WritePrompt(w io.Writer, p agent.Prompt, log agent.LogSink) error {
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
		return err
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	return agent.AppendInputNativeRecord(log, log.LogVersion(), data)
}

// WriteCompact implements agent.CompactCommand by sending /compact as a user
// message. Claude Code recognizes this as a slash command in -p mode.
func (w *wireFormat) WriteCompact(wr io.Writer, instructions string, log agent.LogSink) error {
	text := "/compact"
	if instructions != "" {
		text = "/compact " + instructions
	}
	return w.WritePrompt(wr, agent.Prompt{Text: text}, log)
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
