// Tests that check-agent-logs flags Claude background tasks spawned by unknown or unresolved tools.

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"
)

func TestClaudeBackgroundTools(t *testing.T) {
	t.Parallel()
	toolUse := func(session, name string) string {
		return `{"t":"agent","ts":1.000,"msg":{"type":"assistant","session_id":"` + session + `","uuid":"u1","parent_tool_use_id":null,"message":{"id":"msg","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_1","name":"` + name + `","input":{"command":"sleep 9"}}],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}}`
	}
	started := func(session string, backgrounded bool) string {
		bg := "false"
		if backgrounded {
			bg = "true"
		}
		return `{"t":"agent","ts":2.000,"msg":{"type":"system","subtype":"task_started","task_id":"b1","tool_use_id":"toolu_1","description":"sleep 9","is_backgrounded":` + bg + `,"task_type":"local_bash","uuid":"u2","session_id":"` + session + `"}}`
	}
	check := func(t *testing.T, records ...string) []finding {
		path := writeLog(t, filepath.Join(t.TempDir(), "task.jsonl"), append([]string{taskMeta("claude")}, records...), false)
		result, err := checkFile(path, time.Time{}, backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		return result.findings
	}
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			records []string
		}{
			{"Bash", []string{toolUse("s", "Bash"), started("s", true)}},
			{"Monitor", []string{toolUse("s", "Monitor"), started("s", true)}},
			{"foreground unknown tool", []string{toolUse("s", "Shell"), started("s", false)}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if f := check(t, tc.records...); len(f) != 0 {
					t.Fatalf("findings = %v", f)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			records []string
			want    string
		}{
			{"renamed tool", []string{toolUse("s", "Shell"), started("s", true)}, `unknown tool "Shell"`},
			{"no tool use", []string{started("s", true)}, "has no tool_use"},
			{"tool use in another session", []string{toolUse("other", "Bash"), started("s", true)}, "has no tool_use"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := check(t, tc.records...)
				if len(f) != 1 || f[0].line != len(tc.records)+1 || !strings.Contains(f[0].err.Error(), tc.want) || !strings.HasSuffix(f[0].hint, "claudecode/native_subagent.go") {
					t.Fatalf("findings = %v, want %q", f, tc.want)
				}
			})
		}
	})
}
