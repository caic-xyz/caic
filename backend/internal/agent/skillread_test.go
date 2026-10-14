// Tests skill-name inference from the paths and commands each harness records.

package agent_test

import (
	"slices"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestSkillFromPath(t *testing.T) {
	t.Parallel()

	// Every path below is taken from the retained task-log cache, except the
	// OpenCode config roots, which come from the upstream skill discovery.
	t.Run("installed", func(t *testing.T) {
		t.Parallel()
		data := []struct {
			name string
			in   string
			want string
		}{
			{name: "spaced absolute path", in: "/home/user/My Skills/.agents/skills/code-quality/SKILL.md", want: "code-quality"},
			{name: "agents root", in: "/home/user/.agents/skills/code-quality/SKILL.md", want: "code-quality"},
			{name: "tilde", in: "~/.agents/skills/go-code-quality/SKILL.md", want: "go-code-quality"},
			{name: "claude root", in: "/home/user/.claude/skills/review/SKILL.md", want: "review"},
			{name: "codex plugin cache", in: "/home/user/.codex/plugins/cache/openai-curated-remote/data-analytics/0.2.35-13ceeea1f599/skills/index/SKILL.md", want: "index"},
			{name: "pi extension", in: "/home/user/.pi/agent/npm/node_modules/pi-subagents/skills/pi-subagents/SKILL.md", want: "pi-subagents"},
			{name: "opencode home config", in: "/home/user/.config/opencode/skills/customize-opencode/SKILL.md", want: "customize-opencode"},
			{name: "opencode singular skill dir", in: "/home/user/.config/opencode/skill/customize-opencode/SKILL.md", want: "customize-opencode"},
			{name: "opencode home dir", in: "/home/user/.opencode/skill/deploy/SKILL.md", want: "deploy"},
			{name: "opencode project dir", in: "/home/user/src/caic/.opencode/skills/deploy/SKILL.md", want: "deploy"},
			{name: "project claude dir", in: "/home/user/src/caic/.claude/skills/widget/SKILL.md", want: "widget"},
			// OpenCode globs {skill,skills}/**/SKILL.md, so a skill nests.
			{name: "nested skill", in: "/home/user/.agents/skills/synced/86ba362e-f617_bef8887a/SKILL.md", want: "86ba362e-f617_bef8887a"},
		}
		for _, line := range data {
			t.Run(line.name, func(t *testing.T) {
				t.Parallel()
				want := agent.SkillFile{Name: line.want, Path: line.in}
				if got := agent.SkillFromPath(line.in); got != want {
					t.Errorf("SkillFromPath(%q) = %+v, want %+v", line.in, got, want)
				}
			})
		}
	})

	t.Run("not installed", func(t *testing.T) {
		t.Parallel()
		data := []struct {
			name string
			in   string
		}{
			{name: "empty", in: ""},
			{name: "caic ships its own skill as source", in: "backend/internal/server/skills/tasks/SKILL.md"},
			{name: "widget plugin skill is repository source", in: "backend/internal/agent/claudecode/widget-plugin/skills/widget/SKILL.md"},
			{name: "relative path in the skills checkout", in: "skills/code-quality/SKILL.md"},
			{name: "a URL names no local skill", in: "https://github.com/trailofbits/skills/blob/main/plugins/ask/SKILL.md"},
			{name: "a sibling file is not the skill body", in: "/home/user/.agents/skills/go-code-quality/scripts/commentcheck.py"},
			{name: "a longer name is a different file", in: "/home/user/.agents/skills/review/SKILL.md.bak"},
			{name: "an unrelated dot directory", in: "/home/user/.cache/skills/review/SKILL.md"},
			{name: "glob is not a concrete skill", in: "~/.agents/skills/*/SKILL.md"},
		}
		for _, line := range data {
			t.Run(line.name, func(t *testing.T) {
				t.Parallel()
				if got := agent.SkillFromPath(line.in); got != (agent.SkillFile{}) {
					t.Errorf("SkillFromPath(%q) = %+v, want zero", line.in, got)
				}
			})
		}
	})
}

func TestSkillsFromCommand(t *testing.T) {
	t.Parallel()
	codeQuality := agent.SkillFile{Name: "code-quality", Path: ""}
	review := agent.SkillFile{Name: "review", Path: ""}

	data := []struct {
		name string
		in   string
		want []agent.SkillFile
	}{
		{
			name: "codex bundles reads in one command",
			in:   `/bin/bash -lc "sed -n '1,240p' /home/user/.agents/skills/code-quality/SKILL.md && sed -n '1,220p' /home/user/.agents/skills/go-code-quality/SKILL.md"`,
			want: []agent.SkillFile{
				{Name: "code-quality", Path: "/home/user/.agents/skills/code-quality/SKILL.md"},
				{Name: "go-code-quality", Path: "/home/user/.agents/skills/go-code-quality/SKILL.md"},
			},
		},
		{name: "escaped spaced absolute path", in: `cat /home/user/My\ Skills/.agents/skills/review/SKILL.md`, want: []agent.SkillFile{{Name: "review", Path: "/home/user/My Skills/.agents/skills/review/SKILL.md"}}},
		{name: "quoted spaced absolute path", in: `cat "/home/user/My Skills/.agents/skills/review/SKILL.md"`, want: []agent.SkillFile{{Name: "review", Path: "/home/user/My Skills/.agents/skills/review/SKILL.md"}}},
		{
			name: "repeated reads count raw",
			in:   "cat ~/.agents/skills/code-quality/SKILL.md && cat ~/.agents/skills/code-quality/SKILL.md",
			want: []agent.SkillFile{codeQuality, codeQuality},
		},
		{
			name: "a read beside an unrelated search",
			in:   `sed -n '1,240p' /home/user/.agents/skills/code-quality/SKILL.md && rg -n -i "voice gateway" .`,
			want: []agent.SkillFile{{Name: "code-quality", Path: "/home/user/.agents/skills/code-quality/SKILL.md"}},
		},
		{
			name: "git names a revision, not a working file",
			in:   "git -C /home/user/.agents show HEAD:skills/go-code-quality/SKILL.md",
			want: nil,
		},
		{
			name: "no skill",
			in:   "make verify",
			want: nil,
		},
		{name: "listing skill files", in: "ls ~/.agents/skills/*/SKILL.md", want: nil},
		{name: "grep pattern names a path", in: "rg '~/.agents/skills/review/SKILL.md' .", want: nil},
		{name: "output redirection writes a skill", in: "cat /tmp/notes > ~/.agents/skills/review/SKILL.md", want: nil},
		{name: "shell comment mentions a skill", in: "cat /tmp/notes # ~/.agents/skills/review/SKILL.md", want: nil},
		{name: "shell comment covers later operators", in: "cat /tmp/notes # ignored && cat ~/.agents/skills/review/SKILL.md", want: nil},
		{name: "OR branch may be skipped", in: "true || cat ~/.agents/skills/review/SKILL.md", want: nil},
		{name: "semicolon hides read failure", in: "cat ~/.agents/skills/review/SKILL.md; true", want: nil},
		{name: "pipeline hides read failure", in: "cat ~/.agents/skills/review/SKILL.md | head", want: nil},
		{name: "quoted hash is not a comment", in: "cat '/tmp/#notes' && cat ~/.agents/skills/review/SKILL.md", want: []agent.SkillFile{review}},
		{name: "quoted path", in: `cat "~/.agents/skills/review/SKILL.md"`, want: []agent.SkillFile{review}},
		{name: "sed script mentions a skill", in: "sed -n '/~/.agents/skills/review/SKILL.md/p' /tmp/notes", want: nil},
		{name: "read before output redirection", in: "cat ~/.agents/skills/review/SKILL.md > /tmp/notes", want: []agent.SkillFile{review}},
		{name: "subshell home-relative", in: "(cat ~/.agents/skills/review/SKILL.md)", want: []agent.SkillFile{review}},
		{name: "quoted parenthesis in path", in: `cat "/home/notes)/.agents/skills/review/SKILL.md"`, want: []agent.SkillFile{{Name: "review", Path: "/home/notes)/.agents/skills/review/SKILL.md"}}},
		{name: "subshell absolute", in: "(cat /home/u/.agents/skills/review/SKILL.md)", want: []agent.SkillFile{{Name: "review", Path: "/home/u/.agents/skills/review/SKILL.md"}}},
		{name: "subshell cd", in: "(cd /other && cat .agents/skills/review/SKILL.md)", want: []agent.SkillFile{{Name: "review"}}},
		{name: "escaped wrapper", in: `bash -lc "cat \"/a b/.agents/skills/review/SKILL.md\""`, want: []agent.SkillFile{{Name: "review", Path: "/a b/.agents/skills/review/SKILL.md"}}},
		{name: "split substitution", in: "cat $(dirname /repo/file)/.agents/skills/review/SKILL.md", want: []agent.SkillFile{{Name: "review"}}},
		{name: "empty", in: "", want: nil},
	}
	for _, line := range data {
		t.Run(line.name, func(t *testing.T) {
			t.Parallel()
			got := agent.SkillsFromCommand(line.in, "")
			if !slices.Equal(got, line.want) {
				t.Errorf("SkillsFromCommand(%q) = %v, want %v", line.in, got, line.want)
			}
		})
	}
}

func TestSkillReadTracker(t *testing.T) {
	t.Parallel()
	var tracker agent.SkillReadTracker
	read := &agent.SkillReadMessage{Skill: "review", Inferred: true, SourceToolUseID: "call-1"}
	if count, got := tracker.Confirm(read); count || len(got) != 0 {
		t.Fatalf("unconfirmed read = %v, %v", count, got)
	}
	if count, got := tracker.Confirm(&agent.ToolResultMessage{ToolUseID: "call-1", Error: "missing file"}); !count || len(got) != 0 {
		t.Errorf("failed read = %v, %v, want result only", count, got)
	}
	msgs := agent.InferredSkillReadFromPath("/home/user/.agents/skills/review/SKILL.md", "call-1")
	for _, m := range msgs {
		if count, got := tracker.Confirm(m); count || len(got) != 0 {
			t.Fatalf("unconfirmed %T = %v, %v", m, count, got)
		}
	}
	count, got := tracker.Confirm(&agent.ToolResultMessage{ToolUseID: "call-1"})
	if !count || !slices.Equal(got, msgs) {
		t.Errorf("successful read = %v, %v, want result, read, and load", count, got)
	}
	if load, ok := got[1].(*agent.SkillLoadMessage); !ok || load.Path != "/home/user/.agents/skills/review/SKILL.md" {
		t.Errorf("load = %#v, want the opened path", got[1])
	}
	if count, got := tracker.Confirm(&agent.SkillLoadMessage{Skill: "review"}); !count || len(got) != 0 {
		t.Errorf("reported load = %v, %v, want countable", count, got)
	}
	if msgs := agent.InferredSkillReadFromPath("/home/user/.agents/skills/review/SKILL.md", ""); len(msgs) != 1 {
		t.Errorf("sourceless inference = %#v, want only the read", msgs)
	}
	tracker.Confirm(read)
	tracker.Confirm(&agent.ResultMessage{})
	if count, got := tracker.Confirm(&agent.ToolResultMessage{ToolUseID: "call-1"}); !count || len(got) != 0 {
		t.Errorf("turn boundary retained %v, %v, want only result", count, got)
	}
}

func TestInferredSkillReadsFromCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, cmd, cwd, path string }{
		{name: "relative with cwd", cmd: "cat .agents/skills/review/SKILL.md", cwd: "/repo", path: "/repo/.agents/skills/review/SKILL.md"},
		{name: "relative without cwd", cmd: "cat .agents/skills/review/SKILL.md"},
		{name: "dynamic prefix", cmd: "cat $ROOT/.agents/skills/review/SKILL.md", cwd: "/repo"},
		{name: "cd dependent", cmd: "cd /other && cat .agents/skills/review/SKILL.md", cwd: "/repo"},
		{name: "absolute after cd", cmd: "cd /other && cat /home/user/.agents/skills/review/SKILL.md", cwd: "/repo", path: "/home/user/.agents/skills/review/SKILL.md"},
		{name: "home expansion unavailable", cmd: "cat ~/.agents/skills/review/SKILL.md", cwd: "/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msgs := agent.InferredSkillReadsFromCommand(tc.cmd, tc.cwd, "call-1")
			if len(msgs) != 2 {
				t.Fatalf("messages = %v, want read and load", msgs)
			}
			if read, ok := msgs[0].(*agent.SkillReadMessage); !ok || read.Skill != "review" {
				t.Errorf("read = %#v", msgs[0])
			}
			if load, ok := msgs[1].(*agent.SkillLoadMessage); !ok || load.Skill != "review" || load.Path != tc.path {
				t.Errorf("load = %#v, want review with path %q", msgs[1], tc.path)
			}
		})
	}
}
