// Package antigravity implements agy stream-json sessions with relay-owned task MCP.
package antigravity

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/maruel/genai/providers/antigravity"
)

// modelFamilyOrder lists model families from most to least preferred.
var modelFamilyOrder = []string{"gemini", "claude"}

// modelNameOrder lists, per family, the model names from most to least
// preferred. Names not listed sort last.
var modelNameOrder = map[string][]string{
	"gemini": {"flash", "pro"},
	"claude": {"opus", "sonnet"},
}

// modelEffortOrder lists efforts from highest to lowest.
var modelEffortOrder = []string{"high", "medium", "low"}

// modelParts is an agy model ID split on "-". "gemini-3.8-flash-high" is
// family "gemini", name "flash", version 3.8, effort "high".
type modelParts struct {
	id      string
	family  string
	name    string
	version float64
	effort  string
}

// parseModelID splits id on "-". The first segment is the family, the last is
// the effort, the first run of numeric segments is the version and the other
// segments make the name, so "claude-opus-5-5-low" is family "claude", name
// "opus", version 5.5, effort "low".
func parseModelID(id string) modelParts {
	segs := strings.Split(id, "-")
	p := modelParts{id: id, family: segs[0]}
	if len(segs) < 3 {
		p.name = strings.Join(segs[1:], "-")
		return p
	}
	p.effort = segs[len(segs)-1]
	segs = segs[1 : len(segs)-1]
	start := slices.IndexFunc(segs, isNumeric)
	if start < 0 {
		p.name = strings.Join(segs, "-")
		return p
	}
	end := start
	for end < len(segs) && isNumeric(segs[end]) {
		end++
	}
	p.version, _ = strconv.ParseFloat(strings.Join(segs[start:end], "."), 64)
	p.name = strings.Join(append(slices.Clone(segs[:start]), segs[end:]...), "-")
	return p
}

func isNumeric(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// sortModels hides gpt-oss, keeps only the latest version of each
// family-name-effort combination, then orders the rest by family, name and
// effort.
func sortModels(ids []string) []string {
	type key struct{ family, name, effort string }
	var parts []modelParts
	latest := map[key]float64{}
	for _, id := range ids {
		if strings.HasPrefix(id, "gpt-oss") {
			continue
		}
		p := parseModelID(id)
		parts = append(parts, p)
		k := key{p.family, p.name, p.effort}
		latest[k] = max(latest[k], p.version)
	}
	parts = slices.DeleteFunc(parts, func(p modelParts) bool {
		return p.version < latest[key{p.family, p.name, p.effort}]
	})
	rank := func(order []string, v string) int {
		if i := slices.Index(order, v); i >= 0 {
			return i
		}
		return len(order)
	}
	slices.SortStableFunc(parts, func(a, b modelParts) int {
		return cmp.Or(
			cmp.Compare(rank(modelFamilyOrder, a.family), rank(modelFamilyOrder, b.family)),
			cmp.Compare(rank(modelNameOrder[a.family], a.name), rank(modelNameOrder[b.family], b.name)),
			cmp.Compare(rank(modelEffortOrder, a.effort), rank(modelEffortOrder, b.effort)),
		)
	})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.id
	}
	return out
}

// Backend implements agent.Backend for agy. Task MCP uses a relay-owned plugin
// workspace without changing shared ~/.gemini settings. Images are formatted as
// text data URLs for agy stream-json input.
type Backend struct {
	agent.Base
}

// New creates an Antigravity backend with its cached model inventory.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{
		HarnessID:       harness.Antigravity,
		QuotaProviderID: agent.QuotaProviderAntigravity,
		Images:          true,
	}
	b.SetModelInventory(agent.CachedModelInventory(cacheDir, harness.Antigravity, envVars))
	return b
}

// Start launches agy through the shared relay and its task-local MCP plugin.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	relayArgs := []string{"--harness", "antigravity"}
	if opts.MCP != nil {
		relayArgs = append(relayArgs, "--caic-mcp")
	}
	rp, err := agent.PrepareRelay(ctx, opts, relayArgs, b.AgentArgs(agent.HarnessArgs{
		Model: opts.Model, Effort: opts.Effort, ResumeSessionID: opts.ResumeSessionID,
	}))
	if err != nil {
		return nil, err
	}
	c := agent.NewMCPConn(ctx, opts.Logger, rp.Stdin, opts.Log, b.NewWire(), opts.MCP)
	return agent.StartSession(ctx, rp, c, opts)
}

// AttachRelay reconnects to a running agy process through the shared relay.
func (b *Backend) AttachRelay(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return agent.AttachRelaySession(ctx, opts, b.NewWire(), nil)
}

// AgentArgs returns agy's persistent NDJSON print-mode command. A bare -p
// consumes the next argument; -p= lets the prompt arrive on stdin instead.
func (*Backend) AgentArgs(a agent.HarnessArgs) []string {
	args := []string{"agy", "-p=", "--input-format", "stream-json", "--output-format", "stream-json", "--dangerously-skip-permissions"}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.Effort != "" {
		args = append(args, "--effort", a.Effort)
	}
	if a.ResumeSessionID != "" {
		args = append(args, "--conversation", a.ResumeSessionID)
	}
	return args
}

// NewWire creates independent per-session parser state.
func (*Backend) NewWire() agent.WireFormat {
	return &wireFormat{steps: make(map[string]*step)}
}

// FetchModelInventory discovers models on the agent's SSH target.
func (*Backend) FetchModelInventory(ctx context.Context, target runtime.ConnectionTarget, env []string) (agent.ModelInventory, error) {
	if target.SSHHost == "" {
		return agent.ModelInventory{}, errors.New("agent connection target missing SSH host")
	}
	var args []string
	if len(env) != 0 {
		args = append(args, "env")
		args = append(args, env...)
	}
	args = append(args, "agy", "--output-format", "stream-json", "models")
	out, err := exec.CommandContext(ctx, "ssh", target.SSHHost, shellCommand(args)).Output() //nolint:gosec // target is configured; remote arguments are shell-quoted
	if err != nil {
		return agent.ModelInventory{}, fmt.Errorf("agy models: %w", err)
	}
	return parseModels(out)
}

var (
	_ agent.Backend      = (*Backend)(nil)
	_ agent.ModelFetcher = (*Backend)(nil)
)

// step retains lifecycle state only until the result. IDs include the native
// conversation ID because child conversations can use the same step index.
type step struct {
	started  bool
	done     bool
	text     strings.Builder
	overflow bool
}

// wireFormat state belongs to the single message-reading goroutine. Writing a
// prompt does not mutate it, so live parsing and prompt writes can run together.
type wireFormat struct {
	steps   map[string]*step
	usage   antigravity.JSONUsage
	model   string
	hasText bool
}

// WritePrompt sends text and image input and records native input provenance in v3 logs.
func (*wireFormat) WritePrompt(w io.Writer, p agent.Prompt, log agent.LogSink) error {
	var blocks []antigravity.StreamInputContentBlock
	if p.Text != "" {
		blocks = append(blocks, antigravity.StreamInputContentBlock{Type: "text", Text: p.Text})
	}
	for _, img := range p.Images {
		blocks = append(blocks, antigravity.StreamInputContentBlock{
			Type: "text",
			// TODO: Gross hack but the agent figure it out and saves it as a temporary file. Let's use a proper image once
			// agy supports it with --input-format=stream-json.
			Text: "data:" + img.MediaType + ";base64," + img.Data,
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, antigravity.StreamInputContentBlock{Type: "text", Text: ""})
	}
	data, err := json.Marshal(antigravity.StreamInputMessage{
		Event:   antigravity.EventUser,
		Message: antigravity.StreamInputUserMessage{Content: blocks},
	})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if n, err := w.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	if log.LogVersion() != agent.LogVersionV3 {
		return nil
	}
	return agent.AppendInputNativeRecord(log, log.LogVersion(), data)
}

// ParseMessage converts agy stdout and recorded stdin into neutral messages.
// Unknown fields are accepted here; check-agent-logs owns strict DTO checks.
func (w *wireFormat) ParseMessage(line []byte) ([]agent.Message, error) {
	var ev antigravity.StreamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("antigravity event: %w", err)
	}
	switch ev.Event {
	case antigravity.EventUser:
		var in antigravity.StreamInputMessage
		if err := json.Unmarshal(line, &in); err != nil {
			return nil, fmt.Errorf("antigravity input: %w", err)
		}
		m := &agent.UserInputMessage{}
		for _, c := range in.Message.Content {
			if c.Type != "text" {
				return nil, fmt.Errorf("antigravity: unsupported input content %q", c.Type)
			}
			if mediaType, data, ok := parseDataURL(c.Text); ok {
				m.Images = append(m.Images, v3.ImageData{MediaType: mediaType, Data: data})
			} else {
				m.Text += c.Text
			}
		}
		return []agent.Message{m}, nil
	case antigravity.EventInit:
		w.model = ev.Init.Model
		return []agent.Message{&agent.InitMessage{SessionID: ev.ConversationID, Cwd: ev.Init.Cwd, Tools: ev.Init.Tools, ReportedModel: ev.Init.Model}}, nil
	case antigravity.EventStepUpdate:
		return w.parseStep(&ev.StepUpdate), nil
	case antigravity.EventResult:
		r := &ev.Result
		m := &agent.ResultMessage{MessageType: "result", Subtype: string(r.Status), SessionID: r.ConversationID, Result: r.Response, Usage: convertUsage(&w.usage)}
		if err := r.AsError(); err != nil {
			m.IsError = true
			m.Result = err.Error()
		}
		// Result usage, duration, and turn count are conversation-wide, including
		// resumed processes. Do not present them as per-turn measurements.
		w.usage = antigravity.JSONUsage{}
		clear(w.steps)
		var msgs []agent.Message
		if !w.hasText && r.Response != "" {
			msgs = append(msgs, &agent.TextMessage{Text: r.Response})
		}
		w.hasText = false
		return append(msgs, m), nil
	default:
		return []agent.Message{&agent.RawMessage{MessageType: string(ev.Event), Raw: bytes.Clone(line)}}, nil
	}
}

func (w *wireFormat) parseStep(s *antigravity.StepUpdatePayload) []agent.Message {
	id := s.ConversationID + ":" + strconv.FormatInt(s.StepIndex, 10)
	st := w.steps[id]
	if st == nil {
		st = &step{}
		w.steps[id] = st
	}
	if st.done {
		return nil
	}
	var msgs []agent.Message
	switch s.StepType {
	case antigravity.StepAgentResponse:
		if s.TextDelta != "" {
			w.hasText = true
			msgs = append(msgs, &agent.TextDeltaMessage{Text: s.TextDelta})
			// Deltas remain authoritative if a response exceeds the bounded
			// finalization buffer. Do not retain an incomplete duplicate.
			if st.overflow || len(s.TextDelta) > (1<<20)-st.text.Len() {
				st.text.Reset()
				st.overflow = true
			} else {
				st.text.WriteString(s.TextDelta)
			}
		}
		if s.State == antigravity.StepDone && !st.overflow && st.text.Len() != 0 {
			msgs = append(msgs, &agent.TextMessage{Text: st.text.String()})
			st.text.Reset()
		}
	case antigravity.StepTool:
		if !st.started {
			name := s.ToolName
			if name == "" {
				name = s.ToolInfo.Name
			}
			msgs = append(msgs, &agent.ToolUseMessage{ToolUseID: id, Name: name, Input: s.ToolInfo.Parameters})
			st.started = true
		}
		if s.State == antigravity.StepDone {
			if s.ToolInfo.Output != "" {
				msgs = append(msgs, &agent.ToolOutputDeltaMessage{ToolUseID: id, Delta: s.ToolInfo.Output})
			}
			msgs = append(msgs, &agent.ToolResultMessage{ToolUseID: id, DurationMs: int64(s.DurationSeconds * 1000), Error: s.ToolInfo.Error.Message})
		}
	case antigravity.StepSystemMessage:
		if s.TextDelta != "" {
			msgs = append(msgs, &agent.SystemMessage{MessageType: "system", Subtype: "antigravity", Detail: s.TextDelta})
		}
	case antigravity.StepUserInput, antigravity.StepFinish:
		// User text is retained by the recorded stdin; finish is followed by result.
	}
	if s.State == antigravity.StepDone {
		st.done = true
		if s.Usage != (antigravity.JSONUsage{}) {
			w.usage.Add(&s.Usage)
			msgs = append(msgs, &agent.UsageMessage{Usage: convertUsage(&s.Usage), ReportedModel: w.model, ModelDerived: true})
		}
	}
	return msgs
}

func convertUsage(u *antigravity.JSONUsage) agent.Usage {
	// agy reports uncached input and cached input separately. The recorded
	// cached-tools fixture has cache_read_tokens greater than input_tokens;
	// subtracting cache reads produces negative input. Output includes thinking.
	return agent.Usage{
		InputTokens:           int(u.InputTokens),
		CacheReadInputTokens:  int(u.CacheReadTokens),
		OutputTokens:          int(u.OutputTokens),
		ReasoningOutputTokens: int(u.ThinkingTokens),
	}
}

// shellCommand preserves argv through SSH's remote shell, including spaces,
// quotes, and expansion syntax in environment values.
func shellCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}

func parseModels(out []byte) (agent.ModelInventory, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 4096), 4<<20)
	var ids []string
	for sc.Scan() {
		var ev antigravity.StreamEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return agent.ModelInventory{}, fmt.Errorf("agy models event: %w", err)
		}
		switch ev.Event {
		case antigravity.EventCommandResult:
			if ev.Command.Name != "models" {
				return agent.ModelInventory{}, fmt.Errorf("agy: unexpected command %q", ev.Command.Name)
			}
			var data antigravity.ModelsData
			if err := json.Unmarshal(ev.Command.Data, &data); err != nil {
				return agent.ModelInventory{}, fmt.Errorf("agy models data: %w", err)
			}
			for _, m := range data.Models {
				if m.ID == "" {
					return agent.ModelInventory{}, errors.New("agy: model has no ID")
				}
				ids = append(ids, m.ID)
			}
		case antigravity.EventResult:
			if err := ev.Result.AsError(); err != nil {
				return agent.ModelInventory{}, err
			}
			if len(ids) == 0 {
				return agent.ModelInventory{}, errors.New("agy returned no models")
			}
			inv := agent.ModelInventory{}
			for _, id := range sortModels(ids) {
				inv.Models = append(inv.Models, agent.Model{ID: id})
			}
			return inv, nil
		default:
			return agent.ModelInventory{}, fmt.Errorf("agy: unexpected models event %q", ev.Event)
		}
	}
	if err := sc.Err(); err != nil {
		return agent.ModelInventory{}, fmt.Errorf("read agy models: %w", err)
	}
	return agent.ModelInventory{}, errors.New("agy exited without a models result")
}

func parseDataURL(s string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(s, "data:image/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, "data:")
	mediaType, data, found := strings.Cut(rest, ";base64,")
	if !found || data == "" {
		return "", "", false
	}
	return mediaType, data, true
}
