// Tests, benchmarks, and fixtures for task timelines, delivery evidence, state, accounting, and pending-action recovery.

package taskrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"

	"github.com/caic-xyz/caic/backend/internal/tokens"
	"github.com/klauspost/compress/zstd"

	"github.com/maruel/genai"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/gittest"
	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	quotausage "github.com/caic-xyz/caic/backend/internal/usage"
)

type taskInputWriter func([]byte) (int, error)

func (w taskInputWriter) Write(p []byte) (int, error) { return w(p) }
func (taskInputWriter) Close() error                  { return nil }

// mustTaskRun installs the same configured recording owner used before startup.
func mustTaskRun(t *testing.T, tk *Task) (*AgentRuntime, *Run) {
	bs := backends.Default("", nil)
	r := &AgentRuntime{Backends: bs, LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), bs), LogPath: &taskslog.Path{}, Log: testLogger(), NotifyTaskChange: func() {}}
	run, err := r.beginRun(t.Context(), tk, false, false)
	if err != nil {
		t.Fatal(err)
	}
	close(run.startupDone)
	t.Cleanup(func() {
		if err := r.closeRun(tk, run); err != nil {
			t.Error(err)
		}
	})
	return r, run
}

// taskNative commits one provider response through the real ordered reader.
func taskNative(ctx context.Context, records *agent.Records, native string) error {
	line := []byte(`{"t":"agent","ts":1.000,"msg":` + native + "}\n")
	_, _, err := agent.NewRelayRecordReader(ctx, bytes.NewReader(line), records).ReadRecord()
	return err
}

// addTaskMessage supplies a zero-time semantic fixture through the accepted-message fold.
func addTaskMessage(t *Task, m agent.Message) {
	t.addParsedMessage(agent.TimedMessage{Message: m})
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type titleSequenceProvider struct {
	genai.Provider

	replies  []string
	requests []genai.Messages
}

func (p *titleSequenceProvider) GenSync(_ context.Context, msgs genai.Messages, _ ...genai.GenOption) (genai.Result, error) {
	i := len(p.requests)
	p.requests = append(p.requests, slices.Clone(msgs))
	if i >= len(p.replies) {
		return genai.Result{}, errors.New("unexpected title request")
	}
	return genai.Result{Replies: []genai.Reply{{Text: p.replies[i]}}}, nil
}

type titleBenchmarkProvider struct {
	genai.Provider

	inputBytes int
}

func (p *titleBenchmarkProvider) GenSync(_ context.Context, msgs genai.Messages, _ ...genai.GenOption) (genai.Result, error) {
	p.inputBytes = len(msgs[0].String())
	return genai.Result{Replies: []genai.Reply{{Text: "Task title"}}}, nil
}

type titleBlockingProvider struct {
	genai.Provider

	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (p *titleBlockingProvider) GenSync(context.Context, genai.Messages, ...genai.GenOption) (genai.Result, error) {
	text := "Newer title"
	if p.calls.Add(1) == 1 {
		close(p.started)
		<-p.release
		text = "Stale title"
	}
	return genai.Result{Replies: []genai.Reply{{Text: text}}}, nil
}

func BenchmarkTask(b *testing.B) {
	b.Run("GenerateTitle", func(b *testing.B) {
		for _, turns := range []int{10, 1000} {
			b.Run(strconv.Itoa(turns), func(b *testing.B) {
				tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "Initial prompt"}, "", "", "")
				history := make([]agent.Message, 0, 2*turns)
				for range turns {
					history = append(history, &agent.UserInputMessage{Text: "Recent prompt"}, &agent.ResultMessage{Result: strings.Repeat("result ", 300)})
				}
				tk.SeedTimelineParts(nil, agenttest.TimedMessages(history), nil)
				p := &titleBenchmarkProvider{}
				tk.Provider = p
				log := testLogger()
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					tk.GenerateTitle(b.Context(), log, func() {})
				}
				b.ReportMetric(float64(p.inputBytes), "input-bytes")
			})
		}
	})
}

func TestTask(t *testing.T) {
	t.Run("CodexZeroDeltaReplacesBilledFallback", func(t *testing.T) {
		t.Parallel()
		parser, err := backends.Default("", nil).ResolveParser(harness.Codex)
		if err != nil {
			t.Fatal(err)
		}
		started, err := parser.ParseOutput([]byte(`{"method":"thread/started","params":{"thread":{"id":"root"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		const usage = `{"method":"thread/tokenUsage/updated","params":{"threadId":"root","tokenUsage":{"total":{"totalTokens":100,"inputTokens":80,"outputTokens":20},"last":{"totalTokens":100,"inputTokens":80,"outputTokens":20}}}}`
		if _, err := parser.ParseOutput([]byte(usage)); err != nil {
			t.Fatal(err)
		}
		repeated, err := parser.ParseOutput([]byte(usage))
		if err != nil {
			t.Fatal(err)
		}
		if len(repeated) != 1 {
			t.Fatalf("repeated total messages = %#v", repeated)
		}
		zero, ok := repeated[0].(*agent.UsageMessage)
		if !ok || !zero.Usage.Equal(agent.Usage{}) || zero.PerCall || zero.ContextUsed != nil || zero.Source.IsChild() {
			t.Fatalf("repeated Codex total = %#v", repeated[0])
		}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "", "")
		at := time.Unix(100, 0)
		history := agenttest.TimedMessages(started)
		// A retained cache snapshot must also clear when the native delta is zero.
		history = append(history,
			agent.TimedMessage{ProducerTime: at, Message: &agent.UsageMessage{Usage: agent.Usage{InputTokens: 80, CacheTTLSeconds: 300}}},
			agent.TimedMessage{ProducerTime: at.Add(time.Second), Message: zero},
		)
		for _, message := range history {
			tk.addParsedMessage(message)
		}
		snap := tk.Snapshot()
		if !snap.LastAPIUsage.Equal(agent.Usage{}) || !snap.CacheExpiresAt.IsZero() || snap.ContextUsed != nil {
			t.Fatalf("zero-delta context=%v api=%+v expires=%s", snap.ContextUsed, snap.LastAPIUsage, snap.CacheExpiresAt)
		}
	})
	t.Run("ContextBoundariesPreserveBilledFallback", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			harness  harness.Name
			boundary agent.Message
			input    int
		}{
			{"codex_compact_without_size", harness.Codex, &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, 123},
			{"claude_clear", harness.Claude, agent.ContextCleared(), 123},
			{"claude_new_session", harness.Claude, &agent.InitMessage{SessionID: "next"}, 123},
			{"codex_compact_with_size", harness.Codex, &agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary, ContextTokensAfter: 12_000}, 12_000},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				used := 45_000
				at := time.Unix(100, 0)
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, tc.harness, "", "")
				messages := []agent.TimedMessage{
					{ProducerTime: at, Message: &agent.InitMessage{SessionID: "root"}},
					{ProducerTime: at, Message: &agent.UsageMessage{ContextUsed: &used, Usage: agent.Usage{InputTokens: 123, CacheTTLSeconds: 300}}},
					{ProducerTime: at, Message: tc.boundary},
				}
				for _, message := range messages {
					tk.addParsedMessage(message)
				}
				snap := tk.Snapshot()
				if snap.ContextUsed != nil || snap.LastAPIUsage.InputTokens != tc.input || !snap.CacheExpiresAt.Equal(at.Add(300*time.Second)) {
					t.Fatalf("context=%v api=%+v expires=%s", snap.ContextUsed, snap.LastAPIUsage, snap.CacheExpiresAt)
				}
			})
		}
	})
	t.Run("CompactionFollowedByUsageUpdatesMeter", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
		messages := []agent.Message{
			&agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary, ContextTokensAfter: 12_000},
			&agent.UsageMessage{Usage: agent.Usage{InputTokens: 15_000}},
		}
		for _, message := range messages {
			addTaskMessage(tk, message)
		}
		snap := tk.Snapshot()
		if snap.ContextUsed != nil || snap.LastAPIUsage.InputTokens != 15_000 {
			t.Fatalf("context=%v api=%+v", snap.ContextUsed, snap.LastAPIUsage)
		}
	})
	t.Run("NativeContextOccupancy", func(t *testing.T) {
		t.Parallel()
		for _, mode := range []string{"live", "restored"} {
			t.Run(mode, func(t *testing.T) {
				t.Parallel()
				used, zero, childUsed := 45_000, 0, 999
				steps := []struct {
					message agent.Message
					used    *int
					input   int
				}{
					{&agent.InitMessage{SessionID: "root"}, nil, 0},
					{&agent.UsageMessage{Usage: agent.Usage{InputTokens: 100}, Additive: true}, nil, 100},
					{&agent.UsageMessage{ContextUsed: &used, ContextWindow: 200_000}, &used, 100},
					{&agent.UsageMessage{Source: v3.MessageSource{AgentID: "child"}, ContextUsed: &childUsed, Usage: agent.Usage{InputTokens: 999}}, &used, 100},
					{&agent.UsageMessage{ContextUsed: &zero}, &zero, 100},
					{&agent.UsageMessage{ContextWindow: 200_000}, &zero, 0},
					{&agent.InitMessage{SessionID: "root"}, &zero, 0},
					{&agent.ResultMessage{Usage: agent.Usage{InputTokens: 100}}, &zero, 0},
					{&agent.InitMessage{SessionID: "next"}, nil, 0},
					{&agent.UsageMessage{ContextUsed: &used}, &used, 0},
					{&agent.SystemMessage{Subtype: agent.SystemSubtypeCompactBoundary}, nil, 0},
					{&agent.UsageMessage{ContextUsed: &used}, &used, 0},
					{agent.ContextCleared(), nil, 0},
					{&agent.UsageMessage{ContextUsed: &used, Usage: agent.Usage{InputTokens: 321}}, &used, 321},
					{&agent.UsageMessage{ContextUsed: &zero, PerCall: true}, &zero, 0},
				}
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Dsh, "", "")
				history := make([]agent.Message, 0, len(steps))
				for i, step := range steps {
					history = append(history, step.message)
					tk := live
					if mode == "restored" {
						tk = mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Dsh, "", "")
						tk.SeedTimelineParts(nil, agenttest.TimedMessages(history), nil)
					} else {
						addTaskMessage(tk, step.message)
					}
					snap := tk.Snapshot()
					if !reflect.DeepEqual(snap.ContextUsed, step.used) || snap.LastAPIUsage.InputTokens != step.input {
						t.Fatalf("step %d: occupancy=%v api=%+v; want %v/%d", i, snap.ContextUsed, snap.LastAPIUsage, step.used, step.input)
					}
					if i == 5 && snap.Usage.InputTokens != 100 {
						t.Fatalf("occupancy changed billed usage: %+v", snap.Usage)
					}
				}
			})
		}
	})
	t.Run("ExitAfterInputAttempt", func(t *testing.T) {
		t.Parallel()
		for _, outcome := range []string{"unresolved", "failed", "completed", "partial_output", "result_before_outcome", "historical"} {
			t.Run(outcome, func(t *testing.T) {
				t.Parallel()
				history := []agent.Message{
					&agent.UserInputMessage{Text: "accepted"},
					&agent.ResultMessage{Result: "done"},
				}
				if outcome != "historical" {
					history = append(history, agent.InputDeliveryStarted())
				}
				history = append(history, &agent.UserInputMessage{Text: "attempt"})
				switch outcome {
				case "failed":
					history = append(history, agent.InputDeliveryFailed(io.ErrClosedPipe))
				case "completed":
					history = append(history, agent.InputDeliveryCompleted())
				case "partial_output":
					history = append(history, &agent.TextDeltaMessage{Text: "working"}, agent.InputDeliveryFailed(io.ErrClosedPipe))
				case "result_before_outcome":
					history = append(history, &agent.ResultMessage{Result: "done again"}, agent.InputDeliveryCompleted())
				}
				want := ""
				if outcome == "completed" || outcome == "partial_output" || outcome == "historical" {
					want = "termination artifact"
				}
				history = append(history, &agent.ExitMessage{ExitCode: 1, Error: "termination artifact"})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				for _, m := range history {
					addTaskMessage(tk, m)
				}
				if got := tk.LastExitError(); got != want {
					t.Fatalf("exit = %q, want %q", got, want)
				}
			})
		}
	})
	t.Run("HasAcceptedInputEvidence", func(t *testing.T) {
		t.Parallel()
		input := &agent.UserInputMessage{Text: "attempt"}
		for _, tc := range []struct {
			name     string
			messages []agent.Message
			accepted bool
		}{
			{name: "historical", messages: []agent.Message{input}, accepted: true},
			{name: "started_only", messages: []agent.Message{agent.InputDeliveryStarted()}},
			{name: "pending_without_outcome", messages: []agent.Message{agent.InputDeliveryStarted(), input}},
			{name: "completed", messages: []agent.Message{agent.InputDeliveryStarted(), input, agent.InputDeliveryCompleted()}, accepted: true},
			{name: "failed", messages: []agent.Message{agent.InputDeliveryStarted(), input, agent.InputDeliveryFailed(io.ErrClosedPipe)}},
			{name: "root_output", messages: []agent.Message{agent.InputDeliveryStarted(), input, &agent.TextMessage{Text: "working"}}, accepted: true},
			{name: "child_output", messages: []agent.Message{agent.InputDeliveryStarted(), input, &agent.TextMessage{Source: v3.MessageSource{AgentID: "child"}, Text: "working"}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				{
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
					parser, err := agent.NewLogRecordParser(&agenttest.Wire{Output: func([]byte) ([]agent.Message, error) { return nil, nil }})
					if err != nil {
						t.Fatal(err)
					}
					var restored []agent.Message
					for _, m := range tc.messages {
						data, err := agent.MarshalLogMessage(m)
						if err != nil {
							t.Fatal(err)
						}
						record, err := parser.ParseRecord(data)
						if err != nil {
							t.Fatal(err)
						}
						for _, message := range record.Messages {
							restored = append(restored, message.Message)
						}
					}
					tk.SeedTimelineParts(nil, agenttest.TimedMessages(restored), nil)
					accepted := tc.accepted
					if got := tk.HasAcceptedInputEvidence(); got != accepted {
						t.Fatalf("evidence = %t, want %t", got, accepted)
					}
				}
			})
		}
	})
	t.Run("PendingInputPreservesCompletedTurnSemantics", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateRunning)
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
			&agent.UserInputMessage{Text: "accepted"},
			&agent.ResultMessage{Result: "done"},
			agent.InputDeliveryStarted(),
			&agent.UserInputMessage{Text: "unresolved"},
		}), nil)
		if got := tk.GetState(); got != task.StateWaiting {
			t.Fatalf("state = %s, want previous completed turn to remain waiting", got)
		}
		if got := tk.titleInput(); got != "Current title: test\nRecent activity:\nPrompt: accepted\nResult: done\n" {
			t.Fatalf("title input = %q", got)
		}
	})
	t.Run("PlanSummaryChanges", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
			&agent.ToolUseMessage{ToolUseID: "write", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"first"}`)},
			&agent.ToolUseMessage{ToolUseID: "exit", Name: "ExitPlanMode"},
		}), nil)
		setState(tk, task.StateRunning)
		if changed := tk.addParsedMessage(agent.TimedMessage{Message: &agent.ToolUseMessage{ToolUseID: "edit", Name: "Edit", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","old_string":"first","new_string":"revised"}`)}}); !changed {
			t.Fatal("visible plan edit did not notify task summary subscribers")
		}
		if got := tk.Snapshot().PlanContent; got != "revised" {
			t.Fatalf("edited plan = %q", got)
		}
		if changed := tk.addParsedMessage(agent.TimedMessage{Message: agent.ContextCleared()}); !changed {
			t.Fatal("context clear did not notify task summary subscribers")
		}
		if got := tk.Snapshot().PlanContent; got != "" {
			t.Fatalf("cleared plan = %q", got)
		}
	})
	t.Run("TitleRefreshDue", func(t *testing.T) {
		t.Parallel()
		want := []int{1, 2, 5, 10, 20, 50, 100, 150}
		for _, restored := range []int{0, 4, 49, 99} {
			t.Run(strconv.Itoa(restored), func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				var history []agent.Message
				for range restored {
					history = append(history, &agent.ResultMessage{}, &agent.ResultMessage{Source: v3.MessageSource{AgentID: "child"}})
				}
				tk.SeedTimelineParts(nil, agenttest.TimedMessages(history), nil)
				for n := restored + 1; n <= 151; n++ {
					tk.addParsedMessage(agent.TimedMessage{Message: &agent.ResultMessage{Source: v3.MessageSource{AgentID: "child"}}})
					tk.addParsedMessage(agent.TimedMessage{Message: &agent.ResultMessage{NumTurns: 99}})
					if got := tk.titleRefreshDue(); got != slices.Contains(want, n) {
						t.Fatalf("refresh at turn %d = %v", n, got)
					}
					if n == 20 {
						addTaskMessage(tk, agent.ContextCleared())
					}
				}
			})
		}
	})
	t.Run("GenerateTitle", func(t *testing.T) {
		t.Parallel()
		t.Run("ConcurrentRequests", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			p := &titleBlockingProvider{started: make(chan struct{}), release: make(chan struct{})}
			tk.Provider = p
			var work sync.WaitGroup
			work.Go(func() {
				tk.GenerateTitle(t.Context(), testLogger(), func() {})
			})
			<-p.started
			tk.GenerateTitle(t.Context(), testLogger(), func() {})
			close(p.release)
			work.Wait()
			if got := tk.Title(); got != "Newer title" {
				t.Fatalf("Title = %q, want Newer title", got)
			}
		})
		t.Run("RecentTurns", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "obsolete original subject"}, "", "", "")
			var history []agent.Message
			for n := 1; n <= 12; n++ {
				history = append(history,
					&agent.UserInputMessage{Text: fmt.Sprintf("prompt-%02d", n)},
					&agent.TextMessage{Text: fmt.Sprintf("answer-%02d", n)},
					&agent.ResultMessage{Source: v3.MessageSource{AgentID: "child"}, Result: "child activity"},
					&agent.ResultMessage{},
				)
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(history), nil)
			tk.SetTitle("Current title")
			p := &titleSequenceProvider{replies: []string{"Recent subject"}}
			tk.Provider = p
			tk.GenerateTitle(t.Context(), testLogger(), func() {})
			input := p.requests[0][0].String()
			for _, obsolete := range []string{"obsolete original subject", "prompt-01", "prompt-02", "answer-01", "answer-02", "child activity"} {
				if strings.Contains(input, obsolete) {
					t.Errorf("input contains excluded activity %q", obsolete)
				}
			}
			for n := 3; n <= 12; n++ {
				for _, text := range []string{fmt.Sprintf("prompt-%02d", n), fmt.Sprintf("answer-%02d", n)} {
					if !strings.Contains(input, text) {
						t.Errorf("input missing recent activity %q", text)
					}
				}
			}
		})
		t.Run("BoundedUnicodeInput", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			text := strings.Repeat("\u754c", 50000)
			history := make([]agent.Message, 0, 20)
			for range 10 {
				history = append(history, &agent.UserInputMessage{Text: text}, &agent.ResultMessage{Result: text})
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(history), nil)
			p := &titleSequenceProvider{replies: []string{"Short title"}}
			tk.Provider = p
			tk.GenerateTitle(t.Context(), testLogger(), func() {})
			input := p.requests[0][0].String()
			if len(input) > 50000 || !utf8.ValidString(input) {
				t.Fatalf("input bytes = %d, valid UTF-8 = %v", len(input), utf8.ValidString(input))
			}
		})
		t.Run("ProviderError", func(t *testing.T) {
			t.Parallel()
			for _, replies := range [][]string{nil, {strings.Repeat("x", 81)}, {strings.Repeat("x", 81), strings.Repeat("y", 81)}} {
				t.Run(strconv.Itoa(len(replies)), func(t *testing.T) {
					t.Parallel()
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
					tk.SetTitle("Previous title")
					p := &titleSequenceProvider{replies: replies}
					tk.Provider = p
					tk.GenerateTitle(t.Context(), testLogger(), func() {})
					if got := tk.Title(); got != "Previous title" {
						t.Fatalf("Title = %q after provider error", got)
					}
					if len(p.requests) != len(replies)+1 {
						t.Fatalf("requests = %d, want %d", len(p.requests), len(replies)+1)
					}
				})
			}
		})
		for _, tc := range []struct {
			name    string
			replies []string
			want    string
		}{
			{"short", []string{"  \"Fix task titles\"  "}, "Fix task titles"},
			{"at_limit", []string{strings.Repeat("\u754c", 80)}, strings.Repeat("\u754c", 80)},
			{"shortened", []string{strings.Repeat("x", 81), "Shorter title"}, "Shorter title"},
			{"second_retry_shortens", []string{strings.Repeat("x", 81), strings.Repeat("y", 81), "Short enough"}, "Short enough"},
			{"retries_still_long", []string{strings.Repeat("x", 81), strings.Repeat("y", 81), strings.Repeat("\u754c", 81)}, strings.Repeat("\u754c", 79) + "…"},
			{"empty", []string{"  "}, "test"},
			{"retry_empty", []string{strings.Repeat("x", 81), "  "}, "test"},
			{"second_retry_empty", []string{strings.Repeat("x", 81), strings.Repeat("y", 81), "  "}, "test"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				p := &titleSequenceProvider{replies: tc.replies}
				tk.Provider = p
				tk.GenerateTitle(t.Context(), testLogger(), func() {})
				if got := tk.Title(); got != tc.want || !utf8.ValidString(got) {
					t.Fatalf("Title = %q, want %q", got, tc.want)
				}
				if len(p.requests) != len(tc.replies) {
					t.Fatalf("requests = %d, want %d", len(p.requests), len(tc.replies))
				}
				for i := 1; i < len(p.requests); i++ {
					msgs := p.requests[i]
					if len(msgs) != 2*i+1 || msgs[2*i-1].Role() != "assistant" || msgs[2*i-1].String() != tc.replies[i-1] || !strings.Contains(msgs[2*i].String(), "80") {
						t.Fatalf("shortening request did not continue the title conversation: %v", msgs)
					}
				}
			})
		}
	})
	t.Run("ChildObservedProviderQuotaRemainsShared", func(t *testing.T) {
		t.Parallel()
		for _, live := range []bool{false, true} {
			t.Run(strconv.FormatBool(live), func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "model", "")
				sink := &rollupSpy{}
				tk.Rollup = sink
				quota := &agent.RateLimitMessage{Source: v3.MessageSource{AgentID: "child"}, Status: agent.RateLimitStatusRejected, RateLimitType: "five_hour", ResetsAt: time.Now().Add(time.Hour), QuotaLabel: "shared account", QuotaProvider: agent.QuotaProviderCodex}
				if live {
					addTaskMessage(tk, quota)
				} else {
					tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{quota}), nil)
				}
				history, _ := tk.RateLimitsSince(0)
				if len(history) != 1 || tk.rateLimit.Status != agent.RateLimitStatusRejected || len(sink.Quotas) != 1 {
					t.Fatalf("child-origin account quota discarded: history=%v quota=%+v sink=%v", history, tk.rateLimit, sink.Quotas)
				}
				if prompt := BuildHandoffPrompt(tk, 0); !strings.Contains(prompt, "shared account") {
					t.Fatalf("provider block lost from handoff: %s", prompt)
				}
			})
		}
	})
	t.Run("ChildOwnership", func(t *testing.T) {
		t.Parallel()
		parent := v3.MessageSource{ThreadID: "root", TurnID: "turn", RootThreadID: "root"}
		child := v3.MessageSource{ThreadID: "child", TurnID: "child-turn", RootThreadID: "root", AgentID: "child"}
		for _, live := range []bool{false, true} {
			t.Run(strconv.FormatBool(live), func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "root prompt"}, harness.Codex, "root-model", "")
				sink := &rollupSpy{}
				tk.Rollup = sink
				messages := []agent.Message{
					&agent.InitMessage{Source: parent, SessionID: "root", ReportedModel: "root-model"},
					&agent.UsageMessage{Source: parent, Usage: agent.Usage{InputTokens: 12}, ContextWindow: 100, ReportedModel: "root-model", ModelDerived: true},
					&agent.TextMessage{Source: parent, Text: "root answer"},
					&agent.ResultMessage{Source: parent, Usage: agent.Usage{InputTokens: 30}},
					&agent.UserInputMessage{Source: child, Text: "child prompt"},
					&agent.InitMessage{Source: child, SessionID: "child", ReportedModel: "child-model"},
					&agent.TextMessage{Source: child, Text: "child answer"},
					&agent.ToolUseMessage{Source: child, Name: "EnterPlanMode", ToolUseID: "local"},
					&agent.UsageMessage{Source: child, Usage: agent.Usage{InputTokens: 999}, ContextWindow: 999, ReportedModel: "child-model", ModelDerived: true},
					&agent.SystemMessage{Source: child, Subtype: agent.SystemSubtypeCompactBoundary, ContextTokensAfter: 999},
					&agent.SystemMessage{Source: child, Subtype: agent.SystemSubtypeModelRerouted, ReportedModel: "child-model"},
					&agent.AskMessage{Source: child, ToolUseID: "ask"},
					&agent.RateLimitMessage{Source: child, Status: agent.RateLimitStatusRejected, QuotaLabel: "child-only-quota", RateLimitType: "thread_goal"},
					&agent.ResultMessage{Source: child, IsError: true, Result: "child error", Usage: agent.Usage{InputTokens: 999}},
				}
				if live {
					setState(tk, task.StateRunning)
					for _, message := range messages {
						addTaskMessage(tk, message)
					}
				} else {
					tk.Restore(task.StateStarting, time.Time{})
					tk.SeedTimelineParts(nil, agenttest.TimedMessages(messages), nil)
					tk.GoLive()
				}
				if tk.GetState() != task.StateWaiting || tk.sessionMetadata.SessionID != "root" || tk.sessionMetadata.ReportedModel != "root-model" || tk.costTracker.Model != "root-model" || tk.inPlanMode {
					t.Fatalf("child changed root state: state=%s session=%s model=%s costmodel=%s plan=%t", tk.GetState(), tk.sessionMetadata.SessionID, tk.sessionMetadata.ReportedModel, tk.costTracker.Model, tk.inPlanMode)
				}
				if tk.lastAPIUsage.InputTokens != 12 || tk.reportedContextWindow != 100 || tk.liveUsage.InputTokens != 30 {
					t.Fatalf("root context/totals changed: latest=%+v window=%d totals=%+v", tk.lastAPIUsage, tk.reportedContextWindow, tk.liveUsage)
				}
				if got := tk.LastAgentResult(); got != "root answer" {
					t.Errorf("last result = %q", got)
				}
				history, _ := tk.RateLimitsSince(0)
				if len(sink.Quotas) != 0 {
					t.Fatalf("child goal affected provider quota: %+v", sink.Quotas)
				}
				if len(history) != 0 {
					t.Fatalf("child thread quota entered root rate limits: %+v", history)
				}
				prompt := BuildHandoffPrompt(tk, 0)
				if strings.Contains(prompt, "child answer") || strings.Contains(prompt, "child prompt") || strings.Contains(prompt, "child error") || strings.Contains(prompt, "child-only-quota") {
					t.Errorf("child leaked into handoff: %s", prompt)
				}
			})
		}
	})
	t.Run("ToolCompactionOwnerScope", func(t *testing.T) {
		t.Parallel()
		parent := v3.MessageSource{ThreadID: "root", TurnID: "turn", RootThreadID: "root"}
		child := v3.MessageSource{ThreadID: "child", TurnID: "turn", RootThreadID: "root"}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "", "")
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
			&agent.ToolOutputDeltaMessage{Source: parent, ToolUseID: "same", Delta: "parent output"},
			&agent.ToolResultMessage{Source: child, ToolUseID: "same"},
			&agent.ToolOutputDeltaMessage{Source: child, ToolUseID: "same", Delta: "child output"},
			&agent.ToolResultMessage{Source: child, ToolUseID: "same"},
		}), nil)
		if got, ok := tk.timeline[0].Message.(*agent.ToolOutputDeltaMessage); !ok || got.Delta != "parent output" {
			t.Fatal("child completion erased parent output")
		}
		if got, ok := tk.timeline[2].Message.(*agent.ToolOutputDeltaMessage); !ok || got.Delta != "" || got.Source != child {
			t.Fatalf("compacted delta lost source: %+v", got)
		}
	})
	t.Parallel()
	t.Run("RootTaskHasNoParent", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		if tk.ParentTaskID != 0 {
			t.Errorf("ParentTaskID = %s, want root task", tk.ParentTaskID)
		}
		if got := tk.LogHeader().ParentTaskID; got != "" {
			t.Errorf("LogHeader().ParentTaskID = %q, want empty", got)
		}
	})
	t.Run("ChildTaskPersistsParent", func(t *testing.T) {
		t.Parallel()
		parentID := ksid.NewID()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.ParentTaskID = parentID
		tk.OwnerID = "user-1"
		tk.CaicMCP = true
		if got := tk.LogHeader().ParentTaskID; got != parentID.String() {
			t.Errorf("LogHeader().ParentTaskID = %q, want %q", got, parentID)
		}
	})
	t.Run("UnknownCacheTTLDoesNotCreateExpiry", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.addParsedMessage(agent.TimedMessage{Message: &agent.UsageMessage{
			Usage: agent.Usage{CacheReadInputTokens: 100, CacheTTLSeconds: 300},
		}})
		if got := tk.Snapshot().CacheExpiresAt; got.IsZero() {
			t.Fatal("CacheExpiresAt is unknown after an explicitly reported TTL")
		}
		tk.addParsedMessage(agent.TimedMessage{Message: &agent.UsageMessage{
			Usage: agent.Usage{CacheReadInputTokens: 100},
		}})
		if got := tk.Snapshot().CacheExpiresAt; !got.IsZero() {
			t.Errorf("CacheExpiresAt = %s, want unknown", got)
		}
	})
	t.Run("CacheExpiryUsesRecordedTime", func(t *testing.T) {
		t.Parallel()
		// Replaying a log must not extend a cache whose TTL already elapsed.
		usageAt := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
		want := usageAt.Add(5 * time.Minute)
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.addParsedMessage(agent.TimedMessage{ProducerTime: usageAt, Message: &agent.UsageMessage{
			Usage: agent.Usage{CacheReadInputTokens: 100, CacheTTLSeconds: 300},
		}})
		if got := tk.Snapshot().CacheExpiresAt; !got.Equal(want) {
			t.Errorf("CacheExpiresAt = %s, want %s", got, want)
		}
	})
	t.Run("ResultContextWindowUpdatesSnapshot", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.addParsedMessage(agent.TimedMessage{Message: &agent.ResultMessage{Result: "done", ContextWindow: 200_000}})
		if got := tk.Snapshot().ContextWindowLimit; got != 200_000 {
			t.Fatalf("ContextWindowLimit = %d, want 200000", got)
		}
		// A later result that reports no window must not erase the known one.
		tk.addParsedMessage(agent.TimedMessage{Message: &agent.ResultMessage{Result: "again"}})
		if got := tk.Snapshot().ContextWindowLimit; got != 200_000 {
			t.Fatalf("ContextWindowLimit after unknown result = %d, want 200000", got)
		}
	})
	t.Run("PartialMetadataNotifiesWithoutStartingTurn", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.OpenCode, "", "")
		setState(tk, task.StateWaiting)
		tk.addParsedMessage(agent.TimedMessage{Message: &agent.ExitMessage{ExitCode: 1, Error: "failed"}})
		before := tk.Snapshot()
		message := &agent.SessionMetadataMessage{ReportedModel: "native", ReportedEffort: "high", AgentVersion: "1"}
		if changed := tk.addParsedMessage(agent.TimedMessage{Message: message}); !changed {
			t.Fatal("metadata change did not notify task observers")
		}
		after := tk.Snapshot()
		if after.SessionMetadata != message.SessionMetadata || after.State != before.State || after.TurnStartedAt != before.TurnStartedAt || after.NumTurns != before.NumTurns || tk.LastExitError() == "" {
			t.Fatalf("settings changed activity: before=%+v after=%+v exit=%q", before, after, tk.LastExitError())
		}
		if changed := tk.addParsedMessage(agent.TimedMessage{Message: message}); changed {
			t.Fatal("unchanged metadata notified observers")
		}
	})
	t.Run("SessionMetadataLiveAndPersistedReplay", func(t *testing.T) {
		t.Parallel()
		child := v3.MessageProvenance{Source: v3.MessageSource{DelegationToolUseID: "child"}}
		steps := []struct {
			message agent.Message
			native  string
			want    agent.SessionMetadata
		}{
			{message: &agent.MetaSessionMessage{SessionID: "first", ReportedModel: "initial", ReportedEffort: "low", AgentVersion: "1", AutoCompactTokenLimit: new(100)},
				want: agent.SessionMetadata{SessionID: "first", ReportedModel: "initial", ReportedEffort: "low", AgentVersion: "1", AutoCompactTokenLimit: 100}},
			{message: &agent.InitMessage{SessionID: "native", ReportedModel: "active", ReportedEffort: "high", Version: "2"},
				native: `{"type":"system","subtype":"init","session_id":"native","model":"active","effort":"high","claude_code_version":"2"}`,
				want:   agent.SessionMetadata{SessionID: "native", ReportedModel: "active", ReportedEffort: "high", AgentVersion: "2", AutoCompactTokenLimit: 100}},
			{message: &agent.MetaSessionMessage{SessionID: "restored", ReportedModel: "stale", ReportedEffort: "low", AgentVersion: "3", AutoCompactTokenLimit: new(300)},
				want: agent.SessionMetadata{SessionID: "restored", ReportedModel: "active", ReportedEffort: "high", AgentVersion: "3", AutoCompactTokenLimit: 300}},
			{message: &agent.InitMessage{Version: "4"},
				native: `{"type":"system","subtype":"init","claude_code_version":"4"}`,
				want:   agent.SessionMetadata{SessionID: "restored", ReportedModel: "active", ReportedEffort: "high", AgentVersion: "4", AutoCompactTokenLimit: 300}},
			{message: &agent.InitMessage{MessageProvenance: child, SessionID: "child", ReportedModel: "child-model", ReportedEffort: "max", Version: "child-version"},
				native: `{"type":"system","subtype":"init","parent_tool_use_id":"child","session_id":"child","model":"child-model","effort":"max","claude_code_version":"child-version"}`,
				want:   agent.SessionMetadata{SessionID: "restored", ReportedModel: "active", ReportedEffort: "high", AgentVersion: "4", AutoCompactTokenLimit: 300}},
			{message: &agent.MetaSessionMessage{MessageProvenance: child, SessionID: "child", ReportedModel: "child-model", ReportedEffort: "max", AgentVersion: "child-version", AutoCompactTokenLimit: new(999)},
				want: agent.SessionMetadata{SessionID: "restored", ReportedModel: "active", ReportedEffort: "high", AgentVersion: "4", AutoCompactTokenLimit: 300}},
			{message: &agent.UsageMessage{ReportedModel: "routed", Usage: agent.Usage{InputTokens: 1, OutputTokens: 1}},
				native: `{"type":"assistant","message":{"model":"routed","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
				want:   agent.SessionMetadata{SessionID: "restored", ReportedModel: "routed", ReportedEffort: "high", AgentVersion: "4", AutoCompactTokenLimit: 300}},
			{message: &agent.UsageMessage{MessageProvenance: child, ReportedModel: "child-model", Usage: agent.Usage{InputTokens: 1}},
				native: `{"type":"assistant","parent_tool_use_id":"child","message":{"model":"child-model","content":[],"usage":{"input_tokens":1}}}`,
				want:   agent.SessionMetadata{SessionID: "restored", ReportedModel: "routed", ReportedEffort: "high", AgentVersion: "4", AutoCompactTokenLimit: 300}},
			{message: agent.ContextCleared(),
				want: agent.SessionMetadata{ReportedModel: "routed", ReportedEffort: "high", AgentVersion: "4", AutoCompactTokenLimit: 300}},
			{message: &agent.MetaSessionMessage{SessionID: "next", ReportedModel: "stale", ReportedEffort: "low", AgentVersion: "5", AutoCompactTokenLimit: new(400)},
				want: agent.SessionMetadata{SessionID: "next", ReportedModel: "routed", ReportedEffort: "high", AgentVersion: "5", AutoCompactTokenLimit: 400}},
			{message: &agent.MetaSessionMessage{AutoCompactTokenLimit: new(0)},
				want: agent.SessionMetadata{SessionID: "next", ReportedModel: "routed", ReportedEffort: "high", AgentVersion: "5"}},
		}
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				store := taskhistory.NewStore(testLogger(), taskslog.NewStore(dir), backends.Default("", nil))
				id := ksid.NewID()
				name := id.String() + "-metadata.jsonl"
				if compressed {
					name += ".zst"
				}
				path := filepath.Join(dir, name)
				live := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "", "")
				encoded := []byte("{\"t\":\"caic_meta\",\"version\":3,\"harness\":\"claude\",\"prompt\":\"test\"}\n")
				for i, step := range steps {
					var record []byte
					var err error
					if step.native != "" {
						record = []byte(`{"t":"agent","ts":1.000,"msg":` + step.native + `}`)
					} else {
						record, err = agent.MarshalLogMessage(step.message)
						if err != nil {
							t.Fatal(err)
						}
					}
					encoded = append(append(encoded, record...), '\n')
					complete := append(bytes.Clone(encoded), []byte("{\"t\":\"result\",\"state\":\"purged\"}\n")...)
					if compressed {
						enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
						if err != nil {
							t.Fatal(err)
						}
						complete = enc.EncodeAll(complete, nil)
						if err := enc.Close(); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(path, complete, 0o600); err != nil {
						t.Fatal(err)
					}
					live.addParsedMessage(agent.TimedMessage{Message: step.message})
					if got := live.Snapshot().SessionMetadata; got != step.want {
						t.Fatalf("step %d live metadata = %+v, want %+v", i, got, step.want)
					}
					for _, cache := range []string{"scan", "cache"} {
						var loaded []*taskhistory.LoadedTask
						if compressed {
							loaded, err = store.LoadSettled()
						} else {
							loaded, err = store.LoadUnsettled()
						}
						if err != nil || len(loaded) != 1 {
							t.Fatalf("step %d %s inventory: %d tasks, %v", i, cache, len(loaded), err)
						}
						lt := loaded[0]
						if lt.SessionMetadata != step.want {
							t.Fatalf("step %d %s inventory metadata = %+v, want %+v", i, cache, lt.SessionMetadata, step.want)
						}
						if err := lt.LoadMessages(); err != nil {
							t.Fatal(err)
						}
						replayed := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "", "")
						replayed.SeedTimelineParts(lt.Timeline, nil, nil)
						if got := replayed.Snapshot().SessionMetadata; got != step.want {
							t.Fatalf("step %d %s persisted replay metadata = %+v, want %+v", i, cache, got, step.want)
						}
					}
				}
			})
		}
	})
	t.Run("BackwardMessages", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		first := &agent.TextMessage{Text: "first"}
		last := &agent.TextMessage{Text: "last"}
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{first, &agent.LogMessage{Line: "skip"}, last}), nil)
		messages := tk.BackwardMessages()
		addTaskMessage(tk, &agent.TextMessage{Text: "after snapshot"})

		var got []agent.Message
		for message := range messages {
			got = append(got, message)
		}
		if len(got) != 3 || got[0] != last || got[2] != first {
			t.Fatalf("BackwardMessages = %#v, want the backward three-message call-time snapshot", got)
		}
		for range messages {
			t.Fatal("second BackwardMessages range yielded a message")
		}
		if tk.timelineReaders != 0 {
			t.Errorf("timeline readers = %d, want released", tk.timelineReaders)
		}
	})
	t.Run("TimelineEntriesIteration", func(t *testing.T) {
		t.Parallel()
		first := agent.TimedMessage{Message: &agent.TextMessage{Text: "first"}}
		second := agent.TimedMessage{Message: &agent.TextMessage{Text: "second"}}
		third := agent.TimedMessage{Message: &agent.TextMessage{Text: "third"}}
		entries := timelineEntries{
			prefix: []agent.TimedMessage{first, second},
			suffix: []agent.TimedMessage{third},
		}

		var forward, backward []int
		var forwardText, backwardText []string
		for i, entry := range entries.Forward() {
			forward = append(forward, i)
			message, ok := entry.Message.(*agent.TextMessage)
			if !ok {
				t.Fatalf("Forward entry %d type = %T, want *agent.TextMessage", i, entry.Message)
			}
			forwardText = append(forwardText, message.Text)
		}
		for i, entry := range entries.Backward() {
			backward = append(backward, i)
			message, ok := entry.Message.(*agent.TextMessage)
			if !ok {
				t.Fatalf("Backward entry %d type = %T, want *agent.TextMessage", i, entry.Message)
			}
			backwardText = append(backwardText, message.Text)
		}
		if !reflect.DeepEqual(forward, []int{0, 1, 2}) {
			t.Errorf("Forward indexes = %v, want [0 1 2]", forward)
		}
		if !reflect.DeepEqual(forwardText, []string{"first", "second", "third"}) {
			t.Errorf("Forward entries = %v, want [first second third]", forwardText)
		}
		if !reflect.DeepEqual(backward, []int{2, 1, 0}) {
			t.Errorf("Backward indexes = %v, want [2 1 0]", backward)
		}
		if !reflect.DeepEqual(backwardText, []string{"third", "second", "first"}) {
			t.Errorf("Backward entries = %v, want [third second first]", backwardText)
		}
	})
	t.Run("SeedTimelineWithTimes", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		message := &agent.TextMessage{Text: "timed"}
		const timestamp = int64(1_788_122_692_466)
		tk.SeedTimelineParts(nil, []agent.TimedMessage{{Message: message, ProducerTime: time.UnixMilli(timestamp)}}, nil)

		if got := tk.timeline; len(got) != 1 || got[0].Message != message || got[0].ProducerTime.UnixMilli() != timestamp {
			t.Fatalf("timeline = %#v, want message observed at %d", got, timestamp)
		}
	})
	t.Run("SeedTimelineCompactsFinalizedDeltas", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		textDelta := &agent.TextDeltaMessage{Text: strings.Repeat("text", 1024)}
		thinkingDelta := &agent.ThinkingDeltaMessage{Text: strings.Repeat("thinking", 1024)}
		toolDelta := &agent.ToolOutputDeltaMessage{ToolUseID: "tool-1", Delta: strings.Repeat("output", 1024)}
		unfinished := &agent.TextDeltaMessage{Text: "still live"}
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
			textDelta,
			&agent.TextMessage{Text: "text complete"},
			thinkingDelta,
			&agent.ThinkingMessage{Text: "thinking complete"},
			toolDelta,
			&agent.ToolResultMessage{ToolUseID: "tool-1"},
			unfinished,
		}), nil)

		if compacted, ok := tk.timeline[0].Message.(*agent.TextDeltaMessage); !ok || compacted == textDelta || compacted.Text != "" {
			t.Errorf("text delta = %#v, want shared empty delta", tk.timeline[0].Message)
		}
		if compacted, ok := tk.timeline[2].Message.(*agent.ThinkingDeltaMessage); !ok || compacted == thinkingDelta || compacted.Text != "" {
			t.Errorf("thinking delta = %#v, want shared empty delta", tk.timeline[2].Message)
		}
		if compacted, ok := tk.timeline[4].Message.(*agent.ToolOutputDeltaMessage); !ok || compacted == toolDelta || compacted.Delta != "" || compacted.ToolUseID != toolDelta.ToolUseID {
			t.Errorf("tool delta = %#v, want an immutable empty replacement", tk.timeline[4].Message)
		}
		if toolDelta.Delta == "" {
			t.Error("source tool delta was mutated")
		}
		if tk.timeline[6].Message != unfinished || unfinished.Text != "still live" {
			t.Errorf("unfinished delta = %#v, want original live delta", tk.timeline[6].Message)
		}
	})
	t.Run("SeedTimelinePartsCompactsAcrossOwnershipBoundary", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		delta := &agent.TextDeltaMessage{Text: strings.Repeat("payload", 1024)}
		diff := v3.DiffStat{{Path: "main.go", LinesAdded: 3}}
		repos := []v3.RepoState{{RepoIndex: 0, Branch: "caic-test", ChangedFiles: 1}}
		prefix := []agent.TimedMessage{
			{Message: &agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: diff, Repos: repos}},
			{Message: delta},
		}
		suffix := []agent.TimedMessage{{Message: &agent.TextMessage{Text: "complete"}}}
		tk.SeedTimelineParts(prefix, suffix, nil)

		if tk.timelinePrefix[1].Message != compactedTextDelta {
			t.Errorf("prefix delta = %#v, want compacted placeholder", tk.timelinePrefix[1].Message)
		}
		snapshot := tk.Snapshot()
		if !reflect.DeepEqual(snapshot.DiffStat, diff) {
			t.Errorf("restored diff stat = %#v, want %#v", snapshot.DiffStat, diff)
		}
		if !reflect.DeepEqual(snapshot.RepoStates, repos) {
			t.Errorf("restored repo states = %#v, want %#v", snapshot.RepoStates, repos)
		}
	})
	t.Run("ConcurrentMessageCompaction", func(t *testing.T) {
		t.Parallel()
		for _, live := range []bool{false, true} {
			for _, thinking := range []bool{false, true} {
				t.Run(fmt.Sprintf("live=%t/thinking=%t", live, thinking), func(t *testing.T) {
					t.Parallel()
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
					parent := v3.MessageSource{ThreadID: "parent", RootThreadID: "parent", TurnID: "turn", ItemID: "parent-item"}
					child := v3.MessageSource{ThreadID: "child", RootThreadID: "parent", TurnID: "child-turn", ItemID: "child-item"}
					messages := []agent.Message{
						&agent.TextDeltaMessage{Text: "Parent ", Source: parent},
						&agent.TextDeltaMessage{Text: "Child ", Source: child},
						&agent.TextDeltaMessage{Text: "answer", Source: parent},
						&agent.TextDeltaMessage{Text: "answer", Source: child},
						&agent.TextMessage{Text: "Child answer", Source: child},
					}
					if thinking {
						for i := range 4 {
							m, ok := messages[i].(*agent.TextDeltaMessage)
							if !ok {
								t.Fatalf("fixture fragment %d is %T", i, messages[i])
							}
							messages[i] = &agent.ThinkingDeltaMessage{Text: m.Text, Source: m.Source}
						}
						messages[4] = &agent.ThinkingMessage{Text: "Child answer", Source: child}
					}
					if live {
						for _, m := range messages {
							addTaskMessage(tk, m)
						}
					} else {
						prefix := []agent.TimedMessage{{Message: messages[0]}, {Message: messages[1]}}
						suffix := []agent.TimedMessage{{Message: messages[2]}, {Message: messages[3]}, {Message: messages[4]}}
						tk.SeedTimelineParts(prefix, suffix, nil)
					}
					history := tk.Messages()
					for _, i := range []int{0, 2} {
						if got := history[i]; got != messages[i] {
							t.Errorf("parent fragment %d compacted by child: %#v", i, got)
						}
					}
					for _, i := range []int{1, 3} {
						var text string
						var source v3.MessageSource
						switch m := history[i].(type) {
						case *agent.TextDeltaMessage:
							text, source = m.Text, m.Source
						case *agent.ThinkingDeltaMessage:
							text, source = m.Text, m.Source
						}
						if text != "" || source != child {
							t.Errorf("child placeholder %d lost identity or retained payload: %#v", i, history[i])
						}
					}
				})
			}
		}
	})

	t.Run("LiveFinalCompactsDeltas", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		delta := &agent.TextDeltaMessage{Text: strings.Repeat("payload", 1024)}
		addTaskMessage(tk, delta)
		addTaskMessage(tk, &agent.TextMessage{Text: "complete"})

		if tk.timeline[0].Message != compactedTextDelta {
			t.Errorf("live delta = %#v, want compacted placeholder", tk.timeline[0].Message)
		}
		if delta.Text == "" {
			t.Error("published live delta was mutated")
		}
	})
	t.Run("LiveCompactionWaitsForReaderRelease", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		delta := &agent.TextDeltaMessage{Text: strings.Repeat("payload", 1024)}
		addTaskMessage(tk, delta)
		messages := tk.ForwardMessages()
		addTaskMessage(tk, &agent.TextMessage{Text: "complete"})

		if tk.timeline[0].Message != delta {
			t.Fatal("retained delta compacted while a reader was active")
		}
		var got []agent.Message
		for m := range messages {
			got = append(got, m)
		}
		if len(got) != 1 || got[0] != delta {
			t.Fatalf("active reader messages = %#v, want original delta", got)
		}
		if tk.timeline[0].Message != compactedTextDelta {
			t.Errorf("released reader delta = %#v, want compacted placeholder", tk.timeline[0].Message)
		}
	})
	t.Run("TerminalLogSummary", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "persist me"}, harness.Claude, "requested", "high")
		tk.BaseImage = "image"
		tk.ContainerPlatform = "linux/amd64"
		tk.SetTitle("title")
		tk.Repos = []repo.Mount{{Name: "org/repo", Branch: "caic-1", GitRoot: "/host/checkout"}}
		tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "session-1", ReportedModel: "reported", AgentVersion: "1.2.3", AutoCompactTokenLimit: 244800})
		setState(tk, task.StatePurged)
		result := &task.Result{State: task.StatePurged, AgentResult: "done"}
		parentID := ksid.NewID()
		tk.ParentTaskID = parentID
		tk.OwnerID = "user-1"
		tk.CaicMCP = true

		summary := tk.terminalLogSummary(result)

		if summary.State != task.StatePurged || summary.LastTrailer != result {
			t.Fatalf("terminal summary = %#v", summary)
		}

		if summary.SessionID != "session-1" || summary.RequestedModel != "requested" || summary.ReportedModel != "reported" || summary.AgentVersion != "1.2.3" {
			t.Errorf("session summary = (%q, %q, %q, %q)", summary.SessionID, summary.RequestedModel, summary.ReportedModel, summary.AgentVersion)
		}
		if summary.AutoCompactTokenLimit != 244800 {
			t.Fatalf("terminal summary threshold = %d, want 244800", summary.AutoCompactTokenLimit)
		}
		if got := summary.ParentTaskID; got != parentID.String() {
			t.Errorf("ParentTaskID = %q, want %q", got, parentID)
		}
		if summary.OwnerID != "user-1" || !summary.CaicMCP {
			t.Errorf("terminal CAIC MCP metadata = owner %q, enabled %v", summary.OwnerID, summary.CaicMCP)
		}
		if len(summary.Repos) != 1 || summary.Repos[0].GitRoot != "" {
			t.Fatalf("summary repos = %#v, want process-local GitRoot removed", summary.Repos)
		}
	})
	t.Run("RateLimitSnapshot", func(t *testing.T) {
		t.Parallel()
		resetAt := time.Now().UTC().Add(time.Hour)
		message := &agent.RateLimitMessage{
			Status:        "rejected",
			ResetsAt:      resetAt,
			RateLimitType: "five_hour",
			Utilization:   1,
			QuotaProvider: agent.QuotaProviderAnthropic,
			QuotaLabel:    "Anthropic",
			QuotaWindow:   "5h",
		}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		addTaskMessage(tk, message)
		snapshot := tk.Snapshot()
		if snapshot.RateLimit.Status != "rejected" {
			t.Errorf("RateLimit.Status = %q, want rejected", snapshot.RateLimit.Status)
		}
		if !snapshot.RateLimit.ResetsAt.Equal(resetAt) {
			t.Errorf("RateLimit.ResetsAt = %v, want %v", snapshot.RateLimit.ResetsAt, resetAt)
		}
		if snapshot.RateLimit.QuotaProvider != "anthropic" || snapshot.RateLimit.QuotaWindow != "5h" {
			t.Errorf("canonical rate limit = (%q, %q), want (anthropic, 5h)", snapshot.RateLimit.QuotaProvider, snapshot.RateLimit.QuotaWindow)
		}
		if snapshot.RateLimit.ObservedAt.IsZero() {
			t.Error("RateLimit.ObservedAt is zero")
		}

		addTaskMessage(tk, &agent.RateLimitMessage{
			Status:        agent.RateLimitStatusAllowedWarning,
			ResetsAt:      resetAt.Add(6 * 24 * time.Hour),
			QuotaProvider: agent.QuotaProviderAnthropic,
			QuotaWindow:   "7d",
		})
		if got := tk.Snapshot().RateLimit; got.Status != agent.RateLimitStatusRejected || got.QuotaWindow != "5h" {
			t.Errorf("RateLimit after a different-window update = %+v, want active 5h rejection", got)
		}
	})
	t.Run("SubscribeTimelineWakeups", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		wake, unsub := tk.SubscribeTimelineWakeups(t.Context())
		// A burst larger than any message buffer, with no reader, coalesces into
		// one pending signal instead of dropping the subscriber.
		for range 1000 {
			addTaskMessage(tk, &agent.TextDeltaMessage{Text: "."})
		}
		select {
		case <-wake:
		default:
			t.Fatal("burst left no pending wakeup")
		}
		select {
		case <-wake:
			t.Fatal("burst queued more than one wakeup")
		default:
		}
		addTaskMessage(tk, &agent.TextMessage{Text: "after burst"})
		select {
		case <-wake:
		default:
			t.Fatal("subscription did not survive the burst")
		}
		unsub()
		addTaskMessage(tk, &agent.TextMessage{Text: "after unsubscribe"})
		select {
		case <-wake:
			t.Fatal("wakeup after unsubscribe")
		default:
		}
	})
	t.Run("CompletedRootTurns", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.ResultMessage{}}), nil)
		if got := tk.CompletedRootTurns(); got != 1 {
			t.Fatalf("restored turns = %d, want 1", got)
		}
		wake, unsub := tk.SubscribeTimelineWakeups(t.Context())
		defer unsub()
		addTaskMessage(tk, &agent.ResultMessage{Source: v3.MessageSource{AgentID: "child"}})
		select {
		case <-wake:
		default:
			t.Fatal("result published no wakeup")
		}
		if got := tk.CompletedRootTurns(); got != 1 {
			t.Fatalf("turns after child result = %d, want 1", got)
		}
		addTaskMessage(tk, &agent.ResultMessage{})
		select {
		case <-wake:
		default:
			t.Fatal("result published no wakeup")
		}
		if got := tk.CompletedRootTurns(); got != 2 {
			t.Fatalf("turns after root result = %d, want 2", got)
		}
	})
	t.Run("RateLimitsSince", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		historical := &agent.RateLimitMessage{Status: agent.RateLimitStatusAllowed, QuotaProvider: agent.QuotaProviderClaudeCode, QuotaWindow: "5h"}
		addTaskMessage(tk, historical)
		history, next := tk.RateLimitsSince(0)
		if len(history) != 1 || history[0] != historical || next != 1 {
			t.Fatalf("history = %#v next = %d, want the historical rate-limit message at 1", history, next)
		}
		for range 257 {
			addTaskMessage(tk, &agent.TextMessage{Text: "ordinary task output"})
		}
		live := &agent.RateLimitMessage{Status: agent.RateLimitStatusAllowedWarning, QuotaProvider: agent.QuotaProviderClaudeCode, QuotaWindow: "7d"}
		addTaskMessage(tk, live)
		if got, end := tk.RateLimitsSince(next); len(got) != 1 || got[0] != live || end != 259 {
			t.Fatalf("since %d = %#v end = %d, want only the new rate-limit message and end 259", next, got, end)
		}
	})
	t.Run("ConcurrentMetadataSnapshots", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.Repos = []repo.Mount{
			{Name: "org/repo", Branch: "main"},
			{Name: "org/extra", Branch: "main"},
		}
		tk.Tailscale = true
		tk.Sudo = true
		tk.GitHubToken = true

		const iterations = 1000
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, write := range []func(int){
			func(i int) { tk.SetRepoBranch(0, "task-"+strconv.Itoa(i)) },
			func(i int) { tk.SetRepoBranch(1, "extra-"+strconv.Itoa(i)) },
			func(i int) {
				id := runtime.ID("ctr-" + strconv.Itoa(i))
				tk.SetRuntimeConnectionInfo(id, runtime.ConnectionTarget{SSHHost: string(id)}, "host.example", "https://auth.example", 5900+i)
			},
			func(i int) { tk.SetVNCPort(6000 + i) },
			func(i int) { tk.SetRelayOffset(int64(i)) },
			func(i int) { tk.SetSudoPassword("pw-" + strconv.Itoa(i)) },
		} {
			wg.Go(func() {
				<-start
				for i := range iterations {
					write(i)
				}
			})
		}
		for range 4 {
			wg.Go(func() {
				<-start
				for range iterations {
					snap := tk.Snapshot()
					if len(snap.Repos) > 0 {
						snap.Repos[0].Branch = "mutated"
					}
					_ = tk.Primary()
					_ = tk.RuntimeRepos()
					_ = tk.ExtraRuntimeRepos()
					_ = tk.RuntimeInstanceID()
					_ = tk.RelayOffsetValue()
					_, _, _ = tk.SudoLookupState()
					_ = tk.GitHubTokenEnabled()
				}
			})
		}

		close(start)
		wg.Wait()

		repos := tk.ReposSnapshot()
		if len(repos) == 0 {
			t.Fatal("ReposSnapshot returned no repos")
		}
		if repos[0].Branch == "mutated" {
			t.Fatal("Snapshot exposed mutable repo storage")
		}
	})
	t.Run("SeedTimelineCapturesExitError", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}}), nil)
		if got := tk.LastExitError(); got != "Unknown option: --approve" {
			t.Errorf("LastExitError = %q, want relay stderr", got)
		}
	})
	t.Run("SeedTimelineClearsStaleExitErrorOnLaterSessionActivity", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
			&agent.ExitMessage{ExitCode: 2, Error: "stale crash"},
			&agent.InitMessage{SessionID: "new-session"},
			&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"},
		}), nil)
		if got := tk.LastExitError(); got != "" {
			t.Errorf("LastExitError = %q, want stale exit error cleared", got)
		}
	})
	t.Run("SeedTimelineIgnoresTrailingExitAfterSuccessfulTurn", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateRunning)
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
			&agent.InitMessage{SessionID: "session"},
			&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"},
			&agent.ExitMessage{ExitCode: 2, Error: "stale crash"},
		}), nil)
		if got := tk.LastExitError(); got != "" {
			t.Errorf("LastExitError = %q, want trailing exit ignored", got)
		}
		if got := tk.GetState(); got != task.StateWaiting {
			t.Errorf("state = %v, want waiting", got)
		}
	})
	t.Run("MarkSessionFailed", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateStarting)
		addTaskMessage(tk, &agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"})

		if !tk.MarkSessionFailed() {
			t.Fatal("MarkSessionFailed returned false")
		}
		if got := tk.GetState(); got != task.StateFailed {
			t.Errorf("state = %v, want %v", got, task.StateFailed)
		}
		if got := tk.LastAgentResult(); got != "" {
			t.Fatalf("unrecorded result = %q", got)
		}
		if got := tk.LastExitError(); got != "Unknown option: --approve" {
			t.Fatalf("recorded exit = %q", got)
		}
	})

	t.Run("SendInput", func(t *testing.T) {
		t.Parallel()
		t.Run("error_root_delta_before_delivery_failure_establishes_running", func(t *testing.T) {
			t.Parallel()
			for _, state := range []task.State{task.StateWaiting, task.StateAsking} {
				t.Run(string(state), func(t *testing.T) {
					t.Parallel()
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
					setState(tk, state)
					if state == task.StateAsking {
						addTaskMessage(tk, &agent.AskMessage{ToolUseID: "question"})
					}
					_, run := mustTaskRun(t, tk)
					writer := taskInputWriter(func([]byte) (int, error) {
						if err := taskNative(t.Context(), run.records, `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"working"}}}`); err != nil {
							return 0, err
						}
						return 1, io.ErrUnexpectedEOF
					})
					ctx, cancel := context.WithCancel(t.Context())
					cmd := exec.CommandContext(ctx, "sleep", "60")
					stdout, err := cmd.StdoutPipe()
					if err != nil {
						t.Fatal(err)
					}
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					s := agent.NewSession(ctx, cmd, agent.NewConn(ctx, testLogger(), writer, run.records, nil), stdout, testLogger())
					t.Cleanup(func() { cancel(); _ = s.Wait() })
					tk.AttachSession(&SessionHandle{Session: s, Run: run})
					if err := tk.SendInput(ctx, agent.Prompt{Text: "follow up"}); !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("send = %v, want delivery error", err)
					}
					if got := tk.GetState(); got != task.StateRunning {
						t.Fatalf("state = %s, want running", got)
					}
					if !tk.HasAcceptedInputEvidence() {
						t.Fatal("committed root output did not establish acceptance")
					}
				})
			}
		})
		t.Run("error_partial_delivery_preserves_active_root_output", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			setState(tk, task.StateWaiting)
			_, run := mustTaskRun(t, tk)
			writer := taskInputWriter(func([]byte) (int, error) {
				if err := taskNative(t.Context(), run.records, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}`); err != nil {
					return 0, err
				}
				return 1, io.ErrUnexpectedEOF
			})
			ctx, cancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(ctx, "sleep", "60")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := agent.NewSession(ctx, cmd, agent.NewConn(ctx, testLogger(), writer, run.records, nil), stdout, testLogger())
			t.Cleanup(func() { cancel(); _ = s.Wait() })
			tk.AttachSession(&SessionHandle{Session: s, Run: run})
			if err := tk.SendInput(ctx, agent.Prompt{Text: "follow up"}); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("send = %v", err)
			}
			if tk.GetState() != task.StateRunning || !tk.HasAcceptedInputEvidence() {
				t.Fatal("partial delivery discarded committed output acceptance")
			}
		})
		t.Run("completion_during_delivery_preserves_waiting_state", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			setState(tk, task.StateWaiting)
			live := newMessageCursor(t, tk)
			_, run := mustTaskRun(t, tk)
			writer := taskInputWriter(func(p []byte) (int, error) {
				if err := taskNative(t.Context(), run.records, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`); err != nil {
					return 0, err
				}
				if err := taskNative(t.Context(), run.records, `{"type":"result","subtype":"success","is_error":false,"result":"done"}`); err != nil {
					return 0, err
				}
				return len(p), nil
			})
			ctx, cancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(ctx, "sleep", "60")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := agent.NewSession(ctx, cmd, agent.NewConn(ctx, testLogger(), writer, run.records, nil), stdout, testLogger())
			t.Cleanup(func() { cancel(); _ = s.Wait() })
			tk.AttachSession(&SessionHandle{Session: s, Run: run})
			if err := tk.SendInput(ctx, agent.Prompt{Text: "follow up"}); err != nil {
				t.Fatal(err)
			}
			var order []string
			for len(order) < 3 {
				if m := live.recv(t).Message; !agent.IsInputDeliveryControl(m) {
					order = append(order, m.Type())
				}
			}
			if !slices.Equal(order, []string{"user_input", "text", "result"}) {
				t.Fatalf("live order = %v", order)
			}
			if tk.GetState() != task.StateWaiting {
				t.Fatalf("state = %s", tk.GetState())
			}
			if got := tk.titleInput(); got != "Current title: test\nRecent activity:\nPrompt: follow up\nResult: done\n" {
				t.Fatalf("title input = %q", got)
			}
		})
		t.Run("error_delivery_failure_preserves_waiting_state", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			setState(tk, task.StateWaiting)
			_, run := mustTaskRun(t, tk)
			sendErr := errors.New("delivery failed")
			ctx, cancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(ctx, "sleep", "60")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			writer := taskInputWriter(func([]byte) (int, error) { return 0, sendErr })
			s := agent.NewSession(ctx, cmd, agent.NewConn(ctx, testLogger(), writer, run.records, nil), stdout, testLogger())
			t.Cleanup(func() { cancel(); _ = s.Wait() })
			tk.AttachSession(&SessionHandle{Session: s, Run: run})
			if err := tk.SendInput(ctx, agent.Prompt{Text: "follow up"}); !errors.Is(err, sendErr) {
				t.Fatalf("send = %v", err)
			}
			if tk.GetState() != task.StateWaiting {
				t.Fatalf("state = %s", tk.GetState())
			}
			var msgs []agent.Message
			for _, m := range tk.Messages() {
				if !agent.IsInputDeliveryControl(m) {
					msgs = append(msgs, m)
				}
			}
			if len(msgs) != 2 {
				t.Fatalf("messages = %d", len(msgs))
			}
			failure, ok := msgs[1].(*agent.SystemMessage)
			if !ok || failure.Subtype != agent.SystemSubtypeInputDeliveryFailed || failure.Detail != "Input delivery failed: delivery failed" {
				t.Fatalf("failure = %#v", msgs[1])
			}
			if tk.HasAcceptedInputEvidence() {
				t.Fatal("failed input accepted")
			}
			if strings.Contains(tk.titleInput(), "follow up") {
				t.Fatal("failed input leaked into title context")
			}
		})
		t.Run("PreservesPlanContent", func(t *testing.T) {
			t.Parallel()
			// When the user sends regular input (instead of clicking
			// "Clear and execute plan"), planContent must be preserved
			// so the plan UI reappears after the agent finishes. The
			// UI hides naturally while the task is Running.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			setState(tk, task.StateRunning)
			// Simulate: agent entered plan mode, wrote a plan, exited.
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "EnterPlanMode",
			})
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "ExitPlanMode",
			})
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})

			snap := tk.Snapshot()
			if snap.PlanContent != "the plan" {
				t.Fatalf("PlanContent = %q before SendInput, want %q", snap.PlanContent, "the plan")
			}

			// Attach a live session so SendInput succeeds past the handle check.
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			t.Cleanup(cmdCancel)
			cmd := exec.CommandContext(cmdCtx, "python3", "-c", "import sys; sys.stdin.buffer.read()")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_, run := mustTaskRun(t, tk)
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, run.records, nil), stdout, testLogger())
			tk.AttachSession(&SessionHandle{Session: s, Run: run})
			t.Cleanup(func() { cmdCancel(); _ = s.Wait() })

			// User sends a regular message instead of "Clear and execute plan".
			_ = tk.SendInput(t.Context(), agent.Prompt{Text: "improve the plan"})

			snap = tk.Snapshot()
			if snap.PlanContent != "the plan" {
				t.Errorf("PlanContent = %q after SendInput, want %q", snap.PlanContent, "the plan")
			}
			if snap.PlanFile != "/home/user/.claude/plans/p.md" {
				t.Errorf("PlanFile = %q after SendInput, want %q", snap.PlanFile, "/home/user/.claude/plans/p.md")
			}
		})
		t.Run("EditUpdatesPlanContent", func(t *testing.T) {
			t.Parallel()
			// When the agent uses the Edit tool on a plan file, the
			// in-memory planContent must be updated so the UI shows
			// the revised plan.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			// Agent writes the initial plan.
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"step 1\nstep 2\n"}`),
			})
			if snap := tk.Snapshot(); snap.PlanContent != "step 1\nstep 2\n" {
				t.Fatalf("PlanContent = %q after Write, want %q", snap.PlanContent, "step 1\nstep 2\n")
			}
			// Agent edits the plan file.
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Edit",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","old_string":"step 2","new_string":"step 2 (revised)\nstep 3"}`),
			})
			snap := tk.Snapshot()
			if snap.PlanContent != "step 1\nstep 2 (revised)\nstep 3\n" {
				t.Errorf("PlanContent = %q after Edit, want %q", snap.PlanContent, "step 1\nstep 2 (revised)\nstep 3\n")
			}
		})
		t.Run("EditReplaceAll", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"TODO\nTODO\n"}`),
			})
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Edit",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","old_string":"TODO","new_string":"DONE","replace_all":true}`),
			})
			snap := tk.Snapshot()
			// The repeated "DONE" proves replace_all rewrites both occurrences.
			want := strings.Repeat("DONE\n", 2)
			if snap.PlanContent != want {
				t.Errorf("PlanContent = %q after replace_all Edit, want %q", snap.PlanContent, want)
			}
		})
		t.Run("EditIgnoresNonPlanFile", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			// Edit a non-plan file — planContent must be unchanged.
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Edit",
				Input: json.RawMessage(`{"file_path":"/home/user/src/main.go","old_string":"foo","new_string":"bar"}`),
			})
			snap := tk.Snapshot()
			if snap.PlanContent != "the plan" {
				t.Errorf("PlanContent = %q after non-plan Edit, want %q", snap.PlanContent, "the plan")
			}
		})
		t.Run("EditAfterSendInputUpdatesPlan", func(t *testing.T) {
			t.Parallel()
			// Core regression test: user rejects plan and asks for
			// improvement, agent edits the plan file.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"original plan"}`),
			})
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			})
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})

			// Attach a live session.
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			t.Cleanup(cmdCancel)
			cmd := exec.CommandContext(cmdCtx, "python3", "-c", "import sys; sys.stdin.buffer.read()")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_, run := mustTaskRun(t, tk)
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, run.records, nil), stdout, testLogger())
			tk.AttachSession(&SessionHandle{Session: s, Run: run})
			t.Cleanup(func() { cmdCancel(); _ = s.Wait() })

			// User rejects plan and sends feedback.
			_ = tk.SendInput(t.Context(), agent.Prompt{Text: "add error handling"})

			// Agent edits the plan file.
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "Edit",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","old_string":"original plan","new_string":"updated plan with error handling"}`),
			})
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu4", Name: "ExitPlanMode",
			})
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})

			snap := tk.Snapshot()
			if snap.PlanContent != "updated plan with error handling" {
				t.Errorf("PlanContent = %q, want %q", snap.PlanContent, "updated plan with error handling")
			}
		})
		t.Run("NoSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateWaiting)
			err := tk.SendInput(t.Context(), agent.Prompt{Text: "hello"})
			if err == nil {
				t.Fatal("expected error when no session is active")
			}
			msg := err.Error()
			if !strings.Contains(msg, "session="+string(SessionNone)) {
				t.Errorf("error = %q, want session=%s", msg, SessionNone)
			}
			if !strings.Contains(msg, "state=waiting") {
				t.Errorf("error = %q, want state=waiting", msg)
			}
		})
		t.Run("DeadSessionDetected", func(t *testing.T) {
			t.Parallel()
			// Simulate a session that has already finished (e.g. relay
			// subprocess exited). SendInput should detect it and return
			// "no active session" without changing state.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			setState(tk, task.StateWaiting)
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			t.Cleanup(cmdCancel)
			cmd := exec.CommandContext(cmdCtx, "true")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_, run := mustTaskRun(t, tk)
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, run.records, nil), stdout, testLogger())
			<-s.Done()
			tk.AttachSession(&SessionHandle{Session: s, Run: run})
			err = tk.SendInput(t.Context(), agent.Prompt{Text: "hello"})
			if err == nil {
				t.Fatal("expected error for dead session")
			}
			msg := err.Error()
			if !strings.Contains(msg, "session="+string(SessionExited)) {
				t.Errorf("error = %q, want session=%s", msg, SessionExited)
			}
			if !strings.Contains(msg, "state=waiting") {
				t.Errorf("error = %q, want state=waiting", msg)
			}
		})
	})

	t.Run("AttachDetachSession", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
		if tk.SessionDone() != nil {
			t.Error("SessionDone() should be nil when no session attached")
		}
		if tk.DetachSession() != nil {
			t.Error("DetachSession() should return nil when no session attached")
		}

		cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cmdCancel)
		cmd := exec.CommandContext(cmdCtx, "python3", "-c", "import sys; sys.stdin.buffer.read()")
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		_, run := mustTaskRun(t, tk)
		s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, run.records, nil), stdout, testLogger())
		h := &SessionHandle{Session: s, Run: run}
		tk.AttachSession(h)

		if tk.SessionDone() == nil {
			t.Error("SessionDone() should not be nil after AttachSession")
		}

		got := tk.DetachSession()
		if got != h {
			t.Error("DetachSession() returned wrong handle")
		}
		if tk.SessionDone() != nil {
			t.Error("SessionDone() should be nil after DetachSession")
		}

		cmdCancel()
		_ = s.Wait()
	})

	t.Run("addMessage", func(t *testing.T) {
		t.Parallel()
		t.Run("TransitionsToWaiting", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			result := &agent.ResultMessage{MessageType: "result"}
			addTaskMessage(tk, result)
			if tk.GetState() != task.StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateWaiting)
			}
		})
		t.Run("TransitionsToAsking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			// Add an AskMessage.
			addTaskMessage(tk, &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			if tk.GetState() != task.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateAsking)
			}
		})
		t.Run("AnsweredAskTransitionsToWaiting", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			addTaskMessage(tk, &agent.ToolResultMessage{ToolUseID: "ask1"})
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != task.StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateWaiting)
			}
		})
		t.Run("ErroredAskResultStaysAsking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			addTaskMessage(tk, &agent.ToolResultMessage{
				ToolUseID: "ask1",
				Error:     "permission denied",
			})
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != task.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateAsking)
			}
		})
		t.Run("TransitionsToAskingWithPartialMessages", func(t *testing.T) {
			t.Parallel()
			// With --include-partial-messages, Claude Code emits multiple
			// assistant snapshots per turn. AskUserQuestion appears in an
			// earlier snapshot while the final one is text-only. The state
			// machine must scan all messages in the turn.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.TextMessage{Text: "I need to ask you something."})
			addTaskMessage(tk, &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			if tk.GetState() != task.StateAsking {
				t.Fatalf("state = %v, want %v before result", tk.GetState(), task.StateAsking)
			}
			setState(tk, task.StateRunning)
			// Final partial snapshot: text-only, no tool_use.
			addTaskMessage(tk, &agent.TextMessage{Text: "I need to ask you something."})
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != task.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateAsking)
			}
		})
		t.Run("TurnStartedTransitionsToRunning", func(t *testing.T) {
			t.Parallel()
			for _, from := range []task.State{task.StateStarting, task.StateWaiting, task.StateHasPlan} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, from)
				addTaskMessage(tk, &agent.TextMessage{Text: "output"})
				if got := tk.GetState(); got != from {
					t.Errorf("%v: state after output = %v, want %v", from, got, from)
				}
				addTaskMessage(tk, &agent.TurnStartedMessage{})
				if got := tk.GetState(); got != task.StateRunning {
					t.Errorf("%v: state after turn start = %v, want %v", from, got, task.StateRunning)
				}
			}
		})
		t.Run("ResultTransitionsWaitingToAsking", func(t *testing.T) {
			t.Parallel()
			// When watchSession sets Waiting before the ResultMessage is
			// processed, the ResultMessage should still detect
			// AskMessage and correct the state to Asking.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			// Simulate watchSession setting Waiting before ResultMessage
			// is processed by the dispatch goroutine.
			setState(tk, task.StateWaiting)
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != task.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateAsking)
			}
		})
		t.Run("TransitionsToHasPlan", func(t *testing.T) {
			t.Parallel()
			// ExitPlanMode + plan content + ResultMessage → StateHasPlan.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			})
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != task.StateHasPlan {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateHasPlan)
			}
		})
		t.Run("AskingTakesPriorityOverHasPlan", func(t *testing.T) {
			t.Parallel()
			// Both AskMessage and ExitPlanMode in same turn → StateAsking.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			})
			addTaskMessage(tk, &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != task.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateAsking)
			}
		})
		t.Run("NoHasPlanWithoutPlanContent", func(t *testing.T) {
			t.Parallel()
			// ExitPlanMode without plan content → StateWaiting.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "ExitPlanMode",
			})
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != task.StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateWaiting)
			}
		})
		t.Run("ExitPlanModeProjectsPlanContent", func(t *testing.T) {
			t.Parallel()
			// The snapshot pairs the current plan with its root ExitPlanMode owner.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan A"}`),
			})
			addTaskMessage(tk, &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"})
			if snap := tk.Snapshot(); snap.PlanExitID != "tu2" || snap.PlanContent != "plan A" {
				t.Errorf("current plan = (%q, %q), want (tu2, plan A)", snap.PlanExitID, snap.PlanContent)
			}
			if got := tk.Snapshot().PlanExitID; got == "tu1" {
				t.Errorf("superseded plan owner = %q", got)
			}
		})
		t.Run("NewExitPlanModeClearsPreviousPlanContent", func(t *testing.T) {
			t.Parallel()
			// When a second ExitPlanMode arrives, the plan projection must
			// follow only it, so the frontend doesn't list the stale plan.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v1"}`),
			})
			addTaskMessage(tk, &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"})
			if snap := tk.Snapshot(); snap.PlanExitID != "tu2" || snap.PlanContent != "plan v1" {
				t.Errorf("current plan = (%q, %q), want (tu2, plan v1)", snap.PlanExitID, snap.PlanContent)
			}
			addTaskMessage(tk, &agent.ResultMessage{MessageType: "result"})

			// Agent updates the plan.
			addTaskMessage(tk, &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v2"}`),
			})
			addTaskMessage(tk, &agent.ToolUseMessage{ToolUseID: "tu4", Name: "ExitPlanMode"})
			if got := tk.Snapshot().PlanExitID; got == "tu2" {
				t.Errorf("superseded plan owner = %q", got)
			}
			if snap := tk.Snapshot(); snap.PlanExitID != "tu4" || snap.PlanContent != "plan v2" {
				t.Errorf("current plan = (%q, %q), want (tu4, plan v2)", snap.PlanExitID, snap.PlanContent)
			}
		})
		t.Run("EmptyResultUsesTurnText", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.TextMessage{Text: "I checked the files."})
			addTaskMessage(tk, &agent.ToolUseMessage{ToolUseID: "tu1", Name: "Read"})
			addTaskMessage(tk, &agent.ToolResultMessage{ToolUseID: "tu1"})
			addTaskMessage(tk, &agent.TextMessage{Text: "There is nothing to change."})

			result := &agent.ResultMessage{MessageType: "result"}
			addTaskMessage(tk, result)
			if got := tk.LastAgentResult(); got != "There is nothing to change." {
				t.Errorf("LastAgentResult = %q, want %q", got, "There is nothing to change.")
			}
			if result.Result != "" {
				t.Errorf("Result = %q, want empty (message must stay immutable)", result.Result)
			}
		})
		t.Run("EmptyResultStopsAtThinking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.TextMessage{Text: "before thinking"})
			addTaskMessage(tk, &agent.ThinkingMessage{Text: "private chain"})
			addTaskMessage(tk, &agent.TextMessage{Text: "after thinking"})

			result := &agent.ResultMessage{MessageType: "result"}
			addTaskMessage(tk, result)
			if got := tk.LastAgentResult(); got != "after thinking" {
				t.Errorf("LastAgentResult = %q, want %q (thinking excluded)", got, "after thinking")
			}
			if result.Result != "" {
				t.Errorf("Result = %q, want empty (message must stay immutable)", result.Result)
			}
		})
		t.Run("NonEmptyResultIsPreserved", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.TextMessage{Text: "intermediate"})

			result := &agent.ResultMessage{MessageType: "result", Result: "final"}
			addTaskMessage(tk, result)
			if result.Result != "final" {
				t.Errorf("Result = %q, want final", result.Result)
			}
			if got := tk.LastAgentResult(); got != "final" {
				t.Errorf("LastAgentResult = %q, want final", got)
			}
		})
		t.Run("NoTransitionForNonActiveStates", func(t *testing.T) {
			t.Parallel()
			// TextMessages should NOT transition terminal or
			// setup states (except StateStarting, which is
			// tested separately above).
			for _, state := range []task.State{task.StatePending, task.StateBranching, task.StateProvisioning, task.StatePurging, task.StateCrashed, task.StateFailed, task.StatePurged} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, state)
				addTaskMessage(tk, &agent.TextMessage{Text: "output"})
				if tk.GetState() != state {
					t.Errorf("state %v changed to %v; want unchanged", state, tk.GetState())
				}
			}
		})
	})

	t.Run("addMessageDiffStat", func(t *testing.T) {
		t.Parallel()
		t.Run("DiffStatMessage", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			ds := v3.DiffStat{
				{Path: "main.go", LinesAdded: 10, LinesDeleted: 3},
				{Path: "img.png", Binary: true},
			}
			addTaskMessage(tk, &agent.DiffStatMessage{
				MessageType: "caic_diff_stat",
				DiffStat:    ds,
			})
			got := tk.Snapshot().DiffStat
			if len(got) != 2 {
				t.Fatalf("DiffStat len = %d, want 2", len(got))
			}
			if got[0].Path != "main.go" || got[0].LinesAdded != 10 {
				t.Errorf("DiffStat[0] = %+v", got[0])
			}
			// Update with new diff stat.
			addTaskMessage(tk, &agent.DiffStatMessage{
				MessageType: "caic_diff_stat",
				DiffStat:    v3.DiffStat{{Path: "new.go", LinesAdded: 1, LinesDeleted: 0}},
			})
			got = tk.Snapshot().DiffStat
			if len(got) != 1 || got[0].Path != "new.go" {
				t.Errorf("DiffStat after update = %+v", got)
			}
		})

		t.Run("ResultMessageUpdatesDiffStat", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			addTaskMessage(tk, &agent.ResultMessage{
				MessageType: "result",
				DiffStat:    v3.DiffStat{{Path: "a.go", LinesAdded: 5, LinesDeleted: 2}},
			})
			got := tk.Snapshot().DiffStat
			if len(got) != 1 || got[0].Path != "a.go" || got[0].LinesAdded != 5 {
				t.Errorf("DiffStat = %+v, want [{a.go 5 2}]", got)
			}
		})
	})

	t.Run("SeedTimelineDiffStat", func(t *testing.T) {
		t.Parallel()

		// Regression: the relay's diff_watcher computes git diff HEAD
		// (uncommitted changes). After the agent commits, this is empty.
		// The ResultMessage.DiffStat (host-side branch diff) is set
		// Runtime import refreshes the host branch diff after rebuilding
		// native relay history, including empty relay diff reports.
		t.Run("EmptyRelayDiffAfterCommit", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			setState(tk, task.StateRunning)
			// Simulate relay output: ResultMessage without DiffStat
			// (host-side mutation not persisted) followed by an empty
			// DiffStatMessage (relay sees no uncommitted changes).
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{{Path: "main.go", LinesAdded: 10, LinesDeleted: 2}},
				},
				&agent.ResultMessage{MessageType: "result"},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{},
				},
			}), nil)
			got := tk.Snapshot().DiffStat
			if len(got) != 0 {
				t.Fatalf("DiffStat = %+v, want empty (relay reported no uncommitted changes)", got)
			}
			// Adoption records the branch diff from a fresh host snapshot.
			tk.SetRuntimeConnectionInfo("test-runtime:adopted", runtime.ConnectionTarget{}, "", "", 0)
			r, _ := mustTaskRun(t, tk)
			if _, err := r.RecordRepositorySummary(t.Context(), tk, &repo.GitSnapshot{
				Read:     repo.NewGitRead(tk.RuntimeInstanceID()),
				DiffStat: v3.DiffStat{{Path: "main.go", LinesAdded: 10, LinesDeleted: 2}},
			}); err != nil {
				t.Fatal(err)
			}
			got = tk.Snapshot().DiffStat
			if len(got) != 1 || got[0].Path != "main.go" {
				t.Errorf("DiffStat after set = %+v, want main.go", got)
			}
		})
	})

	t.Run("LiveUsageCumulative", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10, ReasoningOutputTokens: 7},
		})
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 200, OutputTokens: 80, CacheCreationInputTokens: 30, ReasoningOutputTokens: 11},
		})
		snapshot := tk.Snapshot()
		usage, lastUsage := snapshot.Usage, snapshot.LastUsage
		if usage.InputTokens != 300 {
			t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
		}
		if usage.OutputTokens != 130 {
			t.Errorf("OutputTokens = %d, want 130", usage.OutputTokens)
		}
		if usage.CacheReadInputTokens != 10 {
			t.Errorf("CacheReadInputTokens = %d, want 10", usage.CacheReadInputTokens)
		}
		if usage.CacheCreationInputTokens != 30 {
			t.Errorf("CacheCreationInputTokens = %d, want 30", usage.CacheCreationInputTokens)
		}
		if usage.ReasoningOutputTokens != 18 {
			t.Errorf("ReasoningOutputTokens = %d, want 18", usage.ReasoningOutputTokens)
		}
		// lastUsage should reflect only the most recent ResultMessage.
		if lastUsage.InputTokens != 200 {
			t.Errorf("lastUsage.InputTokens = %d, want 200", lastUsage.InputTokens)
		}
		if lastUsage.CacheCreationInputTokens != 30 {
			t.Errorf("lastUsage.CacheCreationInputTokens = %d, want 30", lastUsage.CacheCreationInputTokens)
		}
		if lastUsage.ReasoningOutputTokens != 11 {
			t.Errorf("lastUsage.ReasoningOutputTokens = %d, want 11", lastUsage.ReasoningOutputTokens)
		}
	})

	t.Run("NativeSkillWithoutDatedInput", func(t *testing.T) {
		t.Parallel()
		parser := claudecode.New().NewWire()
		var entries []agent.TimedMessage
		for _, record := range []struct {
			line string
			at   int64
		}{
			{`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"skill","name":"Skill","input":{"skill":"review"}}]}}`, 100},
			{`{"type":"result","subtype":"success"}`, 102},
		} {
			messages, err := parser.ParseOutput([]byte(record.line))
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range messages {
				entries = append(entries, agent.TimedMessage{Message: message, ProducerTime: time.Unix(record.at, 0)})
			}
		}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		for _, entry := range entries {
			tk.addParsedMessage(entry)
		}
		if snap := tk.Snapshot(); snap.Duration != 2*time.Second || snap.NumTurns != 1 {
			t.Fatalf("duration = %v, turns = %d; want 2s, 1", snap.Duration, snap.NumTurns)
		}
	})
	t.Run("MissingNativeTurnMetrics", func(t *testing.T) {
		t.Parallel()
		start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
		messages := []agent.TimedMessage{
			{ProducerTime: start, Message: &agent.UserInputMessage{Text: "first"}},
			{ProducerTime: start.Add(2 * time.Second), Message: &agent.ResultMessage{}},
			{ProducerTime: start.Add(time.Hour), Message: &agent.UserInputMessage{Text: "second"}},
			{ProducerTime: start.Add(time.Hour + 3*time.Second), Message: &agent.ResultMessage{}},
			{ProducerTime: start.Add(time.Hour + 4*time.Second), Message: &agent.ResultMessage{Source: v3.MessageSource{DelegationToolUseID: "child"}, NumTurns: 99, DurationMs: 99000}},
			{ProducerTime: start.Add(2 * time.Hour), Message: &agent.UserInputMessage{Text: "native"}},
			{ProducerTime: start.Add(2*time.Hour + time.Minute), Message: &agent.ResultMessage{NumTurns: 4, DurationMs: 7000}},
		}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		for _, message := range messages {
			tk.addParsedMessage(message)
		}
		if snapshot := tk.Snapshot(); snapshot.NumTurns != 6 || snapshot.Duration != 12*time.Second {
			t.Errorf("turns = %d, duration = %v; want 6, 12s", snapshot.NumTurns, snapshot.Duration)
		}
	})

	t.Run("LiveCostCumulativeAcrossSessions", func(t *testing.T) {
		t.Parallel()
		// Cost, turns, and duration must accumulate across sessions separated
		// by context boundary. Reported totals are already fully priced.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateRunning)
		// Session 1: TotalCostUSD = $10.00.
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 10.0,
			NumTurns:     3,
			DurationMs:   5000,
		})
		addTaskMessage(tk, agent.ContextCleared())
		setState(tk, task.StateRunning)
		// Session 2: TotalCostUSD = $5.00.
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 5.0,
			NumTurns:     2,
			DurationMs:   3000,
		})
		snapshot := tk.Snapshot()
		costUSD, numTurns, duration := snapshot.CostUSD, snapshot.NumTurns, snapshot.Duration
		if costUSD != 15.0 {
			t.Errorf("costUSD = %v, want 15.0", costUSD)
		}
		if numTurns != 5 {
			t.Errorf("numTurns = %d, want 5", numTurns)
		}
		if duration != 8*time.Second {
			t.Errorf("duration = %v, want 8s", duration)
		}
	})

	t.Run("LiveDurationAccumulatesWithinSession", func(t *testing.T) {
		t.Parallel()
		// Multiple result events within a single session (no context boundary/compact_boundary)
		// must accumulate duration rather than overwriting with only the last invocation's value.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType: "result",
			NumTurns:    1,
			DurationMs:  946943,
		})
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType: "result",
			NumTurns:    1,
			DurationMs:  5278,
		})
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType: "result",
			NumTurns:    1,
			DurationMs:  214500,
		})
		snapshot := tk.Snapshot()
		numTurns, duration := snapshot.NumTurns, snapshot.Duration
		if numTurns != 3 {
			t.Errorf("numTurns = %d, want 3", numTurns)
		}
		wantDuration := time.Duration(946943+5278+214500) * time.Millisecond
		if duration != wantDuration {
			t.Errorf("duration = %v, want %v", duration, wantDuration)
		}
	})

	t.Run("LiveCostCumulativeAcrossThreeSessions", func(t *testing.T) {
		t.Parallel()
		// Regression: context boundary used += (double-count) instead of = assignment.
		// Verify cost is correct after two context boundary calls.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateRunning)
		// Session 1: $75.
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 75.0,
			NumTurns:     1,
		})
		addTaskMessage(tk, agent.ContextCleared())
		setState(tk, task.StateRunning)
		// Session 2: $75.
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 75.0,
			NumTurns:     1,
		})
		addTaskMessage(tk, agent.ContextCleared())
		setState(tk, task.StateRunning)
		// Session 3: $75.
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 75.0,
			NumTurns:     1,
		})
		snapshot := tk.Snapshot()
		costUSD, numTurns := snapshot.CostUSD, snapshot.NumTurns
		if costUSD != 225.0 {
			t.Errorf("costUSD = %v, want 225.0 (three sessions × $75)", costUSD)
		}
		if numTurns != 3 {
			t.Errorf("numTurns = %d, want 3", numTurns)
		}
	})

	t.Run("LiveCostUsesClaudeReportedTotal", func(t *testing.T) {
		t.Parallel()
		// Claude's reported total already includes cache reads.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 1.50,
			Usage: agent.Usage{
				InputTokens:          100_000,
				CacheReadInputTokens: 10_000_000,
			},
		})
		costUSD := tk.Snapshot().CostUSD
		if costUSD != 1.50 {
			t.Errorf("costUSD = %v, want reported 1.50", costUSD)
		}
	})

	t.Run("CompactBoundaryAccumulatesStats", func(t *testing.T) {
		t.Parallel()
		// Claude's total cost persists across compaction; turns and duration
		// are per invocation and still accumulate across the boundary.
		newTask := func() *Task {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			setState(tk, task.StateRunning)
			return tk
		}
		tk := newTask()
		for _, m := range []agent.Message{
			&agent.ResultMessage{MessageType: "result", TotalCostUSD: 10.0, NumTurns: 3, DurationMs: 5000},
			&agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary"},
			&agent.ResultMessage{MessageType: "result", TotalCostUSD: 15.0, NumTurns: 2, DurationMs: 3000},
		} {
			addTaskMessage(tk, m)
		}
		snapshot := tk.Snapshot()
		costUSD, numTurns, duration := snapshot.CostUSD, snapshot.NumTurns, snapshot.Duration
		if costUSD != 15.0 {
			t.Errorf("costUSD = %v, want 15.0", costUSD)
		}
		if numTurns != 5 {
			t.Errorf("numTurns = %d, want 5", numTurns)
		}
		if duration != 8*time.Second {
			t.Errorf("duration = %v, want 8s", duration)
		}

		// The boundary carries the harness's post-compaction context size, which
		// must replace the pre-compaction fill until the next usage message.
		t.Run("ContextTokensAfterResetsMeter", func(t *testing.T) {
			t.Parallel()
			tk := newTask()
			addTaskMessage(tk, &agent.UsageMessage{Usage: agent.Usage{InputTokens: 180_000, CacheReadInputTokens: 5_000}})
			addTaskMessage(tk, &agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary", ContextTokensAfter: 12_000})
			usage := tk.Snapshot().LastAPIUsage
			if usage.InputTokens != 12_000 || usage.CacheReadInputTokens != 0 {
				t.Errorf("lastAPIUsage = %+v, want only 12000 input tokens", usage)
			}
		})
	})

	t.Run("SeedTimelineParts", func(t *testing.T) {
		t.Parallel()
		t.Run("PanicsOnNonEmptyTimeline", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			addTaskMessage(tk, &agent.TextMessage{Text: "live"})
			defer func() {
				if recover() == nil {
					t.Error("SeedTimelineParts on a task with messages did not panic")
				}
			}()
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.TextMessage{Text: "from log"}}), nil)
		})
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "sess-123"},
				&agent.TextMessage{Text: "hello"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)

			if len(tk.Messages()) != 3 {
				t.Fatalf("Messages() len = %d, want 3", len(tk.Messages()))
			}
			if tk.GetSessionID() != "sess-123" {
				t.Errorf("SessionID = %q, want %q", tk.GetSessionID(), "sess-123")
			}
			if tk.GetState() != task.StateWaiting {
				t.Errorf("state = %v, want %v (should infer waiting from trailing ResultMessage)", tk.GetState(), task.StateWaiting)
			}
		})
		t.Run("TrailingMetadataKeepsTrailingResult", func(t *testing.T) {
			t.Parallel()
			// Session and model records are bookkeeping; they must not hide the
			// result that settled the turn.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.ResultMessage{MessageType: "result"},
				&agent.MetaSessionMessage{MessageType: "caic_session", SessionID: "s"},
			}), nil)
			if got := tk.GetState(); got != task.StateWaiting {
				t.Errorf("state = %v, want %v (trailing metadata is not semantic)", got, task.StateWaiting)
			}
		})
		t.Run("RestoresWaitingStartFromResultTime", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			start := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
			setStateAt(tk, task.StateRunning, start)
			tk.SeedTimelineParts(nil, []agent.TimedMessage{
				{Message: &agent.TextMessage{Text: "working"}, ProducerTime: start},
				{Message: &agent.ResultMessage{MessageType: "result", DurationMs: 120000}, ProducerTime: start.Add(2 * time.Minute)},
			}, nil)
			snap := tk.Snapshot()
			if !snap.StateUpdatedAt.Equal(start.Add(2 * time.Minute)) {
				t.Errorf("StateUpdatedAt = %v, want result producer time %v", snap.StateUpdatedAt, start.Add(2*time.Minute))
			}
			if !snap.TurnStartedAt.IsZero() {
				t.Errorf("TurnStartedAt = %v, want zero for a waiting task", snap.TurnStartedAt)
			}
			if snap.Duration != 2*time.Minute {
				t.Errorf("Duration = %v, want 2m", snap.Duration)
			}
		})
		t.Run("RestoresTurnStartAfterPreviousResult", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			start := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
			setStateAt(tk, task.StateRunning, start)
			tk.SeedTimelineParts(nil, []agent.TimedMessage{
				{Message: &agent.TextMessage{Text: "first turn"}, ProducerTime: start},
				{Message: &agent.ResultMessage{MessageType: "result", DurationMs: 60000}, ProducerTime: start.Add(time.Minute)},
				{Message: &agent.UserInputMessage{Text: "next"}, ProducerTime: start.Add(5 * time.Minute)},
				{Message: &agent.TextMessage{Text: "working"}, ProducerTime: start.Add(5*time.Minute + time.Second)},
			}, nil)
			snap := tk.Snapshot()
			if snap.State != task.StateRunning {
				t.Fatalf("state = %v, want running", snap.State)
			}
			want := start.Add(5 * time.Minute)
			if !snap.TurnStartedAt.Equal(want) || !snap.StateUpdatedAt.Equal(want) {
				t.Errorf("turn start = %v, state update = %v, want %v", snap.TurnStartedAt, snap.StateUpdatedAt, want)
			}
		})
		t.Run("RestoresAskingStartFromResultTime", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			start := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
			setStateAt(tk, task.StateRunning, start)
			askAt := start.Add(time.Minute)
			tk.SeedTimelineParts(nil, []agent.TimedMessage{
				{Message: &agent.AskMessage{ToolUseID: "ask1", Questions: []v3.AskQuestion{{Question: "which?"}}}, ProducerTime: askAt},
				{Message: &agent.ResultMessage{MessageType: "result"}, ProducerTime: askAt.Add(10 * time.Second)},
			}, nil)
			snap := tk.Snapshot()
			if snap.State != task.StateAsking {
				t.Fatalf("state = %v, want asking", snap.State)
			}
			// The result settles the asking turn, as on the live path.
			if want := askAt.Add(10 * time.Second); !snap.StateUpdatedAt.Equal(want) {
				t.Errorf("StateUpdatedAt = %v, want result producer time %v", snap.StateUpdatedAt, want)
			}
		})
		t.Run("InfersAsking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "s1"},
				&agent.AskMessage{
					ToolUseID: "ask1",
					Questions: []v3.AskQuestion{{Question: "which?"}},
				},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if tk.GetState() != task.StateAsking {
				t.Errorf("state = %v, want %v (should infer asking from AskMessage + ResultMessage)", tk.GetState(), task.StateAsking)
			}
		})
		t.Run("InfersAskingWithoutResult", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "s1"},
				&agent.AskMessage{
					ToolUseID: "ask1",
					Questions: []v3.AskQuestion{{Question: "which?"}},
				},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if tk.GetState() != task.StateAsking {
				t.Errorf("state = %v, want %v (should infer asking from trailing AskMessage)", tk.GetState(), task.StateAsking)
			}
		})
		t.Run("AnsweredAskInfersWaiting", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.AskMessage{
					ToolUseID: "ask1",
					Questions: []v3.AskQuestion{{Question: "which?"}},
				},
				&agent.ToolResultMessage{ToolUseID: "ask1"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if tk.GetState() != task.StateWaiting {
				t.Errorf("state = %v, want %v (answered AskUserQuestion should not remain asking)", tk.GetState(), task.StateWaiting)
			}
		})
		t.Run("ErroredAskInfersAsking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.AskMessage{
					ToolUseID: "ask1",
					Questions: []v3.AskQuestion{{Question: "which?"}},
				},
				&agent.ToolResultMessage{ToolUseID: "ask1", Error: "permission denied"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if tk.GetState() != task.StateAsking {
				t.Errorf("state = %v, want %v (errored AskUserQuestion result should remain asking)", tk.GetState(), task.StateAsking)
			}
		})
		t.Run("InfersHasPlan", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if tk.GetState() != task.StateHasPlan {
				t.Errorf("state = %v, want %v", tk.GetState(), task.StateHasPlan)
			}
		})
		t.Run("SkipsTrailingDiffStat", func(t *testing.T) {
			t.Parallel()
			// The relay emits DiffStatMessage after the ResultMessage.
			// SeedTimelineParts should skip it and still infer Waiting.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.TextMessage{Text: "hello"},
				&agent.ResultMessage{MessageType: "result"},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{{Path: "main.go", LinesAdded: 1}},
				},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if tk.GetState() != task.StateWaiting {
				t.Errorf("state = %v, want %v (trailing DiffStatMessage should be skipped)", tk.GetState(), task.StateWaiting)
			}
		})
		t.Run("NoResultKeepsState", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "s1"},
				&agent.TextMessage{Text: "hello"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			// No trailing ResultMessage → agent was still producing output.
			if tk.GetState() != task.StateRunning {
				t.Errorf("state = %v, want %v (no ResultMessage → still running)", tk.GetState(), task.StateRunning)
			}
		})
		t.Run("TerminalStatePreserved", func(t *testing.T) {
			t.Parallel()
			for _, state := range []task.State{task.StatePurged, task.StateCrashed, task.StateFailed, task.StatePurging} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, state)
				msgs := []agent.Message{
					&agent.TextMessage{Text: "hello"},
					&agent.ResultMessage{MessageType: "result"},
				}
				tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
				if tk.GetState() != state {
					t.Errorf("state = %v, want %v (terminal state must not be overridden)", tk.GetState(), state)
				}
			}
		})
		t.Run("UsesLastSessionID", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "old"},
				&agent.TextMessage{Text: "hello"},
				&agent.InitMessage{SessionID: "new"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)

			if tk.GetSessionID() != "new" {
				t.Errorf("SessionID = %q, want %q", tk.GetSessionID(), "new")
			}
		})
		t.Run("RestoresPlanFile", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/my-plan.md","content":"plan"}`),
				},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if tk.GetPlanFile() != "/home/user/.claude/plans/my-plan.md" {
				t.Errorf("PlanFile = %q, want %q", tk.GetPlanFile(), "/home/user/.claude/plans/my-plan.md")
			}
		})
		t.Run("RestoresInPlanMode", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{ToolUseID: "tu1", Name: "EnterPlanMode"},
				&agent.ToolUseMessage{
					ToolUseID: "tu2", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/foo.md","content":"x"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu3", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if tk.Snapshot().InPlanMode {
				t.Error("InPlanMode = true, want false (ExitPlanMode should clear it)")
			}
			if tk.GetPlanFile() != "/home/user/.claude/plans/foo.md" {
				t.Errorf("PlanFile = %q, want %q", tk.GetPlanFile(), "/home/user/.claude/plans/foo.md")
			}

			// Without ExitPlanMode, should stay in plan mode.
			tk2 := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk2, task.StateRunning)
			tk2.SeedTimelineParts(nil, agenttest.TimedMessages(msgs[:1]), nil)
			if !tk2.Snapshot().InPlanMode {
				t.Error("InPlanMode = false, want true (only EnterPlanMode seen)")
			}
		})
		t.Run("ContextClearedResetsPlanState", func(t *testing.T) {
			t.Parallel()
			// Simulates relay output containing a plan, then a context_cleared
			// marker (from context boundary on restart), then a new session without
			// a plan. SeedTimelineParts must not carry over the stale plan.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{ToolUseID: "tu1", Name: "EnterPlanMode"},
				&agent.ToolUseMessage{
					ToolUseID: "tu2", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"old plan"}`),
				},
				&agent.ResultMessage{MessageType: "result"},
				// context_cleared injected by context boundary on restart.
				&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
				// New session starts — no plan tools used.
				&agent.TextMessage{Text: "done"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			snap := tk.Snapshot()
			if snap.InPlanMode {
				t.Error("InPlanMode = true, want false (context_cleared should reset)")
			}
			if snap.PlanContent != "" {
				t.Errorf("PlanContent = %q, want empty (context_cleared should reset)", snap.PlanContent)
			}
			if tk.GetPlanFile() != "" {
				t.Errorf("PlanFile = %q, want empty (context_cleared should reset)", tk.GetPlanFile())
			}
		})
		t.Run("ContextClearedSuppressesPlanRewrite", func(t *testing.T) {
			t.Parallel()
			// After "Clear and execute plan", the agent may re-enter plan mode
			// and write to .claude/plans/ during execution. The dismissed plan
			// must not resurface when the turn completes.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				// Original plan.
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"old plan"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
				// User clicked "Clear and execute plan".
				&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
				// Agent re-enters plan mode during execution.
				&agent.ToolUseMessage{ToolUseID: "tu3", Name: "EnterPlanMode"},
				&agent.ToolUseMessage{
					ToolUseID: "tu4", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"new plan"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu5", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			snap := tk.Snapshot()
			if snap.PlanContent != "" {
				t.Errorf("PlanContent = %q, want empty (plan written after context_cleared should be suppressed)", snap.PlanContent)
			}
			if tk.GetPlanFile() != "" {
				t.Errorf("PlanFile = %q, want empty", tk.GetPlanFile())
			}
		})
		t.Run("ContextClearedClearsExitPlanModePlanContent", func(t *testing.T) {
			t.Parallel()
			// context_cleared in history must clear the plan projection on
			// preceding ExitPlanMode events so new subscribers see no stale
			// plan.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"old plan"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
				&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
				&agent.TextMessage{Text: "done"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if got := tk.Snapshot().PlanExitID; got == "tu2" {
				t.Errorf("superseded plan owner = %q", got)
			}
		})
		t.Run("PlanUpdateClearsPreviousExitPlanModePlanContent", func(t *testing.T) {
			t.Parallel()
			// When a plan is updated (two ExitPlanMode without context_cleared),
			// the projection must follow only the latest ExitPlanMode.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v1"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
				&agent.ToolUseMessage{
					ToolUseID: "tu3", Name: "EnterPlanMode",
				},
				&agent.ToolUseMessage{
					ToolUseID: "tu4", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v2"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu5", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			if got := tk.Snapshot().PlanExitID; got == "tu2" {
				t.Errorf("superseded plan owner = %q", got)
			}
			if snap := tk.Snapshot(); snap.PlanExitID != "tu5" || snap.PlanContent != "plan v2" {
				t.Errorf("current plan = (%q, %q), want (tu5, plan v2)", snap.PlanExitID, snap.PlanContent)
			}
		})
		t.Run("FoldingTheSameSliceTwiceIsIdempotent", func(t *testing.T) {
			t.Parallel()
			// The same slice is folded into two fresh tasks, as restore and a
			// replay would do. The fold must produce equivalent state and
			// leave the shared messages unmodified.
			msgs := []agent.Message{
				&agent.ToolUseMessage{ToolUseID: "tu1", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan A"}`)},
				&agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
				&agent.ToolUseMessage{ToolUseID: "tu3", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan B"}`)},
				&agent.ToolUseMessage{ToolUseID: "tu4", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result", Result: "done"},
				&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
				&agent.TextMessage{Text: "after clear"},
				&agent.ResultMessage{MessageType: "result", Result: "final"},
			}
			before, err := json.Marshal(msgs)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			a := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			b := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			a.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			afterFirst, err := json.Marshal(msgs)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			b.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)
			afterSecond, err := json.Marshal(msgs)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(before, afterFirst) {
				t.Errorf("first fold mutated the shared slice")
			}
			if !bytes.Equal(before, afterSecond) {
				t.Errorf("second fold mutated the shared slice")
			}
			as, bs := a.Snapshot(), b.Snapshot()
			if as.State != bs.State {
				t.Errorf("state = %v after second fold, want %v (first)", bs.State, as.State)
			}
			if as.Title != bs.Title {
				t.Errorf("title = %q after second fold, want %q (first)", bs.Title, as.Title)
			}
			if as.InPlanMode != bs.InPlanMode {
				t.Errorf("InPlanMode = %v after second fold, want %v (first)", bs.InPlanMode, as.InPlanMode)
			}
			if as.PlanFile != bs.PlanFile {
				t.Errorf("PlanFile = %q after second fold, want %q (first)", bs.PlanFile, as.PlanFile)
			}
			if as.PlanContent != bs.PlanContent {
				t.Errorf("PlanContent = %q after second fold, want %q (first)", bs.PlanContent, as.PlanContent)
			}
			if as.PlanExitID != bs.PlanExitID {
				t.Errorf("PlanExitID differs between folds: %q vs %q", as.PlanExitID, bs.PlanExitID)
			}
		})
		t.Run("NativeSubagentState", func(t *testing.T) {
			t.Parallel()
			native := func(id string, status agent.NativeSubagentStatus) *agent.NativeSubagentMessage {
				return &agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: id, Status: status, Background: true}}
			}
			foreground := func(id string, status agent.NativeSubagentStatus) *agent.NativeSubagentMessage {
				return &agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: id, Status: status}}
			}
			result := func() *agent.ResultMessage { return &agent.ResultMessage{MessageType: "result"} }

			t.Run("SeedTimelineInfersRunningForBackgroundSubagent", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				start := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
				resultAt := start.Add(5 * time.Minute)
				setStateAt(tk, task.StateRunning, start)
				tk.SeedTimelineParts(nil, []agent.TimedMessage{
					{Message: result(), ProducerTime: resultAt},
					{Message: native("child-1", agent.NativeSubagentStatusRunning), ProducerTime: resultAt.Add(time.Second)},
				}, nil)
				if got := tk.GetState(); got != task.StateRunning {
					t.Errorf("state = %v, want %v (a background subagent is still active)", got, task.StateRunning)
				}
				// The task waited from the result until the subagent's update resumed it.
				snap := tk.Snapshot()
				if want := resultAt.Add(time.Second); !snap.TurnStartedAt.Equal(want) || !snap.StateUpdatedAt.Equal(want) {
					t.Errorf("turn start = %v, state update = %v, want subagent update time %v", snap.TurnStartedAt, snap.StateUpdatedAt, want)
				}
			})
			t.Run("SeedTimelineInfersWaitingForForegroundSubagent", func(t *testing.T) {
				t.Parallel()
				// A foreground card still running at the parent result is stale or
				// unread evidence, not detached work.
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
					result(),
					foreground("child-1", agent.NativeSubagentStatusRunning),
				}), nil)
				if got := tk.GetState(); got != task.StateWaiting {
					t.Errorf("state = %v, want %v (a foreground card cannot outlive the turn)", got, task.StateWaiting)
				}
			})
			t.Run("SeedTimelineInfersWaitingForSettledSubagent", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
					result(),
					native("child-1", agent.NativeSubagentStatusRunning),
					native("child-1", agent.NativeSubagentStatusCompleted),
				}), nil)
				if got := tk.GetState(); got != task.StateWaiting {
					t.Errorf("state = %v, want %v (the trailing native update settled the child)", got, task.StateWaiting)
				}
			})
			t.Run("SeedTimelineFallsBackToWaitingForPausedSubagent", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
					result(),
					native("child-1", agent.NativeSubagentStatusPaused),
				}), nil)
				if got := tk.GetState(); got != task.StateWaiting {
					t.Errorf("state = %v, want %v (paused evidence is not active)", got, task.StateWaiting)
				}
			})
			t.Run("ResultKeepsRunningWhileBackgroundSubagentActive", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				addTaskMessage(tk, native("child-1", agent.NativeSubagentStatusRunning))
				addTaskMessage(tk, result())
				if got := tk.GetState(); got != task.StateRunning {
					t.Errorf("state = %v, want %v after the parent turn ended with a background subagent", got, task.StateRunning)
				}
				addTaskMessage(tk, native("child-1", agent.NativeSubagentStatusCompleted))
				if got := tk.GetState(); got != task.StateWaiting {
					t.Errorf("state = %v, want %v after the last subagent settled", got, task.StateWaiting)
				}
			})
			t.Run("HostExitSettlesBackgroundSubagent", func(t *testing.T) {
				t.Parallel()
				// The harness process hosts its subagents: after it exits, no
				// lifecycle report can arrive, so the task must not stay running.
				exit := func() *agent.ExitMessage { return &agent.ExitMessage{MessageType: "exit", ExitCode: -1, Signal: 6} }
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(live, task.StateRunning)
				addTaskMessage(live, native("child-1", agent.NativeSubagentStatusRunning))
				addTaskMessage(live, result())
				addTaskMessage(live, exit())
				if got := live.GetState(); got != task.StateWaiting {
					t.Errorf("live state = %v, want %v after the host exited", got, task.StateWaiting)
				}
			})
			t.Run("ResultAnchorsLiveDeltaAtResult", func(t *testing.T) {
				t.Parallel()
				// Duration covers the turn that just settled, so the live delta must
				// start at its result instead of the turn that ended.
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				start := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
				resultAt := start.Add(time.Minute)
				setStateAt(tk, task.StateRunning, start)
				tk.addParsedMessage(agent.TimedMessage{ProducerTime: start, Message: native("child-1", agent.NativeSubagentStatusRunning)})
				tk.addParsedMessage(agent.TimedMessage{ProducerTime: resultAt, Message: &agent.ResultMessage{MessageType: "result", DurationMs: 60000}})
				snap := tk.Snapshot()
				if snap.State != task.StateRunning {
					t.Fatalf("state = %v, want running", snap.State)
				}
				if !snap.TurnStartedAt.Equal(resultAt) {
					t.Errorf("TurnStartedAt = %v, want result time %v", snap.TurnStartedAt, resultAt)
				}
				if snap.Duration != time.Minute {
					t.Errorf("Duration = %v, want 1m", snap.Duration)
				}
			})
			t.Run("ForegroundRunningCardFallsBackToWaiting", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				addTaskMessage(tk, foreground("child-1", agent.NativeSubagentStatusRunning))
				addTaskMessage(tk, result())
				if got := tk.GetState(); got != task.StateWaiting {
					t.Errorf("state = %v, want %v (unclear foreground card falls back)", got, task.StateWaiting)
				}
			})
			t.Run("LateBackgroundSubagentKeepsTaskRunning", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				addTaskMessage(tk, result())
				addTaskMessage(tk, native("child-1", agent.NativeSubagentStatusRunning))
				if got := tk.GetState(); got != task.StateRunning {
					t.Errorf("state = %v, want %v when the child is reported running after the parent settled", got, task.StateRunning)
				}
			})
			t.Run("UnansweredAskWinsOverRunningSubagent", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				addTaskMessage(tk, &agent.AskMessage{ToolUseID: "ask1", Questions: []v3.AskQuestion{{Question: "which?"}}})
				addTaskMessage(tk, native("child-1", agent.NativeSubagentStatusRunning))
				addTaskMessage(tk, result())
				if got := tk.GetState(); got != task.StateAsking {
					t.Errorf("state = %v, want %v (the user still owes an answer)", got, task.StateAsking)
				}
			})
			t.Run("NativeUpdateKeepsTrailingExitSpurious", func(t *testing.T) {
				t.Parallel()
				// A child lifecycle update between the clean result and the exit does
				// not turn the exit into a real error.
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(live, task.StateRunning)
				addTaskMessage(live, result())
				addTaskMessage(live, native("child-1", agent.NativeSubagentStatusRunning))
				addTaskMessage(live, native("child-1", agent.NativeSubagentStatusCompleted))
				addTaskMessage(live, &agent.ExitMessage{ExitCode: -2})
				if got := live.LastExitError(); got != "" {
					t.Errorf("live LastExitError = %q, want the exit dropped after a clean turn", got)
				}
				if got := live.GetState(); got != task.StateWaiting {
					t.Errorf("live state = %v, want %v", got, task.StateWaiting)
				}
			})
		})
		t.Run("BackgroundCommandState", func(t *testing.T) {
			t.Parallel()
			// Detached checks keep the task busy even when the agent turn ends.
			// command builds a card whose end wakes the agent, as Claude Code's do.
			command := func(id string, status agent.BackgroundCommandStatus) *agent.BackgroundCommandMessage {
				return &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: id, Status: status, WakesAgent: true}}
			}
			result := func() *agent.ResultMessage { return &agent.ResultMessage{MessageType: "result"} }
			t.Run("ShellOutlivesSubagentAndHostExitRetiresIt", func(t *testing.T) {
				t.Parallel()
				messages := []agent.Message{
					&agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: "child", Status: agent.NativeSubagentStatusRunning, Background: true}},
					command("shell", agent.BackgroundCommandStatusRunning),
					result(),
					&agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: "child", Status: agent.NativeSubagentStatusCompleted}},
					&agent.ExitMessage{ExitCode: -1},
				}
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(live, task.StateRunning)
				for _, m := range messages[:len(messages)-1] {
					addTaskMessage(live, m)
				}
				if got := live.GetState(); got != task.StateRunning {
					t.Fatalf("state = %v, want running while the child's shell still runs", got)
				}
				addTaskMessage(live, messages[len(messages)-1])
				if live.GetState() != task.StateWaiting || live.backgroundCommands.Commands()[0].Status != agent.BackgroundCommandStatusUnknown {
					t.Fatalf("state = %v, commands = %#v after host exit", live.GetState(), live.backgroundCommands.Commands())
				}
			})
			t.Run("CommandBeforeResultAndFailureSettlement", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				addTaskMessage(tk, command("shell", agent.BackgroundCommandStatusRunning))
				addTaskMessage(tk, result())
				if got := tk.GetState(); got != task.StateRunning {
					t.Fatalf("state = %v, want running while the shell runs", got)
				}
				addTaskMessage(tk, command("shell", agent.BackgroundCommandStatusFailed))
				if got := tk.GetState(); got != task.StateWaiting {
					t.Fatalf("state = %v, want waiting after the shell failed", got)
				}
			})
			t.Run("SameStateLifecycleKeepsStateClock", func(t *testing.T) {
				t.Parallel()
				for _, state := range []task.State{task.StateRunning, task.StateWaiting} {
					t.Run(string(state), func(t *testing.T) {
						t.Parallel()
						tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
						setState(tk, task.StateRunning)
						if state == task.StateRunning {
							addTaskMessage(tk, command("shell", agent.BackgroundCommandStatusRunning))
						}
						addTaskMessage(tk, result())
						at := tk.Snapshot().StateUpdatedAt
						addTaskMessage(tk, &agent.NativeSubagentMessage{Subagent: agent.NativeSubagent{ID: "foreground", Status: agent.NativeSubagentStatusCompleted}})
						if state == task.StateWaiting {
							addTaskMessage(tk, &agent.ExitMessage{ExitCode: 0})
						} else {
							addTaskMessage(tk, command("shell", agent.BackgroundCommandStatusRunning))
						}
						if snap := tk.Snapshot(); snap.State != state || !snap.StateUpdatedAt.Equal(at) {
							t.Fatalf("state = %v at %v, want %v at %v", snap.State, snap.StateUpdatedAt, state, at)
						}
					})
				}
			})

			t.Run("SeedTimelineKeepsRunningForRunningCommand", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
					result(),
					command("shell-1", agent.BackgroundCommandStatusRunning),
				}), nil)
				if got := tk.GetState(); got != task.StateRunning {
					t.Errorf("state = %v, want running while a detached command runs", got)
				}
			})
			t.Run("RunningWatchAfterResultWaits", func(t *testing.T) {
				t.Parallel()
				watch := command("watch", agent.BackgroundCommandStatusRunning)
				watch.Command.Watch = true
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				addTaskMessage(tk, watch)
				addTaskMessage(tk, result())
				if got := tk.GetState(); got != task.StateWaiting {
					t.Fatalf("state = %v, want waiting while only a watch runs", got)
				}
			})
			t.Run("LateRunningCommandAfterResultKeepsRunning", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				addTaskMessage(tk, result())
				addTaskMessage(tk, command("shell-1", agent.BackgroundCommandStatusRunning))
				if got := tk.GetState(); got != task.StateRunning {
					t.Errorf("state = %v, want running after the parent settled", got)
				}
				addTaskMessage(tk, command("shell-1", agent.BackgroundCommandStatusCompleted))
				if got := tk.GetState(); got != task.StateWaiting {
					t.Errorf("state = %v, want waiting after the command settled", got)
				}
			})
			t.Run("CommandUpdateBetweenResultAndExitKeepsExitSpurious", func(t *testing.T) {
				t.Parallel()
				// A command update between the clean result and the exit does not turn
				// the exit into a real error.
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(live, task.StateRunning)
				addTaskMessage(live, result())
				addTaskMessage(live, command("shell-1", agent.BackgroundCommandStatusCompleted))
				addTaskMessage(live, &agent.ExitMessage{ExitCode: -2})
				if got := live.LastExitError(); got != "" {
					t.Errorf("live LastExitError = %q, want the exit dropped after a clean turn", got)
				}
			})
		})
		t.Run("QueuedTurnState", func(t *testing.T) {
			t.Parallel()
			// Minimized from a task log: when background work ends after the
			// parent result, Claude Code starts a turn to deliver its notification.
			// The task stays running from the terminal update through that turn.
			const (
				session  = `"session_id":"fe242f82"`
				text     = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done"}]},` + session + `}`
				result   = `{"type":"result","subtype":"success","is_error":false,"result":"Done",` + session + `}`
				init     = `{"type":"system","subtype":"init","cwd":"/src",` + session + `}`
				notified = `{"type":"result","subtype":"success","is_error":false,"result":"Done","origin":{"kind":"task-notification","producer":"session-task"},` + session + `}`
				// prompt is an input record, the user's prompt that caic delivered;
				// parseWire parses it as input.
				prompt = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Also update the docs"}]}}`
			)
			// parseWire parses native input and output lines with one wire, one second apart.
			parseWire := func(t *testing.T, wire agent.WireFormat, lines ...string) []agent.TimedMessage {
				start := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
				var entries []agent.TimedMessage
				for i, line := range lines {
					parseLine := wire.ParseOutput
					if line == prompt {
						parseLine = wire.ParseInput
					}
					msgs, err := parseLine([]byte(line))
					if err != nil {
						t.Fatalf("line %d: %v", i, err)
					}
					for _, m := range msgs {
						entries = append(entries, agent.TimedMessage{Message: m, ProducerTime: start.Add(time.Duration(i) * time.Second)})
					}
				}
				return entries
			}
			parse := func(t *testing.T, lines ...string) []agent.TimedMessage {
				return parseWire(t, claudecode.New().NewWire(), lines...)
			}
			// fold returns the state and background counts after each entry.
			fold := func(t *testing.T, entries []agent.TimedMessage) ([]task.State, []BackgroundCounts) {
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(live, task.StateRunning)
				states := make([]task.State, len(entries))
				counts := make([]BackgroundCounts, len(entries))
				for i, entry := range entries {
					live.addParsedMessage(entry)
					snap := live.Snapshot()
					states[i], counts[i] = snap.State, snap.Background
				}
				return states, counts
			}
			// countsAt returns the counts at the first root result and at the end.
			countsAt := func(entries []agent.TimedMessage, counts []BackgroundCounts) (atResult, final BackgroundCounts) {
				i := slices.IndexFunc(entries, func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.ResultMessage); return ok })
				return counts[i], counts[len(counts)-1]
			}
			// runsUntilQueuedTurn checks that no waiting state appears between
			// the first root result and the queued turn's result, which settles
			// the task.
			runsUntilQueuedTurn := func(t *testing.T, entries []agent.TimedMessage, states []task.State) {
				var results []int
				for i, entry := range entries {
					if r, ok := entry.Message.(*agent.ResultMessage); ok && !r.Source.IsChild() {
						results = append(results, i)
					}
				}
				if len(results) != 2 {
					t.Fatalf("root results at %v, want 2", results)
				}
				for i := results[0]; i < results[1]; i++ {
					if states[i] != task.StateRunning {
						t.Errorf("entry %d (%T): state = %v, want running until the queued turn's result", i, entries[i].Message, states[i])
					}
				}
				if got := states[results[1]]; got != task.StateWaiting {
					t.Errorf("state after the queued turn = %v, want waiting", got)
				}
			}
			agentUse := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_spawn","name":"Agent","input":{"description":"Fix","prompt":"Fix","subagent_type":"general-purpose","run_in_background":true}}]},` + session + `}`
			agentStarted := `{"type":"system","subtype":"task_started","task_id":"a3635a","run_id":"001","tool_use_id":"toolu_spawn","description":"Fix","is_backgrounded":true,"task_type":"local_agent",` + session + `}`
			agentLaunched := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_spawn","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"tool_use_result":{"status":"async_launched"},` + session + `}`
			agentUpdated := `{"type":"system","subtype":"task_updated","task_id":"a3635a","run_id":"001","patch":{"status":"completed"},` + session + `}`
			agentNotified := `{"type":"system","subtype":"task_notification","task_id":"a3635a","run_id":"001","tool_use_id":"toolu_spawn","status":"completed","summary":"Fixed",` + session + `}`
			t.Run("BackgroundAgent", func(t *testing.T) {
				t.Parallel()
				entries := parse(t, agentUse, agentStarted, agentLaunched, text, result, agentUpdated, agentNotified, init, text, notified)
				states, counts := fold(t, entries)
				runsUntilQueuedTurn(t, entries, states)
				if atResult, final := countsAt(entries, counts); atResult != (BackgroundCounts{Agents: 1}) || final != (BackgroundCounts{}) {
					t.Errorf("counts at result = %+v, final = %+v, want 1 agent then none", atResult, final)
				}
			})
			t.Run("NotificationDuringFinalRequest", func(t *testing.T) {
				t.Parallel()
				// Minimized from a task log: the background agent ended while
				// the parent's last model request streamed. No tool result followed,
				// so Claude Code started a turn 26 ms after the result.
				snapshot := `{"type":"system","subtype":"background_tasks_changed","tasks":[],` + session + `}`
				entries := parse(t, init, agentUse, agentStarted, agentLaunched, agentUpdated, agentNotified, snapshot, text, result, init, text, notified)
				states, counts := fold(t, entries)
				runsUntilQueuedTurn(t, entries, states)
				if atResult, final := countsAt(entries, counts); atResult != (BackgroundCounts{}) || final != (BackgroundCounts{}) {
					t.Errorf("counts at result = %+v, final = %+v, want none", atResult, final)
				}
			})
			t.Run("PromptDuringFinalRequest", func(t *testing.T) {
				t.Parallel()
				// Minimized from a task log: the user's prompt arrived while the
				// final model request streamed. No tool result followed, so Claude
				// Code started a turn for the prompt 23 ms after the result.
				entries := parse(t, init, prompt, text, result, init, text, result)
				states, _ := fold(t, entries)
				runsUntilQueuedTurn(t, entries, states)
			})
			t.Run("PromptBeforeToolResult", func(t *testing.T) {
				t.Parallel()
				// A later model request carries a prompt that a tool result follows,
				// so the turn's result settles the task.
				use := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ls","name":"Bash","input":{"command":"ls"}}]},` + session + `}`
				reply := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_ls","type":"tool_result","content":"ok"}]},` + session + `}`
				states, _ := fold(t, parse(t, init, use, prompt, reply, text, result))
				if got := states[len(states)-1]; got != task.StateWaiting {
					t.Errorf("state after the result = %v, want waiting", got)
				}
			})
			t.Run("UnflaggedTurnStartsAtInit", func(t *testing.T) {
				t.Parallel()
				// A turn the harness starts without reported evidence is running from
				// its init.
				entries := parse(t, init, text, result, init)
				states, _ := fold(t, entries)
				results := slices.IndexFunc(entries, func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.ResultMessage); return ok })
				if got := states[results]; got != task.StateWaiting {
					t.Errorf("state after the result = %v, want waiting", got)
				}
				if got := states[len(states)-1]; got != task.StateRunning {
					t.Errorf("state after init = %v, want running", got)
				}
			})
			t.Run("NotificationBeforeToolResult", func(t *testing.T) {
				t.Parallel()
				// Minimized from a task log: a background command ended while a
				// foreground command ran. The foreground tool result carried the
				// notification to the model, and no turn followed the result.
				bgUse := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bg","name":"Bash","input":{"command":"make test","run_in_background":true}}]},` + session + `}`
				bgStarted := `{"type":"system","subtype":"task_started","task_id":"but6vcll2","run_id":"0mwindp4h","tool_use_id":"toolu_bg","description":"Run fix, verify, test","is_backgrounded":true,"task_type":"local_bash",` + session + `}`
				bgLaunched := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_bg","type":"tool_result","content":"Command running in background with ID: but6vcll2"}]},` + session + `}`
				fgUse := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_fg","name":"Bash","input":{"command":"wait-for-gates"}}]},` + session + `}`
				fgStarted := `{"type":"system","subtype":"task_started","task_id":"bf0in983f","run_id":"0mwinex1n","tool_use_id":"toolu_fg","description":"Wait for host gate results","is_backgrounded":false,"task_type":"local_bash",` + session + `}`
				bgUpdated := `{"type":"system","subtype":"task_updated","task_id":"but6vcll2","run_id":"0mwindp4h","patch":{"status":"completed"},` + session + `}`
				bgNotified := `{"type":"system","subtype":"task_notification","task_id":"but6vcll2","run_id":"0mwindp4h","tool_use_id":"toolu_bg","status":"completed","summary":"Background command \"Run fix, verify, test\" completed (exit code 0)",` + session + `}`
				snapshot := `{"type":"system","subtype":"background_tasks_changed","tasks":[],` + session + `}`
				fgNotified := `{"type":"system","subtype":"task_notification","task_id":"bf0in983f","run_id":"0mwinex1n","tool_use_id":"toolu_fg","status":"completed","summary":"Wait for host gate results",` + session + `}`
				fgResult := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_fg","type":"tool_result","content":"caic test exit=0","is_error":false}]},` + session + `}`
				entries := parse(t, init, bgUse, bgStarted, bgLaunched, fgUse, fgStarted, bgUpdated, bgNotified, snapshot, fgNotified, fgResult, text, result)
				states, _ := fold(t, entries)
				if got := states[len(states)-1]; got != task.StateWaiting {
					t.Errorf("state after the result = %v, want waiting", got)
				}
			})
			t.Run("BackgroundCommand", func(t *testing.T) {
				t.Parallel()
				bashUse := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"make test","run_in_background":true}}]},` + session + `}`
				started := `{"type":"system","subtype":"task_started","task_id":"b3xwrrd6m","tool_use_id":"toolu_bash","description":"Run tests","is_backgrounded":true,"task_type":"local_bash",` + session + `}`
				backgrounded := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_bash","type":"tool_result","content":"Command running in background with ID: b3xwrrd6m"}]},` + session + `}`
				updated := `{"type":"system","subtype":"task_updated","task_id":"b3xwrrd6m","patch":{"status":"completed"},` + session + `}`
				notifiedCmd := `{"type":"system","subtype":"task_notification","task_id":"b3xwrrd6m","tool_use_id":"toolu_bash","status":"completed","summary":"Background command \"Run tests\" completed (exit code 0)",` + session + `}`
				entries := parse(t, bashUse, started, backgrounded, text, result, updated, notifiedCmd, init, text, notified)
				// Claude Code wakes the agent when the command ends, so the command
				// keeps the task running until that turn.
				for _, e := range entries {
					if m, ok := e.Message.(*agent.BackgroundCommandMessage); ok && !m.Command.WakesAgent {
						t.Fatalf("command = %#v, want it to wake the agent", m.Command)
					}
				}
				states, counts := fold(t, entries)
				runsUntilQueuedTurn(t, entries, states)
				if atResult, final := countsAt(entries, counts); atResult != (BackgroundCounts{Commands: 1}) || final != (BackgroundCounts{}) {
					t.Errorf("counts at result = %+v, final = %+v, want 1 command then none", atResult, final)
				}
			})
			t.Run("WatchExpiryWakesWaitingTask", func(t *testing.T) {
				t.Parallel()
				monitorUse := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_watch","name":"Monitor","input":{"command":"tail -f out","description":"results"}}]},` + session + `}`
				started := `{"type":"system","subtype":"task_started","task_id":"bizvx8b6o","tool_use_id":"toolu_watch","description":"results","is_backgrounded":true,"task_type":"local_bash",` + session + `}`
				killed := `{"type":"system","subtype":"task_updated","task_id":"bizvx8b6o","patch":{"status":"killed"},` + session + `}`
				stopped := `{"type":"system","subtype":"task_notification","task_id":"bizvx8b6o","tool_use_id":"toolu_watch","status":"stopped","summary":"results",` + session + `}`
				entries := parse(t, monitorUse, started, text, result, killed, stopped, init, text, notified)
				states, counts := fold(t, entries)
				if atResult, final := countsAt(entries, counts); atResult != (BackgroundCounts{Watches: 1}) || final != (BackgroundCounts{}) {
					t.Errorf("counts at result = %+v, final = %+v, want 1 watch then none", atResult, final)
				}
				if got := states[slices.IndexFunc(entries, func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.ResultMessage); return ok })]; got != task.StateWaiting {
					t.Fatalf("state with only a watch running = %v, want waiting", got)
				}
				results := slices.IndexFunc(entries, func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.ResultMessage); return ok })
				queued := slices.IndexFunc(entries[results:], func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.TurnStartedMessage); return ok })
				if queued < 0 {
					t.Fatal("watch expiry started no turn")
				}
				queued += results
				for i := queued; i < len(entries)-1; i++ {
					if states[i] != task.StateRunning {
						t.Errorf("entry %d (%T): state = %v, want running until the notification turn's result", i, entries[i].Message, states[i])
					}
				}
				if got := states[len(states)-1]; got != task.StateWaiting {
					t.Errorf("final state = %v, want waiting", got)
				}
			})
			t.Run("HostExitClearsQueuedTurn", func(t *testing.T) {
				t.Parallel()
				// The agent settled while Claude was idle, which starts a turn; the
				// harness exited before that turn's init.
				entries := parse(t, agentUse, agentStarted, agentLaunched, text, result, agentUpdated)
				entries = append(entries, agent.TimedMessage{Message: &agent.ExitMessage{ExitCode: -1}})
				states, _ := fold(t, entries)
				if got := states[len(states)-2]; got != task.StateRunning {
					t.Errorf("state before the exit = %v, want running for the queued turn", got)
				}
				if got := states[len(states)-1]; got != task.StateWaiting {
					t.Errorf("state after the host exit = %v, want waiting", got)
				}
			})
			t.Run("HostExitEndsStartedTurn", func(t *testing.T) {
				t.Parallel()
				// Pi started a run and exited before any output.
				const (
					agentStart = `{"type":"agent_start"}`
					prompt     = `{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"Start"}]}}`
					agentEnd   = `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"Done"}],"usage":{"input":10,"output":5}}]}`
				)
				entries := parseWire(t, pi.New("", nil).NewWire(), agentStart, prompt, agentEnd, agentStart)
				entries = append(entries, agent.TimedMessage{Message: &agent.ExitMessage{ExitCode: 0}})
				states, _ := fold(t, entries)
				if got := states[len(states)-2]; got != task.StateRunning {
					t.Errorf("state after the run start = %v, want running", got)
				}
				if got := states[len(states)-1]; got != task.StateWaiting {
					t.Errorf("state after the host exit = %v, want waiting", got)
				}
			})
			t.Run("HostExitClearsCounts", func(t *testing.T) {
				t.Parallel()
				bashUse := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"make test","run_in_background":true}}]},` + session + `}`
				bashStarted := `{"type":"system","subtype":"task_started","task_id":"b3xwrrd6m","tool_use_id":"toolu_bash","description":"Run tests","is_backgrounded":true,"task_type":"local_bash",` + session + `}`
				entries := parse(t, agentUse, agentStarted, agentLaunched, bashUse, bashStarted, text, result)
				entries = append(entries, agent.TimedMessage{Message: &agent.ExitMessage{ExitCode: -1}})
				_, counts := fold(t, entries)
				if got := counts[len(counts)-2]; got != (BackgroundCounts{Commands: 1, Agents: 1}) {
					t.Errorf("counts before the exit = %+v, want 1 command and 1 agent", got)
				}
				if got := counts[len(counts)-1]; got != (BackgroundCounts{}) {
					t.Errorf("counts after the host exit = %+v, want none", got)
				}
			})
			t.Run("CountOnlyChangeRefreshesSummary", func(t *testing.T) {
				t.Parallel()
				start := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				setState(tk, task.StateRunning)
				tk.addParsedMessage(agent.TimedMessage{Message: &agent.TextMessage{Text: "working"}})
				watch := &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "w", Label: "results", Status: agent.BackgroundCommandStatusRunning, Watch: true}}
				if changed := tk.addParsedMessage(agent.TimedMessage{ProducerTime: start, Message: watch}); !changed {
					t.Fatal("a new watch did not report a summary change")
				}
				cmd := &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: "c", Label: "Run tests", Status: agent.BackgroundCommandStatusRunning, WakesAgent: true}}
				if changed := tk.addParsedMessage(agent.TimedMessage{ProducerTime: start.Add(time.Second), Message: cmd}); !changed {
					t.Fatal("a new command did not report a summary change")
				}
				if changed := tk.addParsedMessage(agent.TimedMessage{ProducerTime: start.Add(2 * time.Second), Message: cmd}); changed {
					t.Fatal("a repeated observation reported a summary change")
				}
				snap := tk.Snapshot()
				if snap.State != task.StateRunning || snap.Background != (BackgroundCounts{Commands: 1, Watches: 1}) {
					t.Fatalf("snapshot = %v %+v, want running with 1 command and 1 watch", snap.State, snap.Background)
				}
				want := []BackgroundItem{
					{Kind: BackgroundKindWatch, Label: "results", StartedAt: start},
					{Kind: BackgroundKindCommand, Label: "Run tests", StartedAt: start.Add(time.Second)},
				}
				if got := tk.BackgroundActivity(); !slices.Equal(got.Items, want) || got.Counts != snap.Background || got.TurnEnded {
					t.Fatalf("activity = %+v, want %+v during the turn", got, want)
				}
				tk.addParsedMessage(agent.TimedMessage{Message: &agent.ResultMessage{MessageType: "result"}})
				if got := tk.BackgroundActivity(); !got.TurnEnded || tk.GetState() != task.StateRunning {
					t.Fatalf("turn ended %t, state %v, want an ended turn kept running", got.TurnEnded, tk.GetState())
				}
			})
			t.Run("CodexCommandOutlivesTurn", func(t *testing.T) {
				t.Parallel()
				// Minimized from a task log: the agent ended its turn while its
				// smoke test still ran; Codex reported the exit later and started no
				// turn, so the command shows but the task waits for the user.
				const (
					thread    = `{"method":"thread/started","params":{"thread":{"id":"thread-root"}}}`
					started   = `{"method":"item/started","params":{"item":{"type":"commandExecution","id":"exec-smoke","command":"/bin/bash -lc 'go test -tags=smoke ./...'","source":"unifiedExecStartup","status":"inProgress"},"threadId":"thread-root","turnId":"turn-1"}}`
					answer    = `{"method":"item/completed","params":{"item":{"type":"agentMessage","id":"msg-1","text":"The smoke suite is still running."},"threadId":"thread-root","turnId":"turn-1"}}`
					idle      = `{"method":"thread/status/changed","params":{"threadId":"thread-root","status":{"type":"idle"}}}`
					turnDone  = `{"method":"turn/completed","params":{"threadId":"thread-root","turn":{"id":"turn-1","status":"completed"}}}`
					output    = `{"method":"item/commandExecution/outputDelta","params":{"threadId":"thread-root","turnId":"turn-1","itemId":"exec-smoke","delta":"ok"}}`
					completed = `{"method":"item/completed","params":{"item":{"type":"commandExecution","id":"exec-smoke","command":"/bin/bash -lc 'go test -tags=smoke ./...'","source":"unifiedExecStartup","status":"completed","aggregatedOutput":"ok","exitCode":0},"threadId":"thread-root","turnId":"turn-1"}}`
				)
				entries := parseWire(t, codex.New("", nil).NewWire(), thread, started, answer, idle, turnDone, output, completed)
				states, counts := fold(t, entries)
				result := slices.IndexFunc(entries, func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.ResultMessage); return ok })
				command := slices.IndexFunc(entries, func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.BackgroundCommandMessage); return ok })
				if command <= result {
					t.Fatalf("command card at %d, result at %d, want the card after the result", command, result)
				}
				// The late output and tool result precede the command's completion
				// and must not reopen the settled turn.
				for i := result; i < len(entries); i++ {
					if states[i] != task.StateWaiting {
						t.Errorf("entry %d (%T): state = %v, want waiting", i, entries[i].Message, states[i])
					}
					if want := (BackgroundCounts{Commands: 1}); i >= command && i < len(entries)-1 && counts[i] != want {
						t.Errorf("entry %d (%T): counts = %+v, want %+v while the command runs", i, entries[i].Message, counts[i], want)
					}
				}
				if !slices.ContainsFunc(entries[result:], func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.ToolOutputDeltaMessage); return ok }) {
					t.Fatal("no late tool output")
				}
				if _, ok := entries[len(entries)-2].Message.(*agent.ToolResultMessage); !ok {
					t.Fatalf("entry %d = %T, want the late tool result", len(entries)-2, entries[len(entries)-2].Message)
				}
				if got := counts[len(counts)-1]; got != (BackgroundCounts{}) {
					t.Errorf("counts after the command exit = %+v, want none", got)
				}
			})
			t.Run("SettlementEndsHostedCommand", func(t *testing.T) {
				t.Parallel()
				// Minimized from a task log: the log has no exit record and ends
				// with the purge trailer while a Codex process was last seen running.
				const (
					thread   = `{"method":"thread/started","params":{"thread":{"id":"thread-root"}}}`
					started  = `{"method":"item/started","params":{"item":{"type":"commandExecution","id":"exec-serve","command":"npm run dev","source":"unifiedExecStartup","status":"inProgress"},"threadId":"thread-root","turnId":"turn-1"}}`
					turnDone = `{"method":"turn/completed","params":{"threadId":"thread-root","turn":{"id":"turn-1","status":"completed"}}}`
				)
				entries := parseWire(t, codex.New("", nil).NewWire(), thread, started, turnDone)
				running := BackgroundCounts{Commands: 1}
				t.Run("Live", func(t *testing.T) {
					t.Parallel()
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
					setState(tk, task.StateRunning)
					for _, entry := range entries {
						tk.addParsedMessage(entry)
					}
					if got := tk.Snapshot().Background; got != running {
						t.Fatalf("counts before the trailer = %+v, want %+v", got, running)
					}
					if changed := tk.addParsedMessage(agent.TimedMessage{Message: &agent.MetaResultMessage{}}); !changed {
						t.Error("the trailer did not report a summary change")
					}
					if got := tk.BackgroundActivity(); got.Counts != (BackgroundCounts{}) || len(got.Items) != 0 {
						t.Fatalf("activity after the trailer = %+v, want none", got)
					}
				})
				t.Run("Restored", func(t *testing.T) {
					t.Parallel()
					for _, c := range []struct {
						name    string
						settled []int
						want    BackgroundCounts
					}{
						{"after", []int{len(entries)}, BackgroundCounts{}},
						// A card a later run reports again outlives an earlier trailer.
						{"before", []int{0}, running},
					} {
						tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
						setState(tk, task.StateRunning)
						tk.SeedTimelineParts(nil, slices.Clone(entries), c.settled)
						if got := tk.BackgroundActivity(); got.Counts != c.want || len(got.Items) != c.want.Commands {
							t.Errorf("%s: activity = %+v, want %+v", c.name, got, c.want)
						}
					}
				})
			})
			t.Run("PiExtensionWake", func(t *testing.T) {
				t.Parallel()
				// Minimized from a task log: the pi-subagents extension woke the
				// idle parent after a detached run ended.
				const (
					agentStart = `{"type":"agent_start"}`
					prompt     = `{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"Start the worker"}]}}`
					delta      = `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"Done"}}`
					agentEnd   = `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"Done"}],"usage":{"input":10,"output":5}}]}`
					notify     = `{"type":"message_start","message":{"role":"custom","customType":"subagent-notify","content":"Background task completed: **worker**","display":false}}`
					assistant  = `{"type":"message_start","message":{"role":"assistant","content":[]}}`
				)
				entries := parseWire(t, pi.New("", nil).NewWire(), agentStart, prompt, delta, agentEnd, agentStart, notify, assistant, delta, agentEnd)
				states, _ := fold(t, entries)
				results := slices.IndexFunc(entries, func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.ResultMessage); return ok })
				queued := slices.IndexFunc(entries[results:], func(e agent.TimedMessage) bool { _, ok := e.Message.(*agent.TurnStartedMessage); return ok })
				if queued < 0 {
					t.Fatal("the extension wake started no turn")
				}
				queued += results
				if got := states[queued-1]; got != task.StateWaiting {
					t.Errorf("state before the wake = %v, want waiting", got)
				}
				for i := queued; i < len(entries)-1; i++ {
					if states[i] != task.StateRunning {
						t.Errorf("entry %d (%T): state = %v, want running until the wake's result", i, entries[i].Message, states[i])
					}
				}
				if got := states[len(states)-1]; got != task.StateWaiting {
					t.Errorf("final state = %v, want waiting", got)
				}
			})
			t.Run("ForegroundAgent", func(t *testing.T) {
				t.Parallel()
				use := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_fg","name":"Agent","input":{"description":"Joke","prompt":"Tell a joke","subagent_type":"general-purpose"}}]},` + session + `}`
				started := `{"type":"system","subtype":"task_started","task_id":"fg","tool_use_id":"toolu_fg","task_type":"local_agent",` + session + `}`
				done := `{"type":"system","subtype":"task_notification","task_id":"fg","tool_use_id":"toolu_fg","status":"completed","summary":"Joke",` + session + `}`
				answer := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_fg","type":"tool_result","content":[{"type":"text","text":"Joke"}]}]},` + session + `}`
				entries := parse(t, use, started, done, answer, text, result)
				states, counts := fold(t, entries)
				if got := counts[1]; got != (BackgroundCounts{}) {
					t.Errorf("counts with a running foreground agent = %+v, want none", got)
				}
				if got := states[len(states)-1]; got != task.StateWaiting {
					t.Errorf("state after a foreground agent = %v, want waiting", got)
				}
			})
			t.Run("SnapshotRetiresLostCard", func(t *testing.T) {
				t.Parallel()
				// Minimized from a task log: a snapshot drops background work
				// whose terminal records were lost. The card ends as on host exit
				// and the task settles without a queued turn.
				listed := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"a3635a","run_id":"001","task_type":"local_agent"},{"task_id":"b3xwrrd6m","task_type":"local_bash"}],` + session + `}`
				dropped := `{"type":"system","subtype":"background_tasks_changed","tasks":[],` + session + `}`
				status := `{"type":"system","subtype":"status","status":"requesting",` + session + `}`
				bashUse := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"make test","run_in_background":true}}]},` + session + `}`
				bashStarted := `{"type":"system","subtype":"task_started","task_id":"b3xwrrd6m","tool_use_id":"toolu_bash","description":"Run tests","is_backgrounded":true,"task_type":"local_bash",` + session + `}`
				entries := parse(t, listed, agentUse, agentStarted, agentLaunched, bashUse, bashStarted, text, result, dropped, status)
				states, counts := fold(t, entries)
				if atResult, final := countsAt(entries, counts); atResult != (BackgroundCounts{Commands: 1, Agents: 1}) || final != (BackgroundCounts{}) {
					t.Errorf("counts at result = %+v, final = %+v, want 1 command and 1 agent then none", atResult, final)
				}
				retired := 0
				for i, entry := range entries {
					switch entry.Message.(type) {
					case *agent.BackgroundRetiredMessage:
						retired++
					case *agent.TurnStartedMessage:
						t.Errorf("entry %d: retirement started a turn", i)
					}
					if _, ok := entry.Message.(*agent.ResultMessage); ok && states[i] != task.StateRunning {
						t.Errorf("state with background work = %v, want running", states[i])
					}
				}
				if retired != 2 {
					t.Errorf("retired cards = %d, want 2", retired)
				}
				if got := states[len(states)-1]; got != task.StateWaiting {
					t.Errorf("state after the retirement = %v, want waiting", got)
				}
			})
		})
		t.Run("EmptyResultUsesRestoredTurnText", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			msgs := []agent.Message{
				&agent.UserInputMessage{Text: "first"},
				&agent.TextMessage{Text: "first result"},
				&agent.ResultMessage{MessageType: "result", Result: "first result"},
				&agent.UserInputMessage{Text: "second"},
				&agent.TextMessage{Text: "status update"},
				&agent.ToolUseMessage{ToolUseID: "tu1", Name: "Read"},
				&agent.ToolResultMessage{ToolUseID: "tu1"},
				&agent.TextMessage{Text: "done"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)

			if got := tk.LastAgentResult(); got != "done" {
				t.Errorf("result = %q, want %q", got, "done")
			}
			if rm, ok := msgs[len(msgs)-1].(*agent.ResultMessage); !ok {
				t.Fatalf("last message type = %T, want *agent.ResultMessage", msgs[len(msgs)-1])
			} else if rm.Result != "" {
				t.Errorf("restored result = %q, want empty (message must stay immutable)", rm.Result)
			}
		})
		t.Run("EmptyResultStopsAtRestoredThinking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			msgs := []agent.Message{
				&agent.UserInputMessage{Text: "test"},
				&agent.TextMessage{Text: "before thinking"},
				&agent.ThinkingDeltaMessage{Text: "reasoning"},
				&agent.TextDeltaMessage{Text: "after "},
				&agent.TextDeltaMessage{Text: "thinking"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(msgs), nil)

			if got := tk.LastAgentResult(); got != "after thinking" {
				t.Errorf("result = %q, want %q (thinking excluded)", got, "after thinking")
			}
			if rm, ok := msgs[len(msgs)-1].(*agent.ResultMessage); !ok {
				t.Fatalf("last message type = %T, want *agent.ResultMessage", msgs[len(msgs)-1])
			} else if rm.Result != "" {
				t.Errorf("restored result = %q, want empty (message must stay immutable)", rm.Result)
			}
		})
	})

	t.Run("ExtraRuntimeRepos", func(t *testing.T) {
		t.Parallel()
		t.Run("NoRepos", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			if extra := tk.ExtraRuntimeRepos(); extra != nil {
				t.Fatalf("ExtraRuntimeRepos with no repos = %+v, want nil", extra)
			}
		})
		t.Run("OneRepo", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Repos = []repo.Mount{{Name: "a/b", Branch: "caic-0", GitRoot: "/foo"}}
			if extra := tk.ExtraRuntimeRepos(); extra != nil {
				t.Fatalf("ExtraRuntimeRepos with one repo = %+v, want nil", extra)
			}
		})
		t.Run("MultipleRepos", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Repos = []repo.Mount{
				{Name: "a/b", Branch: "caic-0", GitRoot: "/foo"},
				{Name: "c/d", Branch: "caic-1", GitRoot: "/bar"},
			}
			extra := tk.ExtraRuntimeRepos()
			if len(extra) != 1 {
				t.Fatalf("ExtraRuntimeRepos len = %d, want 1", len(extra))
			}
			if extra[0].Branch != "caic-1" {
				t.Errorf("extra[0].Branch = %q, want %q", extra[0].Branch, "caic-1")
			}
		})
	})

	t.Run("SnapshotWithStateHistory", func(t *testing.T) {
		t.Parallel()
		t.Run("replaysTransitionsAfterCursor", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			_, seq0, hist0 := tk.SnapshotWithStateHistory(0)
			if len(hist0) != 1 || hist0[0].State != task.StatePending {
				t.Fatalf("initial history = %+v, want one pending transition", hist0)
			}
			setState(tk, task.StateRunning)
			setState(tk, task.StateWaiting)
			snap, seq, hist := tk.SnapshotWithStateHistory(seq0)
			if snap.State != task.StateWaiting {
				t.Errorf("state = %v, want waiting", snap.State)
			}
			want := []task.State{task.StateStarting, task.StateRunning, task.StateWaiting}
			if seq != seq0+uint64(len(want)) {
				t.Errorf("seq = %d, want %d", seq, seq0+uint64(len(want)))
			}
			if len(hist) != len(want) {
				t.Fatalf("history = %+v, want %v", hist, want)
			}
			for i, state := range want {
				if hist[i].State != state {
					t.Errorf("history[%d].State = %v, want %v", i, hist[i].State, state)
				}
				if hist[i].Seq != seq0+uint64(i)+1 {
					t.Errorf("history[%d].Seq = %d, want %d", i, hist[i].Seq, seq0+uint64(i)+1)
				}
			}
		})
		t.Run("unchangedStateIsNotRecorded", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			_, seq, _ := tk.SnapshotWithStateHistory(0)
			setState(tk, task.StatePending)
			if _, gotSeq, hist := tk.SnapshotWithStateHistory(seq); gotSeq != seq || len(hist) != 0 {
				t.Errorf("seq = %d history = %+v, want seq %d and no new transition", gotSeq, hist, seq)
			}
		})
		t.Run("boundsJournal", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			for i := range stateTransitionHistory + 5 {
				if i%2 == 0 {
					setState(tk, task.StateRunning)
				} else {
					setState(tk, task.StateWaiting)
				}
			}
			_, seq, hist := tk.SnapshotWithStateHistory(0)
			if len(hist) != stateTransitionHistory {
				t.Fatalf("history len = %d, want %d", len(hist), stateTransitionHistory)
			}
			if hist[len(hist)-1].Seq != seq {
				t.Errorf("last history seq = %d, want %d", hist[len(hist)-1].Seq, seq)
			}
			if first := seq - uint64(stateTransitionHistory) + 1; hist[0].Seq != first {
				t.Errorf("first history seq = %d, want %d", hist[0].Seq, first)
			}
		})
	})

	t.Run("SnapshotSettings", func(t *testing.T) {
		t.Parallel()
		t.Run("RequestedOnly", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "gpt-4", "")
			snap := tk.Snapshot()
			if snap.RequestedModel != "gpt-4" || snap.ReportedModel != "" {
				t.Errorf("settings = (%q, %q), want (gpt-4, empty)", snap.RequestedModel, snap.ReportedModel)
			}
		})
		t.Run("Reported", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "gpt-4", "")
			addTaskMessage(tk, &agent.InitMessage{SessionID: "s1", ReportedModel: "claude-3-opus"})
			snap := tk.Snapshot()
			if snap.RequestedModel != "gpt-4" || snap.ReportedModel != "claude-3-opus" {
				t.Errorf("settings = (%q, %q), want (gpt-4, claude-3-opus)", snap.RequestedModel, snap.ReportedModel)
			}
		})
		t.Run("UsageReportedModel", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name  string
				apply func(*Task)
			}{
				{"live", func(tk *Task) {
					addTaskMessage(tk, &agent.UsageMessage{ReportedModel: "claude-opus-4-6"})
				}},
				{"replay", func(tk *Task) {
					tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.UsageMessage{ReportedModel: "claude-opus-4-6"}}), nil)
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "requested", "")
					tc.apply(tk)
					if got := tk.Snapshot().ReportedModel; got != "claude-opus-4-6" {
						t.Errorf("ReportedModel = %q, want claude-opus-4-6", got)
					}
				})
			}
		})
	})

	t.Run("ReportedEffort", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-5", "high")
		addTaskMessage(tk, &agent.InitMessage{SessionID: "s1", ReportedEffort: "medium"})
		snap := tk.Snapshot()
		if snap.RequestedEffort != "high" || snap.ReportedEffort != "medium" {
			t.Errorf("settings = (%q, %q), want (high, medium)", snap.RequestedEffort, snap.ReportedEffort)
		}
	})

	t.Run("UnreportedEffortIsNotAddedToInit", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-5", "high")
		init := &agent.InitMessage{SessionID: "s1"}
		addTaskMessage(tk, init)
		if init.ReportedEffort != "" {
			t.Errorf("InitMessage.Effort = %q, want empty", init.ReportedEffort)
		}
	})

	t.Run("ReportedEffort", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-5", "high")
		addTaskMessage(tk, &agent.InitMessage{SessionID: "s1", ReportedEffort: "medium"})
		snap := tk.Snapshot()
		if snap.RequestedEffort != "high" || snap.ReportedEffort != "medium" {
			t.Errorf("settings = (%q, %q), want (high, medium)", snap.RequestedEffort, snap.ReportedEffort)
		}
	})

	t.Run("UnreportedEffortIsNotAddedToInit", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-5", "high")
		init := &agent.InitMessage{SessionID: "s1"}
		addTaskMessage(tk, init)
		if init.ReportedEffort != "" {
			t.Errorf("InitMessage.Effort = %q, want empty", init.ReportedEffort)
		}
	})

	t.Run("AgentVersion", func(t *testing.T) {
		t.Parallel()
		t.Run("AddMessageRecordsVersionWithoutSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			addTaskMessage(tk, &agent.InitMessage{Version: "1.2.3"})
			if snap := tk.Snapshot(); snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("SeedTimelineRecordsVersionWithoutSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.InitMessage{Version: "1.2.3"}}), nil)
			if snap := tk.Snapshot(); snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("EmptyInitDoesNotClearVersion", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			addTaskMessage(tk, &agent.InitMessage{Version: "1.2.3"})
			addTaskMessage(tk, &agent.InitMessage{SessionID: "s1"})
			if snap := tk.Snapshot(); snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("MetaSessionRecordsVersionWithoutConversationMessage", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			addTaskMessage(tk, &agent.MetaSessionMessage{MessageType: "caic_session", AgentVersion: "1.2.3"})
			if snap := tk.Snapshot(); snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
			if got := len(tk.Messages()); got != 0 {
				t.Errorf("Messages length = %d, want 0", got)
			}
		})
	})

	t.Run("SetPR", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetPR("octocat", "hello-world", 42)
		if got := tk.GetPR(); got != 42 {
			t.Errorf("GetPR = %d, want 42", got)
		}
		snap := tk.Snapshot()
		if snap.ForgeOwner != "octocat" {
			t.Errorf("ForgeOwner = %q, want %q", snap.ForgeOwner, "octocat")
		}
		if snap.ForgeRepo != "hello-world" {
			t.Errorf("ForgeRepo = %q, want %q", snap.ForgeRepo, "hello-world")
		}
		if snap.ForgePR != 42 {
			t.Errorf("ForgePR = %d, want 42", snap.ForgePR)
		}
		if snap.ForgePRState != forge.PRStateOpen {
			t.Errorf("ForgePRState = %q, want %q", snap.ForgePRState, forge.PRStateOpen)
		}
	})

	t.Run("SetPRState", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetPR("octocat", "hello-world", 42)
		tk.SetPRState(forge.PRStateClosed)
		snap := tk.Snapshot()
		if snap.ForgePRState != forge.PRStateClosed {
			t.Errorf("ForgePRState = %q, want %q", snap.ForgePRState, forge.PRStateClosed)
		}
	})

	t.Run("SetCIStatus", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		checks := []forge.Check{{Name: "lint", Status: "success"}}
		tk.SetCIStatus(forge.CIStatusPending, checks)
		snap := tk.Snapshot()
		if snap.CIStatus != forge.CIStatusPending {
			t.Errorf("CIStatus = %v, want %v", snap.CIStatus, forge.CIStatusPending)
		}
		if len(snap.CIChecks) != 1 || snap.CIChecks[0].Name != "lint" {
			t.Errorf("CIChecks = %+v", snap.CIChecks)
		}
	})

	t.Run("SetTitle", func(t *testing.T) {
		t.Parallel()
		t.Run("HardLimit", func(t *testing.T) {
			t.Parallel()
			prompt := strings.Repeat("\u754c", 100)
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: prompt}, "", "", "")
			if got := tk.Title(); got != strings.Repeat("\u754c", 79)+"…" {
				t.Fatalf("prompt fallback title = %q", got)
			}
			if tk.InitialPrompt.Text != prompt {
				t.Fatal("title truncation changed the prompt")
			}
			tk.SetTitle(strings.Repeat("a", 81))
			if got := tk.Title(); got != strings.Repeat("a", 79)+"…" {
				t.Fatalf("Title = %q", got)
			}
		})
		t.Run("SetsTitle", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetTitle("hello")
			if tk.Title() != "hello" {
				t.Errorf("Title = %q, want %q", tk.Title(), "hello")
			}
		})
		t.Run("EmptyIgnored", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetTitle("first")
			tk.SetTitle("")
			if tk.Title() != "first" {
				t.Errorf("Title = %q, want %q (empty string should be ignored)", tk.Title(), "first")
			}
		})
	})

	t.Run("SetSessionMetadata", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "requested", "")
		tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "session-1", ReportedModel: "reported", AgentVersion: "2.0.0", AutoCompactTokenLimit: 244800})
		if got := tk.GetSessionID(); got != "session-1" {
			t.Errorf("SessionID = %q, want session-1", got)
		}
		snap := tk.Snapshot()
		if snap.AutoCompactTokenLimit != 244800 {
			t.Fatalf("AutoCompactTokenLimit = %d, want 244800", snap.AutoCompactTokenLimit)
		}
		if snap.RequestedModel != "requested" || snap.ReportedModel != "reported" {
			t.Errorf("settings = (%q, %q), want (requested, reported)", snap.RequestedModel, snap.ReportedModel)
		}
		if snap.AgentVersion != "2.0.0" {
			t.Errorf("AgentVersion = %q, want 2.0.0", snap.AgentVersion)
		}
	})

	t.Run("PendingUserActions", func(t *testing.T) {
		t.Parallel()
		t.Run("AcceptedAnswerClosesBeforeToolResult", func(t *testing.T) {
			t.Parallel()
			for _, outcome := range []string{"completed", "failed", "unresolved", "output"} {
				t.Run(outcome, func(t *testing.T) {
					t.Parallel()
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
					history := []agent.Message{
						&agent.AskMessage{ToolUseID: "ask", Questions: []v3.AskQuestion{{Question: "Which?"}}},
						&agent.PendingUserActionMessage{Action: v3.PendingUserAction{Kind: agent.PendingUserActionAskUserQuestion, RequestID: "request", ToolUseID: "ask", Ask: v3.PendingAskAction{Questions: []v3.AskQuestion{{Question: "Which?"}}}}},
						agent.InputDeliveryStarted(), &agent.UserInputMessage{Text: "answer"},
					}
					switch outcome {
					case "completed":
						history = append(history, agent.InputDeliveryCompleted())
					case "failed":
						history = append(history, agent.InputDeliveryFailed(io.ErrClosedPipe))
					case "output":
						history = append(history, &agent.TextDeltaMessage{Text: "working"})
					}
					tk.SeedTimelineParts(nil, agenttest.TimedMessages(history), nil)
					want := 1
					if outcome == "completed" || outcome == "output" {
						want = 0
					}
					if got := tk.PendingUserActions(); len(got) != want {
						t.Fatalf("pending actions = %#v, want %d", got, want)
					}
				})
			}
		})

		t.Run("AmbiguousClaudeControlDoesNotRestoreRootAsk", func(t *testing.T) {
			t.Parallel()
			for _, initialized := range []bool{false, true} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				wire := claudecode.New().NewWire()
				input := `{"questions":[{"question":"Proceed?","header":"Choice","options":[{"label":"Yes","description":"Go"},{"label":"No","description":"Stop"}]}]}`
				var lines []string
				if initialized {
					lines = append(lines, `{"type":"system","subtype":"init","session_id":"root"}`)
				}
				lines = append(lines,
					`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":`+input+`}]}}`,
					`{"type":"assistant","session_id":"child-thread","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":`+input+`}]}}`,
					`{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":`+input+`}}`,
				)
				var history []agent.Message
				for _, line := range lines {
					messages, err := wire.ParseOutput([]byte(line))
					if err != nil {
						t.Fatal(err)
					}
					history = append(history, messages...)
				}
				tk.SeedTimelineParts(nil, agenttest.TimedMessages(history), nil)
				if got := tk.PendingUserActions(); len(got) != 0 {
					t.Errorf("initialized=%t: ambiguous control restored root action: %#v", initialized, got)
				}
			}
		})
		t.Run("RestoresCurrentAsk", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.AskMessage{
					ToolUseID: "toolu-1",
					Questions: []v3.AskQuestion{{Question: "Which?"}},
				},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: v3.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: v3.PendingAskAction{
							Questions: []v3.AskQuestion{{Question: "Which?"}},
						},
					},
				},
				&agent.ResultMessage{MessageType: "result"},
			}), nil)

			got := tk.PendingUserActions()
			if len(got) != 1 {
				t.Fatalf("PendingUserActions len = %d, want 1", len(got))
			}
			if got[0].Kind != agent.PendingUserActionAskUserQuestion {
				t.Errorf("Kind = %q, want %q", got[0].Kind, agent.PendingUserActionAskUserQuestion)
			}
			if got[0].RequestID != "req-1" {
				t.Errorf("RequestID = %q, want req-1", got[0].RequestID)
			}
			if len(got[0].Ask.Questions) != 1 {
				t.Fatalf("Questions len = %d, want 1", len(got[0].Ask.Questions))
			}
			if got[0].Ask.Questions[0].Question != "Which?" {
				t.Errorf("question = %q, want Which?", got[0].Ask.Questions[0].Question)
			}
		})
		// askPair is a restored question and its pending action.
		askPair := func(question, requestID, toolUseID string) []agent.Message {
			questions := []v3.AskQuestion{{Question: question}}
			return []agent.Message{
				&agent.AskMessage{ToolUseID: toolUseID, Questions: questions},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: v3.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: requestID,
						ToolUseID: toolUseID,
						Ask:       v3.PendingAskAction{Questions: questions},
					},
				},
			}
		}
		t.Run("DedupesDuplicatedRestoredAsk", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			ask := askPair("Which?", "req-1", "toolu-1")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(slices.Concat(ask, ask, []agent.Message{&agent.ResultMessage{MessageType: "result"}})), nil)

			got := tk.PendingUserActions()
			if len(got) != 1 {
				t.Fatalf("PendingUserActions len = %d, want 1", len(got))
			}
			if got[0].RequestID != "req-1" {
				t.Errorf("RequestID = %q, want req-1", got[0].RequestID)
			}
			if got[0].ToolUseID != "toolu-1" {
				t.Errorf("ToolUseID = %q, want toolu-1", got[0].ToolUseID)
			}
		})
		t.Run("RestoresMultipleCurrentAsks", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages(slices.Concat(askPair("First?", "req-1", "toolu-1"), askPair("Second?", "req-2", "toolu-2"), []agent.Message{&agent.ResultMessage{MessageType: "result"}})), nil)

			got := tk.PendingUserActions()
			if len(got) != 2 {
				t.Fatalf("PendingUserActions len = %d, want 2", len(got))
			}
			if got[0].RequestID != "req-1" || got[1].RequestID != "req-2" {
				t.Fatalf("RequestIDs = %q, %q; want req-1, req-2", got[0].RequestID, got[1].RequestID)
			}
		})
		t.Run("AnsweredAskIsClosed", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.AskMessage{
					ToolUseID: "toolu-1",
					Questions: []v3.AskQuestion{{Question: "Which?"}},
				},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: v3.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: v3.PendingAskAction{
							Questions: []v3.AskQuestion{{Question: "Which?"}},
						},
					},
				},
				&agent.ToolResultMessage{ToolUseID: "toolu-1"},
				&agent.ResultMessage{MessageType: "result"},
			}), nil)

			if got := tk.PendingUserActions(); len(got) != 0 {
				t.Fatalf("PendingUserActions = %#v, want none", got)
			}
		})
		t.Run("PreviousTurnAskIsIgnored", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.AskMessage{
					ToolUseID: "toolu-1",
					Questions: []v3.AskQuestion{{Question: "Which?"}},
				},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: v3.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: v3.PendingAskAction{
							Questions: []v3.AskQuestion{{Question: "Which?"}},
						},
					},
				},
				&agent.ResultMessage{MessageType: "result"},
				&agent.TextMessage{Text: "new turn"},
				&agent.ResultMessage{MessageType: "result"},
			}), nil)

			if got := tk.PendingUserActions(); len(got) != 0 {
				t.Fatalf("PendingUserActions = %#v, want none", got)
			}
		})
	})

	t.Run("PushStats", func(t *testing.T) {
		t.Parallel()
		t.Run("StatsSince", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.PushStats(&runtime.Stats{CPUPerc: 50.0, MemUsed: 1024, DiskUsed: -1})
			tk.PushStats(&runtime.Stats{CPUPerc: 75.0, MemUsed: 2048, DiskUsed: -1})

			wake, unsub := tk.SubscribeStatsWakeups(t.Context())
			t.Cleanup(unsub)
			history, seq := tk.StatsSince(0)
			if len(history) != 2 || history[0].CPUPerc != 50.0 || history[1].CPUPerc != 75.0 {
				t.Fatalf("history = %+v, want CPU 50 then 75", history)
			}

			tk.PushStats(&runtime.Stats{CPUPerc: 100.0, MemUsed: 4096, DiskUsed: -1})
			select {
			case <-wake:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for stats wakeup")
			}
			live, seq := tk.StatsSince(seq)
			if len(live) != 1 || live[0].CPUPerc != 100.0 {
				t.Fatalf("live = %+v, want only CPU 100", live)
			}
			// A disk revision resends the latest sample with the same timestamp.
			tk.UpdateDiskUsage(700)
			<-wake
			if revised, _ := tk.StatsSince(seq); len(revised) != 1 || revised[0].CPUPerc != 100.0 || revised[0].DiskUsed != 700 {
				t.Fatalf("revised = %+v, want CPU 100 with disk 700", revised)
			}
		})

		t.Run("SlowReaderGetsRetainedSamples", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			_, seq := tk.StatsSince(0)
			// A reader that misses more writes than a channel buffer or the ring
			// holds still receives the newest samples.
			for i := range 100 {
				tk.PushStats(&runtime.Stats{CPUPerc: float64(i)})
			}
			got, _ := tk.StatsSince(seq)
			if len(got) != statsRingSize || got[0].CPUPerc != 40 || got[len(got)-1].CPUPerc != 99 {
				t.Fatalf("got %d samples from CPU %v to %v, want %d from 40 to 99", len(got), got[0].CPUPerc, got[len(got)-1].CPUPerc, statsRingSize)
			}
		})

		t.Run("RingOverflow", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			if tk.Snapshot().ResourceUsage != nil {
				t.Fatal("resource usage present before first sample")
			}
			for i := range 65 {
				tk.PushStats(&runtime.Stats{CPUPerc: float64(i)})
			}
			history, _ := tk.StatsSince(0)
			if len(history) != statsRingSize {
				t.Errorf("history len = %d, want %d (ring capped at size)", len(history), statsRingSize)
			}
			if history[0].CPUPerc != 5.0 {
				t.Errorf("history[0].CPUPerc = %v, want 5.0 (oldest should be off by delta)", history[0].CPUPerc)
			}
			if history[len(history)-1].CPUPerc != 64.0 {
				t.Errorf("history[last].CPUPerc = %v, want 64.0", history[len(history)-1].CPUPerc)
			}
			snap := tk.Snapshot()
			if snap.ResourceUsage == nil || snap.ResourceUsage.CPUPerc != 64 {
				t.Fatalf("latest resource usage = %+v", snap.ResourceUsage)
			}
			tk.PushStats(&runtime.Stats{CPUPerc: 99})
			if snap.ResourceUsage.CPUPerc != 64 || tk.Snapshot().ResourceUsage.CPUPerc != 99 {
				t.Fatal("snapshot must retain its sample independently of subsequent updates")
			}
		})
	})

	t.Run("UpdateDiskUsage", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "", "")
		tk.PushStats(&runtime.Stats{CPUPerc: 50, DiskUsed: -1})

		tk.UpdateDiskUsage(700)
		tk.PushStats(&runtime.Stats{CPUPerc: 75, DiskUsed: -1})

		history, _ := tk.StatsSince(0)
		if len(history) != 2 {
			t.Fatalf("history len = %d, want 2", len(history))
		}
		if history[0].DiskUsed != 700 || history[1].DiskUsed != 700 {
			t.Fatalf("disk usage = [%d, %d], want [700, 700]", history[0].DiskUsed, history[1].DiskUsed)
		}
	})

	t.Run("RecordsDiskUsageWithoutResourceSample", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "", "")
		tk.UpdateDiskUsage(700)

		if snapshot := tk.Snapshot(); !snapshot.DiskKnown || snapshot.DiskUsed != 700 {
			t.Fatalf("disk usage = (%d, %t), want (700, true)", snapshot.DiskUsed, snapshot.DiskKnown)
		}
		history, _ := tk.StatsSince(0)
		if len(history) != 0 {
			t.Fatalf("stats history = %+v, want none", history)
		}
	})

	t.Run("SendCompact", func(t *testing.T) {
		t.Parallel()
		t.Run("NoSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateWaiting)
			err := tk.SendCompact(t.Context(), "compact now")
			if err == nil {
				t.Fatal("expected error when no session is active")
			}
			if !strings.Contains(err.Error(), "session="+string(SessionNone)) {
				t.Errorf("error = %q, want session=%s", err.Error(), SessionNone)
			}
		})
		t.Run("DeadSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			setState(tk, task.StateWaiting)
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			t.Cleanup(cmdCancel)
			cmd := exec.CommandContext(cmdCtx, "true")
			stdin, _ := cmd.StdinPipe()
			stdout, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_, run := mustTaskRun(t, tk)
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, run.records, nil), stdout, testLogger())
			<-s.Done()
			tk.AttachSession(&SessionHandle{Session: s, Run: run})
			err := tk.SendCompact(t.Context(), "compact now")
			if err == nil {
				t.Fatal("expected error for dead session")
			}
			if !strings.Contains(err.Error(), "session="+string(SessionExited)) {
				t.Errorf("error = %q, want session=%s", err.Error(), SessionExited)
			}
		})
	})

	t.Run("TurnEnded", func(t *testing.T) {
		t.Parallel()
		turnEnded := func(t *testing.T, msgs ...agent.Message) bool {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			for _, m := range msgs {
				addTaskMessage(tk, m)
			}
			return tk.BackgroundActivity().TurnEnded
		}
		t.Run("Empty", func(t *testing.T) {
			t.Parallel()
			if turnEnded(t) {
				t.Error("an empty timeline has no ended turn")
			}
		})
		t.Run("OutputAfterResult", func(t *testing.T) {
			t.Parallel()
			if !turnEnded(t,
				&agent.ResultMessage{MessageType: "result", Result: "done"},
				&agent.TextMessage{Text: "late output"},
				&agent.DiffStatMessage{MessageType: "caic_diff_stat"},
				&agent.TextDeltaMessage{Text: "partial"},
				&agent.RawMessage{},
				&agent.UsageMessage{},
				&agent.SyncActivityMessage{Phase: "end", Status: "pushed"},
			) {
				t.Error("output after a result must not reopen the turn")
			}
		})
		t.Run("TurnStartedLast", func(t *testing.T) {
			t.Parallel()
			if turnEnded(t, &agent.ResultMessage{MessageType: "result"}, &agent.TurnStartedMessage{}) {
				t.Error("a turn start after a result opens a turn")
			}
		})
	})
	t.Run("DiffStatMessageRepoStates", func(t *testing.T) {
		t.Parallel()
		probe := []v3.RepoState{{RepoIndex: 0, Branch: "caic-1", Ahead: 2, ChangedFiles: 3, LinesAdded: 10, LinesDeleted: 4, UncommittedFiles: 1}}
		watcherDiff := v3.DiffStat{{Path: "relay.go", LinesAdded: 1}}
		t.Run("LiveUpdateNotifies", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			stateChanged := tk.addParsedMessage(agent.TimedMessage{Message: &agent.DiffStatMessage{
				MessageType: "caic_diff_stat",
				DiffStat:    v3.DiffStat{{Path: "main.go", LinesAdded: 10, LinesDeleted: 4}},
				Repos:       probe,
			}})
			if !stateChanged {
				t.Fatal("diff data update must notify the task-list stream even without a state change")
			}
			if got := tk.Snapshot().RepoStates; !reflect.DeepEqual(got, probe) {
				t.Fatalf("RepoStates = %+v, want %+v", got, probe)
			}
		})
		t.Run("WatcherOnlyMessageKeepsStates", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, task.StateRunning)
			tk.addParsedMessage(agent.TimedMessage{Message: &agent.DiffStatMessage{MessageType: "caic_diff_stat", Repos: probe}})
			tk.addParsedMessage(agent.TimedMessage{Message: &agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: watcherDiff}})
			if got := tk.Snapshot().RepoStates; !reflect.DeepEqual(got, probe) {
				t.Fatalf("RepoStates = %+v, want the probe state %+v", got, probe)
			}
		})
		t.Run("SeedTimelineRestoresLatestProbe", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
				&agent.DiffStatMessage{MessageType: "caic_diff_stat", Repos: probe},
				&agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: watcherDiff},
			}), nil)
			snap := tk.Snapshot()
			if !reflect.DeepEqual(snap.RepoStates, probe) {
				t.Fatalf("RepoStates = %+v, want the latest probe %+v", snap.RepoStates, probe)
			}
			if !reflect.DeepEqual(snap.DiffStat, watcherDiff) {
				t.Fatalf("DiffStat = %+v, want the latest watcher stat %+v", snap.DiffStat, watcherDiff)
			}
		})
	})
}

// BenchmarkLiveDeltaFinalization measures retaining a long live delta run and
// replacing its payloads when the corresponding final message arrives.
func BenchmarkLiveDeltaFinalization(b *testing.B) {
	const deltaCount = 1_000
	deltas := make([]*agent.TextDeltaMessage, deltaCount)
	for i := range deltas {
		deltas[i] = &agent.TextDeltaMessage{Text: strings.Repeat("delta", 128)}
	}
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark"}, harness.Claude, "", "")
		for _, delta := range deltas {
			addTaskMessage(tk, delta)
		}
		addTaskMessage(tk, &agent.TextMessage{Text: "complete"})
		if tk.timeline[0].Message != compactedTextDelta {
			b.Fatal("live delta payload was not compacted")
		}
	}
}

func TestSessionHandle(t *testing.T) {
	t.Parallel()
	t.Run("Drain", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
		_, run := mustTaskRun(t, tk)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, "true")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		s := agent.NewSession(ctx, cmd, agent.NewConn(ctx, testLogger(), stdin, run.records, nil), stdout, testLogger())
		h := &SessionHandle{Session: s, Run: run}
		if err := h.Drain(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-h.Done():
		default:
			t.Fatal("transport completion not published after Drain")
		}
	})

	t.Run("Unsubscribe", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		ctx, cancel := context.WithCancel(t.Context())
		_, unsubCanceledStats := tk.SubscribeStatsWakeups(ctx)
		_, unsubCanceledWake := tk.SubscribeTimelineWakeups(ctx)
		_, unsubStats := tk.SubscribeStatsWakeups(t.Context())
		_, unsubWake := tk.SubscribeTimelineWakeups(t.Context())
		cancel()
		// Calls after cancellation and repeated calls are no-ops.
		for _, unsub := range []func(){unsubCanceledStats, unsubCanceledWake, unsubStats, unsubWake} {
			unsub()
			unsub()
		}
		tk.mu.Lock()
		defer tk.mu.Unlock()
		if len(tk.statsWakeSubs) != 0 || len(tk.wakeSubs) != 0 {
			t.Fatalf("subscribers left: stats=%d wake=%d", len(tk.statsWakeSubs), len(tk.wakeSubs))
		}
	})
}

func TestState(t *testing.T) {
	t.Parallel()
	t.Run("Transition", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StatePurging)
		if tk.Transition(task.StateStopping) || tk.GetState() != task.StatePurging {
			t.Fatalf("purging task stopped: %s", tk.GetState())
		}
		if !tk.Transition(task.StatePurged) || tk.GetState() != task.StatePurged {
			t.Fatalf("state = %s, want purged", tk.GetState())
		}
	})
	t.Run("GoLive", func(t *testing.T) {
		t.Parallel()
		t.Run("silent_session_waits", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Restore(task.StateStarting, time.Time{})
			if !tk.GoLive() || tk.GetState() != task.StateWaiting {
				t.Fatalf("state = %s, want waiting", tk.GetState())
			}
			if tk.GoLive() {
				t.Fatal("live session went live again")
			}
		})
		t.Run("keeps_reported_activity", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Restore(task.StateStarting, time.Time{})
			addTaskMessage(tk, &agent.UserInputMessage{Text: "go"})
			at := tk.Snapshot().StateUpdatedAt
			if !tk.GoLive() {
				t.Fatal("starting session did not go live")
			}
			if s := tk.Snapshot(); s.State != task.StateRunning || !s.StateUpdatedAt.Equal(at) {
				t.Fatalf("state = %s at %s, want running at %s", s.State, s.StateUpdatedAt, at)
			}
		})
	})
	t.Run("RestartSession", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateAsking)
		if !tk.Transition(task.StateStarting) || tk.GetState() != task.StateStarting {
			t.Fatalf("state = %s, want starting", tk.GetState())
		}
	})
	t.Run("RuntimeExited", func(t *testing.T) {
		t.Parallel()
		for _, s := range []task.State{task.StateStarting, task.StateStopping, task.StateCrashed} {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, s)
			if _, ok := tk.RuntimeExited(); ok || tk.GetState() != s {
				t.Errorf("%s task archived as %s", s, tk.GetState())
			}
		}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateAsking)
		if prev, ok := tk.RuntimeExited(); !ok || prev != task.StateAsking || tk.GetState() != task.StateStopped {
			t.Fatalf("prev=%s ok=%t state=%s", prev, ok, tk.GetState())
		}
	})
	t.Run("RuntimeStarted", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateStarting)
		if tk.RuntimeStarted() || tk.GetState() != task.StateStarting {
			t.Fatalf("starting task reconnected: %s", tk.GetState())
		}
		setState(tk, task.StateRunning)
		if tk.RuntimeStarted() {
			t.Fatal("running task reconnected")
		}
		setState(tk, task.StateStopped)
		if !tk.RuntimeStarted() || tk.GetState() != task.StateWaiting {
			t.Fatalf("state = %s, want waiting", tk.GetState())
		}
		if tk.RuntimeStarted() {
			t.Fatal("live task reconnected")
		}
	})
	t.Run("RuntimeDestroyed", func(t *testing.T) {
		t.Parallel()
		for _, s := range []task.State{task.StateStopped, task.StateWaiting} {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, s)
			if !tk.RuntimeDestroyed() || tk.GetState() != task.StateFailed {
				t.Errorf("%s: state = %s, want failed", s, tk.GetState())
			}
		}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(tk, task.StateStopping)
		if tk.RuntimeDestroyed() {
			t.Fatal("stopping task failed")
		}
	})
	t.Run("SessionLost", func(t *testing.T) {
		t.Parallel()
		for s, want := range map[task.State]task.State{task.StateRunning: task.StateWaiting, task.StateAsking: task.StateAsking, task.StateStopping: task.StateStopping} {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			setState(tk, s)
			tk.SessionLost(time.Time{})
			if got := tk.GetState(); got != want {
				t.Errorf("%s: state = %s, want %s", s, got, want)
			}
		}
	})
	t.Run("MarkSession", func(t *testing.T) {
		t.Parallel()
		silent := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(silent, task.StateStarting)
		if silent.MarkSessionCrashed() || !silent.MarkSessionFailed() || silent.GetState() != task.StateFailed {
			t.Fatalf("silent session state = %s, want failed", silent.GetState())
		}
		active := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(active, task.StateStarting)
		addTaskMessage(active, &agent.UserInputMessage{Text: "go"})
		if active.MarkSessionFailed() || !active.MarkSessionCrashed() || active.GetState() != task.StateCrashed {
			t.Fatalf("active session state = %s, want crashed", active.GetState())
		}
		stopping := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		setState(stopping, task.StateStopping)
		if stopping.MarkSessionFailed() || stopping.MarkSessionCrashed() {
			t.Fatalf("stopping session marked %s", stopping.GetState())
		}
	})
}

func TestPricedCost(t *testing.T) {
	t.Parallel()
	// glm-5.3-flash: $0.15/M input, $0.03/M cache read, $0.50/M output.
	prices := fakePricer{"zai/glm-5.3-flash": {InputPerMTok: 0.15, CachedInputPerMTok: 0.03, OutputPerMTok: 0.50}}

	t.Run("UsageContributionIsHarnessNeutral", func(t *testing.T) {
		t.Parallel()
		for _, h := range []harness.Name{harness.Claude, harness.Codex, harness.OpenCode, harness.Pi} {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, h, "", "")
			messages := []agent.Message{
				&agent.UsageMessage{Additive: true, Usage: agent.Usage{InputTokens: 100}},
				&agent.UsageMessage{Usage: agent.Usage{InputTokens: 900}},
				&agent.ResultMessage{UsageIsSnapshot: true, Usage: agent.Usage{InputTokens: 50}},
				&agent.ResultMessage{Usage: agent.Usage{InputTokens: 7}},
			}
			for _, m := range messages {
				addTaskMessage(tk, m)
			}
			snapshot := tk.Snapshot()
			total, last := snapshot.Usage, snapshot.LastUsage
			if total.InputTokens != 107 || last.InputTokens != 7 {
				t.Fatalf("%s: total=%+v, context=%+v", h, total, last)
			}
		}
	})

	t.Run("PiCacheTotalsAndPersistence", func(t *testing.T) {
		t.Parallel()
		messages := []agent.Message{
			&agent.UsageMessage{Additive: true, PerCall: true, Usage: agent.Usage{CacheCreationInputTokens: 100, CacheWrites: tokens.CacheWrites{{TTL: time.Hour, Tokens: 100}}, CacheTTLSeconds: 3600}},
			&agent.UsageMessage{Additive: true, PerCall: true, Usage: agent.Usage{CacheCreationInputTokens: 10, CacheTTLSeconds: 300}},
			&agent.ResultMessage{MessageType: "result", UsageIsSnapshot: true, Usage: agent.Usage{CacheCreationInputTokens: 10, CacheTTLSeconds: 300}},
			// An invocation without per-call events still contributes its result.
			&agent.ResultMessage{MessageType: "result", Usage: agent.Usage{CacheCreationInputTokens: 7}},
		}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Pi, "anthropic/claude-opus-4-6", "")
		for _, msg := range messages {
			addTaskMessage(tk, msg)
		}
		snapshot := tk.Snapshot()
		total, last := snapshot.Usage, snapshot.LastUsage
		want := agent.Usage{CacheCreationInputTokens: 117, CacheWrites: tokens.CacheWrites{{Tokens: 7}, {TTL: 5 * time.Minute, Tokens: 10}, {TTL: time.Hour, Tokens: 100}}}
		if !total.Equal(want) || last.CacheCreationInputTokens != 7 {
			t.Fatalf("total=%+v, last=%+v", total, last)
		}
		store := taskhistory.NewStore(logtest.Logger(t), taskslog.NewStore(t.TempDir()), backends.Default("", nil))
		log, path, err := store.Open(tk.LogFilename(), tk.LogHeader())
		if err != nil {
			t.Fatal(err)
		}
		res := &task.Result{State: task.StatePurged, Usage: total}
		if err := log.AppendMessage(taskhistory.ResultMessage("done", res)); err != nil {
			t.Fatal(err)
		}
		loaded, err := store.LoadAllReadOnly()
		if err != nil || len(loaded) != 1 || !loaded[0].LastTrailer.Usage.Equal(want) {
			t.Fatalf("trailer restore = %+v, %v", loaded, err)
		}
		if _, err := store.CompressTerminal(path, log, tk.terminalLogSummary(res)); err != nil {
			t.Fatal(err)
		}
		loaded, err = store.LoadAllReadOnly()
		if err != nil || len(loaded) != 1 || !loaded[0].LastTrailer.Usage.Equal(want) {
			t.Fatalf("cached restore = %+v, %v", loaded, err)
		}
	})

	t.Run("CodexRequestCostRollup", func(t *testing.T) {
		t.Parallel()
		for _, exited := range []bool{false, true} {
			messages := []agent.Message{
				&agent.UsageMessage{PerCall: true, ModelDerived: true, ReportedModel: "gpt-6.1-sol", Usage: agent.Usage{InputTokens: 300_000, OutputTokens: 1_000}},
			}
			resultUsage, wantCost := agent.Usage{InputTokens: 300_000, OutputTokens: 1_000}, 1.215
			if exited {
				messages = append(messages, &agent.ExitMessage{ExitCode: 1})
				// A new session's aggregate excludes requests before the exit.
				resultUsage, wantCost = agent.Usage{InputTokens: 100_000}, 1.415
			}
			messages = append(messages, &agent.ResultMessage{MessageType: "result", Usage: resultUsage})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-6.1-sol", "")
			tk.Pricer = quotausage.NewPricer(nil)
			sink := &rollupSpy{}
			tk.Rollup = sink
			for _, msg := range messages {
				addTaskMessage(tk, msg)
			}
			snapshot := tk.Snapshot()
			cost, tokenCounts := snapshot.CostUSD, snapshot.Usage
			if math.Abs(cost-wantCost) > 1e-9 || tokenCounts.InputTokens != resultUsage.InputTokens || tokenCounts.OutputTokens != resultUsage.OutputTokens {
				t.Fatalf("exited %v: cost=%v, tokens=%+v", exited, cost, tokenCounts)
			}
			last := sink.Observes[len(sink.Observes)-1].Event
			if !last.TurnBoundary || math.Abs(last.CostUSD-wantCost) > 1e-9 || last.Delta.Input != int64(resultUsage.InputTokens) {
				t.Fatalf("exited %v: rollup=%+v", exited, last)
			}
		}
	})

	t.Run("CodexResultKeepsRawCostSeparate", func(t *testing.T) {
		t.Parallel()
		const model = "gpt-6-sol"
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, model, "")
		tk.Pricer = fakePricer{model: {InputPerMTok: 2, CachedInputPerMTok: 0.2, OutputPerMTok: 10}}
		setState(tk, task.StateRunning)
		first := &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 100_000, CacheReadInputTokens: 1_000_000, OutputTokens: 10_000}}
		second := &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 50_000, OutputTokens: 10_000}}
		messages := []agent.Message{first, &agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary"}, second}
		for _, m := range messages {
			addTaskMessage(tk, m)
		}
		if first.TotalCostUSD != 0 || second.TotalCostUSD != 0 {
			t.Errorf("raw result costs = %v, %v; want 0, 0", first.TotalCostUSD, second.TotalCostUSD)
		}
		if got := tk.Snapshot().CostUSD; got != 0.7 {
			t.Errorf("task cost = %v, want 0.7", got)
		}
	})

	t.Run("OpenCodeReportedCostAcrossCompactionAndClear", func(t *testing.T) {
		t.Parallel()
		newMessages := func() []agent.Message {
			cost := func(value float64) *agent.UsageMessage {
				return &agent.UsageMessage{CumulativeCostUSD: &value}
			}
			return []agent.Message{
				cost(0.40),
				&agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 1_000_000}},
				&agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary"},
				cost(0.75),
				&agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 1_000_000}},
				agent.ContextCleared(),
				cost(0.20),
				&agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 1_000_000}},
			}
		}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.OpenCode, "zai/glm-5.3-flash", "")
		tk.Pricer = prices
		setState(tk, task.StateRunning)
		for _, m := range newMessages() {
			addTaskMessage(tk, m)
		}
		if got := tk.Snapshot().CostUSD; got != 0.95 {
			t.Errorf("cost = %v, want 0.95", got)
		}
	})
	t.Run("OpenCodeZeroReportedCostRemainsKnown", func(t *testing.T) {
		t.Parallel()
		const model = "zai-coding-plan/glm-4.7"
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.OpenCode, model, "")
		tk.Pricer = fakePricer{model: {InputPerMTok: 0.15}}
		setState(tk, task.StateRunning)
		zero := 0.0
		addTaskMessage(tk, &agent.UsageMessage{CumulativeCostUSD: &zero})
		addTaskMessage(tk, &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 1_000_000}})
		if snapshot := tk.Snapshot(); snapshot.CostUSD != 0 || !snapshot.CostKnown {
			t.Errorf("cost = %v, want known zero", snapshot.CostUSD)
		}
	})

	t.Run("PiCompactionUsageCountsAcrossBoundary", func(t *testing.T) {
		t.Parallel()
		newTask := func(t *testing.T) *Task {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Pi, "zai/glm-5.3-flash", "")
			tk.Pricer = prices
			setState(tk, task.StateRunning)
			return tk
		}
		messages := func() []agent.Message {
			return []agent.Message{
				&agent.UsageMessage{Additive: true, ReportedModel: "zai/glm-5.3-flash", Usage: agent.Usage{InputTokens: 1_000_000}},
				&agent.UsageMessage{Additive: true, Usage: agent.Usage{InputTokens: 1_000_000}},
				&agent.SystemMessage{MessageType: "system", Subtype: agent.SystemSubtypeCompactBoundary, ContextTokensAfter: 20_000},
				&agent.UsageMessage{Additive: true, ReportedModel: "zai/glm-5.3-flash", Usage: agent.Usage{InputTokens: 1_000_000}},
			}
		}
		for _, tc := range []struct {
			name    string
			restore bool
		}{
			{name: "live"},
			{name: "restore", restore: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				tk := newTask(t)
				if tc.restore {
					tk.SeedTimelineParts(nil, agenttest.TimedMessages(messages()), nil)
				} else {
					for _, m := range messages() {
						addTaskMessage(tk, m)
					}
				}
				if snapshot := tk.Snapshot(); math.Abs(snapshot.CostUSD-0.45) > 1e-9 || snapshot.Usage.InputTokens != 3_000_000 {
					t.Errorf("costUSD = %v, input = %d; want 0.45 and 3000000 including compaction", snapshot.CostUSD, snapshot.Usage.InputTokens)
				}
			})
		}
	})

	t.Run("UsageMessagesAccumulatePerCall", func(t *testing.T) {
		t.Parallel()
		// Pi reports usage per API call; its result must not regress the
		// accumulated value when per-call pricing has already accounted for it.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Pi, "zai/glm-5.3-flash", "")
		tk.Pricer = prices
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"})
		addTaskMessage(tk, &agent.UsageMessage{Additive: true,
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		})
		addTaskMessage(tk, &agent.UsageMessage{Additive: true,
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 2_000_000, CacheReadInputTokens: 1_000_000},
		})
		costUSD := tk.Snapshot().CostUSD
		want := 0.15 + 0.50 + 2*0.15 + 0.03
		if costUSD != want {
			t.Fatalf("costUSD = %v, want %v", costUSD, want)
		}
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType:     "result",
			UsageIsSnapshot: true,
			TotalCostUSD:    0.01, // Last call only.
			NumTurns:        2,
		})
		if costUSD = tk.Snapshot().CostUSD; costUSD != want {
			t.Errorf("costUSD after result = %v, want %v (must keep accumulated value)", costUSD, want)
		}
	})

	t.Run("OpenCodeResultPricedPerTurn", func(t *testing.T) {
		t.Parallel()
		// OpenCode reports no per-call usage; its per-turn result usage is
		// priced and accumulated.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.OpenCode, "zai/glm-5.3-flash", "")
		tk.Pricer = prices
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"})
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			NumTurns:    1,
		})
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 2_000_000, CacheReadInputTokens: 1_000_000},
			NumTurns:    1,
		})
		costUSD := tk.Snapshot().CostUSD
		want := 0.15 + 0.50 + 2*0.15 + 0.03
		if costUSD != want {
			t.Errorf("costUSD = %v, want %v", costUSD, want)
		}
	})

	t.Run("CodexResultPricedPerTurn", func(t *testing.T) {
		t.Parallel()
		// Codex reports no cost; its prefixless per-turn result usage is
		// priced at the hinted provider's API-equivalent rates.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-5.6-terra", "")
		codexPrices := fakePricer{"gpt-5.6-terra": {InputPerMTok: 2.0, CachedInputPerMTok: 0.20, OutputPerMTok: 12.0}}
		tk.Pricer = codexPrices
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.InitMessage{ReportedModel: "gpt-5.6-terra"})
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
			NumTurns:    1,
		})
		costUSD := tk.Snapshot().CostUSD
		if want := 2.0 + 0.20; costUSD != want {
			t.Errorf("costUSD = %v, want %v", costUSD, want)
		}
	})

	t.Run("CodexDerivedUsageNotPricedPerCall", func(t *testing.T) {
		t.Parallel()
		// Codex usage records carry a caic-derived session-model stamp for
		// per-model usage analytics. Pricing must stay per turn via the
		// result: derived records add no per-call cost on top of it.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-5.6-terra", "")
		codexPrices := fakePricer{"gpt-5.6-terra": {InputPerMTok: 2.0, CachedInputPerMTok: 0.20, OutputPerMTok: 12.0}}
		tk.Pricer = codexPrices
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.InitMessage{ReportedModel: "gpt-5.6-terra"})
		addTaskMessage(tk, &agent.UsageMessage{
			ReportedModel: "gpt-5.6-terra",
			ModelDerived:  true,
			Usage:         agent.Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
		})
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
			NumTurns:    1,
		})
		costUSD := tk.Snapshot().CostUSD
		if want := 2.0 + 0.20; costUSD != want {
			t.Errorf("costUSD = %v, want %v (per-turn pricing only)", costUSD, want)
		}
	})

	t.Run("ClaudeDerivedDeltaUsageNotPriced", func(t *testing.T) {
		t.Parallel()
		// Claude's assistant records carry the harness-reported model and are
		// priced per call. The backend-derived message_delta stamp describing
		// the same API call must not price again.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "zai/glm-5.3-flash", "")
		tk.Pricer = prices
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"})
		addTaskMessage(tk, &agent.UsageMessage{
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		})
		addTaskMessage(tk, &agent.UsageMessage{
			ReportedModel: "zai/glm-5.3-flash",
			ModelDerived:  true,
			Usage:         agent.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		})
		addTaskMessage(tk, &agent.ResultMessage{MessageType: "result", NumTurns: 1})
		costUSD := tk.Snapshot().CostUSD
		if want := 0.15 + 0.50; costUSD != want {
			t.Errorf("costUSD = %v, want %v (assistant-record pricing only)", costUSD, want)
		}
	})

	t.Run("UnpricedModelKeepsReportedTotal", func(t *testing.T) {
		t.Parallel()
		// Claude Code reports the authoritative session total, which the
		// priced path does not touch for unpriced models.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "claude-sonnet-4-5", "")
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.InitMessage{ReportedModel: "claude-sonnet-4-5"})
		addTaskMessage(tk, &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 10.0,
			Usage:        agent.Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
			NumTurns:     1,
		})
		costUSD := tk.Snapshot().CostUSD
		if want := 10.0; costUSD != want {
			t.Errorf("costUSD = %v, want %v (reported total)", costUSD, want)
		}
	})

	t.Run("PricedCostAccumulatesAcrossSessions", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Pi, "zai/glm-5.3-flash", "")
		tk.Pricer = prices
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"})
		addTaskMessage(tk, &agent.UsageMessage{
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 1_000_000},
		})
		addTaskMessage(tk, agent.ContextCleared())
		setState(tk, task.StateRunning)
		addTaskMessage(tk, &agent.UsageMessage{
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 1_000_000},
		})
		costUSD := tk.Snapshot().CostUSD
		if costUSD != 0.30 {
			t.Errorf("costUSD = %v, want 0.30", costUSD)
		}
	})

	t.Run("SeedTimelinePricesUsageMessages", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Pi, "zai/glm-5.3-flash", "")
		tk.Pricer = prices
		setState(tk, task.StatePurged)
		tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{
			&agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"},
			&agent.UsageMessage{ReportedModel: "zai/glm-5.3-flash", Usage: agent.Usage{InputTokens: 1_000_000}},
			&agent.ResultMessage{MessageType: "result", NumTurns: 1},
		}), nil)
		costUSD := tk.Snapshot().CostUSD
		if costUSD != 0.15 {
			t.Errorf("costUSD = %v, want 0.15", costUSD)
		}
	})
}

// BenchmarkTaskCacheWriteReplay measures restoring 1,000 cached requests across
// 100 invocations, where cumulative accounting must retain cache lifetimes.
func BenchmarkTaskCacheWriteReplay(b *testing.B) {
	const model = "anthropic/claude-opus-4-6"
	prices := quotausage.NewPricer(nil)
	messages := make([]agent.Message, 0, 1100)
	u := agent.Usage{InputTokens: 100, CacheCreationInputTokens: 1000, CacheReadInputTokens: 10_000, OutputTokens: 100, CacheTTLSeconds: 3600}
	for range 100 {
		for range 10 {
			messages = append(messages, &agent.UsageMessage{Additive: true, PerCall: true, ReportedModel: model, Usage: u})
		}
		messages = append(messages, &agent.ResultMessage{MessageType: "result", UsageIsSnapshot: true, Usage: u})
	}
	b.ReportAllocs()
	for b.Loop() {
		tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "cache replay"}, harness.Pi, model, "")
		tk.Pricer = prices
		tk.SeedTimelineParts(nil, agenttest.TimedMessages(messages), nil)
	}
}

// fakePricer prices a fixed set of models; unlisted models are unpriced.
type fakePricer map[string]quotausage.ModelPrice

func (p fakePricer) ModelPrice(modelID string, _ time.Time) (quotausage.ModelPrice, bool) {
	price, ok := p[modelID]
	return price, ok
}

// TestMain isolates the package from the Git configuration of the host.
func TestMain(m *testing.M) {
	gittest.Isolate()
	os.Exit(m.Run())
}

func BenchmarkCodexResultPricing(b *testing.B) {
	const model = "gpt-6-sol"
	prices := fakePricer{model: {InputPerMTok: 2, CachedInputPerMTok: 0.2, OutputPerMTok: 10}}
	messages := make([]agent.Message, 100)
	for i := range messages {
		messages[i] = &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{
			InputTokens: 1_000, CacheReadInputTokens: 10_000, OutputTokens: 500,
		}}
	}
	b.ReportAllocs()
	for b.Loop() {
		tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark"}, harness.Codex, model, "")
		tk.Pricer = prices
		tk.SeedTimelineParts(nil, agenttest.TimedMessages(messages), nil)
	}
}

func BenchmarkCodexRequestPricing(b *testing.B) {
	const model = "gpt-6.1-sol"
	prices := quotausage.NewPricer(nil)
	messages := make([]agent.Message, 0, 1_100)
	for range 100 {
		for range 10 {
			messages = append(messages, &agent.UsageMessage{PerCall: true, ModelDerived: true,
				ReportedModel: model, Usage: agent.Usage{InputTokens: 50_000, CacheReadInputTokens: 250_000, OutputTokens: 500}})
		}
		messages = append(messages, &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{
			InputTokens: 500_000, CacheReadInputTokens: 2_500_000, OutputTokens: 5_000,
		}})
	}
	b.ReportAllocs()
	for b.Loop() {
		tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "benchmark"}, harness.Codex, model, "")
		tk.Pricer = prices
		tk.SeedTimelineParts(nil, agenttest.TimedMessages(messages), nil)
	}
}
