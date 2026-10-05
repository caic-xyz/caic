// Builds a bounded prompt for continuing a task on another harness.

package task

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

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
	if input.rateLimit.Status == agent.RateLimitStatusRejected {
		b.WriteString("# Continue this task after quota exhaustion\n\n")
		b.WriteString("The previous coding harness could not continue because its quota was exhausted. Inspect the repository and current filesystem state before changing files, then continue the task from where it stopped.\n\n")
	} else {
		b.WriteString("# Continue this task in a new agent\n\n")
		b.WriteString("Continue the task in a fresh coding-agent session. Inspect the repository and current filesystem state before changing files, then continue from where the previous agent stopped.\n\n")
	}
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

	if input.hasResult {
		if input.result.IsError {
			b.WriteString("\n## Latest harness error\n\n")
		} else {
			b.WriteString("\n## Latest assistant result\n\n")
		}
		writeQuote(&b, input.result.Text, maxHandoffResultBytes)
	}

	if len(input.diffStat) > 0 {
		b.WriteString("\n## Current changes\n\n")
		for _, file := range input.diffStat[:min(len(input.diffStat), maxHandoffDiffFiles)] {
			if file.Binary {
				fmt.Fprintf(&b, "- %s (binary)\n", oneLine(file.Path))
				continue
			}
			fmt.Fprintf(&b, "- %s (+%d/-%d)\n", oneLine(file.Path), file.LinesAdded, file.LinesDeleted)
		}
		if len(input.diffStat) > maxHandoffDiffFiles {
			fmt.Fprintf(&b, "- ... %d more changed files omitted\n", len(input.diffStat)-maxHandoffDiffFiles)
		}
	}

	if len(input.turns) > 0 {
		b.WriteString("\n## Recent conversation\n")
		for _, turn := range input.turns {
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
		model = source.RequestedModel
	}
	entries := source.timelineViewLocked()
	result, hasResult := latestHandoffResult(entries)
	return handoffPromptInput{
		initialPrompt: source.InitialPrompt,
		title:         source.title,
		harness:       source.Harness,
		model:         model,
		repos:         slices.Clone(source.Repos),
		rateLimit:     handoffRateLimitLocked(source),
		diffStat:      slices.Clone(source.liveDiffStat),
		result:        result,
		hasResult:     hasResult,
		turns:         recentHandoffTurns(source.InitialPrompt.Text, entries),
	}
}

type handoffPromptInput struct {
	initialPrompt agent.Prompt
	title         string
	harness       harness.Name
	model         string
	repos         []taskslog.RepoMount
	rateLimit     RateLimit
	diffStat      v3.DiffStat
	result        handoffResult
	hasResult     bool
	turns         []handoffTurn
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
	entries := source.timelineViewLocked()
	for _, entry := range entries.Backward() {
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

func latestHandoffResult(entries timelineEntries) (handoffResult, bool) {
	for i, entry := range entries.Backward() {
		result, ok := entry.Message.(*agent.ResultMessage)
		if !ok {
			continue
		}
		text := result.Result
		if strings.TrimSpace(text) == "" {
			text = fallbackResultText(entries.Slice(i + 1))
		}
		if text == "" {
			text = "(no result text reported)"
		}
		return handoffResult{Text: text, IsError: result.IsError}, true
	}
	return handoffResult{}, false
}

func recentHandoffTurns(initialPrompt string, entries timelineEntries) []handoffTurn {
	turns := make([]handoffTurn, 0, maxHandoffConversationTurns)
	skippedInitialPrompt := false
	for _, entry := range entries.Forward() {
		var turn handoffTurn
		switch m := entry.Message.(type) {
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
			if len(turns) == maxHandoffConversationTurns {
				copy(turns, turns[1:])
				turns[len(turns)-1] = turn
			} else {
				turns = append(turns, turn)
			}
		}
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
