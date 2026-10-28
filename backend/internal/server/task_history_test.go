// Tests shared live-history source ownership, invalidation, and terminal task cleanup.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/server/history"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

func TestTaskHistory(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: claudecode.New()})
	id := ksid.NewID()
	tk := mustNewTask(t, id, agent.Prompt{Text: "history"}, harness.Claude)
	tk.SetState(taskslog.StateRunning)
	insertTestTask(s, id, tk)
	entry, ok := s.taskMgr.GetEntry(id)
	if !ok {
		t.Fatal("missing test task")
	}
	path := filepath.Join(t.TempDir(), "task.jsonl")
	data := `{"t":"caic_meta","version":3,"harness":"claude","prompt":"history"}` + "\n" +
		`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"answer"}]}}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	entry.LogPath.Set(path)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	owner := newTaskHistory(ctx, s.log, s.taskMgr)
	first, err := owner.Source(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	again, err := owner.Source(t.Context(), entry)
	if err != nil || again != first {
		t.Fatalf("source not shared: %p/%p, %v", first, again, err)
	}
	snapshot, err := first.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner.Invalidate(entry, first)
	if _, err := readHistoryRange(first, t.Context(), snapshot, 1, 2); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("invalidated source remains readable: %v", err)
	}
	second, err := owner.Source(t.Context(), entry)
	if err != nil || second == first {
		t.Fatalf("source did not recover: %p/%p, %v", first, second, err)
	}
	owner.Invalidate(entry, first)
	if same, err := owner.Source(t.Context(), entry); err != nil || same != second {
		t.Fatalf("late invalidation removed replacement: %p/%p, %v", second, same, err)
	}
	tk.SetState(taskslog.StatePurged)
	owner.prune(false)
	if _, err := owner.Source(t.Context(), entry); err == nil {
		t.Fatal("terminal task accepted a live projector")
	}
	tk.SetState(taskslog.StateRunning)
	cancel()
	if _, err := owner.Source(t.Context(), entry); !errors.Is(err, context.Canceled) {
		t.Fatalf("source created after shutdown: %v", err)
	}
}

type historyPricerSpy struct {
	rate  atomic.Int64
	calls atomic.Int64
}

func (p *historyPricerSpy) ModelPrice(agent.QuotaProvider, string, time.Time) (usage.ModelPrice, bool) {
	p.calls.Add(1)
	return usage.ModelPrice{InputPerMTok: float64(p.rate.Load())}, true
}

func TestTaskHistoryImmutableInputs(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, map[harness.Name]agent.Backend{harness.Claude: claudecode.New()})
	id := ksid.NewID()
	tk := mustNewTask(t, id, agent.Prompt{Text: "history"}, harness.Claude)
	tk.SeedTimeline([]agent.Message{
		&agent.ToolUseMessage{ToolUseID: "enter", Name: "EnterPlanMode"},
		&agent.ToolUseMessage{ToolUseID: "write", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"first plan"}`)},
		&agent.ToolUseMessage{ToolUseID: "exit", Name: "ExitPlanMode"},
	})
	tk.SetState(taskslog.StateRunning)
	if snap := tk.Snapshot(); snap.PlanExitID != "exit" || snap.PlanContent != "first plan" {
		t.Fatal("fixture has no mutable visible-plan state")
	}
	price := &historyPricerSpy{}
	price.rate.Store(10)
	tk.Pricer = price
	insertTestTask(s, id, tk)
	entry, ok := s.taskMgr.GetEntry(id)
	if !ok {
		t.Fatal("missing test task")
	}
	path := filepath.Join(t.TempDir(), "task.jsonl")
	data := `{"t":"caic_meta","version":3,"harness":"claude","model":"priced-model","prompt":"history"}` + "\n" +
		`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"model":"priced-model","content":[{"type":"tool_use","id":"write","name":"Write","input":{"file_path":"/home/user/.claude/plans/p.md","content":"first plan"}}],"usage":{"input_tokens":1000000}}}}` + "\n" +
		`{"t":"agent","ts":2.000,"msg":{"type":"assistant","message":{"content":[{"type":"tool_use","id":"exit","name":"ExitPlanMode","input":{}}]}}}` + "\n" +
		`{"t":"agent","ts":3.000,"msg":{"type":"result","subtype":"success","total_cost_usd":1.25,"usage":{"input_tokens":1000000}}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	entry.LogPath.Set(path)
	owner := testTaskHandlers(s).taskSvc.history
	t.Cleanup(func() { owner.prune(true) })
	first, err := owner.Source(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := first.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before, err := readHistoryRange(first, t.Context(), snapshot, 1, snapshot.Through)
	if err != nil {
		t.Fatal(err)
	}
	var projected []history.Event
	if err := json.Unmarshal([]byte(before), &projected); err != nil {
		t.Fatal(err)
	}
	plan, reported := false, false
	for i := range projected {
		event := &projected[i].Message
		if event.ToolUse != nil {
			plan = plan || strings.Contains(string(event.ToolUse.Input), "first plan")
		}
		if event.Result != nil && event.Result.Cost.USD == 1.25 && event.Result.Cost.Source == "reported" {
			reported = true
		}
	}
	if !plan || !reported || price.calls.Load() != 0 {
		t.Fatalf("native plan=%v reported cost=%v live price calls=%d", plan, reported, price.calls.Load())
	}
	price.rate.Store(200)
	tk.ClearMessages(t.Context())
	if snap := tk.Snapshot(); snap.PlanExitID != "" || snap.PlanContent != "" {
		t.Fatal("fixture did not clear plan visibility")
	}
	price.calls.Store(0)
	generation := tk.TimelineID()
	owner.Invalidate(entry, first)
	for _, revive := range []bool{false, true} {
		if revive {
			tk.SetState(taskslog.StateStopped)
			owner.prune(false)
			tk.SetState(taskslog.StateRunning)
		}
		rebuilt, err := owner.Source(t.Context(), entry)
		if err != nil {
			t.Fatal(err)
		}
		rebuiltSnapshot, err := rebuilt.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		after, err := readHistoryRange(rebuilt, t.Context(), rebuiltSnapshot, 1, snapshot.Through)
		if err != nil || after != before || tk.TimelineID() != generation {
			t.Fatalf("same-generation recovery changed published bytes: revive=%v err=%v", revive, err)
		}
	}
	if price.calls.Load() != 0 {
		t.Fatal("recovery consulted live pricing")
	}
}

func readHistoryRange(s *history.Source, ctx context.Context, snapshot history.Snapshot, first, last uint64) (string, error) {
	var result strings.Builder
	if err := s.WriteRange(ctx, snapshot, first, last, &result); err != nil {
		return "", err
	}
	return result.String(), nil
}
