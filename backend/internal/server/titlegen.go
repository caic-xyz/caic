// Title generation using a cheap LLM call to summarize task conversations.
package server

import (
	"context"
	"log/slog"
	"strings"

	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/task"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
)

// titleGenerator generates short task titles from conversation content using a
// cheap LLM. If the provider is nil (unconfigured), all operations are no-ops.
type titleGenerator struct {
	provider genai.Provider
}

// newTitleGenerator creates a titleGenerator from provider/model config strings.
// Returns a no-op generator if provider is empty or initialization fails.
func newTitleGenerator(ctx context.Context, providerName, model string) *titleGenerator {
	if providerName == "" {
		return &titleGenerator{}
	}
	cfg, ok := providers.All[providerName]
	if !ok || cfg.Factory == nil {
		slog.Warn("unknown LLM provider for title generation", "provider", providerName)
		return &titleGenerator{}
	}
	var opts []genai.ProviderOption
	if model != "" {
		opts = append(opts, genai.ProviderOptionModel(model))
	} else {
		opts = append(opts, genai.ModelCheap)
	}
	p, err := cfg.Factory(ctx, opts...)
	if err != nil {
		slog.Warn("failed to create LLM provider for title generation", "provider", providerName, "err", err)
		return &titleGenerator{}
	}
	slog.Info("title generation enabled", "provider", providerName, "model", p.ModelID())
	return &titleGenerator{provider: p}
}

const titleSystemPrompt = "Summarize this coding task conversation in 3-8 words as a short title. Reply with ONLY the title, no quotes."

// generate extracts user prompt texts and result texts from the task's messages
// and asks the LLM for a short title. Returns "" on failure or if unconfigured.
func (tg *titleGenerator) generate(ctx context.Context, t *task.Task) string {
	if tg.provider == nil {
		return ""
	}
	msgs := t.Messages()
	var b strings.Builder
	for _, m := range msgs {
		if v, ok := m.(*agent.ResultMessage); ok && v.Result != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("Result: ")
			b.WriteString(v.Result)
		}
	}
	// Prepend the original prompt.
	input := "Prompt: " + t.Prompt
	if b.Len() > 0 {
		input += "\n" + b.String()
	}
	// Truncate to ~2000 chars to keep costs minimal.
	if len(input) > 2000 {
		input = input[:2000]
	}

	res, err := tg.provider.GenSync(ctx,
		genai.Messages{genai.NewTextMessage(input)},
		&genai.GenOptionText{
			SystemPrompt: titleSystemPrompt,
			MaxTokens:    64,
			Temperature:  0.3,
		},
	)
	if err != nil {
		slog.Warn("title generation LLM call failed", "task", t.ID, "err", err)
		return ""
	}
	title := strings.TrimSpace(res.String())
	// Strip surrounding quotes if the model adds them despite instructions.
	title = strings.Trim(title, "\"'`")
	return title
}
