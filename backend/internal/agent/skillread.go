// Skill-read inference and confirmation recover analytics reads and confirmed loads from harness tools.

package agent

import (
	"path"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
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
// against cwd and a literal cd target; unresolved paths retain only the skill
// name.
//
// A read counts when its statement certainly ran and its status stays visible:
// a `;` list, a newline, and an `&&` chain all count. A pipeline masks the
// read's status behind its last stage, the right side of `||` may be skipped,
// and a background statement outlives the result, so those contribute nothing.
// A failing read inside the accepted forms still counts, because the shell
// reports no per-statement status and the confirming tool result gates the
// credit.
func SkillsFromCommand(cmd, cwd string) []SkillFile {
	if !strings.Contains(cmd, "SKILL.md") {
		return nil // most commands open no skill, and a parse cannot change that
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return nil
	}
	s := commandScanner{cwd: cwd, cwdKnown: true}
	s.statements(file.Stmts)
	return s.files
}

// commandScanner collects the listed skills of the statements that certainly
// ran, tracking the cwd a literal cd establishes.
type commandScanner struct {
	cwd      string
	cwdKnown bool
	files    []SkillFile
}

// statements visits statements that certainly run: semicolons and newlines.
func (s *commandScanner) statements(stmts []*syntax.Stmt) {
	for _, st := range stmts {
		s.statement(st)
	}
}

func (s *commandScanner) statement(st *syntax.Stmt) {
	if st.Background || st.Coprocess {
		return
	}
	switch cmd := st.Cmd.(type) {
	case *syntax.CallExpr:
		s.call(cmd)
	case *syntax.BinaryCmd:
		// `&&` proves both sides ran once the tool result succeeds. A pipeline
		// masks each side's status, and an `||` right side may be skipped.
		if cmd.Op == syntax.AndStmt {
			s.statement(cmd.X)
			s.statement(cmd.Y)
		}
	case *syntax.Subshell:
		s.statements(cmd.Stmts)
	case *syntax.Block:
		s.statements(cmd.Stmts)
	}
}

func (s *commandScanner) call(call *syntax.CallExpr) {
	if len(call.Args) == 0 {
		return
	}
	verb, ok := literal(call.Args[0])
	if !ok {
		return
	}
	if verb == "cd" || verb == "pushd" || verb == "popd" {
		s.changeDir(verb, call.Args[1:])
		return
	}
	if verb == "bash" || verb == "/bin/bash" {
		s.wrapper(call.Args[1:])
		return
	}
	switch verb {
	case "cat", "head", "tail", "less", "more", "bat", "nl", "sed":
		s.reads(verb, call.Args[1:])
	}
}

// wrapper follows a bash -lc script argument to the reads it runs.
func (s *commandScanner) wrapper(args []*syntax.Word) {
	if len(args) != 2 {
		return
	}
	flag, ok := literal(args[0])
	if !ok || !strings.HasPrefix(flag, "-") || !strings.Contains(flag, "c") {
		return
	}
	script, ok := literal(args[1])
	if !ok {
		return
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		return
	}
	s.statements(file.Stmts)
}

// changeDir resolves a literal cd target. Every other form leaves the cwd
// unknown, so a relative read after it keeps only its skill name.
func (s *commandScanner) changeDir(verb string, args []*syntax.Word) {
	s.cwdKnown = false
	if verb != "cd" || len(args) != 1 {
		return
	}
	target, ok := literal(args[0])
	if !ok || strings.HasPrefix(target, "~") {
		return
	}
	switch {
	case path.IsAbs(target):
		s.cwd = path.Clean(target)
	case s.cwdKnown && path.IsAbs(s.cwd):
		s.cwd = path.Join(s.cwd, target)
	default:
		return
	}
	s.cwdKnown = true
}

// reads appends the skills named by a read command's operands. A redirection
// target is never an operand here, so a write to a skill path stays uncounted.
func (s *commandScanner) reads(verb string, args []*syntax.Word) {
	scriptSeen := verb != "sed"
	for i := 0; i < len(args); i++ {
		word, dynamic := wordText(args[i])
		if strings.HasPrefix(word, "-") && !dynamic {
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
		// Resolve before matching so a cd into a skills root reveals a relative
		// name/SKILL.md operand. An unresolved or expanded operand names the
		// skill without a path.
		operand := word
		if s.cwdKnown && !dynamic && !strings.HasPrefix(word, "~") && !path.IsAbs(word) {
			if path.IsAbs(s.cwd) {
				operand = path.Join(s.cwd, word)
			}
		}
		f := SkillFromPath(operand)
		if f.Name == "" {
			continue
		}
		if dynamic || !path.IsAbs(f.Path) {
			f.Path = ""
		}
		s.files = append(s.files, f)
	}
}

// wordText returns a word's literal text and whether any part of it expands at
// run time. An expansion contributes no text, so a skill name after it stays
// visible while its path stays unknown.
func wordText(word *syntax.Word) (string, bool) {
	var b strings.Builder
	dynamic := false
	var walk func(parts []syntax.WordPart, quoted bool)
	walk = func(parts []syntax.WordPart, quoted bool) {
		for _, part := range parts {
			switch part := part.(type) {
			case *syntax.Lit:
				b.WriteString(unescape(part.Value, quoted))
			case *syntax.SglQuoted:
				b.WriteString(part.Value)
			case *syntax.DblQuoted:
				walk(part.Parts, true)
			default:
				dynamic = true
			}
		}
	}
	walk(word.Parts, false)
	return b.String(), dynamic
}

// unescape resolves a literal's backslash escapes. Inside double quotes the
// shell only escapes $, `, ", backslash, and a newline.
func unescape(s string, quoted bool) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			next := s[i+1]
			if !quoted || next == '$' || next == '`' || next == '"' || next == '\\' || next == '\n' {
				i++
				if next != '\n' {
					b.WriteByte(next)
				}
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// literal returns a word's text when nothing in it expands.
func literal(word *syntax.Word) (string, bool) {
	text, dynamic := wordText(word)
	return text, !dynamic
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
	pending map[ToolKey][]Message
}

// Confirm reports whether the input is already countable and returns the
// inferred reads and loads confirmed by a successful tool result, in arrival
// order. Ordinary messages allocate nothing.
func (t *SkillReadTracker) Confirm(m Message) (bool, []Message) {
	var source ToolKey
	switch m := m.(type) {
	case *SkillReadMessage:
		if !m.Inferred {
			return true, nil
		}
		if m.SourceToolUseID == "" {
			return false, nil
		}
		source = ToolKeyForSource(m.Provenance(), m.SourceToolUseID)
	case *SkillLoadMessage:
		if m.SourceToolUseID == "" {
			return true, nil
		}
		source = ToolKeyForSource(m.Provenance(), m.SourceToolUseID)
	case *ToolResultMessage:
		key := ToolKeyForSource(m.Provenance(), m.ToolUseID)
		confirmed := t.pending[key]
		delete(t.pending, key)
		if m.Error != "" {
			return true, nil
		}
		return true, confirmed
	case *ResultMessage:
		scope := ToolKeyForSource(m.Provenance(), "")
		for pending := range t.pending {
			key := pending
			key.ToolUseID = ""
			if key == scope {
				delete(t.pending, pending)
			}
		}
		return true, nil
	default:
		return true, nil
	}
	if t.pending == nil {
		t.pending = make(map[ToolKey][]Message)
	}
	t.pending[source] = append(t.pending[source], m)
	return false, nil
}
