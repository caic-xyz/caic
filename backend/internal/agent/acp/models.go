// Normalizes ACP model inventories and flattens grouped selectable configuration values.

package acp

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/maruel/genai/acp"
)

// NormalizeModels sorts and deduplicates native model inventories and effort choices.
func NormalizeModels(models []agent.Model) []agent.Model {
	byID := make(map[string]agent.Model, len(models))
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model.ID == "" {
			continue
		}
		if _, ok := byID[model.ID]; !ok {
			ids = append(ids, model.ID)
		}
		model.EffortOptions = agent.SortEfforts(model.EffortOptions)
		byID[model.ID] = model
	}

	ids = agent.SortModels(ids)
	normalized := make([]agent.Model, 0, len(ids))
	for _, id := range ids {
		normalized = append(normalized, byID[id])
	}
	return normalized
}

// ConfigValues returns advertised leaf values, preserving native option order.
func ConfigValues(options []acp.ConfigOptionValue) []string {
	var values []string
	for _, option := range options {
		if len(option.Options) > 0 {
			values = append(values, ConfigValues(option.Options)...)
		} else if option.Value != "" {
			values = append(values, option.Value)
		}
	}
	return values
}
