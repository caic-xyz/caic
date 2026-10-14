// Skill-read inference and confirmation recover analytics reads and confirmed loads from harness tools.

package agent

import (
	"path"
	"regexp"
	"strings"
)

const (
	// skillRootDir matches a directory a harness scans for installed skills.
	// OpenCode reads ~/.claude and ~/.agents like Claude Code, and adds its
	// own config directories: ~/.config/opencode, ~/.opencode, and any
	// .opencode between the working directory and the worktree root.
	skillRootDir = `(?:\.(?:agents|claude|codex|opencode|pi)|\.config/opencode)`
	// pathChar accepts literal file paths, including spaces. Shell operands
	// are unquoted before matching.
	pathChar = "[^\"'`]"
	// nameChar additionally excludes the separator, so a capture is one
	// directory name.
	nameChar = "[^\"'`/]"
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
	`(?:^|[^\w.@-])` + skillRootDir + `/(?:` + pathChar + `*/)?skills?/(?:` + pathChar + `*/)?(` + nameChar + `+)/SKILL\.md$`)

// SkillFile identifies an installed skill and the SKILL.md path that loads it.
type SkillFile struct {
	// Name is the directory holding SKILL.md.
	Name string
	// Path is the literal file-tool path or a resolved shell operand. Shell
	// inference omits it when expansion makes it unknown, or when a relative
	// operand lacks an absolute cwd or follows a directory-changing command.
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
	return SkillFile{Name: p[loc[2]:loc[3]], Path: p}
}

// inferred appends the read and the pending load that opening f implies.
func (f *SkillFile) inferred(msgs []Message, sourceID string) []Message {
	msgs = append(msgs, &SkillReadMessage{Skill: f.Name, Inferred: true, SourceToolUseID: sourceID})
	if sourceID != "" {
		msgs = append(msgs, &SkillLoadMessage{Skill: f.Name, Path: f.Path, SourceToolUseID: sourceID})
	}
	return msgs
}

// SkillsFromCommand returns the skills a shell command opens, in the order
// they appear. One command can open several: Codex bundles its reads into a
// single bash invocation.
//
// The caller must pass a command, not a whole tool input. Only simple
// file-reading commands are considered; shell listings, searches, and globs
// are not evidence that the skill file was opened. Relative operands resolve
// against cwd; unresolved paths retain only the skill name.
func SkillsFromCommand(cmd, cwd string) []SkillFile {
	if cmd == "" {
		return nil
	}
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, "/bin/bash -lc ") || strings.HasPrefix(cmd, "bash -lc ") {
		fields := shellReadWords(cmd)
		if len(fields) == 3 {
			return SkillsFromCommand(fields[2], cwd)
		}
		return nil
	}

	var files []SkillFile
	cwdKnown := true
	for _, segment := range shellReadSegments(cmd) {
		fields := shellReadWords(segment)
		// A substitution may span several words. None can supply a reliable
		// path without the shell environment and the substitution result.
		dynamic := strings.ContainsAny(segment, "$`")
		if len(fields) == 0 {
			continue
		}
		verb := fields[0]
		switch verb {
		case "cd", "pushd", "popd":
			cwdKnown = false
			continue
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
				if dynamic || strings.HasPrefix(word, "~") || strings.ContainsAny(word, "*?[]{}") {
					f.Path = ""
				} else if !path.IsAbs(f.Path) {
					if cwdKnown && path.IsAbs(cwd) {
						f.Path = path.Join(cwd, f.Path)
					} else {
						f.Path = ""
					}
				}
				files = append(files, f)
			}
		}
	}
	return files
}

// shellReadWords splits simple shell operands, removes their quoting, and
// skips unquoted subshell parentheses.
// Dynamic expansion is left intact; it cannot supply a reliable load path.
func shellReadWords(s string) []string {
	var words []string
	for i := 0; i < len(s); {
		if strings.ContainsRune("() \t\r\n", rune(s[i])) {
			i++
			continue
		}
		start := i
		var word strings.Builder
		quote := byte(0)
		for i < len(s) {
			c := s[i]
			if quote == 0 && strings.ContainsRune("() \t\r\n", rune(c)) {
				break
			}
			if c == '\\' && quote != '\'' && i+1 < len(s) &&
				(quote != '"' || strings.ContainsRune("$`\"\\\n", rune(s[i+1]))) {
				word.WriteString(s[start:i])
				i++
				if s[i] != '\n' {
					word.WriteByte(s[i])
				}
				i++
				start = i
				continue
			}
			if c == '\'' || c == '"' {
				if quote == 0 || quote == c {
					word.WriteString(s[start:i])
					if quote == 0 {
						quote = c
					} else {
						quote = 0
					}
					i++
					start = i
					continue
				}
			}
			i++
		}
		if quote != 0 {
			return nil
		}
		if word.Len() == 0 {
			words = append(words, s[start:i])
		} else {
			word.WriteString(s[start:i])
			words = append(words, word.String())
		}
	}
	return words
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
// command opens, as InferredSkillReadFromPath does for one path. Relative
// operands resolve against cwd; unresolved paths retain only the skill name.
func InferredSkillReadsFromCommand(cmd, cwd, sourceID string) []Message {
	files := SkillsFromCommand(cmd, cwd)
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
