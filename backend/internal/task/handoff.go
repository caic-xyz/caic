// Builds a bounded prompt for continuing a task on another harness.

package task

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

const (
	defaultHandoffPromptMaxBytes = 16 * 1024
	maxHandoffConversationTurns  = 6
	maxHandoffDiffFiles          = 16
	maxHandoffMetadataBytes      = 160
	maxHandoffOriginalBytes      = 2 * 1024
	maxHandoffRepos              = 4
	maxHandoffResultBytes        = 2 * 1024
	maxHandoffTurnBytes          = 768
	handoffTruncationNotice      = "\n\n[Handoff prompt truncated to fit the configured size limit.]\n"
)

// BuildHandoffPrompt renders task context for a fresh harness session.
//
// Only direct user and assistant text is included from the conversation.
// Thinking, tool activity, raw output, and other private operational messages
// are omitted. A non-positive maxBytes uses a conservative default.
func BuildHandoffPrompt(source *Task, maxBytes int) string {
	input := snapshotHandoffPromptInput(source)
	if maxBytes <= 0 {
		maxBytes = defaultHandoffPromptMaxBytes
	}

	var b strings.Builder
	b.WriteString("# Continue this task after quota exhaustion\n\n")
	b.WriteString("The previous coding harness could not continue because its quota was exhausted. Inspect the repository and current filesystem state before changing files, then continue the task from where it stopped.\n\n")
	b.WriteString("## Source task\n\n")
	fmt.Fprintf(&b, "- Title: %s\n", oneLine(input.title))
	fmt.Fprintf(&b, "- Harness: %s\n", input.harness)
	if input.model != "" {
		fmt.Fprintf(&b, "- Model: %s\n", oneLine(input.model))
	}
	writeQuota(&b, &input.rateLimit)
	for _, repo := range input.repos[:min(len(input.repos), maxHandoffRepos)] {
		branch := repo.Branch
		if repo.BaseBranch != "" {
			branch = repo.BaseBranch + ".." + repo.Branch
		}
		fmt.Fprintf(&b, "- Repository: %s (%s)\n", oneLine(repo.Name), oneLine(branch))
	}
	if len(input.repos) > maxHandoffRepos {
		fmt.Fprintf(&b, "- ... %d more repositories omitted\n", len(input.repos)-maxHandoffRepos)
	}

	b.WriteString("\n### Original request\n\n")
	writeQuote(&b, input.initialPrompt.Text, maxHandoffOriginalBytes)
	if len(input.initialPrompt.Images) > 0 {
		fmt.Fprintf(&b, "\nThe original request included %d image attachment(s).\n", len(input.initialPrompt.Images))
	}

	if result, ok := latestHandoffResult(input.messages); ok {
		if result.IsError {
			b.WriteString("\n## Latest harness error\n\n")
		} else {
			b.WriteString("\n## Latest assistant result\n\n")
		}
		writeQuote(&b, result.Text, maxHandoffResultBytes)
	}

	if len(input.diffStat) > 0 {
		b.WriteString("\n## Current changes\n\n")
		for _, file := range input.diffStat[:min(len(input.diffStat), maxHandoffDiffFiles)] {
			if file.Binary {
				fmt.Fprintf(&b, "- %s (binary)\n", oneLine(file.Path))
				continue
			}
			fmt.Fprintf(&b, "- %s (+%d/-%d)\n", oneLine(file.Path), file.Added, file.Deleted)
		}
		if len(input.diffStat) > maxHandoffDiffFiles {
			fmt.Fprintf(&b, "- ... %d more changed files omitted\n", len(input.diffStat)-maxHandoffDiffFiles)
		}
	}

	turns := recentHandoffTurns(input.initialPrompt.Text, input.messages)
	if len(turns) > 0 {
		b.WriteString("\n## Recent conversation\n")
		for _, turn := range turns {
			fmt.Fprintf(&b, "\n### %s\n\n", turn.role)
			writeQuote(&b, turn.text, maxHandoffTurnBytes)
		}
	}

	return limitHandoffPrompt(b.String(), maxBytes)
}

func snapshotHandoffPromptInput(source *Task) handoffPromptInput {
	source.mu.Lock()
	defer source.mu.Unlock()

	model := source.reportedModel
	if model == "" {
		model = source.Model
	}
	messages := make([]agent.Message, len(source.timeline))
	for i, entry := range source.timeline {
		messages[i] = entry.Message
	}
	return handoffPromptInput{
		initialPrompt: source.InitialPrompt,
		title:         source.title,
		harness:       source.Harness,
		model:         model,
		repos:         slices.Clone(source.Repos),
		rateLimit:     handoffRateLimitLocked(source),
		diffStat:      slices.Clone(source.liveDiffStat),
		messages:      messages,
	}
}

type handoffPromptInput struct {
	initialPrompt agent.Prompt
	title         string
	harness       harness.Name
	model         string
	repos         []taskslog.RepoMount
	rateLimit     RateLimit
	diffStat      agent.DiffStat
	messages      []agent.Message
}

type handoffTurn struct {
	role string
	text string
}

type handoffResult struct {
	Text    string
	IsError bool
}

func writeQuota(b *strings.Builder, limit *RateLimit) {
	if limit.Status != agent.RateLimitStatusRejected {
		return
	}
	label := limit.QuotaLabel
	if label == "" {
		label = string(limit.QuotaProvider)
	}
	if label == "" {
		label = "provider"
	}
	window := limit.QuotaWindow
	if window == "" {
		window = limit.RateLimitType
	}
	fmt.Fprintf(b, "- Quota block: %s", oneLine(label))
	if window != "" {
		fmt.Fprintf(b, " %s window", oneLine(window))
	}
	b.WriteString(" rejected the request")
	if !limit.ResetsAt.IsZero() {
		fmt.Fprintf(b, "; resets at %s", limit.ResetsAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	b.WriteString("\n")
}

func handoffRateLimitLocked(source *Task) RateLimit {
	if source.rateLimit.Status == agent.RateLimitStatusRejected {
		return source.rateLimit
	}
	seen := make(map[quotaWindowKey]struct{})
	for _, entry := range slices.Backward(source.timeline) {
		message, ok := entry.Message.(*agent.RateLimitMessage)
		if !ok {
			continue
		}
		limit := rateLimitFromMessage(message)
		key := rateLimitKey(&limit)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if limit.Status == agent.RateLimitStatusRejected && !limit.IsUsingOverage {
			return limit
		}
	}
	return RateLimit{}
}

func latestHandoffResult(messages []agent.Message) (handoffResult, bool) {
	for i, message := range slices.Backward(messages) {
		result, ok := message.(*agent.ResultMessage)
		if !ok {
			continue
		}
		text := result.Result
		if strings.TrimSpace(text) == "" {
			entries := make([]agent.TimedMessage, i+1)
			for j, message := range messages[:i+1] {
				entries[j].Message = message
			}
			text = fallbackResultText(entries)
		}
		if text == "" {
			text = "(no result text reported)"
		}
		return handoffResult{Text: text, IsError: result.IsError}, true
	}
	return handoffResult{}, false
}

func recentHandoffTurns(initialPrompt string, messages []agent.Message) []handoffTurn {
	turns := make([]handoffTurn, 0, maxHandoffConversationTurns)
	skippedInitialPrompt := false
	for _, message := range messages {
		var turn handoffTurn
		switch m := message.(type) {
		case *agent.UserInputMessage:
			if !skippedInitialPrompt && m.Text == initialPrompt {
				skippedInitialPrompt = true
				continue
			}
			turn = handoffTurn{role: "User", text: m.Text}
		case *agent.TextMessage:
			turn = handoffTurn{role: "Assistant", text: m.Text}
		default:
			continue
		}
		if strings.TrimSpace(turn.text) != "" {
			turns = append(turns, turn)
		}
	}
	if len(turns) > maxHandoffConversationTurns {
		turns = turns[len(turns)-maxHandoffConversationTurns:]
	}
	return turns
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxHandoffMetadataBytes {
		return s
	}
	return truncateUTF8(s, maxHandoffMetadataBytes-len("…")) + "…"
}

func writeQuote(b *strings.Builder, s string, maxBytes int) {
	if strings.TrimSpace(s) == "" {
		b.WriteString("> (none)\n")
		return
	}
	var quote strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		fmt.Fprintf(&quote, "> %s\n", line)
	}
	text := quote.String()
	if len(text) > maxBytes {
		text = truncateUTF8(text, maxBytes-len("…\n")) + "…\n"
	}
	b.WriteString(text)
}

func limitHandoffPrompt(prompt string, maxBytes int) string {
	if len(prompt) <= maxBytes {
		return prompt
	}
	if maxBytes <= len(handoffTruncationNotice) {
		return truncateUTF8(handoffTruncationNotice, maxBytes)
	}
	return truncateUTF8(prompt, maxBytes-len(handoffTruncationNotice)) + handoffTruncationNotice
}

func truncateUTF8(s string, maxBytes int) string {
	maxBytes = min(maxBytes, len(s))
	for maxBytes > 0 && !utf8.ValidString(s[:maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}
