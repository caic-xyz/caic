// Package antigravity implements agy sessions over shared ACP and reads stream-json records from earlier agy tasks.
package antigravity

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/maruel/genai/acp"
	"github.com/maruel/genai/providers/antigravity"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agentacp "github.com/caic-xyz/caic/backend/internal/agent/acp"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

// effortID is agy 1.5.9's ACP reasoning-effort configuration option. Its
// values vary by model, like the effort suffixes of `agy models`.
const effortID acp.ConfigOptionID = "thought_level"

// Backend implements agy over shared ACP. Task MCP is registered through
// session/new, so shared ~/.gemini settings stay untouched.
type Backend struct{ *agentacp.Backend }

// New creates an Antigravity backend with its cached model inventory.
// agy 1.5.3 accepts --settings, but its published schema has no general
// compaction threshold: https://antigravity.google/schemas/v1/settings.json.
// maxTokens in customModelsConfig.customModels entries describes context capacity
// for Gemini API-authenticated custom models, not a built-in subscription model threshold.
// Unknown settings are ignored, so no unsupported compaction override is exposed.
// See https://antigravity.google/docs/settings?tab=cli.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{Backend: agentacp.New(harness.Antigravity, &agentacp.Config{
		// agy keeps conversations in ~/.gemini/antigravity-cli/conversations,
		// which md mounts into new containers. The ACP session ID is that
		// conversation ID, so stream-json conversations resume too.
		ResumeMethod: acp.MethodSessionResume,
		// Without authenticate, agy 1.5.9 ACP prefers GEMINI_API_KEY over
		// the CLI's Google sign-in, which changes the model catalog and
		// billing. The CLI and quota use the sign-in. A missing sign-in
		// fails with agy's "Run agy acp --login" error.
		AuthMethod: "oauth-personal",
		EffortID:   effortID,
		Images:     true,
		NewTools:   newTools,
		// agy's advertised "compact" command summarizes the conversation
		// and ends the turn; testdata/compact.jsonl records it. Text after
		// it is a prompt agy answers after compacting, not summary
		// instructions, so custom instructions stay unsupported.
		CompactPrompt: "/compact",
	})}
	b.QuotaProviderID = agent.QuotaProviderAntigravity
	b.SetModelInventory(agent.CachedModelInventory(cacheDir, harness.Antigravity, envVars))
	return b
}

// Start launches agy's ACP server through the shared relay.
func (b *Backend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	return b.Backend.Start(ctx, opts, agentacp.Launch{Args: b.AgentArgs(agent.HarnessArgs{})})
}

// AgentArgs implements agent.Backend. caic tasks run unattended, so agy
// approves its own tool requests, as it did in stream-json mode.
func (*Backend) AgentArgs(_ agent.HarnessArgs) []string {
	return []string{"agy", "acp", "--dangerously-skip-permissions"}
}

// SetModelInventory keeps the order sortModels chose; shared ACP
// normalization would reorder it alphabetically.
func (b *Backend) SetModelInventory(inventory agent.ModelInventory) {
	b.Base.SetModelInventory(inventory)
}

// NewWire routes each record to the protocol that wrote it.
func (b *Backend) NewWire() agent.WireFormat {
	w := b.Backend.NewWire().(acpWire) //nolint:forcetypeassert // Shared ACP's wire encodes compaction.
	return &wire{router: router{acp: w, streamJSON: newStreamWire()}, acp: w}
}

// NewMetadataParser routes each record to the protocol that wrote it.
func (b *Backend) NewMetadataParser() agent.MessageParser {
	return &router{acp: b.Backend.NewMetadataParser(), streamJSON: &metadataParser{accounting: newAccounting()}}
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
	out, err := agent.SSHCommand(ctx, target.SSHHost, args...).Output()
	if err != nil {
		// agy 1.5.14 reports a missing sign-in on stderr only, with no
		// stream-json event.
		if e, ok := errors.AsType[*exec.ExitError](err); ok {
			if msg := lastStderrLine(e.Stderr); msg != "" {
				return agent.ModelInventory{}, fmt.Errorf("agy models: %w: %s", err, msg)
			}
		}
		return agent.ModelInventory{}, fmt.Errorf("agy models: %w", err)
	}
	return parseModels(out)
}

// terminalControl matches the ANSI control sequences agy writes to redraw
// its status line, such as "\x1b[K".
var terminalControl = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// lastStderrLine returns the last visible stderr line. Text before a carriage
// return is overwritten on a terminal, so it is dropped.
func lastStderrLine(stderr []byte) string {
	last := ""
	for line := range strings.Lines(string(stderr)) {
		line = strings.TrimRight(line, "\r\n")
		line = line[strings.LastIndexByte(line, '\r')+1:]
		if line = strings.TrimSpace(terminalControl.ReplaceAllString(line, "")); line != "" {
			last = line
		}
	}
	return last
}

var (
	_ agent.Backend      = (*Backend)(nil)
	_ agent.ModelFetcher = (*Backend)(nil)
)

// router dispatches records by protocol. A task's log holds stream-json
// records only before its first ACP record: once caic launches agy in ACP mode,
// every later process of the task speaks ACP, including a stream-json
// conversation resumed through session/resume. Its state belongs to the
// goroutine that parses the ordered records.
type router struct {
	acp, streamJSON agent.MessageParser

	acpSeen bool
}

// ParseInput implements agent.MessageParser.
func (r *router) ParseInput(line []byte) ([]agent.Message, error) {
	return r.parser(line).ParseInput(line)
}

// ParseOutput implements agent.MessageParser.
func (r *router) ParseOutput(line []byte) ([]agent.Message, error) {
	return r.parser(line).ParseOutput(line)
}

// parser selects the protocol of line. Each JSON-RPC record carries a
// top-level "jsonrpc" member; a stream-json record never does. A malformed
// line reaches the ACP parser, which reports it.
func (r *router) parser(line []byte) agent.MessageParser {
	if r.acpSeen {
		return r.acp
	}
	if v, err := agent.JSONField(line, "jsonrpc"); err != nil || v != nil {
		r.acpSeen = err == nil
		return r.acp
	}
	return r.streamJSON
}

// acpWire is shared ACP's wire, which also encodes compaction.
type acpWire interface {
	agent.WireFormat
	agent.CompactCommand
}

// wire encodes every prompt and compaction over ACP. A stream-json process
// that caic adopts across the upgrade has no ACP session, so its prompts fail;
// reviving the task resumes the conversation over ACP.
type wire struct {
	router

	acp acpWire
}

// EncodePrompt implements agent.WireFormat.
func (w *wire) EncodePrompt(p agent.Prompt) ([]byte, error) {
	return w.acp.EncodePrompt(p)
}

// EncodeCompact implements agent.CompactCommand.
func (w *wire) EncodeCompact(instructions string) ([]byte, error) {
	return w.acp.EncodeCompact(instructions)
}

// modelFamilyOrder lists model families from most to least preferred.
var modelFamilyOrder = []string{"gemini", "claude"}

// modelNameOrder lists, per family, the model names from most to least
// preferred. Names not listed sort last.
var modelNameOrder = map[string][]string{
	"gemini": {"flash", "pro"},
	"claude": {"opus", "sonnet"},
}

// modelEffortSuffixes lists reasoning-effort suffixes that agy appends to
// model IDs in `agy models`.
var modelEffortSuffixes = []string{"xhigh", "high", "medium", "low"}

// modelParts is a base agy model ID split on "-". "gemini-3.8-flash" is
// family "gemini", name "flash", version 3.8.
type modelParts struct {
	id      string
	family  string
	name    string
	version float64
}

// splitModelEffort strips a trailing reasoning-effort suffix from id.
// Models without an effort suffix, such as gemini-pro-orchestrator-g1-pro,
// are returned unchanged with an empty effort.
func splitModelEffort(id string) (base, effort string) {
	for _, e := range modelEffortSuffixes {
		if b, ok := strings.CutSuffix(id, "-"+e); ok {
			return b, e
		}
	}
	return id, ""
}

// parseModelID splits a base model ID on "-". The first segment is the family,
// the first run of numeric segments is the version and the other segments make
// the name, so "claude-opus-5-5" is family "claude", name "opus", version 5.5.
func parseModelID(id string) modelParts {
	segs := strings.Split(id, "-")
	p := modelParts{id: id, family: segs[0]}
	segs = segs[1:]
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

// sortModels hides gpt-oss, groups effort variants under their base model ID,
// keeps only the latest version of each family-name combination, then orders
// the rest by family and name.
func sortModels(ids []string) []agent.Model {
	type key struct{ family, name string }
	var parts []modelParts
	efforts := map[string][]string{}
	latest := map[key]float64{}
	for _, id := range ids {
		if strings.HasPrefix(id, "gpt-oss") {
			continue
		}
		base, effort := splitModelEffort(id)
		if _, seen := efforts[base]; !seen {
			p := parseModelID(base)
			parts = append(parts, p)
			k := key{p.family, p.name}
			latest[k] = max(latest[k], p.version)
		}
		if effort != "" {
			efforts[base] = append(efforts[base], effort)
		} else if _, seen := efforts[base]; !seen {
			efforts[base] = nil
		}
	}
	parts = slices.DeleteFunc(parts, func(p modelParts) bool {
		return p.version < latest[key{p.family, p.name}]
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
		)
	})
	out := make([]agent.Model, len(parts))
	for i, p := range parts {
		var opts []string
		if len(efforts[p.id]) != 0 {
			opts = agent.SortEfforts(efforts[p.id])
		}
		out[i] = agent.Model{ID: p.id, EffortOptions: opts}
	}
	return out
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
			return agent.ModelInventory{Models: sortModels(ids)}, nil
		default:
			return agent.ModelInventory{}, fmt.Errorf("agy: unexpected models event %q", ev.Event)
		}
	}
	if err := sc.Err(); err != nil {
		return agent.ModelInventory{}, fmt.Errorf("read agy models: %w", err)
	}
	return agent.ModelInventory{}, errors.New("agy exited without a models result")
}
