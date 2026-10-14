// Skill-read inference and confirmation recover analytics reads and confirmed loads from harness tools.

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

// SkillFile identifies an installed skill and the SKILL.md path that loads it.
type SkillFile struct {
	// Name is the directory holding SKILL.md.
	Name string
	// Path is the SKILL.md path as the harness wrote it: absolute,
	// home-relative, or relative to the harness working directory.
	Path string
}

// SkillFromPath returns the skill a file path belongs to. It returns the zero
// value when the path names no installed skill.
func SkillFromPath(p string) SkillFile {
	if p == "" {
		return SkillFile{}
	}
	loc := installedSkillPath.FindStringSubmatchIndex(p)
	if loc == nil {
		return SkillFile{}
	}
	if strings.ContainsAny(p[loc[0]:loc[1]], "*?[]{}$") {
		return SkillFile{}
	}
	// The match starts at the boundary before the skill root; the path
	// extends back to the first byte that cannot appear in one.
	start := loc[0]
	if !isPathChar(p[start]) {
		start++
	}
	for start > 0 && isPathChar(p[start-1]) {
		start--
	}
	return SkillFile{Name: p[loc[2]:loc[3]], Path: p[start : loc[3]+len("/SKILL.md")]}
}

// inferred appends the read and the pending load that opening f implies.
func (f *SkillFile) inferred(msgs []Message, sourceID string) []Message {
	msgs = append(msgs, &SkillReadMessage{Skill: f.Name, Inferred: true, SourceToolUseID: sourceID})
	if sourceID != "" {
		msgs = append(msgs, &SkillLoadMessage{Skill: f.Name, Path: f.Path, SourceToolUseID: sourceID})
	}
	return msgs
}

// isPathChar reports whether c can appear in a path; it mirrors pathChar.
func isPathChar(c byte) bool {
	return !strings.ContainsRune(" \t\n\f\r\"'`", rune(c))
}

// SkillsFromCommand returns the skills a shell command opens, in the order
// they appear. One command can open several: Codex bundles its reads into a
// single bash invocation.
//
// The caller must pass a command, not a whole tool input. Only simple
// file-reading commands are considered; shell listings, searches, and globs
// are not evidence that the skill file was opened.
func SkillsFromCommand(cmd string) []SkillFile {
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
	var files []SkillFile
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
			if f := SkillFromPath(word); f.Name != "" {
				files = append(files, f)
			}
		}
	}
	return files
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

// InferredSkillReadFromPath returns the messages for a path a harness opened,
// or nil when the path names no installed skill: an analytics read and, when
// sourceID can confirm it, a load carrying the opened SKILL.md.
//
// Only Claude Code reports a skill load directly, through its Skill tool. The
// other harnesses open the file, so the path is the only evidence they give.
func InferredSkillReadFromPath(p, sourceID string) []Message {
	f := SkillFromPath(p)
	if f.Name == "" {
		return nil
	}
	return f.inferred(nil, sourceID)
}

// InferredSkillReadsFromCommand returns the messages for each skill a shell
// command opens, as InferredSkillReadFromPath does for one path.
func InferredSkillReadsFromCommand(cmd, sourceID string) []Message {
	files := SkillsFromCommand(cmd)
	if len(files) == 0 {
		return nil
	}
	msgs := make([]Message, 0, 2*len(files))
	for _, f := range files {
		msgs = f.inferred(msgs, sourceID)
	}
	return msgs
}

// SkillReadTracker releases inferred reads and loads only after the opening
// tool succeeds. A turn boundary discards those whose result never arrived.
type SkillReadTracker struct {
	pending map[string][]Message
}

// Confirm reports whether the input is already countable and returns the
// inferred reads and loads confirmed by a successful tool result, in arrival
// order. Ordinary messages allocate nothing.
func (t *SkillReadTracker) Confirm(m Message) (bool, []Message) {
	var source string
	switch m := m.(type) {
	case *SkillReadMessage:
		if !m.Inferred {
			return true, nil
		}
		if m.SourceToolUseID == "" {
			return false, nil
		}
		source = m.SourceToolUseID
	case *SkillLoadMessage:
		if m.SourceToolUseID == "" {
			return true, nil
		}
		source = m.SourceToolUseID
	case *ToolResultMessage:
		confirmed := t.pending[m.ToolUseID]
		delete(t.pending, m.ToolUseID)
		if m.Error != "" {
			return true, nil
		}
		return true, confirmed
	case *ResultMessage:
		clear(t.pending)
		return true, nil
	default:
		return true, nil
	}
	if t.pending == nil {
		t.pending = make(map[string][]Message)
	}
	t.pending[source] = append(t.pending[source], m)
	return false, nil
}
