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
				if got := agent.SkillFromPath(line.in); got != line.want {
					t.Errorf("SkillFromPath(%q) = %q, want %q", line.in, got, line.want)
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
				if got := agent.SkillFromPath(line.in); got != "" {
					t.Errorf("SkillFromPath(%q) = %q, want empty", line.in, got)
				}
			})
		}
	})
}

func TestSkillsFromCommand(t *testing.T) {
	t.Parallel()

	data := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "codex bundles reads in one command",
			in:   `/bin/bash -lc "sed -n '1,240p' /home/user/.agents/skills/code-quality/SKILL.md && sed -n '1,220p' /home/user/.agents/skills/go-code-quality/SKILL.md"`,
			want: []string{"code-quality", "go-code-quality"},
		},
		{
			name: "repeated reads count raw",
			in:   "cat ~/.agents/skills/code-quality/SKILL.md && cat ~/.agents/skills/code-quality/SKILL.md",
			want: []string{"code-quality", "code-quality"},
		},
		{
			name: "a read beside an unrelated search",
			in:   `sed -n '1,240p' /home/user/.agents/skills/code-quality/SKILL.md && rg -n -i "voice gateway" .`,
			want: []string{"code-quality"},
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
		{name: "quoted hash is not a comment", in: "cat '/tmp/#notes' && cat ~/.agents/skills/review/SKILL.md", want: []string{"review"}},
		{name: "sed script mentions a skill", in: "sed -n '/~/.agents/skills/review/SKILL.md/p' /tmp/notes", want: nil},
		{name: "read before output redirection", in: "cat ~/.agents/skills/review/SKILL.md > /tmp/notes", want: []string{"review"}},
		{name: "empty", in: "", want: nil},
	}
	for _, line := range data {
		t.Run(line.name, func(t *testing.T) {
			t.Parallel()
			got := agent.SkillsFromCommand(line.in)
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
	tracker.Confirm(read)
	count, got := tracker.Confirm(&agent.ToolResultMessage{ToolUseID: "call-1"})
	if !count || len(got) != 1 || got[0] != read {
		t.Errorf("successful read = %v, %v, want result and read", count, got)
	}
	tracker.Confirm(read)
	tracker.Confirm(&agent.ResultMessage{})
	if count, got := tracker.Confirm(&agent.ToolResultMessage{ToolUseID: "call-1"}); !count || len(got) != 0 {
		t.Errorf("turn boundary retained %v, %v, want only result", count, got)
	}
}
