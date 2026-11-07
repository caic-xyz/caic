// Tests root turn metrics across context replacement, skill activity, and missing native accounting.

package task

import (
	"fmt"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestTurnMetrics(t *testing.T) {
	t.Parallel()
	t.Run("Observe", func(t *testing.T) {
		t.Parallel()
		t.Run("ContextReplacement", func(t *testing.T) {
			t.Parallel()
			childClear := agent.ContextCleared()
			childClear.Source = agent.MessageSource{DelegationToolUseID: "child"}
			for _, tc := range []struct {
				name     string
				boundary agent.Message
				failed   bool
				duration time.Duration
			}{
				{"root clear", agent.ContextCleared(), false, 9 * time.Second},
				{"root clear then failed delivery", agent.ContextCleared(), true, 7 * time.Second},
				{"child clear", childClear, false, 109 * time.Second},
				{"automatic compaction", &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, false, 109 * time.Second},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					var metrics TurnMetrics
					metrics.Observe(&agent.ResultMessage{DurationMs: 7000, NumTurns: 4}, time.Unix(90, 0))
					metrics.Observe(&agent.UserInputMessage{Text: "unfinished"}, time.Unix(100, 0))
					metrics.Observe(&agent.TextDeltaMessage{Text: "working"}, time.Unix(101, 0))
					metrics.Observe(tc.boundary, time.Unix(150, 0))
					metrics.Observe(&agent.UserInputMessage{Text: "new"}, time.Unix(200, 0))
					if tc.failed {
						metrics.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeInputDeliveryFailed}, time.Unix(201, 0))
					}
					metrics.Observe(&agent.ResultMessage{}, time.Unix(202, 0))
					if metrics.NumTurns != 5 || metrics.Duration != tc.duration {
						t.Fatalf("metrics = %+v; want 5 turns, %v", metrics, tc.duration)
					}
				})
			}
		})
		t.Run("SkillReads", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name     string
				read     *agent.SkillReadMessage
				tool     bool
				duration time.Duration
			}{
				{"reported", &agent.SkillReadMessage{Skill: "review", ToolUseID: "skill"}, false, 2 * time.Second},
				{"inferred with owning tool", &agent.SkillReadMessage{Skill: "review", Inferred: true, SourceToolUseID: "read"}, true, 2 * time.Second},
				{"inferred alone", &agent.SkillReadMessage{Skill: "review", Inferred: true}, false, 0},
				{"child reported", &agent.SkillReadMessage{Source: agent.MessageSource{DelegationToolUseID: "child"}, Skill: "review", ToolUseID: "skill"}, false, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					var metrics TurnMetrics
					if tc.tool {
						metrics.Observe(&agent.ToolUseMessage{ToolUseID: "read", Name: "Read"}, time.Unix(100, 0))
					}
					metrics.Observe(tc.read, time.Unix(100, 0))
					metrics.Observe(&agent.ResultMessage{}, time.Unix(102, 0))
					if metrics.Duration != tc.duration || metrics.NumTurns != 1 {
						t.Fatalf("metrics = %+v; want 1 turn, %v", metrics, tc.duration)
					}
				})
			}
		})
		t.Run("FailedDeliveryRetry", func(t *testing.T) {
			t.Parallel()
			for _, accepted := range []bool{false, true} {
				t.Run(fmt.Sprintf("accepted=%v", accepted), func(t *testing.T) {
					t.Parallel()
					var metrics TurnMetrics
					at := time.Unix(100, 0)
					metrics.Observe(&agent.UserInputMessage{Text: "first"}, at)
					if accepted {
						metrics.Observe(&agent.TextDeltaMessage{Text: "working"}, at.Add(time.Second))
					}
					metrics.Observe(&agent.SystemMessage{Subtype: agent.SystemSubtypeInputDeliveryFailed}, at.Add(2*time.Second))
					metrics.Observe(&agent.UserInputMessage{Text: "retry"}, at.Add(time.Hour))
					metrics.Observe(&agent.ResultMessage{}, at.Add(time.Hour+3*time.Second))
					want := 3 * time.Second
					if accepted {
						want += time.Hour
					}
					if metrics.Duration != want || metrics.NumTurns != 1 {
						t.Fatalf("metrics = %+v; want 1 turn, %v", metrics, want)
					}
				})
			}
		})
		for _, tc := range []struct {
			name              string
			inputAt, resultAt time.Time
		}{
			{name: "unknown start", resultAt: time.Unix(100, 0)},
			{name: "unknown end", inputAt: time.Unix(100, 0)},
			{name: "reversed clock", inputAt: time.Unix(100, 0), resultAt: time.Unix(99, 0)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				var metrics TurnMetrics
				metrics.Observe(&agent.UserInputMessage{Text: "prompt"}, tc.inputAt)
				metrics.Observe(&agent.ResultMessage{}, tc.resultAt)
				if metrics.Duration != 0 || metrics.NumTurns != 1 {
					t.Fatalf("metrics = %+v; want 1 turn and unknown duration", metrics)
				}
			})
		}
	})
}
