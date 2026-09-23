// Skill-read inference and confirmation recover successful skill file loads from harness tools.

package agent

import (
	"regexp"
	"strings"
)

const (
	// skillRootDir matches a directory a harness scans for installed skills.
	// OpenCode reads ~/.claude and ~/.agents like Claude Code, and adds its
	// own config directories: ~/.config/opencode, ~/.opencode, and any
	// .opencode between the working directory and the worktree root.
	skillRootDir = `(?:\.(?:agents|claude|codex|opencode|pi)|\.config/opencode)`
	// pathChar excludes the whitespace and quoting that bound a path inside a
	// shell command or a JSON tool input.
	pathChar = "[^\\s\"'`]"
	// nameChar additionally excludes the separator, so a capture is one
	// directory name.
	nameChar = "[^\\s\"'`/]"
)

// installedSkillPath matches one SKILL.md under an installed skill root and
// captures the directory holding it.
//
// A skill nests: OpenCode globs {skill,skills}/**/SKILL.md and the external
// roots use skills/**/SKILL.md, so ~/.agents/skills/synced/<id>/SKILL.md is
// one skill. The directory holding SKILL.md names it here. A harness reads the
// real name from the file's frontmatter, which a path cannot see.
//
// The root anchor keeps repository sources out. caic ships
// internal/server/skills/tasks/SKILL.md and the widget plugin's own skill, and
// a bare skills/ path also appears in a URL and in a git revision argument.
var installedSkillPath = regexp.MustCompile(
	`(?:^|[^\w.@-])` + skillRootDir + `/(?:` + pathChar + `*/)?skills?/(?:` + pathChar + `*/)?(` + nameChar + `+)/SKILL\.md(?:[^A-Za-z0-9.]|$)`)

// SkillFromPath returns the skill a file path belongs to. It returns an empty
// string when the path names no installed skill.
func SkillFromPath(p string) string {
	if p == "" {
		return ""
	}
	m := installedSkillPath.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	if strings.ContainsAny(m[0], "*?[]{}$") {
		return ""
	}
	return m[1]
}

// SkillsFromCommand returns the skills a shell command opens, in the order
// they appear. One command can open several: Codex bundles its reads into a
// single bash invocation.
//
// The caller must pass a command, not a whole tool input. Only simple
// file-reading commands are considered; shell listings, searches, and globs
// are not evidence that the skill file was opened.
func SkillsFromCommand(cmd string) []string {
	if cmd == "" {
		return nil
	}
	cmd = strings.TrimSpace(cmd)
	for _, prefix := range []string{"/bin/bash -lc ", "bash -lc "} {
		if rest, ok := strings.CutPrefix(cmd, prefix); ok && len(rest) >= 2 && (rest[0] == '\'' || rest[0] == '"') && rest[len(rest)-1] == rest[0] {
			cmd = rest[1 : len(rest)-1]
			break
		}
	}
	var names []string
	for _, segment := range shellReadSegments(cmd) {
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}
		verb := strings.TrimLeft(fields[0], "\"'(")
		switch verb {
		case "cat", "head", "tail", "less", "more", "bat", "nl", "sed":
		default:
			continue
		}
		scriptSeen := verb != "sed"
		for i := 1; i < len(fields); i++ {
			word := fields[i]
			if strings.Contains(word, ">") {
				break // output redirection names a destination, not a read
			}
			if strings.HasPrefix(word, "-") {
				if verb == "sed" && (word == "-e" || word == "-f") {
					i++
					scriptSeen = true
				}
				continue
			}
			if !scriptSeen {
				scriptSeen = true // sed's first positional operand is its script
				continue
			}
			if name := SkillFromPath(word); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// shellReadSegments keeps only commands whose successful overall exit proves
// every read ran and succeeded. A semicolon, pipeline, background command, or
// OR branch makes that inference ambiguous; an AND chain preserves it.
func shellReadSegments(cmd string) []string {
	var segments []string
	start := 0
	quote := byte(0)
	escaped := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '#' && (i == start || strings.ContainsRune(" \t;&|\n", rune(cmd[i-1]))) {
			segments = append(segments, cmd[start:i])
			for i < len(cmd) && cmd[i] != '\n' {
				i++
			}
			if i < len(cmd) && strings.TrimSpace(cmd[i+1:]) != "" {
				return nil
			}
			return segments
		}
		if c == ';' || c == '|' || c == '&' && (i+1 == len(cmd) || cmd[i+1] != '&') {
			return nil
		}
		if c == '\n' {
			if strings.TrimSpace(cmd[i+1:]) != "" {
				return nil
			}
			segments = append(segments, cmd[start:i])
			return segments
		}
		if c == '&' {
			segments = append(segments, cmd[start:i])
			i++
			start = i + 1
		}
	}
	if start < len(cmd) {
		segments = append(segments, cmd[start:])
	}
	return segments
}

// InferredSkillReadFromPath returns the read message for a path a harness
// opened, or nil when the path names no installed skill.
//
// Only Claude Code reports a skill load directly, through its Skill tool. The
// other harnesses open the file, so the path is the only evidence they give.
func InferredSkillReadFromPath(p, sourceID string) []Message {
	name := SkillFromPath(p)
	if name == "" {
		return nil
	}
	return []Message{&SkillReadMessage{Skill: name, Inferred: true, SourceToolUseID: sourceID}}
}

// InferredSkillReadsFromCommand returns one read message per skill a shell
// command opens.
func InferredSkillReadsFromCommand(cmd, sourceID string) []Message {
	names := SkillsFromCommand(cmd)
	if len(names) == 0 {
		return nil
	}
	msgs := make([]Message, 0, len(names))
	for _, name := range names {
		msgs = append(msgs, &SkillReadMessage{Skill: name, Inferred: true, SourceToolUseID: sourceID})
	}
	return msgs
}

// SkillReadTracker releases inferred reads only after the opening tool
// succeeds. A turn boundary discards reads whose result never arrived.
type SkillReadTracker struct {
	pending map[string][]*SkillReadMessage
}

// Confirm reports whether the input is already countable and returns reads
// confirmed by a successful tool result. Ordinary messages allocate nothing.
func (t *SkillReadTracker) Confirm(m Message) (bool, []*SkillReadMessage) {
	switch m := m.(type) {
	case *SkillReadMessage:
		if !m.Inferred {
			return true, nil
		}
		if m.SourceToolUseID == "" {
			return false, nil
		}
		if t.pending == nil {
			t.pending = make(map[string][]*SkillReadMessage)
		}
		t.pending[m.SourceToolUseID] = append(t.pending[m.SourceToolUseID], m)
		return false, nil
	case *ToolResultMessage:
		reads := t.pending[m.ToolUseID]
		delete(t.pending, m.ToolUseID)
		if m.Error != "" {
			return true, nil
		}
		return true, reads
	case *ResultMessage:
		clear(t.pending)
	}
	return true, nil
}
