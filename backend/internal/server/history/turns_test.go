// Tests turn timelines: concurrent and child tools, subagents, outliving commands, queued turns, per-call usage across harnesses, tool totals, and caps.

package history

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	taskslogv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/history/data"
)

// timedRecord is one raw record parsed into msgs, written at ms since the epoch.
type timedRecord struct {
	ms   int64
	msgs []agent.Message
}

func timelineFixture(t *testing.T, records ...timedRecord) *Source {
	f := newLiveTimeline(t)
	f.add(records...)
	return f.source
}

// liveTimeline is a task log that a test appends records to while its source
// stays open, as a running task's log grows.
type liveTimeline struct {
	t       *testing.T
	path    string
	source  *Source
	records []timedRecord
}

func newLiveTimeline(t *testing.T) *liveTimeline {
	f := &liveTimeline{t: t, path: writeFixture(t, `{"t":"caic_meta","version":3,"harness":"claude","model":"model","prompt":"p"}`)}
	resolver := agent.Backends{harness.Claude: &agenttest.FakeBackend{HarnessName: harness.Claude, WireFactory: func() agent.WireFormat {
		return &agenttest.Wire{Output: func(data []byte) ([]agent.Message, error) {
			var index int
			if err := json.Unmarshal(data, &index); err != nil {
				return nil, err
			}
			return f.records[index].msgs, nil
		}}
	}}}
	source, err := NewSource(t.Context(), f.path, "", resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	f.source = source
	return f
}

// add appends records to the log and projects them.
func (f *liveTimeline) add(records ...timedRecord) {
	var b strings.Builder
	for _, r := range records {
		fmt.Fprintf(&b, `{"t":"agent","ts":%d.%03d,"msg":%d}`+"\n", r.ms/1000, r.ms%1000, len(f.records))
		f.records = append(f.records, r)
	}
	appendFixture(f.t, f.path, b.String())
	if _, err := f.source.Refresh(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}

// running returns the manifest's running turn.
func (f *liveTimeline) running() *v1.HistoryRunningTurn {
	_, _, meta, err := f.source.Sections(f.t.Context(), "")
	if err != nil {
		f.t.Fatal(err)
	}
	return meta.RunningTurn
}

func at(ms int64, msgs ...agent.Message) timedRecord { return timedRecord{ms: ms, msgs: msgs} }

func prompt() *agent.UserInputMessage { return &agent.UserInputMessage{Text: "go"} }

func use(id, name string, src *taskslogv3.MessageSource) *agent.ToolUseMessage {
	return &agent.ToolUseMessage{Source: *src, ToolUseID: id, Name: name, Detail: name + " detail"}
}

func done(id string, src *taskslogv3.MessageSource) *agent.ToolResultMessage {
	return &agent.ToolResultMessage{Source: *src, ToolUseID: id}
}

func result() *agent.ResultMessage {
	return &agent.ResultMessage{MessageType: "result", Subtype: "success"}
}

// spanShape drops labels so cases compare times, kinds, and identities.
type spanShape struct {
	Kind       v1.HistoryTurnSpanKind
	Start      int64
	End        *int64
	Status     string
	Agent      string
	ToolUseID  string
	ActivityID string
	CommandID  string
}

func shapes(spans []v1.HistoryTurnSpan) []spanShape {
	out := make([]spanShape, 0, len(spans))
	for i := range spans {
		s := &spans[i]
		out = append(out, spanShape{s.Kind, s.StartMS, s.EndMS, s.Status, s.Source.AgentID, s.ToolUseID, s.ActivityID, s.CommandID})
	}
	return out
}

func TestSourceTurnTimeline(t *testing.T) {
	t.Parallel()
	root := taskslogv3.MessageSource{}
	child := taskslogv3.MessageSource{AgentID: "agent-1"}
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		t.Run("a turn in an archived context", func(t *testing.T) {
			t.Parallel()
			source := timelineFixture(t,
				at(500, &agent.InitMessage{SessionID: "one"}),
				at(1_000, prompt()),
				at(2_000, use("a", "Read", &root)),
				at(3_000, done("a", &root)),
				at(4_000, result()),
				at(5_000, agent.ContextCleared()),
				at(6_000, prompt()),
				at(7_000, use("a", "Read", &root)),
				at(9_000, done("a", &root)),
				at(10_000, result()),
			)
			if _, sections, _, err := source.Sections(t.Context(), ""); err != nil || len(sections) != 2 {
				t.Fatalf("sections = %d, %v; want 2", len(sections), err)
			}
			got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []spanShape{
				{Kind: v1.HistoryTurnSpanKindModel, Start: 0, End: new(int64(1000))},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 1000, End: new(int64(2000)), Status: "completed", ToolUseID: "a"},
				{Kind: v1.HistoryTurnSpanKindModel, Start: 2000, End: new(int64(3000))},
			}
			if s := shapes(got.Spans); !reflect.DeepEqual(s, want) {
				t.Errorf("spans = %+v\nwant %+v", s, want)
			}
		})
		t.Run("concurrent tools leave inferred model gaps", func(t *testing.T) {
			t.Parallel()
			source := timelineFixture(t,
				at(10_000, prompt()),
				at(11_000, use("a", "Read", &root), use("b", "Grep", &root)),
				at(11_500, done("b", &root)),
				at(13_000, done("a", &root)),
				at(14_000, &agent.ToolUseMessage{ToolUseID: "c", Name: "Bash"}),
				at(14_500, &agent.ToolResultMessage{ToolUseID: "c", Error: "exit 1"}),
				at(16_000, result()),
			)
			got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []spanShape{
				{Kind: v1.HistoryTurnSpanKindModel, Start: 0, End: new(int64(1000))},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 1000, End: new(int64(3000)), Status: "completed", ToolUseID: "a"},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 1000, End: new(int64(1500)), Status: "completed", ToolUseID: "b"},
				{Kind: v1.HistoryTurnSpanKindModel, Start: 3000, End: new(int64(4000))},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 4000, End: new(int64(4500)), Status: "failed", ToolUseID: "c"},
				{Kind: v1.HistoryTurnSpanKindModel, Start: 4500, End: new(int64(6000))},
			}
			if g := shapes(got.Spans); !reflect.DeepEqual(g, want) {
				t.Fatalf("spans =\n%+v\nwant\n%+v", g, want)
			}
			if got.ModelMS != 3500 || got.Number != 1 || !got.StartedAt.Equal(time.UnixMilli(10_000)) || !got.EndedAt.Equal(time.UnixMilli(16_000)) || len(got.Omitted) != 0 {
				t.Fatalf("timeline = %+v", got)
			}
			if s := got.Spans[1]; s.Label != "Read" || s.Detail != "Read detail" {
				t.Fatalf("tool label = %q, detail = %q", s.Label, s.Detail)
			}
			// Equal totals sort by name; concurrent calls overlap.
			wantTools := []v1.HistoryTurnTool{
				{Name: "Read", Calls: 1, DurationMS: 2000, LongestMS: 2000},
				{Name: "Bash", Calls: 1, DurationMS: 500, LongestMS: 500},
				{Name: "Grep", Calls: 1, DurationMS: 500, LongestMS: 500},
			}
			if !reflect.DeepEqual(got.Tools, wantTools) {
				t.Fatalf("tools = %+v, want %+v", got.Tools, wantTools)
			}
		})
		t.Run("a turn without tools is model time", func(t *testing.T) {
			t.Parallel()
			source := timelineFixture(t,
				at(1_000, prompt()),
				at(1_500, &agent.TextMessage{Text: "hello"}),
				at(3_000, result()),
			)
			got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []spanShape{{Kind: v1.HistoryTurnSpanKindModel, Start: 0, End: new(int64(2000))}}
			if g := shapes(got.Spans); !reflect.DeepEqual(g, want) || got.ModelMS != 2000 {
				t.Fatalf("spans = %+v, model = %d", g, got.ModelMS)
			}
		})
		t.Run("subagents own child tools and outlive the turn", func(t *testing.T) {
			t.Parallel()
			source := timelineFixture(t,
				at(1_000, prompt()),
				at(2_000, use("task", "Agent", &root), &agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: "agent-1", ToolUseID: "task", Label: "explore", Status: agent.NativeSubagentStatusRunning}}),
				at(2_500, use("child-read", "Read", &child)),
				at(3_500, done("child-read", &child)),
				at(3_600, use("child-open", "Bash", &child)),
				at(4_000, &agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: "agent-1", ToolUseID: "task", Status: agent.NativeSubagentStatusCompleted}}, done("task", &root)),
				at(4_500, use("bg", "Agent", &root), &agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: "agent-2", ToolUseID: "bg", Label: "watch", Status: agent.NativeSubagentStatusRunning, Background: true}}),
				at(4_600, done("bg", &root)),
				at(5_000, result()),
				// The child's tool and the background agent end after the turn.
				at(6_000, done("child-open", &child)),
			)
			got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []spanShape{
				{Kind: v1.HistoryTurnSpanKindModel, Start: 0, End: new(int64(1000))},
				{Kind: v1.HistoryTurnSpanKindSubagent, Start: 1000, End: new(int64(3000)), Status: "completed", ActivityID: "agent-1"},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 1000, End: new(int64(3000)), Status: "completed", ToolUseID: "task"},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 1500, End: new(int64(2500)), Status: "completed", Agent: "agent-1", ToolUseID: "child-read", ActivityID: "agent-1"},
				// A child call still open when the turn ended has no end.
				{Kind: v1.HistoryTurnSpanKindTool, Start: 2600, Status: "", Agent: "agent-1", ToolUseID: "child-open", ActivityID: "agent-1"},
				{Kind: v1.HistoryTurnSpanKindModel, Start: 3000, End: new(int64(3500))},
				{Kind: v1.HistoryTurnSpanKindSubagent, Start: 3500, Status: "running", ActivityID: "agent-2"},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 3500, End: new(int64(3600)), Status: "completed", ToolUseID: "bg"},
				{Kind: v1.HistoryTurnSpanKindModel, Start: 3600, End: new(int64(4000))},
			}
			if g := shapes(got.Spans); !reflect.DeepEqual(g, want) {
				t.Fatalf("spans =\n%+v\nwant\n%+v", g, want)
			}
			// Child calls and the background agent do not reduce model time.
			if got.ModelMS != 1900 {
				t.Fatalf("model = %d, want 1900", got.ModelMS)
			}
			// The open child call counts up to the turn end.
			wantTools := []v1.HistoryTurnTool{
				{Name: "Agent", Calls: 2, DurationMS: 2100, LongestMS: 2000},
				{Name: "Bash", Calls: 1, DurationMS: 1400, LongestMS: 1400},
				{Name: "Read", Calls: 1, DurationMS: 1000, LongestMS: 1000},
			}
			if !reflect.DeepEqual(got.Tools, wantTools) {
				t.Fatalf("tools = %+v, want %+v", got.Tools, wantTools)
			}
		})
		t.Run("background commands extend past the turn end", func(t *testing.T) {
			t.Parallel()
			source := timelineFixture(t,
				// A command from an earlier turn is not this turn's work.
				at(500, &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "old", Status: agent.BackgroundCommandStatusRunning}}),
				at(1_000, prompt()),
				at(1_200, use("serve", "Bash", &root)),
				at(1_300, done("serve", &root), &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "cmd-1", Label: "npm run dev", ToolUseID: "serve", Status: agent.BackgroundCommandStatusRunning}}),
				at(1_400, &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "cmd-2", Label: "sleep", Status: agent.BackgroundCommandStatusRunning}}),
				at(2_000, result()),
				at(9_000, &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "cmd-1", Status: agent.BackgroundCommandStatusCompleted}}),
			)
			got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []spanShape{
				{Kind: v1.HistoryTurnSpanKindModel, Start: 0, End: new(int64(200))},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 200, End: new(int64(300)), Status: "completed", ToolUseID: "serve"},
				{Kind: v1.HistoryTurnSpanKindBackground, Start: 300, End: new(int64(8000)), Status: "completed", ToolUseID: "serve", CommandID: "cmd-1"},
				{Kind: v1.HistoryTurnSpanKindModel, Start: 300, End: new(int64(1000))},
				{Kind: v1.HistoryTurnSpanKindBackground, Start: 400, Status: "running", CommandID: "cmd-2"},
			}
			if g := shapes(got.Spans); !reflect.DeepEqual(g, want) {
				t.Fatalf("spans =\n%+v\nwant\n%+v", g, want)
			}
			if got.Spans[2].Label != "npm run dev" {
				t.Fatalf("label = %q", got.Spans[2].Label)
			}
		})
		t.Run("a harness-queued turn starts at its first root activity", func(t *testing.T) {
			t.Parallel()
			source := timelineFixture(t,
				at(1_000, prompt()),
				at(2_000, result()),
				at(5_000, &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "cmd", Status: agent.BackgroundCommandStatusCompleted}}),
				at(6_000, &agent.ThinkingMessage{Text: "the command ended"}),
				at(6_500, use("read", "Read", &root)),
				at(7_000, done("read", &root)),
				at(8_000, result()),
			)
			got, err := source.TurnTimeline(t.Context(), 2, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []spanShape{
				{Kind: v1.HistoryTurnSpanKindModel, Start: 0, End: new(int64(500))},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 500, End: new(int64(1000)), Status: "completed", ToolUseID: "read"},
				{Kind: v1.HistoryTurnSpanKindModel, Start: 1000, End: new(int64(2000))},
			}
			if g := shapes(got.Spans); !reflect.DeepEqual(g, want) || !got.StartedAt.Equal(time.UnixMilli(6_000)) {
				t.Fatalf("started = %v, spans = %+v", got.StartedAt, g)
			}
		})
		t.Run("the cap keeps the longest spans and counts the rest", func(t *testing.T) {
			t.Parallel()
			records := make([]timedRecord, 1, 603)
			records[0] = at(1_000, prompt())
			// 300 sequential calls lasting 1..300ms after 1s model gaps.
			now := int64(1_000)
			for i := range 300 {
				id := fmt.Sprintf("t%d", i)
				now += 1000
				records = append(records, at(now, use(id, "Read", &root)))
				now += int64(i + 1)
				records = append(records, at(now, done(id, &root)))
			}
			records = append(records, at(now+1000, use("open", "Bash", &root)), at(now+2000, result()))
			source := timelineFixture(t, records...)
			got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Spans) != maxTurnSpans {
				t.Fatalf("spans = %d, want %d", len(got.Spans), maxTurnSpans)
			}
			// The open call and 301 gaps outrank every call; the 102 shortest
			// of the 300 ended calls are left out.
			wantOmitted := []v1.HistoryTurnOmission{{Kind: v1.HistoryTurnSpanKindTool, Count: 102, DurationMS: 102 * 103 / 2}}
			if !reflect.DeepEqual(got.Omitted, wantOmitted) {
				t.Fatalf("omitted = %+v, want %+v", got.Omitted, wantOmitted)
			}
			if got.ModelMS != 301_000 {
				t.Fatalf("model = %d: the cap must not change the exact total", got.ModelMS)
			}
			// Tool totals include the omitted calls.
			if read := got.Tools[0]; read.Name != "Read" || read.Calls != 300 || read.DurationMS != 300*301/2 || read.LongestMS != 300 {
				t.Fatalf("tools = %+v", got.Tools)
			}
			for i := 1; i < len(got.Spans); i++ {
				if got.Spans[i].StartMS < got.Spans[i-1].StartMS {
					t.Fatal("kept spans are not sorted by start")
				}
			}
		})
		t.Run("omitted open spans count up to the turn end", func(t *testing.T) {
			t.Parallel()
			// 501 calls never end, so every one outranks the model gap before
			// them; the latest open call is left out with that gap.
			records := make([]timedRecord, 1, 503)
			records[0] = at(1_000, prompt())
			for i := range maxTurnSpans + 1 {
				records = append(records, at(2_000+int64(i), use(fmt.Sprintf("t%d", i), "Bash", &root)))
			}
			records = append(records, at(5_000, result()))
			source := timelineFixture(t, records...)
			got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []v1.HistoryTurnOmission{
				{Kind: v1.HistoryTurnSpanKindModel, Count: 1, DurationMS: 1_000},
				{Kind: v1.HistoryTurnSpanKindTool, Count: 1, DurationMS: 4_000 - 1_000 - maxTurnSpans},
			}
			if len(got.Spans) != maxTurnSpans || !reflect.DeepEqual(got.Omitted, want) {
				t.Fatalf("spans = %d, omitted = %+v, want %+v", len(got.Spans), got.Omitted, want)
			}
		})
	})
	t.Run("calls", testTurnCalls)
	t.Run("running", testRunningTurn)
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		source := timelineFixture(t, at(1_000, prompt()), at(2_000, result()), at(3_000, prompt()))
		for _, n := range []int{0, 3} {
			if _, err := source.TurnTimeline(t.Context(), n, time.Time{}, nil); !errors.Is(err, ErrUnknownTurn) {
				t.Fatalf("turn %d: err = %v, want ErrUnknownTurn", n, err)
			}
		}
	})
}

// testRunningTurn follows one turn while a view would poll it: the turn
// starts, streams, and completes, and a command it starts ends after it.
func testRunningTurn(t *testing.T) {
	t.Parallel()
	root := taskslogv3.MessageSource{}
	timeline := func(f *liveTimeline, n int, now int64) v1.HistoryTurnTimeline {
		got, err := f.source.TurnTimeline(t.Context(), n, time.UnixMilli(now), nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	t.Run("starts, streams, and completes", func(t *testing.T) {
		t.Parallel()
		f := newLiveTimeline(t)
		f.add(at(500, &agent.InitMessage{SessionID: "one"}), at(1_000, prompt()), at(2_000, result()))
		if r := f.running(); r != nil {
			t.Fatalf("idle: running = %+v", r)
		}
		if _, err := f.source.TurnTimeline(t.Context(), 2, time.UnixMilli(3_000), nil); !errors.Is(err, ErrUnknownTurn) {
			t.Fatalf("idle turn 2: err = %v, want ErrUnknownTurn", err)
		}

		f.add(at(3_000, prompt()), at(4_000, use("a", "Bash", &root)))
		want := &v1.HistoryRunningTurn{Number: 2, SectionID: "1", StartedAt: time.UnixMilli(3_000).UTC()}
		if r := f.running(); !reflect.DeepEqual(r, want) {
			t.Fatalf("running = %+v, want %+v", r, want)
		}
		// An open root tool call covers the rest of the axis: no model span
		// follows it, and the tool has no end yet.
		got := timeline(f, 2, 4_500)
		if !got.EndedAt.IsZero() || got.DurationMS != 1_500 || got.ModelMS != 1_000 {
			t.Fatalf("open tool: endedAt = %v, duration = %d, model = %d", got.EndedAt, got.DurationMS, got.ModelMS)
		}
		wantSpans := []spanShape{
			{Kind: v1.HistoryTurnSpanKindModel, Start: 0, End: new(int64(1_000))},
			{Kind: v1.HistoryTurnSpanKindTool, Start: 1_000, ToolUseID: "a"},
		}
		if s := shapes(got.Spans); !reflect.DeepEqual(s, wantSpans) {
			t.Fatalf("open tool: spans = %+v\nwant %+v", s, wantSpans)
		}
		if len(got.Tools) != 1 || got.Tools[0].DurationMS != 500 {
			t.Fatalf("open tool: tools = %+v, want Bash up to the axis end", got.Tools)
		}

		f.add(at(5_000, done("a", &root)), at(5_500, &agent.TextMessage{Text: "checking"}))
		// The model span after the tool is provisional: it has no end and
		// grows to the axis end, the later of the clock and the last record.
		for _, c := range []struct {
			now, duration int64
		}{{now: 7_000, duration: 4_000}, {now: 4_000, duration: 2_500}} {
			got = timeline(f, 2, c.now)
			wantSpans = []spanShape{
				{Kind: v1.HistoryTurnSpanKindModel, Start: 0, End: new(int64(1_000))},
				{Kind: v1.HistoryTurnSpanKindTool, Start: 1_000, End: new(int64(2_000)), Status: "completed", ToolUseID: "a"},
				{Kind: v1.HistoryTurnSpanKindModel, Start: 2_000},
			}
			if s := shapes(got.Spans); !reflect.DeepEqual(s, wantSpans) || got.DurationMS != c.duration || got.ModelMS != c.duration-1_000 {
				t.Fatalf("now %d: duration = %d, model = %d, spans = %+v\nwant %d, %+v", c.now, got.DurationMS, got.ModelMS, s, c.duration, wantSpans)
			}
		}

		f.add(at(8_000, result()))
		if r := f.running(); r != nil {
			t.Fatalf("completed: running = %+v", r)
		}
		got = timeline(f, 2, 60_000)
		if !got.EndedAt.Equal(time.UnixMilli(8_000)) || got.DurationMS != 5_000 || got.ModelMS != 4_000 {
			t.Fatalf("completed: endedAt = %v, duration = %d, model = %d", got.EndedAt, got.DurationMS, got.ModelMS)
		}
		if last := got.Spans[len(got.Spans)-1]; last.EndMS == nil || *last.EndMS != 5_000 {
			t.Fatalf("completed: last span = %+v, want a model span ending at the result", last)
		}
	})
	t.Run("a command that outlives its turn ends later", func(t *testing.T) {
		t.Parallel()
		f := newLiveTimeline(t)
		started := &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "cmd", Label: "sleep", Status: agent.BackgroundCommandStatusRunning}}
		f.add(at(1_000, prompt()), at(2_000, started), at(3_000, result()))
		got := timeline(f, 1, 60_000)
		if s := shapes(got.Spans); len(s) != 2 || s[1].Kind != v1.HistoryTurnSpanKindBackground || s[1].End != nil {
			t.Fatalf("before end: spans = %+v", s)
		}
		ended := &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "cmd", Status: agent.BackgroundCommandStatusCompleted}}
		f.add(at(9_000, ended))
		got = timeline(f, 1, 60_000)
		if s := shapes(got.Spans); len(s) != 2 || s[1].End == nil || *s[1].End != 8_000 || got.DurationMS != 2_000 {
			t.Fatalf("after end: duration = %d, spans = %+v", got.DurationMS, s)
		}
	})
	t.Run("host exit halts the turn until input resumes it", func(t *testing.T) {
		t.Parallel()
		f := newLiveTimeline(t)
		f.add(at(1_000, prompt()), at(2_000, &agent.TextMessage{Text: "working"}), at(3_000, &agent.ExitMessage{}))
		if r := f.running(); r != nil {
			t.Fatalf("after exit: running = %+v", r)
		}
		if _, err := f.source.TurnTimeline(t.Context(), 1, time.UnixMilli(4_000), nil); !errors.Is(err, ErrUnknownTurn) {
			t.Fatalf("after exit: err = %v, want ErrUnknownTurn", err)
		}
		f.add(at(5_000, prompt()))
		want := &v1.HistoryRunningTurn{Number: 1, SectionID: "1", StartedAt: time.UnixMilli(1_000).UTC()}
		if r := f.running(); !reflect.DeepEqual(r, want) {
			t.Fatalf("resumed: running = %+v, want %+v", r, want)
		}
	})
}

// callShape drops the source so cases compare numbers, times, and accounting.
type callShape struct {
	Number int
	At     int64
	Model  string
	Tokens v1.HistoryTurnTokens
	Window int
	Cost   v1.EventCost
}

func callShapes(calls []v1.HistoryTurnCall) []callShape {
	out := make([]callShape, 0, len(calls))
	for i := range calls {
		c := &calls[i]
		out = append(out, callShape{c.Number, c.AtMS, c.Model, c.HistoryTurnTokens, c.ContextWindow, v1.EventCost{USD: math.Round(c.Cost.USD*1e9) / 1e9, Source: c.Cost.Source}})
	}
	return out
}

func estimated(usd float64) v1.EventCost {
	return v1.EventCost{USD: usd, Source: v1.EventCostEstimated}
}

func testTurnCalls(t *testing.T) {
	t.Parallel()
	t.Run("Claude folds each call's repeated reports and prices it once", func(t *testing.T) {
		t.Parallel()
		// Claude Code repeats a request's usage with each content block, then
		// reports the final output in a model-derived message delta. A
		// subagent's call carries its delegating tool and its own model.
		const usage1 = `"usage":{"input_tokens":10,"cache_creation_input_tokens":6000,"cache_read_input_tokens":12000,"output_tokens":8}`
		const usage2 = `"usage":{"input_tokens":8,"cache_creation_input_tokens":183,"cache_read_input_tokens":18247,"output_tokens":1}`
		path := writeFixture(t,
			`{"t":"caic_meta","version":3,"harness":"claude","model":"requested","prompt":"p"}`,
			`{"t":"agent","ts":0.500,"msg":{"type":"system","subtype":"init","session_id":"s","model":"claude-root"}}`,
			`{"t":"agent","ts":1.000,"msg":{"type":"user","message":{"role":"user","content":[{"type":"text","text":"go"}]},"session_id":"s"}}`,
			`{"t":"agent","ts":2.000,"msg":{"type":"assistant","message":{"model":"claude-root","id":"m1","role":"assistant","content":[{"type":"thinking","thinking":"plan"}],`+usage1+`},"session_id":"s"}}`,
			`{"t":"agent","ts":2.100,"msg":{"type":"assistant","message":{"model":"claude-root","id":"m1","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"prompt":"look"}}],`+usage1+`},"session_id":"s"}}`,
			`{"t":"agent","ts":2.200,"msg":{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"cache_creation_input_tokens":6000,"cache_read_input_tokens":12000,"output_tokens":119,"output_tokens_details":{"thinking_tokens":58}}},"session_id":"s","parent_tool_use_id":null}}`,
			`{"t":"agent","ts":3.000,"msg":{"type":"assistant","message":{"model":"claude-child","id":"c1","role":"assistant","content":[{"type":"text","text":"found"}],"usage":{"input_tokens":5,"cache_read_input_tokens":3000,"output_tokens":2}},"session_id":"s","parent_tool_use_id":"t1"}}`,
			`{"t":"agent","ts":3.500,"msg":{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"cache_read_input_tokens":3000,"output_tokens":40}},"session_id":"s","parent_tool_use_id":"t1"}}`,
			`{"t":"agent","ts":4.000,"msg":{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"found"}]},"session_id":"s"}}`,
			`{"t":"agent","ts":5.000,"msg":{"type":"assistant","message":{"model":"claude-root","id":"m2","role":"assistant","content":[{"type":"text","text":"done"}],`+usage2+`},"session_id":"s"}}`,
			`{"t":"agent","ts":5.500,"msg":{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":8,"cache_creation_input_tokens":183,"cache_read_input_tokens":18247,"output_tokens":445,"output_tokens_details":{"thinking_tokens":237}}},"session_id":"s","parent_tool_use_id":null}}`,
			`{"t":"agent","ts":6.000,"msg":{"type":"result","subtype":"success","total_cost_usd":0.5,"num_turns":2,"usage":{"input_tokens":23,"cache_creation_input_tokens":6183,"cache_read_input_tokens":33247,"output_tokens":604},"modelUsage":{"claude-root":{"contextWindow":200000}},"session_id":"s"}}`,
		)
		source, err := NewSource(t.Context(), path, "", backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
		if _, err := source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, journalPricer{})
		if err != nil {
			t.Fatal(err)
		}
		// journalPricer charges $2/Mt for new input and cache writes, nothing
		// for cache reads, and $10/Mt for output. The turn's reported total
		// does not make per-call costs reported.
		want := []callShape{
			{1, 1200, "claude-root", v1.HistoryTurnTokens{InputTokens: 10, CacheCreationInputTokens: 6000, CacheReadInputTokens: 12000, OutputTokens: 119, ReasoningOutputTokens: 58}, 200000, estimated(0.01321)},
			{2, 2500, "claude-child", v1.HistoryTurnTokens{InputTokens: 5, CacheReadInputTokens: 3000, OutputTokens: 40}, 0, estimated(0.00041)},
			{3, 4500, "claude-root", v1.HistoryTurnTokens{InputTokens: 8, CacheCreationInputTokens: 183, CacheReadInputTokens: 18247, OutputTokens: 445, ReasoningOutputTokens: 237}, 200000, estimated(0.004832)},
		}
		if g := callShapes(got.Calls); !reflect.DeepEqual(g, want) {
			t.Fatalf("calls =\n%+v\nwant\n%+v", g, want)
		}
		if got.Calls[1].Source.DelegationToolUseID != "t1" || got.Calls[0].Source.DelegationToolUseID != "" {
			t.Fatalf("sources = %+v, %+v", got.Calls[0].Source, got.Calls[1].Source)
		}
		if got.OmittedCalls != (v1.HistoryTurnCallOmission{}) || got.Unattributed != (v1.HistoryTurnTokens{}) {
			t.Fatalf("omitted = %+v, unattributed = %+v", got.OmittedCalls, got.Unattributed)
		}
	})
	t.Run("Codex prices deduplicated calls and reports cumulative deltas as unattributed", func(t *testing.T) {
		t.Parallel()
		// The Codex adapter marks a notification PerCall when its delta is
		// exactly one request; a delta across missed notifications covers
		// requests that have no call of their own, so it is neither a call nor
		// priced per call, but its tokens stay in the turn's unattributed sum.
		root := taskslogv3.MessageSource{ThreadID: "root", TurnID: "turn"}
		child := taskslogv3.MessageSource{ThreadID: "child", RootThreadID: "root"}
		call := func(src taskslogv3.MessageSource, model string, input, cached, output int) *agent.UsageMessage {
			return &agent.UsageMessage{Source: src, PerCall: true, ModelDerived: true, ReportedModel: model, ContextWindow: 272_000, Usage: agent.Usage{InputTokens: input, CacheReadInputTokens: cached, OutputTokens: output}}
		}
		delta := call(root, "gpt-root", 500, 0, 50)
		delta.PerCall = false
		source := timelineFixture(t,
			at(1_000, prompt()),
			at(2_000, call(root, "gpt-root", 100, 1_000, 10)),
			// Identical input: Codex deduplicated both, so both are calls.
			at(3_000, call(root, "gpt-root", 100, 1_000, 10)),
			at(3_500, call(child, "gpt-child", 50, 0, 5)),
			at(4_000, delta),
			at(5_000, &agent.ResultMessage{MessageType: "result", Subtype: "success", Usage: agent.Usage{InputTokens: 750, CacheReadInputTokens: 2_000, OutputTokens: 75}}),
		)
		got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, journalPricer{})
		if err != nil {
			t.Fatal(err)
		}
		want := []callShape{
			{1, 1000, "gpt-root", v1.HistoryTurnTokens{InputTokens: 100, CacheReadInputTokens: 1000, OutputTokens: 10}, 272000, estimated(0.0003)},
			{2, 2000, "gpt-root", v1.HistoryTurnTokens{InputTokens: 100, CacheReadInputTokens: 1000, OutputTokens: 10}, 272000, estimated(0.0003)},
			{3, 2500, "gpt-child", v1.HistoryTurnTokens{InputTokens: 50, OutputTokens: 5}, 272000, estimated(0.00015)},
		}
		if g := callShapes(got.Calls); !reflect.DeepEqual(g, want) {
			t.Fatalf("calls =\n%+v\nwant\n%+v", g, want)
		}
		if want := (v1.HistoryTurnTokens{InputTokens: 500, OutputTokens: 50}); got.Unattributed != want {
			t.Fatalf("unattributed = %+v, want %+v", got.Unattributed, want)
		}
	})
	t.Run("a harness reporting each call's cost keeps it reported", func(t *testing.T) {
		t.Parallel()
		cost := 0.25
		source := timelineFixture(t,
			at(1_000, prompt()),
			at(2_000, &agent.UsageMessage{PerCall: true, ReportedModel: "pi-model", ReportedCostUSD: &cost, ContextWindow: 128_000, Usage: agent.Usage{InputTokens: 10, OutputTokens: 4}}),
			// A summary without request detail adds no call; its tokens are
			// unattributed.
			at(2_500, &agent.UsageMessage{ReportedCostUSD: &cost, Usage: agent.Usage{InputTokens: 30, OutputTokens: 9}}),
			at(3_000, result()),
		)
		got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, journalPricer{})
		if err != nil {
			t.Fatal(err)
		}
		want := []callShape{{1, 1000, "pi-model", v1.HistoryTurnTokens{InputTokens: 10, OutputTokens: 4}, 128000, v1.EventCost{USD: 0.25, Source: v1.EventCostReported}}}
		if g := callShapes(got.Calls); !reflect.DeepEqual(g, want) {
			t.Fatalf("calls = %+v, want %+v", g, want)
		}
		if want := (v1.HistoryTurnTokens{InputTokens: 30, OutputTokens: 9}); got.Unattributed != want {
			t.Fatalf("unattributed = %+v, want %+v", got.Unattributed, want)
		}
	})
	t.Run("a harness without per-call usage has no calls", func(t *testing.T) {
		t.Parallel()
		// An ACP harness reports occupancy without tokens, then turn totals.
		used := 45_000
		source := timelineFixture(t,
			at(1_000, prompt()),
			at(2_000, &agent.UsageMessage{ContextWindow: 1_000_000, ContextUsed: &used}),
			at(3_000, &agent.ResultMessage{MessageType: "result", Subtype: "success", Usage: agent.Usage{InputTokens: 40_000, OutputTokens: 900}}),
		)
		got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, journalPricer{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var shape struct {
			Calls        []json.RawMessage `json:"calls"`
			OmittedCalls *json.RawMessage  `json:"omittedCalls"`
			Unattributed *json.RawMessage  `json:"unattributed"`
		}
		if err := json.Unmarshal(encoded, &shape); err != nil {
			t.Fatal(err)
		}
		if shape.Calls == nil || len(shape.Calls) != 0 || shape.OmittedCalls != nil || shape.Unattributed != nil {
			t.Fatalf("calls = %s", encoded)
		}
	})
	t.Run("an unpriced call has no cost", func(t *testing.T) {
		t.Parallel()
		source := timelineFixture(t,
			at(1_000, prompt()),
			at(2_000, &agent.UsageMessage{PerCall: true, ReportedModel: "unpriced", Usage: agent.Usage{InputTokens: 10}}),
			at(3_000, result()),
		)
		got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Calls) != 1 || got.Calls[0].Cost != (v1.EventCost{}) {
			t.Fatalf("calls = %+v", got.Calls)
		}
	})
	t.Run("the cap keeps the calls with the most tokens", func(t *testing.T) {
		t.Parallel()
		records := make([]timedRecord, 0, maxTurnCalls+4)
		records = append(records, at(1_000, prompt()))
		for i := range maxTurnCalls + 2 {
			// Calls 1 and 2 are the smallest.
			records = append(records, at(2_000+int64(i), &agent.UsageMessage{PerCall: true, ReportedModel: "m", Usage: agent.Usage{InputTokens: 100 + i, OutputTokens: 1, ReasoningOutputTokens: 1}}))
		}
		records = append(records, at(9_000, result()))
		source := timelineFixture(t, records...)
		got, err := source.TurnTimeline(t.Context(), 1, time.Time{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Calls) != maxTurnCalls || got.Calls[0].Number != 3 || got.Calls[len(got.Calls)-1].Number != maxTurnCalls+2 {
			t.Fatalf("calls = %d, from %d", len(got.Calls), got.Calls[0].Number)
		}
		want := v1.HistoryTurnCallOmission{Count: 2, InputTokens: 201, OutputTokens: 2, ReasoningOutputTokens: 2}
		if got.OmittedCalls != want {
			t.Fatalf("omitted = %+v, want %+v", got.OmittedCalls, want)
		}
	})
	t.Run("a late turn reads only its own calls", func(t *testing.T) {
		t.Parallel()
		// Thousands of earlier calls fill the journal well past one seek
		// window, so the turn's facts are located by record, not scanned.
		const turns = 3_000
		records := make([]timedRecord, 0, turns*3)
		for i := range turns {
			ms := int64(1_000 + i*1_000)
			records = append(records, at(ms, prompt()), at(ms+100, &agent.UsageMessage{PerCall: true, ReportedModel: "m", Usage: agent.Usage{InputTokens: i + 1}}), at(ms+200, result()))
		}
		source := timelineFixture(t, records...)
		if source.journal.end < 4*seekScanBytes {
			t.Fatalf("journal = %d bytes, too short to seek", source.journal.end)
		}
		// seek lands on a line start at most one scan window before the
		// record's first fact, never after it.
		if err := source.journal.writer.Flush(); err != nil {
			t.Fatal(err)
		}
		journal := make([]byte, source.journal.end)
		if _, err := source.journal.file.ReadAt(journal, 0); err != nil {
			t.Fatal(err)
		}
		var starts []int64
		var ids []uint64
		for offset := int64(0); offset < int64(len(journal)); {
			line := journal[offset:]
			line = line[:bytes.IndexByte(line, '\n')]
			var fact data.Fact
			if err := json.Unmarshal(line, &fact); err != nil {
				t.Fatal(err)
			}
			starts, ids = append(starts, offset), append(ids, fact.RecordID)
			offset += int64(len(line)) + 1
		}
		for first := uint64(1); first <= ids[len(ids)-1]; first++ {
			got, err := source.journal.seek(source.journal.end, first)
			if err != nil {
				t.Fatal(err)
			}
			i, _ := slices.BinarySearch(ids, first)
			target := starts[i]
			if _, ok := slices.BinarySearch(starts, got); !ok || got > target || target-got > seekScanBytes {
				t.Fatalf("seek(%d) = %d, want a line start within %d bytes before %d", first, got, seekScanBytes, target)
			}
		}
		for _, n := range []int{1, 2_000, turns} {
			got, err := source.TurnTimeline(t.Context(), n, time.Time{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Calls) != 1 || got.Calls[0].InputTokens != n || got.Calls[0].AtMS != 100 {
				t.Fatalf("turn %d calls = %+v", n, got.Calls)
			}
		}
	})
}
