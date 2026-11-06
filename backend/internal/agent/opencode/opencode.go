// Package opencode implements OpenCode launch, discovery, native delegation, and skill evidence over shared ACP.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	agentacp "github.com/caic-xyz/caic/backend/internal/agent/acp"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/maruel/genai/acp"
	"github.com/maruel/genai/providers/opencode"
)

// Backend implements the OpenCode harness using shared ACP sessions.
type Backend struct{ *agentacp.Backend }

// New creates an OpenCode backend with independent native tool evidence.
func New(cacheDir string, envVars []string) *Backend {
	b := &Backend{Backend: agentacp.New(harness.OpenCode, &agentacp.Config{
		Args: []string{"opencode", "acp"},
		// session/load needs OpenCode's persisted ~/.local/share/opencode state.
		// Md mounts it across containers; custom storage must also stay accessible.
		// https://opencode.ai/docs/troubleshooting/#storage
		ResumeMethod: acp.MethodSessionLoad,
		EffortID:     opencode.ConfigOptionEffort, Images: true, NewTools: newTools,
		// Native ACP compact calls session.summarize without command arguments.
		// https://github.com/anomalyco/opencode/blob/148842ef7cb1bc34192b3fff57d0f9721ab9c801/packages/opencode/src/acp/service.ts#L570-L584
		CompactPrompt: "/compact",
	})}
	b.SetModelInventory(agent.CachedModelInventory(cacheDir, harness.OpenCode, envVars))
	return b
}

// FetchModelInventory discovers OpenCode's native provider/model catalog.
func (b *Backend) FetchModelInventory(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) (agent.ModelInventory, error) {
	models, err := fetchModels(ctx, target, extraEnv)
	if err != nil {
		return agent.ModelInventory{}, err
	}
	return agent.ModelInventory{Models: models}, nil
}

func fetchModels(ctx context.Context, target runtime.ConnectionTarget, extraEnv []string) ([]agent.Model, error) {
	if target.SSHHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	args := []string{target.SSHHost}
	if len(extraEnv) > 0 {
		args = append(args, "env")
		args = append(args, extraEnv...)
	}
	args = append(args, "opencode", "models", "--refresh", "--verbose")
	out, err := exec.CommandContext(ctx, "ssh", args...).Output() //nolint:gosec // target is not user-controlled
	if err != nil {
		return nil, fmt.Errorf("opencode models: %w", err)
	}
	models, err := parseModels(out)
	if err != nil {
		return nil, fmt.Errorf("parse opencode models: %w", err)
	}
	return models, nil
}

// modelMetadata is the metadata "opencode models --verbose" prints beneath a
// model ID.
type modelMetadata struct {
	Variants map[string]json.RawMessage `json:"variants"`
	Limit    modelLimit                 `json:"limit"`
}

// modelLimit holds the token limits OpenCode publishes for a model.
type modelLimit struct {
	Context int `json:"context"`
}

// parseModels reads the model ID and pretty-printed metadata pairs
// emitted by "opencode models --verbose". OpenCode exposes model variants as
// its ACP effort options.
func parseModels(out []byte) ([]agent.Model, error) {
	lines := strings.Split(string(out), "\n")
	models := make([]agent.Model, 0)
	for line := 0; line < len(lines); line++ {
		model := strings.TrimSpace(lines[line])
		provider, id, ok := strings.Cut(model, "/")
		if !ok || provider == "" || id == "" {
			continue // E.g. the colored "Models cache refreshed" status line.
		}

		var data []byte
		var info modelMetadata
		for line++; line < len(lines); line++ {
			data = append(data, lines[line]...)
			data = append(data, '\n')
			if err := json.Unmarshal(data, &info); err == nil {
				break
			} else if line == len(lines)-1 {
				return nil, fmt.Errorf("%s metadata: %w", model, err)
			}
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("%s missing metadata", model)
		}
		if err := json.Unmarshal(data, &info); err != nil {
			return nil, fmt.Errorf("%s metadata: %w", model, err)
		}

		efforts := make([]string, 0, len(info.Variants))
		for effort := range info.Variants {
			efforts = append(efforts, effort)
		}
		models = append(models, agent.Model{ID: model, EffortOptions: efforts, ContextWindow: info.Limit.Context})
	}
	if len(models) == 0 {
		return nil, errors.New("no model metadata")
	}
	return agentacp.NormalizeModels(models), nil
}
