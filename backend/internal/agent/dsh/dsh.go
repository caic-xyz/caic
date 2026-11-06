// Package dsh implements DeepSeek Harness launch, native model routes, tool evidence, and discovery over shared ACP.

package dsh

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/maruel/genai/acp"

	"slices"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agentacp "github.com/caic-xyz/caic/backend/internal/agent/acp"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

const dshEffortID acp.ConfigOptionID = "reasoning_effort"

// New creates a DeepSeek Harness backend sharing the standard ACP transport.
// Native dsh sessions use session/resume and opaque JSON model-route values.
// Image input depends on the selected route and durable store support; those
// capabilities and compaction are not yet verified for caic task sessions, so
// neither capability is advertised by this backend.
//
// TODO: Project usage_update.used as independent canonical context occupancy,
// preserving zero and billed usage across live/restored task and API views.
// TODO: Enable images after verifying exact-route capabilities, durable
// attachments, and native prompt/history behavior.
// TODO: Enable compaction after proving a native ACP command and its context
// boundary and accounting semantics.
// TODO: Add native subagent cards only after retaining dsh-specific ownership
// and lifecycle evidence; generic tool titles do not establish delegation.
// TODO: Project native skill-tool reads after retaining dsh-specific evidence;
// shell skill reads use the native bash command field.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{Backend: agentacp.New(harness.Dsh, &agentacp.Config{
		Args: []string{"dsh", "--profile", "acp"}, ResumeMethod: acp.MethodSessionResume,
		EffortID: dshEffortID, Models: modelCodec{}, NewTools: newTools, Permissions: agentacp.PermissionApprover{},
	})}
	b.SetModelInventory(agent.CachedModelInventory(cacheDir, harness.Dsh, envVars))
	return b
}

func encodeDSHModel(id string) (string, error) {
	provider, model, ok := strings.Cut(id, "/")
	if !ok || provider == "" || model == "" {
		return "", fmt.Errorf("invalid dsh model ID %q; want provider/model", id)
	}
	data, err := json.Marshal([2]string{provider, model})
	return string(data), err
}

func decodeDSHModel(value string) (string, error) {
	var route []string
	if err := json.Unmarshal([]byte(value), &route); err != nil {
		return "", fmt.Errorf("invalid dsh model route: %w", err)
	}
	if len(route) != 2 || route[0] == "" || route[1] == "" || strings.Contains(route[0], "/") {
		return "", errors.New("invalid dsh model route; want [provider, model]")
	}
	return route[0] + "/" + route[1], nil
}

// Backend implements DeepSeek Harness independently of other ACP harnesses.
type Backend struct{ *agentacp.Backend }

// FetchModelInventory discovers native DeepSeek model routes and their effort choices.
func (b *Backend) FetchModelInventory(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) (agent.ModelInventory, error) {
	return fetchDSHModels(ctx, target, extraEnv)
}

type modelCodec struct{}

func (modelCodec) Select(requested, current string, options []acp.ConfigOptionValue) (string, error) {
	route, err := encodeDSHModel(requested)
	if err != nil {
		return "", err
	}
	if route != current && !slices.Contains(agentacp.ConfigValues(options), route) {
		return "", errors.New("requested dsh model is not advertised by the ACP session")
	}
	return route, nil
}
func (modelCodec) Decode(route string) (string, error) { return decodeDSHModel(route) }

type toolParser struct{}

func (toolParser) Parse(tool *acp.ToolCallUpdateUpdate, messages []agent.Message) ([]agent.Message, error) {
	if tool != nil && tool.Status == acp.StatusInProgress {
		for i, message := range messages {
			if use, ok := message.(*agent.ToolUseMessage); ok && use.Name == "Bash" {
				// A malformed or differently shaped input does not prove a skill read.
				if command, err := agent.JSONString(tool.RawInput, "command"); err == nil {
					messages = slices.Insert(messages, i+1, agent.InferredSkillReadsFromCommand(command, "", tool.ToolCallID)...)
				}
				break
			}
		}
	}
	if tool != nil && tool.Status == acp.StatusCompleted {
		if output := agentacp.ToolOutput(tool); output != "" {
			messages = append([]agent.Message{&agent.ToolOutputDeltaMessage{ToolUseID: tool.ToolCallID, Delta: output}}, messages...)
		}
	}
	return messages, nil
}

func newTools() agentacp.ToolParser { return toolParser{} }

func fetchDSHModels(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) (inventory agent.ModelInventory, err error) {
	if target.SSHHost == "" {
		return inventory, errors.New("agent connection target missing SSH host")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := append([]string{"env"}, extraEnv...)
	args = append(args, "dsh", "--profile", "acp")
	for i := range args {
		args[i] = agent.ShellQuote(args[i])
	}
	// Dsh reads cwd/.env before ACP starts. The login directory contains md's
	// environment file, whose launch controls (including DSH_HOME) dsh rejects
	// in project files. Match the inventory session's stable root workspace.
	cmd := exec.CommandContext(ctx, "ssh", target.SSHHost, "cd / && "+strings.Join(args, " ")) //nolint:gosec // SSH target is owned by the runtime; remote arguments are shell quoted.
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return inventory, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return inventory, errors.Join(err, stdin.Close())
	}
	if err := cmd.Start(); err != nil {
		return inventory, errors.Join(err, stdin.Close(), stdout.Close())
	}
	defer func() {
		closeErr := stdin.Close()
		if err != nil {
			cancel()
		}
		err = errors.Join(err, closeErr, cmd.Wait())
	}()
	inventory, err = discoverDSHModels(stdin, stdout)
	return inventory, err
}

// discoveryRPC is a bounded, content-free ACP exchange used only for inventory.
// Task sessions use the ordered recording handshake instead.
type discoveryRPC struct {
	stdin io.Writer
	sc    *bufio.Scanner
	id    int64
}

func (r *discoveryRPC) call(method acp.Method, params any) (json.RawMessage, error) {
	r.id++
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	data, err := agentacp.MarshalInput(acp.JSONRPCRequest{JSONRPC: "2.0", ID: r.id, Method: method, Params: raw})
	if err != nil {
		return nil, err
	}
	if _, err := r.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write dsh %s: %w", method, err)
	}
	for r.sc.Scan() {
		var msg acp.JSONRPCMessage
		if err := json.Unmarshal(r.sc.Bytes(), &msg); err != nil {
			return nil, fmt.Errorf("decode dsh %s response: %w", method, err)
		}
		if msg.IsAgentRequest() {
			return nil, fmt.Errorf("unexpected dsh inventory request %q", msg.Method)
		}
		if !msg.IsResponse() {
			continue
		}
		var id int64
		if err := json.Unmarshal(msg.ID, &id); err != nil {
			return nil, fmt.Errorf("decode dsh response ID: %w", err)
		}
		if id != r.id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("dsh %s failed (JSON-RPC code %d)", method, msg.Error.Code)
		}
		if len(msg.Result) == 0 || string(msg.Result) == "null" {
			return nil, fmt.Errorf("dsh %s response missing result", method)
		}
		return msg.Result, nil
	}
	return nil, fmt.Errorf("read dsh %s: %w", method, errors.Join(io.ErrUnexpectedEOF, r.sc.Err()))
}

func discoverDSHModels(stdin io.Writer, stdout io.Reader) (agent.ModelInventory, error) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 4096), 16<<20)
	rpc := discoveryRPC{stdin: stdin, sc: sc}
	if _, err := rpc.call(acp.MethodInitialize, acp.InitializeParams{
		ProtocolVersion: 1,
		ClientInfo:      acp.ClientInfo{Name: "caic", Version: "1.0.0"},
	}); err != nil {
		return agent.ModelInventory{}, err
	}
	// Discovery does not execute prompts or tools; the container's root is a
	// stable absolute workspace that does not depend on an individual task.
	result, err := rpc.call(acp.MethodSessionNew, acp.SessionNewParams{Cwd: "/", McpServers: []acp.MCPServer{}})
	if err != nil {
		return agent.ModelInventory{}, err
	}
	var session acp.SessionNewResult
	if err := json.Unmarshal(result, &session); err != nil {
		return agent.ModelInventory{}, fmt.Errorf("decode dsh inventory session: %w", err)
	}
	if session.SessionID == "" {
		return agent.ModelInventory{}, errors.New("dsh inventory session missing ID")
	}
	var choices []string
	for i := range session.ConfigOptions {
		option := &session.ConfigOptions[i]
		if option.ID == acp.ConfigOptionModel {
			choices = append(choices, agentacp.ConfigValues(option.Options)...)
		}
	}
	if len(choices) == 0 {
		return agent.ModelInventory{}, errors.New("dsh ACP returned no selectable models")
	}
	models := make([]agent.Model, 0, len(choices))
	for _, route := range choices {
		id, err := decodeDSHModel(route)
		if err != nil {
			return agent.ModelInventory{}, err
		}
		// Effort choices are model-specific and can change with model selection.
		result, err := rpc.call(acp.MethodSessionSetConfigOption, acp.SetSessionConfigOptionParams{
			SessionID: session.SessionID, ConfigID: acp.ConfigOptionModel, Value: route,
		})
		if err != nil {
			return agent.ModelInventory{}, fmt.Errorf("discover dsh model %s: %w", id, err)
		}
		var configured acp.SetSessionConfigOptionResult
		if err := json.Unmarshal(result, &configured); err != nil {
			return agent.ModelInventory{}, fmt.Errorf("decode dsh model configuration: %w", err)
		}
		if len(configured.ConfigOptions) == 0 {
			return agent.ModelInventory{}, errors.New("dsh model configuration missing configOptions")
		}
		model := agent.Model{ID: id}
		for i := range configured.ConfigOptions {
			option := &configured.ConfigOptions[i]
			if option.ID == dshEffortID {
				model.EffortOptions = append(model.EffortOptions, agentacp.ConfigValues(option.Options)...)
			}
		}
		models = append(models, model)
	}
	return agent.ModelInventory{Models: agentacp.NormalizeModels(models)}, nil
}
