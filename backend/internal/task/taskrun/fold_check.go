// Compares live ingestion with adoption replay of one task log, for check-agent-logs.

package taskrun

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/history"
)

// foldCheckFullReplayLimit bounds the timeline length whose every root prefix
// is replayed; longer logs replay only at turn boundaries.
const foldCheckFullReplayLimit = 500

// foldCheckBoundaryReplays bounds the boundary replays of one long log; a log
// with more boundaries replays an evenly spaced subset.
const foldCheckBoundaryReplays = 200

// FoldCheck counts the root messages CheckFold compared.
type FoldCheck struct {
	Points     int // Root messages fed through live ingestion.
	Replays    int // Points also replayed the way adoption does.
	Mismatches int // Replays whose activity or summary differs from live ingestion.
}

// CheckFold feeds lt through live ingestion and records the task activity after
// each root message. It replays root prefixes the way adoption does and
// counts each replay whose activity differs. Logs up to
// foldCheckFullReplayLimit messages replay every root prefix; longer logs
// replay at turn boundaries, at most foldCheckBoundaryReplays of them evenly
// spaced, and at the end. The end also compares the other message-derived
// fields. CheckFold writes one line per root message to out, so two runs can
// be diffed.
func CheckFold(lt *history.LoadedTask, out io.Writer) (FoldCheck, error) {
	full := len(lt.Timeline) <= foldCheckFullReplayLimit
	boundaries := 0
	for _, parsed := range lt.Timeline {
		if !parsed.Message.Provenance().IsChild() && foldCheckTurnBoundary(parsed.Message) {
			boundaries++
		}
	}
	stride := max(1, (boundaries+foldCheckBoundaryReplays-1)/foldCheckBoundaryReplays)
	live, err := foldCheckTask(lt)
	if err != nil {
		return FoldCheck{}, err
	}
	var c FoldCheck
	name := filepath.Base(lt.LogPath())
	settled, boundary := 0, 0
	for i, parsed := range lt.Timeline {
		for ; settled < len(lt.Settlements) && lt.Settlements[settled] <= i; settled++ {
			live.addParsedMessage(agent.TimedMessage{Message: &agent.MetaResultMessage{}})
		}
		live.addParsedMessage(parsed)
		if parsed.Message.Provenance().IsChild() {
			continue
		}
		c.Points++
		last := i == len(lt.Timeline)-1
		sampled := false
		if foldCheckTurnBoundary(parsed.Message) {
			sampled = boundary%stride == 0
			boundary++
		}
		replayed := full || last || sampled
		got := foldCheckActivityOf(live, replayed)
		want := foldCheckActivity{}
		if replayed {
			c.Replays++
			replay, err := foldCheckTask(lt)
			if err != nil {
				return c, err
			}
			replay.SeedTimelineParts(slices.Clone(lt.Timeline[:i+1]), nil, lt.Settlements[:settled])
			want = foldCheckActivityOf(replay, true)
			if got != want {
				c.Mismatches++
			}
			if last {
				got, want := foldCheckSummaryOf(live), foldCheckSummaryOf(replay)
				if got != want {
					c.Mismatches++
				}
				if _, err := fmt.Fprintf(out, "%s\tsummary\tlive=%s\treplay=%s\n", name, got, want); err != nil {
					return c, err
				}
			}
		}
		if _, err := fmt.Fprintf(out, "%s\t%d\t%T\tlive=%s\treplay=%s\n", name, i, parsed.Message, got, want); err != nil {
			return c, err
		}
	}
	return c, nil
}

// foldCheckSummaryOf renders the other message-derived task fields.
func foldCheckSummaryOf(tk *Task) string {
	s := tk.Snapshot()
	ctx := -1
	if s.ContextUsed != nil {
		ctx = *s.ContextUsed
	}
	return fmt.Sprintf("turns=%d dur=%s cost=%.6f/%t/%t usage=%+v last=%+v api=%+v ctx=%d/%d plan=%t/%s/%d/%s diff=%d/%d bg=%+v turnErr=%q exitErr=%q session=%s",
		s.NumTurns, s.Duration, s.CostUSD, s.CostKnown, s.CostComplete, s.Usage, s.LastUsage, s.LastAPIUsage, ctx, s.ContextWindowLimit,
		s.InPlanMode, s.PlanFile, len(s.PlanContent), s.PlanExitID, len(s.DiffStat), len(s.RepoStates), s.Background, s.TurnError, tk.LastExitError(), s.SessionID)
}

// foldCheckStart is when the compared tasks enter the state import gives
// an adopted task before replay; live ingestion starts from it too.
var foldCheckStart = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func foldCheckTask(lt *history.LoadedTask) (*Task, error) {
	tk, err := NewTask(ksid.NewID(), agent.Prompt{Text: "replay"}, lt.Harness, "", "", "", "", "")
	if err != nil {
		return nil, err
	}
	tk.Restore(task.StateStarting, foldCheckStart)
	return tk, nil
}

// foldCheckTurnBoundary reports a root message that starts, interrupts, or
// ends a turn.
func foldCheckTurnBoundary(m agent.Message) bool {
	switch m.(type) {
	case *agent.TurnStartedMessage, *agent.UserInputMessage, *agent.AskMessage, *agent.ResultMessage, *agent.ExitMessage:
		return true
	default:
		return false
	}
}

// foldCheckActivity is the activity of a task. The fields after replayed are
// recorded only where the comparison replays, to keep each point cheap.
type foldCheckActivity struct {
	state                task.State
	stateAt, turnStarted time.Time
	replayed             bool
	turnEnded            bool
	pendingActions       int
}

func foldCheckActivityOf(tk *Task, replayed bool) foldCheckActivity {
	s := tk.Snapshot()
	a := foldCheckActivity{state: s.State, stateAt: s.StateUpdatedAt, turnStarted: s.TurnStartedAt, replayed: replayed}
	if replayed {
		a.turnEnded = tk.BackgroundActivity().TurnEnded
		a.pendingActions = len(tk.PendingUserActions())
	}
	return a
}

func (a foldCheckActivity) String() string {
	at := func(t time.Time) string {
		if t.IsZero() {
			return "0"
		}
		return fmt.Sprintf("%.3f", float64(t.UnixMilli())/1000)
	}
	if a.state == "" {
		return "-"
	}
	if !a.replayed {
		return fmt.Sprintf("%s|%s|%s", a.state, at(a.stateAt), at(a.turnStarted))
	}
	return fmt.Sprintf("%s|%s|%s|%t|%d", a.state, at(a.stateAt), at(a.turnStarted), a.turnEnded, a.pendingActions)
}
