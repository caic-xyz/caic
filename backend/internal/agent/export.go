// Reads a harness-specific JSONL log and renders the conversation as markdown.
// Used by Backend.ExportDiscussion to provide a harness-agnostic export.

package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// exportLine is the minimal envelope for routing a JSONL line during export.
type exportLine struct {
	Type string `json:"type"`
}

// ExportDiscussion reads a JSONL log file using the given harness parser and
// returns a self-contained markdown document. The parser should be a fresh
// instance from Backend.NewParser() so stateful wire formats can synthesize
// terminal messages.
func ExportDiscussion(path string, parseFn func([]byte) ([]Message, error)) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)

	var (
		meta    MetaMessage
		result  *MetaResultMessage
		pr      *MetaPRMessage
		msgs    []Message
		metaSet bool
	)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var env exportLine
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}

		switch env.Type {
		case "caic_meta":
			var m MetaMessage
			if err := json.Unmarshal(line, &m); err == nil {
				meta = m
				metaSet = true
			}

		case "caic_result":
			var m MetaResultMessage
			if err := json.Unmarshal(line, &m); err == nil {
				result = &m
			}

		case "caic_pr":
			var m MetaPRMessage
			if err := json.Unmarshal(line, &m); err == nil {
				pr = &m
			}

		case "caic_diff_stat", "caic_exit", "caic_model_info", "caic_stripped_env":
			// Skip internal control records.

		default:
			parsed, parseErr := parseFn(line)
			if parseErr != nil {
				continue
			}
			msgs = append(msgs, parsed...)
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan %s: %w", path, err)
	}
	if !metaSet {
		return "", fmt.Errorf("%s: no caic_meta header", path)
	}

	return renderDiscussion(&meta, result, pr, msgs), nil
}

// renderDiscussion assembles the markdown document from parsed task data.
func renderDiscussion(meta *MetaMessage, result *MetaResultMessage, pr *MetaPRMessage, msgs []Message) string {
	var b strings.Builder

	b.WriteString("# Task Discussion\n\n")

	// Metadata
	b.WriteString("## Metadata\n\n")
	fmt.Fprintf(&b, "- **Prompt**: %s\n", meta.Prompt)
	if meta.Title != "" {
		fmt.Fprintf(&b, "- **Title**: %s\n", meta.Title)
	}
	fmt.Fprintf(&b, "- **Harness**: %s\n", meta.Harness)
	if meta.Model != "" {
		fmt.Fprintf(&b, "- **Model**: %s\n", meta.Model)
	}
	for _, r := range meta.Repos {
		branch := r.Branch
		if r.BaseBranch != "" {
			branch = r.BaseBranch + ".." + r.Branch
		}
		fmt.Fprintf(&b, "- **Repo**: %s (`%s`)\n", r.Name, branch)
	}
	fmt.Fprintf(&b, "- **Started**: %s\n", meta.StartedAt.Format("2006-01-02 15:04:05"))

	var flags []string
	if meta.Tailscale {
		flags = append(flags, "tailscale")
	}
	if meta.USB {
		flags = append(flags, "usb")
	}
	if meta.Display {
		flags = append(flags, "display")
	}
	if meta.Sudo {
		flags = append(flags, "sudo")
	}
	if len(flags) > 0 {
		fmt.Fprintf(&b, "- **Flags**: %s\n", strings.Join(flags, ", "))
	}

	if pr != nil && pr.ForgePR > 0 {
		fmt.Fprintf(&b, "- **PR**: %s/%s#%d\n", pr.ForgeOwner, pr.ForgeRepo, pr.ForgePR)
	}

	if result != nil {
		fmt.Fprintf(&b, "- **State**: %s\n", result.State)
		if result.CostUSD > 0 {
			fmt.Fprintf(&b, "- **Cost**: $%.4f\n", result.CostUSD)
		}
		if result.Duration > 0 {
			dur := result.Duration // seconds
			switch {
			case dur >= 3600:
				fmt.Fprintf(&b, "- **Duration**: %.1fh\n", dur/3600)
			case dur >= 60:
				fmt.Fprintf(&b, "- **Duration**: %.1fm\n", dur/60)
			default:
				fmt.Fprintf(&b, "- **Duration**: %.0fs\n", dur)
			}
		}
		if result.NumTurns > 0 {
			fmt.Fprintf(&b, "- **Turns**: %d\n", result.NumTurns)
		}
		if result.Error != "" {
			fmt.Fprintf(&b, "- **Error**: %s\n", result.Error)
		}
		if result.AgentResult != "" {
			fmt.Fprintf(&b, "- **Result**: %s\n", result.AgentResult)
		}
	}

	b.WriteString("\n---\n\n")

	// Conversation
	seenTools := make(map[string]bool) // Track tool IDs to skip duplicates.
	for _, msg := range msgs {
		if tu, ok := msg.(*ToolUseMessage); ok && !isInputEmpty(tu.Input) {
			if seenTools[tu.ToolUseID] {
				continue
			}
			seenTools[tu.ToolUseID] = true
		}
		renderMsg(&b, msg)
	}

	return b.String()
}

// renderMsg writes a single message to the markdown builder.
func renderMsg(b *strings.Builder, msg Message) {
	switch m := msg.(type) {
	case *UserInputMessage:
		if m.Text == "" && len(m.Images) == 0 {
			return
		}
		b.WriteString("## User\n\n")
		if m.Text != "" {
			b.WriteString(m.Text)
			b.WriteString("\n")
		}
		if len(m.Images) > 0 {
			fmt.Fprintf(b, "\n*(%d image(s) attached)*\n", len(m.Images))
		}
		b.WriteString("\n")

	case *TextMessage:
		text := strings.TrimSpace(m.Text)
		if text == "" {
			return
		}
		b.WriteString("## Assistant\n\n")
		b.WriteString(text)
		b.WriteString("\n\n")

	case *ThinkingMessage:
		text := strings.TrimSpace(m.Text)
		if text == "" {
			return
		}
		const maxThinking = 500
		truncated := text
		if len(text) > maxThinking {
			truncated = text[:maxThinking] + "..."
		}
		b.WriteString("<details><summary>💭 Thinking</summary>\n\n")
		b.WriteString(truncated)
		b.WriteString("\n\n</details>\n\n")

	case *ToolUseMessage:
		// Skip tool use messages whose input is empty or just {};
		// some harnesses (e.g. OpenCode) announce tools before the
		// real input arrives in a subsequent update.
		if isInputEmpty(m.Input) {
			return
		}
		fmt.Fprintf(b, "### 🔧 Tool: `%s`\n", m.Name)
		renderToolInput(b, m.Name, m.Input)
		b.WriteString("\n")

	case *ToolResultMessage:
		if m.Error != "" {
			fmt.Fprintf(b, "⚠️ Tool error: %s\n\n", m.Error)
		}

	case *SubagentStartMessage:
		fmt.Fprintf(b, "### 🤖 Subagent: %s\n\n", m.Description)

	case *SubagentEndMessage:
		fmt.Fprintf(b, "### Subagent %s\n\n", m.Status)

	case *SystemMessage:
		if m.Subtype == "compact_boundary" {
			b.WriteString("---\n*Context compaction boundary*\n\n---\n\n")
		}

	case *ResultMessage, *UsageMessage, *TextDeltaMessage, *ThinkingDeltaMessage,
		*ToolOutputDeltaMessage, *WidgetDeltaMessage, *WidgetMessage,
		*RateLimitMessage, *RawMessage, *ParseErrorMessage, *LogMessage,
		*DiffStatMessage, *ExitMessage, *AskMessage, *TodoMessage:
		// Skip: already captured in metadata, streaming deltas, or not
		// useful for the exported discussion.
	}
}

// renderToolInput writes a human-readable summary of tool arguments.
func renderToolInput(b *strings.Builder, name string, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var inp map[string]any
	if err := json.Unmarshal(raw, &inp); err != nil {
		return
	}

	switch name {
	case "Bash", "Shell", "Terminal":
		if cmd, ok := inp["command"].(string); ok && cmd != "" {
			fmt.Fprintf(b, "\n```bash\n%s\n```\n", cmd)
		} else if cmd, ok := inp["cmd"].(string); ok && cmd != "" {
			fmt.Fprintf(b, "\n```bash\n%s\n```\n", cmd)
		}

	case "Edit":
		if p := toolStr(inp, "path", "file_path", "filePath"); p != "" {
			fmt.Fprintf(b, "\n**File**: `%s`\n", p)
		}
		old := toolStr(inp, "old_text", "old_string", "oldString")
		newText := toolStr(inp, "new_text", "new_string", "newString")
		if old != "" || newText != "" {
			b.WriteString("\n```diff\n")
			if old != "" {
				fmt.Fprintf(b, "- %s\n", trunc(old, 200))
			}
			if newText != "" {
				fmt.Fprintf(b, "+ %s\n", trunc(newText, 200))
			}
			b.WriteString("```\n")
		}

	case "Write":
		if p := toolStr(inp, "path", "file_path", "filePath"); p != "" {
			fmt.Fprintf(b, "\n**File**: `%s`\n", p)
		}
		if c, ok := inp["content"].(string); ok && c != "" {
			fmt.Fprintf(b, "\n```\n%s\n```\n", trunc(c, 300))
		}

	case "Read":
		if p := toolStr(inp, "path", "file_path", "filePath"); p != "" {
			fmt.Fprintf(b, "\n**File**: `%s`\n", p)
		}

	case "Glob", "Grep", "Search":
		if p := toolStr(inp, "pattern", "query", "glob"); p != "" {
			fmt.Fprintf(b, "\n**Pattern**: `%s`\n", p)
		}

	case "WebFetch":
		if u, ok := inp["url"].(string); ok && u != "" {
			fmt.Fprintf(b, "\n**URL**: `%s`\n", u)
		}

	case "WebSearch":
		if q, ok := inp["query"].(string); ok && q != "" {
			fmt.Fprintf(b, "\n**Query**: `%s`\n", q)
		}

	default:
		summary := string(raw)
		if len(summary) > 300 {
			summary = summary[:300]
		}
		fmt.Fprintf(b, "\n```json\n%s\n```\n", summary)
	}
}

// isInputEmpty reports whether raw is nil, "null", or "{}".
func isInputEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	switch string(raw) {
	case "{}", "null", `""`:
		return true
	}
	return false
}

// toolStr returns the first non-empty value from the map under one of the
// given keys.
func toolStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// trunc returns s truncated to limit runes, appending "..." if needed.
func trunc(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "..."
}
