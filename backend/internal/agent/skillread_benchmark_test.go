// Benchmarks skill-read path inference and success confirmation in the usage fold.

package agent_test

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func BenchmarkSkillsFromCommand(b *testing.B) {
	for _, tc := range []struct {
		name string
		cmd  string
		want int
	}{
		{name: "no skill", cmd: "make verify && go test ./... && git status --short"},
		{
			name: "two reads",
			cmd:  `sed -n '1,240p' /home/user/.agents/skills/code-quality/SKILL.md && sed -n '1,240p' /home/user/.agents/skills/go-code-quality/SKILL.md`,
			want: 2,
		},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if len(agent.SkillsFromCommand(tc.cmd, "/repo")) != tc.want {
					b.Fatal("wrong skill reads")
				}
			}
		})
	}
}

func BenchmarkSkillReadTrackerConfirm(b *testing.B) {
	var tracker agent.SkillReadTracker
	read := &agent.SkillReadMessage{Skill: "review", Inferred: true, SourceToolUseID: "call-1"}
	result := &agent.ToolResultMessage{ToolUseID: "call-1"}
	b.ReportAllocs()
	for range b.N {
		tracker.Confirm(read)
		if count, confirmed := tracker.Confirm(result); !count || len(confirmed) != 1 {
			b.Fatal("lost confirmed read")
		}
	}
}
