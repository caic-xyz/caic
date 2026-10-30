// Tests task timelines, delivery evidence, state, accounting, and pending-action recovery.

package task

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

	"github.com/caic-xyz/caic/backend/internal/tokens"

	"github.com/maruel/genai"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/logtest"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	quotausage "github.com/caic-xyz/caic/backend/internal/usage"
)

type failingConn struct {
	err error
}

func (c *failingConn) SendPrompt(agent.Prompt, []byte) error { return c.err }

func (*failingConn) RecordInputFailure(_ agent.Prompt, err error) error { return err }

func (*failingConn) SendRaw([]byte) error { return nil }

func (*failingConn) SendCompact(string) error { return nil }

func (*failingConn) ReadMessages(io.ReadCloser, chan<- agent.TimedMessage, agent.MessageInterceptor) error {
	return nil
}

func (*failingConn) SendStop(context.Context) {}

func (*failingConn) Cancel() {}

// completingInputWriter models an agent that finishes before its input write returns.
type completingInputWriter struct {
	msgCh chan<- agent.TimedMessage
}

func (w *completingInputWriter) Write(p []byte) (int, error) {
	w.msgCh <- agent.TimedMessage{Message: &agent.TextMessage{Text: "done"}}
	w.msgCh <- agent.TimedMessage{Message: &agent.ResultMessage{MessageType: "result"}}
	return len(p), nil
}

func (*completingInputWriter) Close() error { return nil }

type partialInputWriter struct {
	msgCh    chan<- agent.TimedMessage
	observed <-chan struct{}
}

func (w *partialInputWriter) Write([]byte) (int, error) {
	w.msgCh <- agent.TimedMessage{Message: &agent.TextMessage{Text: "working"}}
	<-w.observed
	return 1, io.ErrUnexpectedEOF
}

func (*partialInputWriter) Close() error { return nil }

type queuedDeltaInputWriter struct {
	msgCh chan<- agent.TimedMessage
}

func (w *queuedDeltaInputWriter) Write([]byte) (int, error) {
	w.msgCh <- agent.TimedMessage{Message: &agent.TextDeltaMessage{Text: "working"}}
	return 1, io.ErrUnexpectedEOF
}

func (*queuedDeltaInputWriter) Close() error { return nil }

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
				tk.SeedTimeline(history)
				p := &titleBenchmarkProvider{}
				tk.Provider = p
				log := testLogger()
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					tk.GenerateTitle(b.Context(), log)
				}
				b.ReportMetric(float64(p.inputBytes), "input-bytes")
			})
		}
	})
}

// recvType reads one message type from a live subscriber channel. The task
// fanout performs a synchronous non-blocking send into a buffered channel
// during addMessage, so a message is available immediately on return; a
// non-blocking receive is therefore deterministic with no timing dependency.
func recvType(ch <-chan TimelineMessage) string {
	select {
	case m, ok := <-ch:
		if !ok {
			return ""
		}
		return m.Message.Type()
	default:
		return ""
	}
}

func TestTask(t *testing.T) {
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
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				live.SetState(taskslog.StateRunning)
				for _, m := range history {
					live.addMessage(t.Context(), m)
				}
				seeded := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				seeded.SetState(taskslog.StateRunning)
				seeded.SeedTimeline(history)
				if got := live.LastExitError(); got != want {
					t.Fatalf("live exit = %q, want %q", got, want)
				}
				if got := seeded.LastExitError(); got != live.LastExitError() {
					t.Fatalf("seeded exit = %q, live = %q", got, live.LastExitError())
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
			{name: "child_output", messages: []agent.Message{agent.InputDeliveryStarted(), input, &agent.TextMessage{Source: agent.MessageSource{AgentID: "child"}, Text: "working"}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				for _, version := range []agent.LogVersion{agent.LogVersionV2, agent.LogVersionV3} {
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
					parser, err := agent.NewLogRecordParser(version, func([]byte) ([]agent.Message, error) { return nil, nil })
					if err != nil {
						t.Fatal(err)
					}
					var restored []agent.Message
					for _, m := range tc.messages {
						data, err := agent.MarshalLogMessage(version, m)
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
					tk.SeedTimeline(restored)
					// V2 has no child provenance; its historical output is global.
					accepted := tc.accepted || version == agent.LogVersionV2 && tc.name == "child_output"
					if got := tk.HasAcceptedInputEvidence(); got != accepted {
						t.Fatalf("v%d evidence = %t, want %t", version, got, accepted)
					}
				}
			})
		}
	})
	t.Run("PendingInputPreservesCompletedTurnSemantics", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.UserInputMessage{Text: "accepted"},
			&agent.ResultMessage{Result: "done"},
			agent.InputDeliveryStarted(),
			&agent.UserInputMessage{Text: "unresolved"},
		})
		if got := tk.GetState(); got != taskslog.StateWaiting {
			t.Fatalf("state = %s, want previous completed turn to remain waiting", got)
		}
		if got := tk.titleInput(); got != "Current title: test\nRecent activity:\nPrompt: accepted\nResult: done\n" {
			t.Fatalf("title input = %q", got)
		}
	})
	t.Run("PlanSummaryChanges", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.ToolUseMessage{ToolUseID: "write", Name: "Write", Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"first"}`)},
			&agent.ToolUseMessage{ToolUseID: "exit", Name: "ExitPlanMode"},
		})
		tk.SetState(taskslog.StateRunning)
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
					history = append(history, &agent.ResultMessage{}, &agent.ResultMessage{Source: agent.MessageSource{AgentID: "child"}})
				}
				tk.SeedTimeline(history)
				for n := restored + 1; n <= 151; n++ {
					tk.addParsedMessage(agent.TimedMessage{Message: &agent.ResultMessage{Source: agent.MessageSource{AgentID: "child"}}})
					tk.addParsedMessage(agent.TimedMessage{Message: &agent.ResultMessage{NumTurns: 99}})
					if got := tk.titleRefreshDue(); got != slices.Contains(want, n) {
						t.Fatalf("refresh at turn %d = %v", n, got)
					}
					if n == 20 {
						tk.ClearMessages(t.Context())
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
				tk.GenerateTitle(t.Context(), testLogger())
			})
			<-p.started
			tk.GenerateTitle(t.Context(), testLogger())
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
					&agent.ResultMessage{Source: agent.MessageSource{AgentID: "child"}, Result: "child activity"},
					&agent.ResultMessage{},
				)
			}
			tk.SeedTimeline(history)
			tk.SetTitle("Current title")
			p := &titleSequenceProvider{replies: []string{"Recent subject"}}
			tk.Provider = p
			tk.GenerateTitle(t.Context(), testLogger())
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
			tk.SeedTimeline(history)
			p := &titleSequenceProvider{replies: []string{"Short title"}}
			tk.Provider = p
			tk.GenerateTitle(t.Context(), testLogger())
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
					tk.GenerateTitle(t.Context(), testLogger())
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
				tk.GenerateTitle(t.Context(), testLogger())
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
				quota := &agent.RateLimitMessage{Source: agent.MessageSource{AgentID: "child"}, Status: agent.RateLimitStatusRejected, RateLimitType: "five_hour", ResetsAt: time.Now().Add(time.Hour), QuotaLabel: "shared account", QuotaProvider: agent.QuotaProviderCodex}
				if live {
					tk.addMessage(t.Context(), quota)
				} else {
					tk.SeedTimeline([]agent.Message{quota})
				}
				history, _, unsub := tk.SubscribeRateLimits(t.Context())
				unsub()
				if len(history) != 1 || tk.rateLimit.Status != agent.RateLimitStatusRejected || len(sink.Quotas) != 1 {
					t.Fatalf("child-origin account quota discarded: history=%v quota=%+v sink=%v", history, tk.rateLimit, sink.Quotas)
				}
				if prompt := BuildHandoffPrompt(tk, 0); !strings.Contains(prompt, "shared account") {
					t.Fatalf("provider block lost from handoff: %s", prompt)
				}
			})
		}
	})
	t.Run("ChildOwnershipLiveAndSeed", func(t *testing.T) {
		t.Parallel()
		parent := agent.MessageSource{ThreadID: "root", TurnID: "turn", RootThreadID: "root"}
		child := agent.MessageSource{ThreadID: "child", TurnID: "child-turn", RootThreadID: "root", AgentID: "child"}
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
					tk.SetState(taskslog.StateRunning)
					for _, message := range messages {
						tk.addMessage(t.Context(), message)
					}
				} else {
					tk.SeedTimeline(messages)
				}
				if tk.GetState() != taskslog.StateWaiting || tk.sessionID != "root" || tk.reportedModel != "root-model" || tk.costTracker.Model != "root-model" || tk.inPlanMode {
					t.Fatalf("child changed root state: state=%s session=%s model=%s costmodel=%s plan=%t", tk.GetState(), tk.sessionID, tk.reportedModel, tk.costTracker.Model, tk.inPlanMode)
				}
				if tk.lastAPIUsage.InputTokens != 12 || tk.reportedContextWindow != 100 || tk.liveUsage.InputTokens != 30 {
					t.Fatalf("root context/totals changed: latest=%+v window=%d totals=%+v", tk.lastAPIUsage, tk.reportedContextWindow, tk.liveUsage)
				}
				if got := tk.LastAgentResult(); got != "root answer" {
					t.Errorf("last result = %q", got)
				}
				history, _, unsub := tk.SubscribeRateLimits(t.Context())
				unsub()
				if len(sink.Quotas) != 0 {
					t.Fatalf("child goal affected provider quota: %+v", sink.Quotas)
				}
				if len(history) != 0 {
					t.Fatalf("child thread quota entered root subscription: %+v", history)
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
		parent := agent.MessageSource{ThreadID: "root", TurnID: "turn", RootThreadID: "root"}
		child := agent.MessageSource{ThreadID: "child", TurnID: "turn", RootThreadID: "root"}
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.ToolOutputDeltaMessage{Source: parent, ToolUseID: "same", Delta: "parent output"},
			&agent.ToolResultMessage{Source: child, ToolUseID: "same"},
			&agent.ToolOutputDeltaMessage{Source: child, ToolUseID: "same", Delta: "child output"},
			&agent.ToolResultMessage{Source: child, ToolUseID: "same"},
		})
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
	t.Run("SeededResultContextWindowUpdatesSnapshot", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SeedTimeline([]agent.Message{&agent.ResultMessage{Result: "done", ContextWindow: 180_000}})
		if got := tk.Snapshot().ContextWindowLimit; got != 180_000 {
			t.Fatalf("ContextWindowLimit = %d, want 180000", got)
		}
	})
	t.Run("AutoCompactThresholdLiveAndReplay", func(t *testing.T) {
		t.Parallel()
		live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		live.addParsedMessage(agent.TimedMessage{Message: &agent.InitMessage{AutoCompactTokenLimit: 244800}})
		if got := live.Snapshot().AutoCompactTokenLimit; got != 244800 {
			t.Fatalf("live threshold = %d", got)
		}
		restored := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		restored.SeedTimeline([]agent.Message{&agent.InitMessage{AutoCompactTokenLimit: 244800}})
		if got := restored.Snapshot().AutoCompactTokenLimit; got != 244800 {
			t.Fatalf("restored threshold = %d", got)
		}
	})
	t.Run("BackwardMessages", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		first := &agent.TextMessage{Text: "first"}
		last := &agent.TextMessage{Text: "last"}
		tk.SeedTimeline([]agent.Message{first, &agent.LogMessage{Line: "skip"}, last})
		messages := tk.BackwardMessages()
		tk.addMessage(t.Context(), &agent.TextMessage{Text: "after snapshot"})

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
		tk.SeedTimelineParts(nil, []agent.TimedMessage{{Message: message, ProducerTime: time.UnixMilli(timestamp)}})

		history, _, unsubscribe := tk.Subscribe(t.Context())
		defer unsubscribe()
		if len(history) != 1 || history[0].Message != message || history[0].ObservedAt.UnixMilli() != timestamp {
			t.Fatalf("history = %#v, want message observed at %d", history, timestamp)
		}
	})
	t.Run("SeedTimelineCompactsFinalizedDeltas", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		textDelta := &agent.TextDeltaMessage{Text: strings.Repeat("text", 1024)}
		thinkingDelta := &agent.ThinkingDeltaMessage{Text: strings.Repeat("thinking", 1024)}
		toolDelta := &agent.ToolOutputDeltaMessage{ToolUseID: "tool-1", Delta: strings.Repeat("output", 1024)}
		unfinished := &agent.TextDeltaMessage{Text: "still live"}
		tk.SeedTimeline([]agent.Message{
			textDelta,
			&agent.TextMessage{Text: "text complete"},
			thinkingDelta,
			&agent.ThinkingMessage{Text: "thinking complete"},
			toolDelta,
			&agent.ToolResultMessage{ToolUseID: "tool-1"},
			unfinished,
		})

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
		tk.SeedTimelineParts(prefix, suffix)

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
					parent := agent.MessageSource{ThreadID: "parent", RootThreadID: "parent", TurnID: "turn", ItemID: "parent-item"}
					child := agent.MessageSource{ThreadID: "child", RootThreadID: "parent", TurnID: "child-turn", ItemID: "child-item"}
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
							tk.addMessage(t.Context(), m)
						}
					} else {
						prefix := []agent.TimedMessage{{Message: messages[0]}, {Message: messages[1]}}
						suffix := []agent.TimedMessage{{Message: messages[2]}, {Message: messages[3]}, {Message: messages[4]}}
						tk.SeedTimelineParts(prefix, suffix)
					}
					history, _, unsubscribe := tk.SubscribeSnapshot(t.Context())
					defer unsubscribe()
					defer history.Release()
					for _, i := range []int{0, 2} {
						if got := history.At(i).Message; got != messages[i] {
							t.Errorf("parent fragment %d compacted by child: %#v", i, got)
						}
					}
					for _, i := range []int{1, 3} {
						var text string
						var source agent.MessageSource
						switch m := history.At(i).Message.(type) {
						case *agent.TextDeltaMessage:
							text, source = m.Text, m.Source
						case *agent.ThinkingDeltaMessage:
							text, source = m.Text, m.Source
						}
						if text != "" || source != child {
							t.Errorf("child placeholder %d lost identity or retained payload: %#v", i, history.At(i).Message)
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
		tk.addMessage(t.Context(), delta)
		tk.addMessage(t.Context(), &agent.TextMessage{Text: "complete"})

		if tk.timeline[0].Message != compactedTextDelta {
			t.Errorf("live delta = %#v, want compacted placeholder", tk.timeline[0].Message)
		}
		if delta.Text == "" {
			t.Error("published live delta was mutated")
		}
	})
	t.Run("LiveCompactionWaitsForSnapshotRelease", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		delta := &agent.TextDeltaMessage{Text: strings.Repeat("payload", 1024)}
		tk.addMessage(t.Context(), delta)
		history, _, unsubscribe := tk.SubscribeSnapshot(t.Context())
		defer unsubscribe()
		tk.addMessage(t.Context(), &agent.TextMessage{Text: "complete"})

		if got := history.At(0).Message; got != delta {
			t.Fatalf("active snapshot delta = %#v, want original", got)
		}
		if tk.timeline[0].Message != delta {
			t.Fatal("retained delta compacted while snapshot was active")
		}
		history.Release()
		if tk.timeline[0].Message != compactedTextDelta {
			t.Errorf("released snapshot delta = %#v, want compacted placeholder", tk.timeline[0].Message)
		}
	})
	t.Run("CanceledSnapshotRemainsStableUntilRelease", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		delta := &agent.TextDeltaMessage{Text: strings.Repeat("payload", 1024)}
		tk.addMessage(t.Context(), delta)
		ctx, cancel := context.WithCancel(t.Context())
		history, _, unsubscribe := tk.SubscribeSnapshot(ctx)
		cancel()
		tk.addMessage(t.Context(), &agent.TextMessage{Text: "complete"})

		if got := history.At(0).Message; got != delta {
			t.Fatalf("canceled snapshot delta = %#v, want original", got)
		}
		if tk.timeline[0].Message != delta {
			t.Fatal("retained delta compacted before canceled snapshot was released")
		}
		unsubscribe()
		if tk.timeline[0].Message != compactedTextDelta {
			t.Errorf("released canceled snapshot delta = %#v, want compacted placeholder", tk.timeline[0].Message)
		}
	})
	t.Run("LiveSubscriberRetainsPublishedDelta", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		_, live, unsubscribe := tk.SubscribeLiveMessages(t.Context())
		defer unsubscribe()
		delta := &agent.TextDeltaMessage{Text: strings.Repeat("payload", 1024)}
		tk.addMessage(t.Context(), delta)
		tk.addMessage(t.Context(), &agent.TextMessage{Text: "complete"})

		if got := recvMsg(t, live); got != delta || delta.Text == "" {
			t.Errorf("published delta = %#v, want original payload", got)
		}
	})
	t.Run("SubscribeSnapshotRetainsCallTimeHistory", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		first := &agent.TextMessage{Text: "first"}
		tk.SeedTimeline([]agent.Message{first})
		history, live, unsubscribe := tk.SubscribeSnapshot(t.Context())
		defer unsubscribe()
		defer history.Release()

		second := &agent.TextMessage{Text: "second"}
		tk.addMessage(t.Context(), second)
		if history.Len() != 1 {
			t.Fatalf("snapshot length = %d, want 1", history.Len())
		}
		if got := history.At(0); got.Message != first || got.Sequence != 1 {
			t.Errorf("snapshot message = %#v, want first message at sequence 1", got)
		}
		if got := recvMsg(t, live); got != second {
			t.Errorf("live message = %#v, want second message", got)
		}
	})
	t.Run("TerminalLogSummary", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "persist me"}, harness.Claude, "requested", "high")
		tk.BaseImage = "image"
		tk.ContainerPlatform = "linux/amd64"
		tk.SetTitle("title")
		tk.Repos = []taskslog.RepoMount{{Name: "org/repo", Branch: "caic-1", GitRoot: "/host/checkout"}}
		tk.SetSessionMetadata("session-1", "reported", "", "1.2.3")
		tk.SetState(taskslog.StatePurged)
		result := &taskslog.Result{State: taskslog.StatePurged, AgentResult: "done"}
		parentID := ksid.NewID()
		tk.ParentTaskID = parentID
		tk.OwnerID = "user-1"
		tk.CaicMCP = true

		summary := tk.terminalLogSummary(agent.LogVersionV1, result)
		if summary.LogVersion != agent.LogVersionV1 || summary.State != taskslog.StatePurged || summary.LastTrailer != result {
			t.Fatalf("terminal summary = %#v", summary)
		}
		if summary.SessionID != "session-1" || summary.RequestedModel != "requested" || summary.ReportedModel != "reported" || summary.AgentVersion != "1.2.3" {
			t.Errorf("session summary = (%q, %q, %q, %q)", summary.SessionID, summary.RequestedModel, summary.ReportedModel, summary.AgentVersion)
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
		tk.addMessage(t.Context(), message)
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

		// Seeding a fresh task from the same log recovers the rate limit.
		seeded := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		seeded.SeedTimeline([]agent.Message{message})
		if got := seeded.Snapshot().RateLimit; got.Status != "rejected" || !got.ResetsAt.Equal(resetAt) {
			t.Errorf("seeded RateLimit = %+v, want rejected until %v", got, resetAt)
		}

		tk.addMessage(t.Context(), &agent.RateLimitMessage{
			Status:        agent.RateLimitStatusAllowedWarning,
			ResetsAt:      resetAt.Add(6 * 24 * time.Hour),
			QuotaProvider: agent.QuotaProviderAnthropic,
			QuotaWindow:   "7d",
		})
		if got := tk.Snapshot().RateLimit; got.Status != agent.RateLimitStatusRejected || got.QuotaWindow != "5h" {
			t.Errorf("RateLimit after a different-window update = %+v, want active 5h rejection", got)
		}
	})
	t.Run("SubscribeRateLimits", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		historical := &agent.RateLimitMessage{Status: agent.RateLimitStatusAllowed, QuotaProvider: agent.QuotaProviderClaudeCode, QuotaWindow: "5h"}
		tk.addMessage(t.Context(), historical)
		history, live, _ := tk.SubscribeRateLimits(t.Context())
		if len(history) != 1 || history[0] != historical {
			t.Fatalf("history = %#v, want the historical rate-limit message", history)
		}

		for range 257 {
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "ordinary task output"})
		}
		liveRateLimit := &agent.RateLimitMessage{Status: agent.RateLimitStatusAllowedWarning, QuotaProvider: agent.QuotaProviderClaudeCode, QuotaWindow: "7d"}
		tk.addMessage(t.Context(), liveRateLimit)
		select {
		case got := <-live:
			if got != liveRateLimit {
				t.Errorf("live rate-limit = %#v, want %#v", got, liveRateLimit)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for live rate-limit message")
		}
	})
	t.Run("ConcurrentMetadataSnapshots", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.Repos = []taskslog.RepoMount{
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
		tk.SeedTimeline([]agent.Message{&agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}})
		if got := tk.LastExitError(); got != "Unknown option: --approve" {
			t.Errorf("LastExitError = %q, want relay stderr", got)
		}
	})
	t.Run("SeedTimelineClearsStaleExitErrorOnLaterSessionActivity", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SeedTimeline([]agent.Message{
			&agent.ExitMessage{ExitCode: 2, Error: "stale crash"},
			&agent.InitMessage{SessionID: "new-session"},
			&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"},
		})
		if got := tk.LastExitError(); got != "" {
			t.Errorf("LastExitError = %q, want stale exit error cleared", got)
		}
	})
	t.Run("SeedTimelineIgnoresTrailingExitAfterSuccessfulTurn", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StateRunning)
		tk.SeedTimeline([]agent.Message{
			&agent.InitMessage{SessionID: "session"},
			&agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"},
			&agent.ExitMessage{ExitCode: 2, Error: "stale crash"},
		})
		if got := tk.LastExitError(); got != "" {
			t.Errorf("LastExitError = %q, want trailing exit ignored", got)
		}
		if got := tk.GetState(); got != taskslog.StateWaiting {
			t.Errorf("state = %v, want waiting", got)
		}
	})
	t.Run("AddMessageSkipsSpuriousExitAfterCleanTurn", func(t *testing.T) {
		t.Parallel()
		// A non-zero exit that follows a cleanly completed turn (e.g. SIGINT
		// from a user-requested stop, exit code -2) must not be fanned out to
		// live SSE subscribers: it is a spurious termination artifact already
		// dropped from the persisted replay, so the live stream must match.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StateRunning)
		_, ch, unsub := tk.Subscribe(t.Context())
		defer unsub()

		tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result", Subtype: "success", Result: "done"})
		tk.addMessage(t.Context(), &agent.ExitMessage{ExitCode: -2})

		got := recvType(ch)
		if got != "result" {
			t.Fatalf("live msg = %q, want %q", got, "result")
		}
		if extra := recvType(ch); extra != "" {
			t.Fatalf("spurious exit fanned out: got %q, want none", extra)
		}
	})
	t.Run("AddMessageFansOutExitMidTurn", func(t *testing.T) {
		t.Parallel()
		// A non-zero exit with no preceding clean result is a genuine
		// interruption and must still reach live subscribers.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StateRunning)
		_, ch, unsub := tk.Subscribe(t.Context())
		defer unsub()

		tk.addMessage(t.Context(), &agent.ExitMessage{ExitCode: -2})

		if got := recvType(ch); got != "caic_exit" {
			t.Fatalf("live msg = %q, want %q", got, "caic_exit")
		}
	})
	t.Run("RecordSessionFailure", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StateStarting)
		tk.addMessage(t.Context(), &agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"})

		if !tk.RecordSessionFailure(t.Context(), errors.New("agent exited: exit status 2")) {
			t.Fatal("RecordSessionFailure returned false")
		}
		if got := tk.GetState(); got != taskslog.StateFailed {
			t.Errorf("state = %v, want %v", got, taskslog.StateFailed)
		}
		if got := tk.LastAgentResult(); !strings.Contains(got, "Unknown option: --approve") {
			t.Errorf("LastAgentResult = %q, want relay stderr", got)
		}
	})
	t.Run("Subscribe", func(t *testing.T) {
		t.Parallel()
		t.Run("SlowSubscriberThenCancel", func(t *testing.T) {
			t.Parallel()
			// Regression test: if fan-out drops and closes a slow subscriber,
			// later cancellation and explicit cleanup must not double-close it.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			ctx, cancel := context.WithCancel(t.Context())
			_, ch, unsub := tk.Subscribe(ctx)
			defer unsub()

			// Fill the subscriber buffer (256) so the next send overflows.
			for range 256 {
				tk.addMessage(t.Context(), &agent.SystemMessage{MessageType: "system", Subtype: "status"})
			}
			// This send should trigger the slow-subscriber drop+close.
			tk.addMessage(t.Context(), &agent.SystemMessage{MessageType: "system", Subtype: "status"})

			// Drain to confirm channel was closed by fan-out.
			for range ch {
			}

			cancel()
			unsub()
		})
		t.Run("ExplicitUnsubscribeClosesChannel", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			_, ch, unsub := tk.Subscribe(t.Context())
			unsub()
			unsub()

			if _, ok := <-ch; ok {
				t.Fatal("subscriber channel remained open after unsubscribe")
			}
		})
		t.Run("Replay", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			// Add messages before subscribing.
			msg1 := &agent.SystemMessage{MessageType: "system", Subtype: "status"}
			msg2 := &agent.TextMessage{Text: "hello"}
			tk.addMessage(t.Context(), msg1)
			tk.addMessage(t.Context(), msg2)

			history, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()
			_ = ch

			if len(history) != 2 {
				t.Fatalf("history len = %d, want 2", len(history))
			}
			if history[0].Message.Type() != "system" {
				t.Errorf("history[0].Message.Type() = %q, want %q", history[0].Message.Type(), "system")
			}
			if history[1].Message.Type() != "text" {
				t.Errorf("history[1].Message.Type() = %q, want %q", history[1].Message.Type(), "text")
			}
		})
		t.Run("ReplayLargeHistory", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			// Add more messages than any reasonable channel buffer to verify no deadlock.
			const n = 1000
			for range n {
				tk.addMessage(t.Context(), &agent.TextMessage{Text: "msg"})
			}

			history, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()
			_ = ch

			if len(history) != n {
				t.Fatalf("history len = %d, want %d", len(history), n)
			}
		})
		t.Run("MultipleListeners", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.addMessage(t.Context(), &agent.SystemMessage{MessageType: "system", Subtype: "init"})

			// Start two subscribers.
			h1, ch1, unsub1 := tk.Subscribe(t.Context())
			defer unsub1()
			h2, ch2, unsub2 := tk.Subscribe(t.Context())
			defer unsub2()

			// Both get the same history.
			if len(h1) != 1 || len(h2) != 1 {
				t.Fatalf("history lens = %d, %d; want 1, 1", len(h1), len(h2))
			}

			// Send a live message — both channels should receive it.
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "live"})

			timeout := time.After(time.Second)
			for i, ch := range []<-chan TimelineMessage{ch1, ch2} {
				select {
				case msg := <-ch:
					if msg.Message.Type() != "text" {
						t.Errorf("subscriber %d: type = %q, want %q", i, msg.Message.Type(), "text")
					}
				case <-timeout:
					t.Fatalf("subscriber %d: timed out waiting for live message", i)
				}
			}
		})
		t.Run("Live", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")

			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			// Send a live message after subscribing.
			msg := &agent.TextMessage{Text: "live"}
			tk.addMessage(t.Context(), msg)

			timeout := time.After(time.Second)
			select {
			case got := <-ch:
				if got.Message.Type() != "text" {
					t.Errorf("type = %q, want %q", got.Message.Type(), "text")
				}
			case <-timeout:
				t.Fatal("timed out waiting for live message")
			}
		})
		t.Run("TimelinePreservesSuppressedMessageGaps", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			_, ch, unsub := tk.Subscribe(t.Context())
			defer unsub()

			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result", Subtype: "success"})
			if got := <-ch; got.Sequence != 1 {
				t.Fatalf("result sequence = %d, want 1", got.Sequence)
			}
			tk.addMessage(t.Context(), &agent.ExitMessage{ExitCode: 2, Error: "interrupted"})
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "next turn"})
			if got := <-ch; got.Sequence != 3 {
				t.Fatalf("message sequence = %d, want 3 after suppressed exit", got.Sequence)
			}
		})
		t.Run("LiveOnlyDoesNotReplayHistory", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "old"})
			_, ch, unsub := tk.SubscribeLiveMessages(t.Context())
			defer unsub()

			select {
			case got := <-ch:
				t.Fatalf("unexpected historical message %q", got.Message.Type())
			default:
			}
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "new"})
			select {
			case got := <-ch:
				text, ok := got.Message.(*agent.TextMessage)
				if !ok {
					t.Fatalf("message = %T, want *agent.TextMessage", got.Message)
				}
				if text.Text != "new" {
					t.Fatalf("message = %q, want new", text.Text)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for live message")
			}
		})
	})

	t.Run("SendInput", func(t *testing.T) {
		t.Parallel()
		t.Run("error_queued_delta_after_rollback_establishes_running", func(t *testing.T) {
			t.Parallel()
			for _, state := range []taskslog.State{taskslog.StateWaiting, taskslog.StateAsking} {
				t.Run(string(state), func(t *testing.T) {
					t.Parallel()
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
					tk.SetState(state)
					if state == taskslog.StateAsking {
						tk.addMessage(t.Context(), &agent.AskMessage{ToolUseID: "question"})
					}
					ctx, cancel := context.WithCancel(t.Context())
					cmd := exec.CommandContext(ctx, "sleep", "60")
					stdout, err := cmd.StdoutPipe()
					if err != nil {
						t.Fatal(err)
					}
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					msgCh := make(chan agent.TimedMessage, 8)
					log := agent.DiscardLogSink{Version: agent.LogVersionV3}
					c := agent.NewConn(ctx, testLogger(), &queuedDeltaInputWriter{msgCh: msgCh}, log, &testWire{})
					s := agent.NewSession(ctx, cmd, c, stdout, msgCh, testLogger())
					t.Cleanup(func() { cancel(); _ = s.Wait(); close(msgCh) })
					tk.AttachSession(&SessionHandle{Session: s})
					if err := tk.SendInput(ctx, agent.Prompt{Text: "follow up"}); !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("send = %v, want delivery error", err)
					}
					if got := tk.GetState(); got != state {
						t.Fatalf("state = %s before queued output dispatch, want %s", got, state)
					}
					// No dispatcher ran during the send. Fold its FIFO only after the
					// transport error and rollback, reproducing either scheduler order.
					for range 4 {
						tk.addParsedMessage(<-msgCh)
					}
					if got := tk.GetState(); got != taskslog.StateRunning {
						t.Fatalf("state = %s after queued root delta, want running", got)
					}
				})
			}
		})
		t.Run("error_partial_delivery_preserves_active_root_output", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateWaiting)
			_, live, unsubscribe := tk.Subscribe(t.Context())
			t.Cleanup(unsubscribe)
			ctx, cancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(ctx, "sleep", "60")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			r := &AgentRuntime{Log: testLogger(), NotifyTaskChange: func() {}}
			log := agent.DiscardLogSink{Version: agent.LogVersionV3}
			msgCh, _, work := r.startMessageDispatch(ctx, tk, log)
			observed := make(chan struct{})
			conn := agent.NewConn(ctx, testLogger(), &partialInputWriter{msgCh: msgCh, observed: observed}, log, &testWire{})
			s := agent.NewSession(ctx, cmd, conn, stdout, msgCh, testLogger())
			var readers sync.WaitGroup
			readers.Go(func() {
				<-live // User input.
				<-live // Root text is folded before the transport error returns.
				close(observed)
			})
			t.Cleanup(func() {
				cancel()
				_ = s.Wait()
				close(msgCh)
				work.Wait()
				readers.Wait()
			})
			tk.AttachSession(&SessionHandle{Session: s})
			if err := tk.SendInput(ctx, agent.Prompt{Text: "follow up"}); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("send = %v, want partial-delivery error", err)
			}
			if got := tk.GetState(); got != taskslog.StateRunning {
				t.Fatalf("state = %s, want active root output to remain running", got)
			}
			if !tk.HasAcceptedInputEvidence() {
				t.Fatal("real root output did not prove input acceptance")
			}
		})
		t.Run("completion_during_delivery_preserves_waiting_state", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateWaiting)
			_, live, unsubscribe := tk.Subscribe(t.Context())
			t.Cleanup(unsubscribe)
			ctx, cancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(ctx, "sleep", "60")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			r := &AgentRuntime{Log: testLogger(), NotifyTaskChange: func() {}}
			msgCh, _, work := r.startMessageDispatch(ctx, tk, agent.DiscardLogSink{Version: agent.LogVersionV3})
			conn := agent.NewConn(ctx, testLogger(), &completingInputWriter{msgCh: msgCh}, agent.DiscardLogSink{Version: agent.LogVersionV3}, &testWire{})
			s := agent.NewSession(ctx, cmd, conn, stdout, msgCh, testLogger())
			t.Cleanup(func() {
				cancel()
				_ = s.Wait()
				close(msgCh)
				work.Wait()
			})
			tk.AttachSession(&SessionHandle{Session: s})
			if err := tk.SendInput(ctx, agent.Prompt{Text: "follow up"}); err != nil {
				t.Fatal(err)
			}
			var order []string
			for range 3 {
				select {
				case message := <-live:
					order = append(order, message.Message.Type())
				case <-time.After(time.Second):
					t.Fatal("missing live message")
				}
			}
			if !slices.Equal(order, []string{"user_input", "text", "result"}) {
				t.Fatalf("live order = %v, want prompt before answer and result", order)
			}
			if got := tk.GetState(); got != taskslog.StateWaiting {
				t.Fatalf("state = %s, want %s after agent completion", got, taskslog.StateWaiting)
			}
			if got := tk.titleInput(); got != "Current title: test\nRecent activity:\nPrompt: follow up\nResult: done\n" {
				t.Fatalf("title input = %q", got)
			}
		})
		t.Run("error_delivery_failure_preserves_waiting_state", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateWaiting)

			cmdCtx, cmdCancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(cmdCtx, "sleep", "60")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			sendErr := errors.New("delivery failed")
			r := &AgentRuntime{Log: testLogger(), NotifyTaskChange: func() {}}
			msgCh, _, work := r.startMessageDispatch(t.Context(), tk, agent.DiscardLogSink{Version: agent.LogVersionV3})
			_, live, unsubscribe := tk.Subscribe(t.Context())
			t.Cleanup(unsubscribe)
			s := agent.NewSession(t.Context(), cmd, &failingConn{err: sendErr}, stdout, msgCh, testLogger())
			t.Cleanup(func() {
				cmdCancel()
				_ = s.Wait()
				close(msgCh)
				work.Wait()
			})
			tk.AttachSession(&SessionHandle{Session: s})

			err = tk.SendInput(t.Context(), agent.Prompt{Text: "follow up"})
			if !errors.Is(err, sendErr) {
				t.Fatalf("SendInput err = %v, want %v", err, sendErr)
			}
			if got := tk.GetState(); got != taskslog.StateWaiting {
				t.Errorf("state = %s, want %s", got, taskslog.StateWaiting)
			}
			for range 2 {
				select {
				case <-live:
				case <-time.After(time.Second):
					t.Fatal("missing attempted input or failure")
				}
			}
			var msgs []agent.Message
			for _, m := range tk.Messages() {
				if !agent.IsInputDeliveryControl(m) {
					msgs = append(msgs, m)
				}
			}
			if len(msgs) != 2 {
				t.Fatalf("messages = %d, want attempted input and visible failure", len(msgs))
			}
			failure, ok := msgs[1].(*agent.SystemMessage)
			if !ok || failure.Subtype != agent.SystemSubtypeInputDeliveryFailed || failure.Detail != "Input delivery failed: delivery failed" {
				t.Fatalf("failure = %#v", msgs[1])
			}
			if tk.HasAcceptedInputEvidence() {
				t.Fatal("failed attempt was accepted as evidence of delivery")
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			// Simulate: agent entered plan mode, wrote a plan, exited.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "EnterPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "ExitPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})

			snap := tk.Snapshot()
			if snap.PlanContent != "the plan" {
				t.Fatalf("PlanContent = %q before SendInput, want %q", snap.PlanContent, "the plan")
			}

			// Attach a live session so SendInput succeeds past the handle check.
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cmdCancel()
			cmd := exec.CommandContext(cmdCtx, "cat")
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
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, agent.DiscardLogSink{Version: agent.LogVersionV1}, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.TimedMessage, 256), testLogger())
			tk.AttachSession(&SessionHandle{Session: s})
			defer func() { _ = stdin.Close(); _ = s.Wait() }()

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
			tk.SetState(taskslog.StateRunning)
			// Agent writes the initial plan.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"step 1\nstep 2\n"}`),
			})
			if snap := tk.Snapshot(); snap.PlanContent != "step 1\nstep 2\n" {
				t.Fatalf("PlanContent = %q after Write, want %q", snap.PlanContent, "step 1\nstep 2\n")
			}
			// Agent edits the plan file.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
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
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"TODO\nTODO\n"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
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
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			// Edit a non-plan file — planContent must be unchanged.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"original plan"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})

			// Attach a live session.
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cmdCancel()
			cmd := exec.CommandContext(cmdCtx, "cat")
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
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, agent.DiscardLogSink{Version: agent.LogVersionV1}, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.TimedMessage, 256), testLogger())
			tk.AttachSession(&SessionHandle{Session: s})
			defer func() { _ = stdin.Close(); _ = s.Wait() }()

			// User rejects plan and sends feedback.
			_ = tk.SendInput(t.Context(), agent.Prompt{Text: "add error handling"})

			// Agent edits the plan file.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "Edit",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","old_string":"original plan","new_string":"updated plan with error handling"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu4", Name: "ExitPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})

			snap := tk.Snapshot()
			if snap.PlanContent != "updated plan with error handling" {
				t.Errorf("PlanContent = %q, want %q", snap.PlanContent, "updated plan with error handling")
			}
		})
		t.Run("NoSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateWaiting)
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateWaiting)
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cmdCancel()
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
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, agent.DiscardLogSink{Version: agent.LogVersionV1}, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.TimedMessage, 256), testLogger())
			<-s.Done()
			tk.AttachSession(&SessionHandle{Session: s})
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
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		if tk.SessionDone() != nil {
			t.Error("SessionDone() should be nil when no session attached")
		}
		if tk.DetachSession() != nil {
			t.Error("DetachSession() should return nil when no session attached")
		}

		cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cmdCancel()
		cmd := exec.CommandContext(cmdCtx, "cat")
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, agent.DiscardLogSink{Version: agent.LogVersionV1}, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.TimedMessage, 256), testLogger())
		h := &SessionHandle{Session: s}
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

		// Cleanup: close stdin so the process exits, then wait via Session
		// (which owns cmd.Wait) to avoid a double-Wait race.
		_ = stdin.Close()
		_ = s.Wait()
	})

	t.Run("addMessage", func(t *testing.T) {
		t.Parallel()
		t.Run("TransitionsToWaiting", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			result := &agent.ResultMessage{MessageType: "result"}
			tk.addMessage(t.Context(), result)
			if tk.GetState() != taskslog.StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateWaiting)
			}
		})
		t.Run("TransitionsToAsking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			// Add an AskMessage.
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			if tk.GetState() != taskslog.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateAsking)
			}
		})
		t.Run("AnsweredAskTransitionsToWaiting", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			tk.addMessage(t.Context(), &agent.ToolResultMessage{ToolUseID: "ask1"})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != taskslog.StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateWaiting)
			}
		})
		t.Run("ErroredAskResultStaysAsking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			tk.addMessage(t.Context(), &agent.ToolResultMessage{
				ToolUseID: "ask1",
				Error:     "permission denied",
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != taskslog.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateAsking)
			}
		})
		t.Run("TransitionsToAskingWithPartialMessages", func(t *testing.T) {
			t.Parallel()
			// With --include-partial-messages, Claude Code emits multiple
			// assistant snapshots per turn. AskUserQuestion appears in an
			// earlier snapshot while the final one is text-only. The state
			// machine must scan all messages in the turn.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "I need to ask you something."})
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			if tk.GetState() != taskslog.StateAsking {
				t.Fatalf("state = %v, want %v before result", tk.GetState(), taskslog.StateAsking)
			}
			tk.SetState(taskslog.StateRunning)
			// Final partial snapshot: text-only, no tool_use.
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "I need to ask you something."})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != taskslog.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateAsking)
			}
		})
		t.Run("TextMessageTransitionsWaitingToRunning", func(t *testing.T) {
			t.Parallel()
			// When the agent starts producing output while the task is
			// waiting (e.g. relay reconnect after server restart), the
			// state should transition back to running.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateWaiting)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "output"})
			if tk.GetState() != taskslog.StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateRunning)
			}
		})
		t.Run("ToolUseMessageTransitionsAskingToRunning", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateAsking)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{ToolUseID: "tu1", Name: "Read"})
			if tk.GetState() != taskslog.StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateRunning)
			}
		})
		t.Run("ResultTransitionsWaitingToAsking", func(t *testing.T) {
			t.Parallel()
			// When watchSession sets Waiting before the ResultMessage is
			// processed, the ResultMessage should still detect
			// AskMessage and correct the state to Asking.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			// Simulate watchSession setting Waiting before ResultMessage
			// is processed by the dispatch goroutine.
			tk.SetState(taskslog.StateWaiting)
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != taskslog.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateAsking)
			}
		})
		t.Run("TransitionsToHasPlan", func(t *testing.T) {
			t.Parallel()
			// ExitPlanMode + plan content + ResultMessage → StateHasPlan.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != taskslog.StateHasPlan {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateHasPlan)
			}
		})
		t.Run("AskingTakesPriorityOverHasPlan", func(t *testing.T) {
			t.Parallel()
			// Both AskMessage and ExitPlanMode in same turn → StateAsking.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			})
			tk.addMessage(t.Context(), &agent.AskMessage{
				ToolUseID: "ask1",
				Questions: []v3.AskQuestion{{Question: "which?"}},
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != taskslog.StateAsking {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateAsking)
			}
		})
		t.Run("NoHasPlanWithoutPlanContent", func(t *testing.T) {
			t.Parallel()
			// ExitPlanMode without plan content → StateWaiting.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "ExitPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})
			if tk.GetState() != taskslog.StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateWaiting)
			}
		})
		t.Run("ExitPlanModeProjectsPlanContent", func(t *testing.T) {
			t.Parallel()
			// The snapshot pairs the current plan with its root ExitPlanMode owner.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan A"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"})
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
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v1"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"})
			if snap := tk.Snapshot(); snap.PlanExitID != "tu2" || snap.PlanContent != "plan v1" {
				t.Errorf("current plan = (%q, %q), want (tu2, plan v1)", snap.PlanExitID, snap.PlanContent)
			}
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})

			// Agent updates the plan.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan v2"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{ToolUseID: "tu4", Name: "ExitPlanMode"})
			if got := tk.Snapshot().PlanExitID; got == "tu2" {
				t.Errorf("superseded plan owner = %q", got)
			}
			if snap := tk.Snapshot(); snap.PlanExitID != "tu4" || snap.PlanContent != "plan v2" {
				t.Errorf("current plan = (%q, %q), want (tu4, plan v2)", snap.PlanExitID, snap.PlanContent)
			}
		})
		t.Run("HasPlanToRunningOnText", func(t *testing.T) {
			t.Parallel()
			// TextMessage while HasPlan → Running.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateHasPlan)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "output"})
			if tk.GetState() != taskslog.StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateRunning)
			}
		})
		t.Run("TextMessageTransitionsStartingToRunning", func(t *testing.T) {
			t.Parallel()
			// When the agent subprocess produces output before
			// Checkout.Start calls SetState(Running), StateStarting
			// must transition to Running so the subsequent
			// ResultMessage can transition further.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateStarting)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "output"})
			if tk.GetState() != taskslog.StateRunning {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateRunning)
			}
		})
		t.Run("EmptyResultUsesTurnText", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "I checked the files."})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{ToolUseID: "tu1", Name: "Read"})
			tk.addMessage(t.Context(), &agent.ToolResultMessage{ToolUseID: "tu1"})
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "There is nothing to change."})

			result := &agent.ResultMessage{MessageType: "result"}
			tk.addMessage(t.Context(), result)
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
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "before thinking"})
			tk.addMessage(t.Context(), &agent.ThinkingMessage{Text: "private chain"})
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "after thinking"})

			result := &agent.ResultMessage{MessageType: "result"}
			tk.addMessage(t.Context(), result)
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
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "intermediate"})

			result := &agent.ResultMessage{MessageType: "result", Result: "final"}
			tk.addMessage(t.Context(), result)
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
			for _, state := range []taskslog.State{taskslog.StatePending, taskslog.StateBranching, taskslog.StateProvisioning, taskslog.StatePurging, taskslog.StateCrashed, taskslog.StateFailed, taskslog.StatePurged} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(state)
				tk.addMessage(t.Context(), &agent.TextMessage{Text: "output"})
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
			tk.SetState(taskslog.StateRunning)
			ds := v3.DiffStat{
				{Path: "main.go", LinesAdded: 10, LinesDeleted: 3},
				{Path: "img.png", Binary: true},
			}
			tk.addMessage(t.Context(), &agent.DiffStatMessage{
				MessageType: "caic_diff_stat",
				DiffStat:    ds,
			})
			got := tk.LiveDiffStat()
			if len(got) != 2 {
				t.Fatalf("LiveDiffStat len = %d, want 2", len(got))
			}
			if got[0].Path != "main.go" || got[0].LinesAdded != 10 {
				t.Errorf("LiveDiffStat[0] = %+v", got[0])
			}
			// Update with new diff stat.
			tk.addMessage(t.Context(), &agent.DiffStatMessage{
				MessageType: "caic_diff_stat",
				DiffStat:    v3.DiffStat{{Path: "new.go", LinesAdded: 1, LinesDeleted: 0}},
			})
			got = tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "new.go" {
				t.Errorf("LiveDiffStat after update = %+v", got)
			}
		})

		t.Run("ResultMessageUpdatesLiveDiffStat", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ResultMessage{
				MessageType: "result",
				DiffStat:    v3.DiffStat{{Path: "a.go", LinesAdded: 5, LinesDeleted: 2}},
			})
			got := tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "a.go" || got[0].LinesAdded != 5 {
				t.Errorf("LiveDiffStat = %+v, want [{a.go 5 2}]", got)
			}
		})
	})

	t.Run("SeedTimelineDiffStat", func(t *testing.T) {
		t.Parallel()
		t.Run("DiffStatMessage", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StatePurged)
			tk.SeedTimeline([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{{Path: "old.go", LinesAdded: 1}},
				},
				&agent.TextMessage{Text: "hello"},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{{Path: "latest.go", LinesAdded: 5}},
				},
			})
			got := tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "latest.go" {
				t.Errorf("LiveDiffStat = %+v, want latest.go", got)
			}
		})

		t.Run("ResultMessageAfterDiffStat", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StatePurged)
			tk.SeedTimeline([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{{Path: "stale.go", LinesAdded: 1}},
				},
				&agent.ResultMessage{
					MessageType: "result",
					DiffStat:    v3.DiffStat{{Path: "authoritative.go", LinesAdded: 10}},
				},
			})
			got := tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "authoritative.go" {
				t.Errorf("LiveDiffStat = %+v, want authoritative.go", got)
			}
		})

		t.Run("DiffStatAfterResult", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StatePurged)
			tk.SeedTimeline([]agent.Message{
				&agent.ResultMessage{
					MessageType: "result",
					DiffStat:    v3.DiffStat{{Path: "result.go", LinesAdded: 5}},
				},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{{Path: "relay.go", LinesAdded: 3}},
				},
			})
			got := tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "relay.go" {
				t.Errorf("LiveDiffStat = %+v, want relay.go", got)
			}
		})

		// Regression: the relay's diff_watcher computes git diff HEAD
		// (uncommitted changes). After the agent commits, this is empty.
		// The ResultMessage.DiffStat (host-side branch diff) is set
		// in-memory by startMessageDispatch and NOT persisted to the
		// relay output, so SeedTimeline only sees the empty relay
		// diff. Runtime import must compute the host-side diff stat
		// separately after SeedTimeline.
		t.Run("EmptyRelayDiffAfterCommit", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			// Simulate relay output: ResultMessage without DiffStat
			// (host-side mutation not persisted) followed by an empty
			// DiffStatMessage (relay sees no uncommitted changes).
			tk.SeedTimeline([]agent.Message{
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{{Path: "main.go", LinesAdded: 10, LinesDeleted: 2}},
				},
				&agent.ResultMessage{MessageType: "result"},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{},
				},
			})
			got := tk.LiveDiffStat()
			if len(got) != 0 {
				t.Fatalf("LiveDiffStat = %+v, want empty (relay reported no uncommitted changes)", got)
			}
			// Adoption restores the branch diff from a fresh host snapshot.
			tk.SetRuntimeConnectionInfo("test-runtime:adopted", runtime.ConnectionTarget{}, "", "", 0)
			tk.SetLiveRepositorySummary(&repo.GitSnapshot{
				Read:     repo.NewGitRead(tk.RuntimeInstanceID()),
				DiffStat: v3.DiffStat{{Path: "main.go", LinesAdded: 10, LinesDeleted: 2}},
			})
			got = tk.LiveDiffStat()
			if len(got) != 1 || got[0].Path != "main.go" {
				t.Errorf("LiveDiffStat after set = %+v, want main.go", got)
			}
		})
	})

	t.Run("LiveUsageCumulative", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10, ReasoningOutputTokens: 7},
		})
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 200, OutputTokens: 80, CacheCreationInputTokens: 30, ReasoningOutputTokens: 11},
		})
		_, _, _, usage, lastUsage := tk.LiveStats()
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

	t.Run("SeedTimelineUsageCumulative", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StatePurged)
		tk.SeedTimeline([]agent.Message{
			&agent.ResultMessage{
				MessageType: "result",
				Usage:       agent.Usage{InputTokens: 100, OutputTokens: 50, ReasoningOutputTokens: 7},
			},
			&agent.TextMessage{Text: "hello"},
			&agent.ResultMessage{
				MessageType: "result",
				Usage:       agent.Usage{InputTokens: 200, OutputTokens: 80, ReasoningOutputTokens: 11},
			},
		})
		_, _, _, usage, lastUsage := tk.LiveStats()
		if usage.InputTokens != 300 {
			t.Errorf("InputTokens = %d, want 300", usage.InputTokens)
		}
		if usage.OutputTokens != 130 {
			t.Errorf("OutputTokens = %d, want 130", usage.OutputTokens)
		}
		if usage.ReasoningOutputTokens != 18 {
			t.Errorf("ReasoningOutputTokens = %d, want 18", usage.ReasoningOutputTokens)
		}
		// lastUsage should reflect only the last ResultMessage.
		if lastUsage.InputTokens != 200 {
			t.Errorf("lastUsage.InputTokens = %d, want 200", lastUsage.InputTokens)
		}
		if lastUsage.OutputTokens != 80 {
			t.Errorf("lastUsage.OutputTokens = %d, want 80", lastUsage.OutputTokens)
		}
		if lastUsage.ReasoningOutputTokens != 11 {
			t.Errorf("lastUsage.ReasoningOutputTokens = %d, want 11", lastUsage.ReasoningOutputTokens)
		}
	})

	t.Run("LiveCostCumulativeAcrossSessions", func(t *testing.T) {
		t.Parallel()
		// Cost, turns, and duration must accumulate across sessions separated
		// by ClearMessages. Reported totals are already fully priced.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StateRunning)
		// Session 1: TotalCostUSD = $10.00.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 10.0,
			NumTurns:     3,
			DurationMs:   5000,
		})
		tk.ClearMessages(t.Context())
		tk.SetState(taskslog.StateRunning)
		// Session 2: TotalCostUSD = $5.00.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 5.0,
			NumTurns:     2,
			DurationMs:   3000,
		})
		costUSD, numTurns, duration, _, _ := tk.LiveStats()
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
		// Multiple result events within a single session (no ClearMessages/compact_boundary)
		// must accumulate duration rather than overwriting with only the last invocation's value.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			NumTurns:    1,
			DurationMs:  946943,
		})
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			NumTurns:    1,
			DurationMs:  5278,
		})
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			NumTurns:    1,
			DurationMs:  214500,
		})
		_, numTurns, duration, _, _ := tk.LiveStats()
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
		// Regression: ClearMessages used += (double-count) instead of = assignment.
		// Verify cost is correct after two ClearMessages calls.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StateRunning)
		// Session 1: $75.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 75.0,
			NumTurns:     1,
		})
		tk.ClearMessages(t.Context())
		tk.SetState(taskslog.StateRunning)
		// Session 2: $75.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 75.0,
			NumTurns:     1,
		})
		tk.ClearMessages(t.Context())
		tk.SetState(taskslog.StateRunning)
		// Session 3: $75.
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 75.0,
			NumTurns:     1,
		})
		costUSD, numTurns, _, _, _ := tk.LiveStats()
		if costUSD != 225.0 {
			t.Errorf("costUSD = %v, want 225.0 (three sessions × $75)", costUSD)
		}
		if numTurns != 3 {
			t.Errorf("numTurns = %d, want 3", numTurns)
		}
	})

	t.Run("SeedTimelineDurationAccumulatesWithinSession", func(t *testing.T) {
		t.Parallel()
		// SeedTimeline (reloadFromMsgs) must accumulate DurationMs across
		// multiple result events within a single session.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StatePurged)
		tk.SeedTimeline([]agent.Message{
			&agent.ResultMessage{MessageType: "result", NumTurns: 1, DurationMs: 946943},
			&agent.ResultMessage{MessageType: "result", NumTurns: 1, DurationMs: 5278},
			&agent.ResultMessage{MessageType: "result", NumTurns: 1, DurationMs: 214500},
		})
		_, numTurns, duration, _, _ := tk.LiveStats()
		if numTurns != 3 {
			t.Errorf("numTurns = %d, want 3", numTurns)
		}
		wantDuration := time.Duration(946943+5278+214500) * time.Millisecond
		if duration != wantDuration {
			t.Errorf("duration = %v, want %v", duration, wantDuration)
		}
	})

	t.Run("SeedTimelineCostCumulativeAcrossSessions", func(t *testing.T) {
		t.Parallel()
		// SeedTimeline must sum cost/turns/duration across context_cleared
		// boundaries, mirroring the live path.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StatePurged)
		tk.SeedTimeline([]agent.Message{
			// Session 1: TotalCostUSD = $10.00.
			&agent.ResultMessage{
				MessageType:  "result",
				TotalCostUSD: 10.0,
				NumTurns:     3,
				DurationMs:   5000,
			},
			&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
			// Session 2: TotalCostUSD = $5.00.
			&agent.ResultMessage{
				MessageType:  "result",
				TotalCostUSD: 5.0,
				NumTurns:     2,
				DurationMs:   3000,
			},
		})
		costUSD, numTurns, duration, _, _ := tk.LiveStats()
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

	t.Run("LiveCostUsesClaudeReportedTotal", func(t *testing.T) {
		t.Parallel()
		// Claude's reported total already includes cache reads.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 1.50,
			Usage: agent.Usage{
				InputTokens:          100_000,
				CacheReadInputTokens: 10_000_000,
			},
		})
		costUSD, _, _, _, _ := tk.LiveStats()
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
			tk.SetState(taskslog.StateRunning)
			return tk
		}
		// newMsgs returns fresh messages per subtest: addMessage and
		// SeedTimeline both mutate ResultMessage.Result, so the parallel
		// subtests must not share the same pointers.
		newMsgs := func() []agent.Message {
			return []agent.Message{
				&agent.ResultMessage{MessageType: "result", TotalCostUSD: 10.0, NumTurns: 3, DurationMs: 5000},
				&agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary"},
				&agent.ResultMessage{MessageType: "result", TotalCostUSD: 15.0, NumTurns: 2, DurationMs: 3000},
			}
		}

		t.Run("Live", func(t *testing.T) {
			t.Parallel()
			tk := newTask()
			for _, m := range newMsgs() {
				tk.addMessage(t.Context(), m)
			}
			costUSD, numTurns, duration, _, _ := tk.LiveStats()
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

		t.Run("Restore", func(t *testing.T) {
			t.Parallel()
			tk := newTask()
			tk.SetState(taskslog.StatePurged)
			tk.SeedTimeline(newMsgs())
			costUSD, numTurns, duration, _, _ := tk.LiveStats()
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

		// The boundary carries the harness's post-compaction context size, which
		// must replace the pre-compaction fill until the next usage message.
		t.Run("ContextTokensAfterResetsMeter", func(t *testing.T) {
			t.Parallel()
			newMsgList := func() []agent.Message {
				return []agent.Message{
					&agent.UsageMessage{Usage: agent.Usage{InputTokens: 180_000, CacheReadInputTokens: 5_000}},
					&agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary", ContextTokensAfter: 12_000},
				}
			}
			t.Run("Live", func(t *testing.T) {
				t.Parallel()
				tk := newTask()
				for _, m := range newMsgList() {
					tk.addMessage(t.Context(), m)
				}
				usage := tk.Snapshot().LastAPIUsage
				if usage.InputTokens != 12_000 || usage.CacheReadInputTokens != 0 {
					t.Errorf("lastAPIUsage = %+v, want only 12000 input tokens", usage)
				}
			})
			t.Run("Restore", func(t *testing.T) {
				t.Parallel()
				tk := newTask()
				tk.SetState(taskslog.StatePurged)
				tk.SeedTimeline(newMsgList())
				usage := tk.Snapshot().LastAPIUsage
				if usage.InputTokens != 12_000 || usage.CacheReadInputTokens != 0 {
					t.Errorf("lastAPIUsage = %+v, want only 12000 input tokens", usage)
				}
			})
		})
	})

	t.Run("ClearMessages", func(t *testing.T) {
		t.Parallel()
		t.Run("ResetsPlanState", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			// Simulate an agent entering plan mode and writing a plan file.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "EnterPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			snap := tk.Snapshot()
			if !snap.InPlanMode {
				t.Fatal("InPlanMode = false before ClearMessages, want true")
			}
			if snap.PlanContent != "the plan" {
				t.Fatalf("PlanContent = %q before ClearMessages, want %q", snap.PlanContent, "the plan")
			}

			tk.ClearMessages(t.Context())

			snap = tk.Snapshot()
			if snap.InPlanMode {
				t.Error("InPlanMode = true after ClearMessages, want false")
			}
			if snap.PlanContent != "" {
				t.Errorf("PlanContent = %q after ClearMessages, want empty", snap.PlanContent)
			}
			if tk.GetPlanFile() != "" {
				t.Errorf("PlanFile = %q after ClearMessages, want empty", tk.GetPlanFile())
			}
		})
		t.Run("SuppressesPlanRewrite", func(t *testing.T) {
			t.Parallel()
			// After ClearMessages (restart), the agent may re-enter plan mode
			// and write to .claude/plans/. The plan must not resurface.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			// Original plan.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "ExitPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})

			// User clicks "Clear and execute plan".
			tk.ClearMessages(t.Context())
			tk.SetState(taskslog.StateRunning)

			// Agent re-enters plan mode during execution.
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu3", Name: "EnterPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu4", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"rewritten plan"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu5", Name: "ExitPlanMode",
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})

			snap := tk.Snapshot()
			if snap.PlanContent != "" {
				t.Errorf("PlanContent = %q, want empty (plan written after ClearMessages should be suppressed)", snap.PlanContent)
			}
			if tk.GetPlanFile() != "" {
				t.Errorf("PlanFile = %q, want empty", tk.GetPlanFile())
			}
		})
		t.Run("ClearsExitPlanModePlanContent", func(t *testing.T) {
			t.Parallel()
			// After ClearMessages the plan projection must be dropped so new
			// subscribers don't see stale plans; the message itself stays
			// immutable.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
			})
			tk.addMessage(t.Context(), &agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})
			if snap := tk.Snapshot(); snap.PlanExitID != "tu2" || snap.PlanContent != "the plan" {
				t.Errorf("current plan = (%q, %q), want (tu2, the plan)", snap.PlanExitID, snap.PlanContent)
			}

			tk.ClearMessages(t.Context())

			if got := tk.Snapshot().PlanExitID; got == "tu2" {
				t.Errorf("superseded plan owner = %q", got)
			}
		})
		t.Run("SuppressionLiftsAfterTurn", func(t *testing.T) {
			t.Parallel()
			// After the restart turn completes, a subsequent user-initiated turn
			// must be able to produce a plan again.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu1", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"plan"}`),
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})

			// Restart.
			tk.ClearMessages(t.Context())
			tk.SetState(taskslog.StateRunning)
			// Turn completes without plan.
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "done"})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})

			// Suppression lifted — next turn can set plan.
			tk.SetState(taskslog.StateRunning)
			tk.addMessage(t.Context(), &agent.ToolUseMessage{
				ToolUseID: "tu2", Name: "Write",
				Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"fresh plan"}`),
			})
			tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result"})

			snap := tk.Snapshot()
			if snap.PlanContent != "fresh plan" {
				t.Errorf("PlanContent = %q, want %q (suppression should have lifted)", snap.PlanContent, "fresh plan")
			}
		})
	})

	t.Run("SeedTimeline", func(t *testing.T) {
		t.Parallel()
		t.Run("PanicsOnNonEmptyTimeline", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.addMessage(t.Context(), &agent.TextMessage{Text: "live"})
			defer func() {
				if recover() == nil {
					t.Error("SeedTimeline on a task with messages did not panic")
				}
			}()
			tk.SeedTimeline([]agent.Message{&agent.TextMessage{Text: "from log"}})
		})
		t.Run("Basic", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "sess-123"},
				&agent.TextMessage{Text: "hello"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimeline(msgs)

			if len(tk.Messages()) != 3 {
				t.Fatalf("Messages() len = %d, want 3", len(tk.Messages()))
			}
			if tk.GetSessionID() != "sess-123" {
				t.Errorf("SessionID = %q, want %q", tk.GetSessionID(), "sess-123")
			}
			if tk.GetState() != taskslog.StateWaiting {
				t.Errorf("state = %v, want %v (should infer waiting from trailing ResultMessage)", tk.GetState(), taskslog.StateWaiting)
			}
		})
		t.Run("InfersAsking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "s1"},
				&agent.AskMessage{
					ToolUseID: "ask1",
					Questions: []v3.AskQuestion{{Question: "which?"}},
				},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimeline(msgs)
			if tk.GetState() != taskslog.StateAsking {
				t.Errorf("state = %v, want %v (should infer asking from AskMessage + ResultMessage)", tk.GetState(), taskslog.StateAsking)
			}
		})
		t.Run("InfersAskingWithoutResult", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "s1"},
				&agent.AskMessage{
					ToolUseID: "ask1",
					Questions: []v3.AskQuestion{{Question: "which?"}},
				},
			}
			tk.SeedTimeline(msgs)
			if tk.GetState() != taskslog.StateAsking {
				t.Errorf("state = %v, want %v (should infer asking from trailing AskMessage)", tk.GetState(), taskslog.StateAsking)
			}
		})
		t.Run("AnsweredAskInfersWaiting", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.AskMessage{
					ToolUseID: "ask1",
					Questions: []v3.AskQuestion{{Question: "which?"}},
				},
				&agent.ToolResultMessage{ToolUseID: "ask1"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimeline(msgs)
			if tk.GetState() != taskslog.StateWaiting {
				t.Errorf("state = %v, want %v (answered AskUserQuestion should not remain asking)", tk.GetState(), taskslog.StateWaiting)
			}
		})
		t.Run("ErroredAskInfersAsking", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.AskMessage{
					ToolUseID: "ask1",
					Questions: []v3.AskQuestion{{Question: "which?"}},
				},
				&agent.ToolResultMessage{ToolUseID: "ask1", Error: "permission denied"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimeline(msgs)
			if tk.GetState() != taskslog.StateAsking {
				t.Errorf("state = %v, want %v (errored AskUserQuestion result should remain asking)", tk.GetState(), taskslog.StateAsking)
			}
		})
		t.Run("InfersHasPlan", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"the plan"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu2", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimeline(msgs)
			if tk.GetState() != taskslog.StateHasPlan {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateHasPlan)
			}
		})
		t.Run("SkipsTrailingDiffStat", func(t *testing.T) {
			t.Parallel()
			// The relay emits DiffStatMessage after the ResultMessage.
			// SeedTimeline should skip it and still infer Waiting.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.TextMessage{Text: "hello"},
				&agent.ResultMessage{MessageType: "result"},
				&agent.DiffStatMessage{
					MessageType: "caic_diff_stat",
					DiffStat:    v3.DiffStat{{Path: "main.go", LinesAdded: 1}},
				},
			}
			tk.SeedTimeline(msgs)
			if tk.GetState() != taskslog.StateWaiting {
				t.Errorf("state = %v, want %v (trailing DiffStatMessage should be skipped)", tk.GetState(), taskslog.StateWaiting)
			}
		})
		t.Run("NoResultKeepsState", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.InitMessage{SessionID: "s1"},
				&agent.TextMessage{Text: "hello"},
			}
			tk.SeedTimeline(msgs)
			// No trailing ResultMessage → agent was still producing output.
			if tk.GetState() != taskslog.StateRunning {
				t.Errorf("state = %v, want %v (no ResultMessage → still running)", tk.GetState(), taskslog.StateRunning)
			}
		})
		t.Run("TerminalStatePreserved", func(t *testing.T) {
			t.Parallel()
			for _, state := range []taskslog.State{taskslog.StatePurged, taskslog.StateCrashed, taskslog.StateFailed, taskslog.StatePurging} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(state)
				msgs := []agent.Message{
					&agent.TextMessage{Text: "hello"},
					&agent.ResultMessage{MessageType: "result"},
				}
				tk.SeedTimeline(msgs)
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
			tk.SeedTimeline(msgs)

			if tk.GetSessionID() != "new" {
				t.Errorf("SessionID = %q, want %q", tk.GetSessionID(), "new")
			}
		})
		t.Run("RestoresPlanFile", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{
					ToolUseID: "tu1", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/my-plan.md","content":"plan"}`),
				},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimeline(msgs)
			if tk.GetPlanFile() != "/home/user/.claude/plans/my-plan.md" {
				t.Errorf("PlanFile = %q, want %q", tk.GetPlanFile(), "/home/user/.claude/plans/my-plan.md")
			}
		})
		t.Run("RestoresInPlanMode", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{ToolUseID: "tu1", Name: "EnterPlanMode"},
				&agent.ToolUseMessage{
					ToolUseID: "tu2", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/foo.md","content":"x"}`),
				},
				&agent.ToolUseMessage{ToolUseID: "tu3", Name: "ExitPlanMode"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimeline(msgs)
			if tk.Snapshot().InPlanMode {
				t.Error("InPlanMode = true, want false (ExitPlanMode should clear it)")
			}
			if tk.GetPlanFile() != "/home/user/.claude/plans/foo.md" {
				t.Errorf("PlanFile = %q, want %q", tk.GetPlanFile(), "/home/user/.claude/plans/foo.md")
			}

			// Without ExitPlanMode, should stay in plan mode.
			tk2 := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk2.SetState(taskslog.StateRunning)
			tk2.SeedTimeline(msgs[:1])
			if !tk2.Snapshot().InPlanMode {
				t.Error("InPlanMode = false, want true (only EnterPlanMode seen)")
			}
		})
		t.Run("ContextClearedResetsPlanState", func(t *testing.T) {
			t.Parallel()
			// Simulates relay output containing a plan, then a context_cleared
			// marker (from ClearMessages on restart), then a new session without
			// a plan. SeedTimeline must not carry over the stale plan.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			msgs := []agent.Message{
				&agent.ToolUseMessage{ToolUseID: "tu1", Name: "EnterPlanMode"},
				&agent.ToolUseMessage{
					ToolUseID: "tu2", Name: "Write",
					Input: json.RawMessage(`{"file_path":"/home/user/.claude/plans/p.md","content":"old plan"}`),
				},
				&agent.ResultMessage{MessageType: "result"},
				// context_cleared injected by ClearMessages on restart.
				&agent.SystemMessage{MessageType: "system", Subtype: "context_cleared"},
				// New session starts — no plan tools used.
				&agent.TextMessage{Text: "done"},
				&agent.ResultMessage{MessageType: "result"},
			}
			tk.SeedTimeline(msgs)
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
			tk.SetState(taskslog.StateRunning)
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
			tk.SeedTimeline(msgs)
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
			tk.SetState(taskslog.StateRunning)
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
			tk.SeedTimeline(msgs)
			if got := tk.Snapshot().PlanExitID; got == "tu2" {
				t.Errorf("superseded plan owner = %q", got)
			}
		})
		t.Run("PlanUpdateClearsPreviousExitPlanModePlanContent", func(t *testing.T) {
			t.Parallel()
			// When a plan is updated (two ExitPlanMode without context_cleared),
			// the projection must follow only the latest ExitPlanMode.
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
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
			tk.SeedTimeline(msgs)
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
			a.SeedTimeline(msgs)
			afterFirst, err := json.Marshal(msgs)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			b.SeedTimeline(msgs)
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
				tk.SetState(taskslog.StateRunning)
				tk.SeedTimeline([]agent.Message{
					result(),
					native("child-1", agent.NativeSubagentStatusRunning),
				})
				if got := tk.GetState(); got != taskslog.StateRunning {
					t.Errorf("state = %v, want %v (a background subagent is still active)", got, taskslog.StateRunning)
				}
			})
			t.Run("SeedTimelineInfersWaitingForForegroundSubagent", func(t *testing.T) {
				t.Parallel()
				// A foreground card still running at the parent result is stale or
				// unread evidence, not detached work.
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(taskslog.StateRunning)
				tk.SeedTimeline([]agent.Message{
					result(),
					foreground("child-1", agent.NativeSubagentStatusRunning),
				})
				if got := tk.GetState(); got != taskslog.StateWaiting {
					t.Errorf("state = %v, want %v (a foreground card cannot outlive the turn)", got, taskslog.StateWaiting)
				}
			})
			t.Run("SeedTimelineInfersWaitingForSettledSubagent", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(taskslog.StateRunning)
				tk.SeedTimeline([]agent.Message{
					result(),
					native("child-1", agent.NativeSubagentStatusRunning),
					native("child-1", agent.NativeSubagentStatusCompleted),
				})
				if got := tk.GetState(); got != taskslog.StateWaiting {
					t.Errorf("state = %v, want %v (the trailing native update settled the child)", got, taskslog.StateWaiting)
				}
			})
			t.Run("SeedTimelineFallsBackToWaitingForPausedSubagent", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(taskslog.StateRunning)
				tk.SeedTimeline([]agent.Message{
					result(),
					native("child-1", agent.NativeSubagentStatusPaused),
				})
				if got := tk.GetState(); got != taskslog.StateWaiting {
					t.Errorf("state = %v, want %v (paused evidence is not active)", got, taskslog.StateWaiting)
				}
			})
			t.Run("ResultKeepsRunningWhileBackgroundSubagentActive", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(taskslog.StateRunning)
				tk.addMessage(t.Context(), native("child-1", agent.NativeSubagentStatusRunning))
				tk.addMessage(t.Context(), result())
				if got := tk.GetState(); got != taskslog.StateRunning {
					t.Errorf("state = %v, want %v after the parent turn ended with a background subagent", got, taskslog.StateRunning)
				}
				tk.addMessage(t.Context(), native("child-1", agent.NativeSubagentStatusCompleted))
				if got := tk.GetState(); got != taskslog.StateWaiting {
					t.Errorf("state = %v, want %v after the last subagent settled", got, taskslog.StateWaiting)
				}
			})
			t.Run("ForegroundRunningCardFallsBackToWaiting", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(taskslog.StateRunning)
				tk.addMessage(t.Context(), foreground("child-1", agent.NativeSubagentStatusRunning))
				tk.addMessage(t.Context(), result())
				if got := tk.GetState(); got != taskslog.StateWaiting {
					t.Errorf("state = %v, want %v (unclear foreground card falls back)", got, taskslog.StateWaiting)
				}
			})
			t.Run("LateBackgroundSubagentKeepsTaskRunning", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(taskslog.StateRunning)
				tk.addMessage(t.Context(), result())
				tk.addMessage(t.Context(), native("child-1", agent.NativeSubagentStatusRunning))
				if got := tk.GetState(); got != taskslog.StateRunning {
					t.Errorf("state = %v, want %v when the child is reported running after the parent settled", got, taskslog.StateRunning)
				}
			})
			t.Run("UnansweredAskWinsOverRunningSubagent", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(taskslog.StateRunning)
				tk.addMessage(t.Context(), &agent.AskMessage{ToolUseID: "ask1", Questions: []v3.AskQuestion{{Question: "which?"}}})
				tk.addMessage(t.Context(), native("child-1", agent.NativeSubagentStatusRunning))
				tk.addMessage(t.Context(), result())
				if got := tk.GetState(); got != taskslog.StateAsking {
					t.Errorf("state = %v, want %v (the user still owes an answer)", got, taskslog.StateAsking)
				}
			})
			t.Run("NativeUpdateKeepsTrailingExitSpurious", func(t *testing.T) {
				t.Parallel()
				// Live and replay must agree that a child lifecycle update between the
				// clean result and the exit does not turn the exit into a real error.
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				live.SetState(taskslog.StateRunning)
				live.addMessage(t.Context(), result())
				live.addMessage(t.Context(), native("child-1", agent.NativeSubagentStatusRunning))
				live.addMessage(t.Context(), native("child-1", agent.NativeSubagentStatusCompleted))
				live.addMessage(t.Context(), &agent.ExitMessage{ExitCode: -2})
				if got := live.LastExitError(); got != "" {
					t.Errorf("live LastExitError = %q, want the exit dropped after a clean turn", got)
				}
				if got := live.GetState(); got != taskslog.StateWaiting {
					t.Errorf("live state = %v, want %v", got, taskslog.StateWaiting)
				}

				replayed := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				replayed.SetState(taskslog.StateRunning)
				replayed.SeedTimeline([]agent.Message{
					result(),
					native("child-1", agent.NativeSubagentStatusRunning),
					native("child-1", agent.NativeSubagentStatusCompleted),
					&agent.ExitMessage{ExitCode: -2},
				})
				if got := replayed.LastExitError(); got != "" {
					t.Errorf("replayed LastExitError = %q, want the exit dropped after a clean turn", got)
				}
				if got := replayed.GetState(); got != taskslog.StateWaiting {
					t.Errorf("replayed state = %v, want %v", got, taskslog.StateWaiting)
				}
			})
		})
		t.Run("BackgroundCommandState", func(t *testing.T) {
			t.Parallel()
			// A detached shell command is informational: it is folded into its card
			// set, but unlike a background subagent it never justifies keeping the
			// task running, live or replayed.
			command := func(id string, status agent.BackgroundCommandStatus) *agent.BackgroundCommandMessage {
				return &agent.BackgroundCommandMessage{Command: agent.BackgroundCommand{ID: id, Status: status}}
			}
			result := func() *agent.ResultMessage { return &agent.ResultMessage{MessageType: "result"} }

			t.Run("SeedTimelineKeepsWaitingForRunningCommand", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(taskslog.StateRunning)
				tk.SeedTimeline([]agent.Message{
					result(),
					command("shell-1", agent.BackgroundCommandStatusRunning),
				})
				if got := tk.GetState(); got != taskslog.StateWaiting {
					t.Errorf("state = %v, want %v (a shell command never drives state)", got, taskslog.StateWaiting)
				}
			})
			t.Run("LateRunningCommandAfterResultKeepsWaiting", func(t *testing.T) {
				t.Parallel()
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				tk.SetState(taskslog.StateRunning)
				tk.addMessage(t.Context(), result())
				tk.addMessage(t.Context(), command("shell-1", agent.BackgroundCommandStatusRunning))
				if got := tk.GetState(); got != taskslog.StateWaiting {
					t.Errorf("state = %v, want %v after the parent settled", got, taskslog.StateWaiting)
				}
			})
			t.Run("CommandUpdateBetweenResultAndExitKeepsExitSpurious", func(t *testing.T) {
				t.Parallel()
				// Live and replay must agree that a command update between the clean
				// result and the exit does not turn the exit into a real error.
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				live.SetState(taskslog.StateRunning)
				live.addMessage(t.Context(), result())
				live.addMessage(t.Context(), command("shell-1", agent.BackgroundCommandStatusCompleted))
				live.addMessage(t.Context(), &agent.ExitMessage{ExitCode: -2})
				if got := live.LastExitError(); got != "" {
					t.Errorf("live LastExitError = %q, want the exit dropped after a clean turn", got)
				}

				replayed := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
				replayed.SetState(taskslog.StateRunning)
				replayed.SeedTimeline([]agent.Message{
					result(),
					command("shell-1", agent.BackgroundCommandStatusCompleted),
					&agent.ExitMessage{ExitCode: -2},
				})
				if got := replayed.LastExitError(); got != "" {
					t.Errorf("replayed LastExitError = %q, want the exit dropped after a clean turn", got)
				}
			})
		})
		t.Run("Subscribe", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			msgs := []agent.Message{
				&agent.TextMessage{Text: "msg1"},
				&agent.TextMessage{Text: "msg2"},
			}
			tk.SeedTimeline(msgs)

			// A subscriber should see restored messages in the history snapshot.
			history, _, unsub := tk.Subscribe(t.Context())
			t.Cleanup(unsub)

			if len(history) != 2 {
				t.Fatalf("history len = %d, want 2", len(history))
			}
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
			tk.SeedTimeline(msgs)

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
			tk.SeedTimeline(msgs)

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
			tk.Repos = []taskslog.RepoMount{{Name: "a/b", Branch: "caic-0", GitRoot: "/foo"}}
			if extra := tk.ExtraRuntimeRepos(); extra != nil {
				t.Fatalf("ExtraRuntimeRepos with one repo = %+v, want nil", extra)
			}
		})
		t.Run("MultipleRepos", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.Repos = []taskslog.RepoMount{
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

	t.Run("SetStateAt", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		tk.SetStateAt(taskslog.StateRunning, now)
		if tk.GetState() != taskslog.StateRunning {
			t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateRunning)
		}
		snap := tk.Snapshot()
		if !snap.StateUpdatedAt.Equal(now) {
			t.Errorf("StateUpdatedAt = %v, want %v", snap.StateUpdatedAt, now)
		}
		if !snap.TurnStartedAt.IsZero() {
			t.Error("TurnStartedAt should be zero when SetStateAt is used")
		}
	})

	t.Run("SnapshotWithStateHistory", func(t *testing.T) {
		t.Parallel()
		t.Run("replaysTransitionsAfterCursor", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			_, seq0, hist0 := tk.SnapshotWithStateHistory(0)
			if len(hist0) != 1 || hist0[0].State != taskslog.StatePending {
				t.Fatalf("initial history = %+v, want one pending transition", hist0)
			}
			tk.SetState(taskslog.StateRunning)
			tk.SetState(taskslog.StateWaiting)
			snap, seq, hist := tk.SnapshotWithStateHistory(seq0)
			if snap.State != taskslog.StateWaiting {
				t.Errorf("state = %v, want waiting", snap.State)
			}
			want := []taskslog.State{taskslog.StateRunning, taskslog.StateWaiting}
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
			tk.SetState(taskslog.StatePending)
			if _, gotSeq, hist := tk.SnapshotWithStateHistory(seq); gotSeq != seq || len(hist) != 0 {
				t.Errorf("seq = %d history = %+v, want seq %d and no new transition", gotSeq, hist, seq)
			}
		})
		t.Run("boundsJournal", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			for i := range stateTransitionHistory + 5 {
				if i%2 == 0 {
					tk.SetState(taskslog.StateRunning)
				} else {
					tk.SetState(taskslog.StateWaiting)
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

	t.Run("SetTurnStartedAt", func(t *testing.T) {
		t.Parallel()
		t.Run("Running", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
			tk.SetTurnStartedAt(now)
			if snap := tk.Snapshot(); !snap.TurnStartedAt.Equal(now) {
				t.Errorf("TurnStartedAt = %v, want %v", snap.TurnStartedAt, now)
			}
		})
		t.Run("NonRunning", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateWaiting)
			now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
			tk.SetTurnStartedAt(now)
			if snap := tk.Snapshot(); !snap.TurnStartedAt.IsZero() {
				t.Errorf("TurnStartedAt = %v, want zero when non-running", snap.TurnStartedAt)
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
			tk.addMessage(t.Context(), &agent.InitMessage{SessionID: "s1", ReportedModel: "claude-3-opus"})
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
					tk.addMessage(t.Context(), &agent.UsageMessage{ReportedModel: "claude-opus-4-6"})
				}},
				{"replay", func(tk *Task) {
					tk.SeedTimeline([]agent.Message{&agent.UsageMessage{ReportedModel: "claude-opus-4-6"}})
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
		tk.addMessage(t.Context(), &agent.InitMessage{SessionID: "s1", ReportedEffort: "medium"})
		snap := tk.Snapshot()
		if snap.RequestedEffort != "high" || snap.ReportedEffort != "medium" {
			t.Errorf("settings = (%q, %q), want (high, medium)", snap.RequestedEffort, snap.ReportedEffort)
		}
	})

	t.Run("UnreportedEffortIsNotAddedToInit", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-5", "high")
		init := &agent.InitMessage{SessionID: "s1"}
		tk.addMessage(t.Context(), init)
		if init.ReportedEffort != "" {
			t.Errorf("InitMessage.Effort = %q, want empty", init.ReportedEffort)
		}
	})

	t.Run("ReportedEffort", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-5", "high")
		tk.addMessage(t.Context(), &agent.InitMessage{SessionID: "s1", ReportedEffort: "medium"})
		snap := tk.Snapshot()
		if snap.RequestedEffort != "high" || snap.ReportedEffort != "medium" {
			t.Errorf("settings = (%q, %q), want (high, medium)", snap.RequestedEffort, snap.ReportedEffort)
		}
	})

	t.Run("UnreportedEffortIsNotAddedToInit", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-5", "high")
		init := &agent.InitMessage{SessionID: "s1"}
		tk.addMessage(t.Context(), init)
		if init.ReportedEffort != "" {
			t.Errorf("InitMessage.Effort = %q, want empty", init.ReportedEffort)
		}
	})

	t.Run("AgentVersion", func(t *testing.T) {
		t.Parallel()
		t.Run("AddMessageRecordsVersionWithoutSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.addMessage(t.Context(), &agent.InitMessage{Version: "1.2.3"})
			if snap := tk.Snapshot(); snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("SeedTimelineRecordsVersionWithoutSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimeline([]agent.Message{&agent.InitMessage{Version: "1.2.3"}})
			if snap := tk.Snapshot(); snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("EmptyInitDoesNotClearVersion", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.addMessage(t.Context(), &agent.InitMessage{Version: "1.2.3"})
			tk.addMessage(t.Context(), &agent.InitMessage{SessionID: "s1"})
			if snap := tk.Snapshot(); snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("MetaSessionRecordsVersionWithoutConversationMessage", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.addMessage(t.Context(), &agent.MetaSessionMessage{MessageType: "caic_session", AgentVersion: "1.2.3"})
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

	t.Run("SetAgentVersion", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
		tk.SetAgentVersion("2.0.0")
		snap := tk.Snapshot()
		if snap.AgentVersion != "2.0.0" {
			t.Errorf("AgentVersion = %q, want %q", snap.AgentVersion, "2.0.0")
		}
	})
	t.Run("SetSessionMetadata", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "requested", "")
		tk.SetSessionMetadata("session-1", "reported", "", "2.0.0")
		if got := tk.GetSessionID(); got != "session-1" {
			t.Errorf("SessionID = %q, want session-1", got)
		}
		snap := tk.Snapshot()
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
					tk.SeedTimeline(history)
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
					messages, err := wire.ParseMessage([]byte(line))
					if err != nil {
						t.Fatal(err)
					}
					history = append(history, messages...)
				}
				tk.SeedTimeline(history)
				if got := tk.PendingUserActions(); len(got) != 0 {
					t.Errorf("initialized=%t: ambiguous control restored root action: %#v", initialized, got)
				}
			}
		})
		t.Run("RestoresCurrentAsk", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimeline([]agent.Message{
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
			})

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
		t.Run("DedupesDuplicatedRestoredAsk", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimeline([]agent.Message{
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
			})

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
			tk.SeedTimeline([]agent.Message{
				&agent.AskMessage{
					ToolUseID: "toolu-1",
					Questions: []v3.AskQuestion{{Question: "First?"}},
				},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: v3.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-1",
						ToolUseID: "toolu-1",
						Ask: v3.PendingAskAction{
							Questions: []v3.AskQuestion{{Question: "First?"}},
						},
					},
				},
				&agent.AskMessage{
					ToolUseID: "toolu-2",
					Questions: []v3.AskQuestion{{Question: "Second?"}},
				},
				&agent.PendingUserActionMessage{
					MessageType: agent.PendingUserActionMessageType,
					Action: v3.PendingUserAction{
						Kind:      agent.PendingUserActionAskUserQuestion,
						RequestID: "req-2",
						ToolUseID: "toolu-2",
						Ask: v3.PendingAskAction{
							Questions: []v3.AskQuestion{{Question: "Second?"}},
						},
					},
				},
				&agent.ResultMessage{MessageType: "result"},
			})

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
			tk.SeedTimeline([]agent.Message{
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
			})

			if got := tk.PendingUserActions(); len(got) != 0 {
				t.Fatalf("PendingUserActions = %#v, want none", got)
			}
		})
		t.Run("PreviousTurnAskIsIgnored", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimeline([]agent.Message{
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
			})

			if got := tk.PendingUserActions(); len(got) != 0 {
				t.Fatalf("PendingUserActions = %#v, want none", got)
			}
		})
	})

	t.Run("WriteToLog", func(t *testing.T) {
		t.Parallel()
		t.Run("NoSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			err := tk.WriteToLog(&agent.TextMessage{Text: "hello"})
			if !errors.Is(err, taskslog.ErrNoLog) {
				t.Fatalf("WriteToLog err = %v, want ErrNoLog", err)
			}
		})
		t.Run("WithSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			buf := &agenttest.LogSink{Version: agent.LogVersionV2}
			h := &SessionHandle{Log: buf}
			tk.AttachSession(h)
			if err := tk.WriteToLog(&agent.LogMessage{MessageType: "caic_log", Line: "hello"}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), `"t":"log"`) {
				t.Errorf("log buffer = %q, want log message", buf.String())
			}
		})
		t.Run("ReopensPersistedLogWithSession", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "claude", "", "")
			path := filepath.Join(dir, tk.LogFilename())
			header, err := json.Marshal(agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     int(agent.LogVersionV3),
				Prompt:      "test",
				Harness:     "claude",
			})
			if err != nil {
				t.Fatal(err)
			}
			before := append(append([]byte(nil), header...), '\n')
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}

			msg := &agent.MetaPRMessage{
				MessageType: "caic_pr",
				ForgeOwner:  "acme",
				ForgeRepo:   "widget",
				ForgePR:     42,
			}
			log, _, err := (taskslog.NewStore(testLogger(), dir)).Reopen(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			tk.AttachSession(&SessionHandle{Log: log})
			if err := tk.WriteToLog(msg); err != nil {
				t.Fatal(err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
			if err != nil {
				t.Fatal(err)
			}
			encoded := []byte(`{"forge_owner":"acme","forge_pr":42,"forge_repo":"widget","t":"pr"}`)
			want := append(append([]byte(nil), before...), append(encoded, '\n')...)
			if !bytes.Equal(data, want) {
				t.Fatalf("log = %q, want %q", data, want)
			}
		})
		t.Run("RejectsInvalidPersistedAuthorityWithoutMutation", func(t *testing.T) {
			t.Parallel()
			validHeader := func(version agent.LogVersion, h harness.Name) []byte {
				meta := agent.MetaMessage{
					MessageType: "caic_meta",
					Version:     int(version),
					Prompt:      "test",
					Harness:     string(h),
				}
				value := any(meta)
				if version == agent.LogVersionV2 {
					value = map[string]any{
						"t":       "caic_meta",
						"version": meta.Version,
						"prompt":  meta.Prompt,
						"harness": meta.Harness,
					}
				}
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return append(data, '\n')
			}
			cases := []struct {
				name string
				data []byte
			}{
				{name: "v2", data: validHeader(agent.LogVersionV2, "claude")},
				{name: "corrupt", data: []byte("not json\n")},
				{name: "unknown", data: validHeader(agent.LogVersion(4), "claude")},
				{name: "harness mismatch", data: validHeader(agent.LogVersionV1, "codex")},
				{name: "missing"},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					path := filepath.Join(t.TempDir(), "task.jsonl")
					if tc.data != nil {
						if err := os.WriteFile(path, tc.data, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "claude", "", "")
					if err := tk.WriteToLog(&agent.TextMessage{Text: "hello"}); err == nil {
						t.Fatal("WriteToLog error = nil")
					}
					if tc.data == nil {
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Fatalf("stat error = %v, want os.ErrNotExist", err)
						}
						return
					}
					after, err := os.ReadFile(path) //nolint:gosec // path is test-controlled.
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(after, tc.data) {
						t.Fatalf("log mutated: got %q, want %q", after, tc.data)
					}
				})
			}
		})
		t.Run("ReturnsAppendError", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			if err := tk.WriteToLog(&agent.TextMessage{Text: "hello"}); err == nil {
				t.Fatal("WriteToLog err = nil, want append error")
			}
		})
	})

	t.Run("PushStats", func(t *testing.T) {
		t.Parallel()
		t.Run("SubscribeStats", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.PushStats(&runtime.Stats{CPUPerc: 50.0, MemUsed: 1024})
			tk.PushStats(&runtime.Stats{CPUPerc: 75.0, MemUsed: 2048})

			ctx := t.Context()
			history, live, unsub := tk.SubscribeStats(ctx)
			t.Cleanup(unsub)

			if len(history) != 2 {
				t.Fatalf("history len = %d, want 2", len(history))
			}
			if history[0].CPUPerc != 50.0 {
				t.Errorf("history[0].CPUPerc = %v, want 50.0", history[0].CPUPerc)
			}
			if history[1].CPUPerc != 75.0 {
				t.Errorf("history[1].CPUPerc = %v, want 75.0", history[1].CPUPerc)
			}

			tk.PushStats(&runtime.Stats{CPUPerc: 100.0, MemUsed: 4096})
			timeout := time.After(time.Second)
			select {
			case s := <-live:
				if s.CPUPerc != 100.0 {
					t.Errorf("live.CPUPerc = %v, want 100.0", s.CPUPerc)
				}
			case <-timeout:
				t.Fatal("timed out waiting for live stat")
			}
			unsub()
			if _, ok := <-live; ok {
				t.Fatal("unsubscribe left the live stats channel open")
			}
		})

		t.Run("RingOverflow", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			for i := range 65 {
				tk.PushStats(&runtime.Stats{CPUPerc: float64(i)})
			}
			ctx := t.Context()
			history, _, unsub := tk.SubscribeStats(ctx)
			t.Cleanup(unsub)

			if len(history) != statsRingSize {
				t.Errorf("history len = %d, want %d (ring capped at size)", len(history), statsRingSize)
			}
			if history[0].CPUPerc != 5.0 {
				t.Errorf("history[0].CPUPerc = %v, want 5.0 (oldest should be off by delta)", history[0].CPUPerc)
			}
			if history[len(history)-1].CPUPerc != 64.0 {
				t.Errorf("history[last].CPUPerc = %v, want 64.0", history[len(history)-1].CPUPerc)
			}
		})
	})

	t.Run("UpdateDiskUsage", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "", "")
		tk.PushStats(&runtime.Stats{CPUPerc: 50, DiskUsed: -1})

		tk.UpdateDiskUsage(700)
		tk.PushStats(&runtime.Stats{CPUPerc: 75, DiskUsed: -1})

		history, _, unsub := tk.SubscribeStats(t.Context())
		unsub()
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

		if diskUsed, ok := tk.DiskUsage(); !ok || diskUsed != 700 {
			t.Fatalf("DiskUsage() = (%d, %t), want (700, true)", diskUsed, ok)
		}
		history, _, unsub := tk.SubscribeStats(t.Context())
		unsub()
		if len(history) != 0 {
			t.Fatalf("stats history = %+v, want none", history)
		}
	})

	t.Run("SendCompact", func(t *testing.T) {
		t.Parallel()
		t.Run("NoSession", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateWaiting)
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
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateWaiting)
			cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
			t.Cleanup(cmdCancel)
			cmd := exec.CommandContext(cmdCtx, "true")
			stdin, _ := cmd.StdinPipe()
			stdout, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, agent.DiscardLogSink{Version: agent.LogVersionV1}, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.TimedMessage, 256), testLogger())
			<-s.Done()
			tk.AttachSession(&SessionHandle{Session: s})
			err := tk.SendCompact(t.Context(), "compact now")
			if err == nil {
				t.Fatal("expected error for dead session")
			}
			if !strings.Contains(err.Error(), "session="+string(SessionExited)) {
				t.Errorf("error = %q, want session=%s", err.Error(), SessionExited)
			}
		})
	})

	t.Run("LastAgentMessage", func(t *testing.T) {
		t.Parallel()
		t.Run("Empty", func(t *testing.T) {
			t.Parallel()
			if lastAgentMessage(timelineEntries{}) != nil {
				t.Error("lastAgentMessage(nil) should be nil")
			}
		})
		t.Run("SkipsNonSemantic", func(t *testing.T) {
			t.Parallel()
			msgs := []agent.Message{
				&agent.ResultMessage{MessageType: "result", Result: "done"},
				&agent.DiffStatMessage{MessageType: "caic_diff_stat"},
				&agent.TextDeltaMessage{Text: "partial"},
				&agent.RawMessage{},
				&agent.UsageMessage{},
			}
			entries := make([]agent.TimedMessage, len(msgs))
			for i, message := range msgs {
				entries[i].Message = message
			}
			got := lastAgentMessage(timelineEntries{suffix: entries})
			if got == nil || got.Result != "done" {
				t.Errorf("lastAgentMessage should find ResultMessage skipping non-semantic, got %+v", got)
			}
		})
		t.Run("NonResultLast", func(t *testing.T) {
			t.Parallel()
			msgs := []agent.Message{
				&agent.ResultMessage{MessageType: "result"},
				&agent.TextMessage{Text: "mid output"},
			}
			entries := make([]agent.TimedMessage, len(msgs))
			for i, message := range msgs {
				entries[i].Message = message
			}
			if lastAgentMessage(timelineEntries{suffix: entries}) != nil {
				t.Error("lastAgentMessage should be nil when last semantic is not result")
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
			tk.SetState(taskslog.StateRunning)
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
			tk.SetState(taskslog.StateRunning)
			tk.addParsedMessage(agent.TimedMessage{Message: &agent.DiffStatMessage{MessageType: "caic_diff_stat", Repos: probe}})
			tk.addParsedMessage(agent.TimedMessage{Message: &agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: watcherDiff}})
			if got := tk.Snapshot().RepoStates; !reflect.DeepEqual(got, probe) {
				t.Fatalf("RepoStates = %+v, want the probe state %+v", got, probe)
			}
		})
		t.Run("SeedTimelineRestoresLatestProbe", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SeedTimeline([]agent.Message{
				&agent.DiffStatMessage{MessageType: "caic_diff_stat", Repos: probe},
				&agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: watcherDiff},
			})
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
			tk.addMessage(b.Context(), delta)
		}
		tk.addMessage(b.Context(), &agent.TextMessage{Text: "complete"})
		if tk.timeline[0].Message != compactedTextDelta {
			b.Fatal("live delta payload was not compacted")
		}
	}
}

func TestSessionHandle(t *testing.T) {
	t.Parallel()
	t.Run("Drain", func(t *testing.T) {
		t.Parallel()
		cmdCtx, cmdCancel := context.WithTimeout(t.Context(), 5*time.Second)
		t.Cleanup(cmdCancel)
		cmd := exec.CommandContext(cmdCtx, "cat")
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		s := agent.NewSession(t.Context(), cmd, agent.NewConn(t.Context(), testLogger(), stdin, agent.DiscardLogSink{Version: agent.LogVersionV1}, &testWire{parse: claudecode.New().NewWire().ParseMessage}), stdout, make(chan agent.TimedMessage, 256), testLogger())
		ch := make(chan agent.TimedMessage)
		done := make(chan struct{})
		var workers sync.WaitGroup
		workers.Go(func() {
			for range ch {
			}
			close(done)
		})
		h := &SessionHandle{Session: s, MsgCh: ch, DispatchDone: done}
		t.Cleanup(func() { h.CloseMsgCh(); workers.Wait() })
		_ = stdin.Close()
		_ = h.Drain()
		select {
		case <-done:
		default:
			t.Error("DispatchDone should be closed after Drain")
		}
	})

	t.Run("CloseMsgCh", func(t *testing.T) {
		t.Parallel()
		for range 2 {
			h := &SessionHandle{MsgCh: make(chan agent.TimedMessage)}
			h.CloseMsgCh()
		}
	})

	t.Run("StatsSubClose", func(t *testing.T) {
		t.Parallel()
		s := &statsSub{ch: make(chan runtime.Stats)}
		s.close()
		s.close() // double close must not panic.
	})
}

func TestState(t *testing.T) {
	t.Parallel()
	t.Run("SetStateIf", func(t *testing.T) {
		t.Parallel()
		t.Run("Match", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			if !tk.SetStateIf(taskslog.StateRunning, taskslog.StateWaiting) {
				t.Fatal("SetStateIf returned false when state matched")
			}
			if tk.GetState() != taskslog.StateWaiting {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateWaiting)
			}
		})
		t.Run("Mismatch", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateAsking)
			if tk.SetStateIf(taskslog.StateRunning, taskslog.StateWaiting) {
				t.Fatal("SetStateIf returned true when state did not match")
			}
			if tk.GetState() != taskslog.StateAsking {
				t.Errorf("state = %v, want %v (should be unchanged)", tk.GetState(), taskslog.StateAsking)
			}
		})
	})
	t.Run("SetStateUnless", func(t *testing.T) {
		t.Parallel()
		t.Run("Transitions", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateRunning)
			prev, changed := tk.SetStateUnless(taskslog.StateStopped, taskslog.StatePurged, taskslog.StateStopping)
			if !changed {
				t.Fatal("changed = false, want true when state not excluded")
			}
			if prev != taskslog.StateRunning {
				t.Errorf("prev = %v, want %v", prev, taskslog.StateRunning)
			}
			if tk.GetState() != taskslog.StateStopped {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateStopped)
			}
		})
		t.Run("Excluded", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StatePurging)
			prev, changed := tk.SetStateUnless(taskslog.StateStopped, taskslog.StatePurging, taskslog.StateStopping)
			if changed {
				t.Fatal("changed = true, want false when state excluded")
			}
			if prev != taskslog.StatePurging {
				t.Errorf("prev = %v, want %v", prev, taskslog.StatePurging)
			}
			if tk.GetState() != taskslog.StatePurging {
				t.Errorf("state = %v, want %v (should be unchanged)", tk.GetState(), taskslog.StatePurging)
			}
		})
	})
	t.Run("SetStateIfAny", func(t *testing.T) {
		t.Parallel()
		t.Run("Transitions", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StateAsking)
			prev, changed := tk.SetStateIfAny(taskslog.StateStarting, taskslog.StateWaiting, taskslog.StateAsking, taskslog.StateHasPlan)
			if !changed {
				t.Fatal("changed = false, want true when state is allowed")
			}
			if prev != taskslog.StateAsking {
				t.Errorf("prev = %v, want %v", prev, taskslog.StateAsking)
			}
			if tk.GetState() != taskslog.StateStarting {
				t.Errorf("state = %v, want %v", tk.GetState(), taskslog.StateStarting)
			}
		})
		t.Run("Rejected", func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
			tk.SetState(taskslog.StatePurging)
			prev, changed := tk.SetStateIfAny(taskslog.StateStopping, taskslog.StateWaiting, taskslog.StateRunning)
			if changed {
				t.Fatal("changed = true, want false when state is not allowed")
			}
			if prev != taskslog.StatePurging {
				t.Errorf("prev = %v, want %v", prev, taskslog.StatePurging)
			}
			if tk.GetState() != taskslog.StatePurging {
				t.Errorf("state = %v, want %v (should be unchanged)", tk.GetState(), taskslog.StatePurging)
			}
		})
	})
}

// fakePricer prices a fixed set of models; unlisted models are unpriced.
type fakePricer map[string]quotausage.ModelPrice

func (p fakePricer) ModelPrice(_ agent.QuotaProvider, modelID string, _ time.Time) (quotausage.ModelPrice, bool) {
	price, ok := p[modelID]
	return price, ok
}

func TestPricedCost(t *testing.T) {
	t.Parallel()
	// glm-5.3-flash: $0.15/M input, $0.03/M cache read, $0.50/M output.
	prices := fakePricer{"zai/glm-5.3-flash": {InputPerMTok: 0.15, CachedInputPerMTok: 0.03, OutputPerMTok: 0.50}}

	t.Run("UsageContributionIsHarnessNeutral", func(t *testing.T) {
		t.Parallel()
		for _, h := range []harness.Name{harness.Claude, harness.Codex, harness.OpenCode, harness.Pi} {
			for _, replay := range []bool{false, true} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, h, "", "")
				messages := []agent.Message{
					&agent.UsageMessage{Additive: true, Usage: agent.Usage{InputTokens: 100}},
					&agent.UsageMessage{Usage: agent.Usage{InputTokens: 900}},
					&agent.ResultMessage{UsageIsSnapshot: true, Usage: agent.Usage{InputTokens: 50}},
					&agent.ResultMessage{Usage: agent.Usage{InputTokens: 7}},
				}
				if replay {
					tk.SeedTimeline(messages)
				} else {
					for _, m := range messages {
						tk.addMessage(t.Context(), m)
					}
				}
				_, _, _, total, last := tk.LiveStats()
				if total.InputTokens != 107 || last.InputTokens != 7 {
					t.Fatalf("%s replay %v: total=%+v, context=%+v", h, replay, total, last)
				}
			}
		}
	})

	t.Run("PiCacheTotalsLiveReplayAndPersistence", func(t *testing.T) {
		t.Parallel()
		messages := []agent.Message{
			&agent.UsageMessage{Additive: true, PerCall: true, Usage: agent.Usage{CacheCreationInputTokens: 100, CacheWrites: tokens.CacheWrites{{TTL: time.Hour, Tokens: 100}}, CacheTTLSeconds: 3600}},
			&agent.UsageMessage{Additive: true, PerCall: true, Usage: agent.Usage{CacheCreationInputTokens: 10, CacheTTLSeconds: 300}},
			&agent.ResultMessage{MessageType: "result", UsageIsSnapshot: true, Usage: agent.Usage{CacheCreationInputTokens: 10, CacheTTLSeconds: 300}},
			// An invocation without per-call events still contributes its result.
			&agent.ResultMessage{MessageType: "result", Usage: agent.Usage{CacheCreationInputTokens: 7}},
		}
		for _, replay := range []bool{false, true} {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Pi, "anthropic/claude-opus-4-6", "")
			if replay {
				tk.SeedTimeline(messages)
			} else {
				for _, msg := range messages {
					tk.addMessage(t.Context(), msg)
				}
			}
			_, _, _, total, last := tk.LiveStats()
			want := agent.Usage{CacheCreationInputTokens: 117, CacheWrites: tokens.CacheWrites{{Tokens: 7}, {TTL: 5 * time.Minute, Tokens: 10}, {TTL: time.Hour, Tokens: 100}}}
			if !total.Equal(want) || last.CacheCreationInputTokens != 7 {
				t.Fatalf("replay %v: total=%+v, last=%+v", replay, total, last)
			}
			store := taskslog.NewStore(logtest.Logger(t), t.TempDir())
			log, path, err := store.Open(tk.LogFilename(), tk.LogHeader())
			if err != nil {
				t.Fatal(err)
			}
			res := &taskslog.Result{State: taskslog.StatePurged, Usage: total}
			if err := store.WriteResultTrailer(log, "done", res); err != nil {
				t.Fatal(err)
			}
			loaded, err := store.LoadAllReadOnly()
			if err != nil || len(loaded) != 1 || !loaded[0].LastTrailer.Usage.Equal(want) {
				t.Fatalf("trailer restore = %+v, %v", loaded, err)
			}
			if _, err := store.CompressTerminal(path, log, tk.terminalLogSummary(agent.LogVersionV3, res)); err != nil {
				t.Fatal(err)
			}
			loaded, err = store.LoadAllReadOnly()
			if err != nil || len(loaded) != 1 || !loaded[0].LastTrailer.Usage.Equal(want) {
				t.Fatalf("cached restore = %+v, %v", loaded, err)
			}
		}
	})

	t.Run("CodexRequestCostLiveReplayRollup", func(t *testing.T) {
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
			for _, replay := range []bool{false, true} {
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, "gpt-6.1-sol", "")
				tk.Pricer = quotausage.NewPricer(nil)
				sink := &rollupSpy{}
				tk.Rollup = sink
				if replay {
					tk.SeedTimeline(messages)
				} else {
					for _, msg := range messages {
						tk.addMessage(t.Context(), msg)
					}
				}
				cost, _, _, tokenCounts, _ := tk.LiveStats()
				if math.Abs(cost-wantCost) > 1e-9 || tokenCounts.InputTokens != resultUsage.InputTokens || tokenCounts.OutputTokens != resultUsage.OutputTokens {
					t.Fatalf("exited %v/replay %v: cost=%v, tokens=%+v", exited, replay, cost, tokenCounts)
				}
				last := sink.Observes[len(sink.Observes)-1].Event
				if !last.TurnBoundary || math.Abs(last.CostUSD-wantCost) > 1e-9 || last.Delta.Input != int64(resultUsage.InputTokens) {
					t.Fatalf("exited %v/replay %v: rollup=%+v", exited, replay, last)
				}
			}
		}
	})

	t.Run("CodexResultKeepsRawCostSeparate", func(t *testing.T) {
		t.Parallel()
		const model = "gpt-6-sol"
		for _, replay := range []bool{false, true} {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Codex, model, "")
			tk.Pricer = fakePricer{model: {InputPerMTok: 2, CachedInputPerMTok: 0.2, OutputPerMTok: 10}}
			tk.SetState(taskslog.StateRunning)
			first := &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 100_000, CacheReadInputTokens: 1_000_000, OutputTokens: 10_000}}
			second := &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 50_000, OutputTokens: 10_000}}
			messages := []agent.Message{first, &agent.SystemMessage{MessageType: "system", Subtype: "compact_boundary"}, second}
			if replay {
				tk.SeedTimeline(messages)
			} else {
				for _, m := range messages {
					tk.addMessage(t.Context(), m)
				}
			}
			if first.TotalCostUSD != 0 || second.TotalCostUSD != 0 {
				t.Errorf("replay=%v raw result costs = %v, %v; want 0, 0", replay, first.TotalCostUSD, second.TotalCostUSD)
			}
			if got, _, _, _, _ := tk.LiveStats(); got != 0.7 {
				t.Errorf("replay=%v task cost = %v, want 0.7", replay, got)
			}
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
		for _, replay := range []bool{false, true} {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.OpenCode, "zai/glm-5.3-flash", "")
			tk.Pricer = prices
			tk.SetState(taskslog.StateRunning)
			if replay {
				tk.SeedTimeline(newMessages())
			} else {
				for _, m := range newMessages() {
					tk.addMessage(t.Context(), m)
				}
			}
			if got, _, _, _, _ := tk.LiveStats(); got != 0.95 {
				t.Errorf("replay=%v cost = %v, want 0.95", replay, got)
			}
		}
	})
	t.Run("OpenCodeZeroReportedCostUsesPricer", func(t *testing.T) {
		t.Parallel()
		const model = "zai-coding-plan/glm-4.7"
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.OpenCode, model, "")
		tk.Pricer = fakePricer{model: {InputPerMTok: 0.15}}
		tk.SetState(taskslog.StateRunning)
		zero := 0.0
		tk.addMessage(t.Context(), &agent.UsageMessage{CumulativeCostUSD: &zero})
		tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result", Usage: agent.Usage{InputTokens: 1_000_000}})
		if got, _, _, _, _ := tk.LiveStats(); got != 0.15 {
			t.Errorf("cost = %v, want 0.15 API-equivalent pricing", got)
		}
	})

	t.Run("PiCompactionUsageCountsAcrossBoundary", func(t *testing.T) {
		t.Parallel()
		newTask := func(t *testing.T) *Task {
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Pi, "zai/glm-5.3-flash", "")
			tk.Pricer = prices
			tk.SetState(taskslog.StateRunning)
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
					tk.SeedTimeline(messages())
				} else {
					for _, m := range messages() {
						tk.addMessage(t.Context(), m)
					}
				}
				if cost, _, _, total, _ := tk.LiveStats(); math.Abs(cost-0.45) > 1e-9 || total.InputTokens != 3_000_000 {
					t.Errorf("costUSD = %v, input = %d; want 0.45 and 3000000 including compaction", cost, total.InputTokens)
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
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"})
		tk.addMessage(t.Context(), &agent.UsageMessage{Additive: true,
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		})
		tk.addMessage(t.Context(), &agent.UsageMessage{Additive: true,
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 2_000_000, CacheReadInputTokens: 1_000_000},
		})
		costUSD, _, _, _, _ := tk.LiveStats()
		want := 0.15 + 0.50 + 2*0.15 + 0.03
		if costUSD != want {
			t.Fatalf("costUSD = %v, want %v", costUSD, want)
		}
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:     "result",
			UsageIsSnapshot: true,
			TotalCostUSD:    0.01, // Last call only.
			NumTurns:        2,
		})
		if costUSD, _, _, _, _ = tk.LiveStats(); costUSD != want {
			t.Errorf("costUSD after result = %v, want %v (must keep accumulated value)", costUSD, want)
		}
	})

	t.Run("OpenCodeResultPricedPerTurn", func(t *testing.T) {
		t.Parallel()
		// OpenCode reports no per-call usage; its per-turn result usage is
		// priced and accumulated.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.OpenCode, "zai/glm-5.3-flash", "")
		tk.Pricer = prices
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"})
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			NumTurns:    1,
		})
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 2_000_000, CacheReadInputTokens: 1_000_000},
			NumTurns:    1,
		})
		costUSD, _, _, _, _ := tk.LiveStats()
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
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.InitMessage{ReportedModel: "gpt-5.6-terra"})
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
			NumTurns:    1,
		})
		costUSD, _, _, _, _ := tk.LiveStats()
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
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.InitMessage{ReportedModel: "gpt-5.6-terra"})
		tk.addMessage(t.Context(), &agent.UsageMessage{
			ReportedModel: "gpt-5.6-terra",
			ModelDerived:  true,
			Usage:         agent.Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
		})
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType: "result",
			Usage:       agent.Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
			NumTurns:    1,
		})
		costUSD, _, _, _, _ := tk.LiveStats()
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
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"})
		tk.addMessage(t.Context(), &agent.UsageMessage{
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		})
		tk.addMessage(t.Context(), &agent.UsageMessage{
			ReportedModel: "zai/glm-5.3-flash",
			ModelDerived:  true,
			Usage:         agent.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
		})
		tk.addMessage(t.Context(), &agent.ResultMessage{MessageType: "result", NumTurns: 1})
		costUSD, _, _, _, _ := tk.LiveStats()
		if want := 0.15 + 0.50; costUSD != want {
			t.Errorf("costUSD = %v, want %v (assistant-record pricing only)", costUSD, want)
		}
	})

	t.Run("UnpricedModelKeepsReportedTotal", func(t *testing.T) {
		t.Parallel()
		// Claude Code reports the authoritative session total, which the
		// priced path does not touch for unpriced models.
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "claude-sonnet-4-5", "")
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.InitMessage{ReportedModel: "claude-sonnet-4-5"})
		tk.addMessage(t.Context(), &agent.ResultMessage{
			MessageType:  "result",
			TotalCostUSD: 10.0,
			Usage:        agent.Usage{InputTokens: 1_000_000, CacheReadInputTokens: 1_000_000},
			NumTurns:     1,
		})
		costUSD, _, _, _, _ := tk.LiveStats()
		if want := 10.0; costUSD != want {
			t.Errorf("costUSD = %v, want %v (reported total)", costUSD, want)
		}
	})

	t.Run("PricedCostAccumulatesAcrossSessions", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Pi, "zai/glm-5.3-flash", "")
		tk.Pricer = prices
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"})
		tk.addMessage(t.Context(), &agent.UsageMessage{
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 1_000_000},
		})
		tk.ClearMessages(t.Context())
		tk.SetState(taskslog.StateRunning)
		tk.addMessage(t.Context(), &agent.UsageMessage{
			ReportedModel: "zai/glm-5.3-flash",
			Usage:         agent.Usage{InputTokens: 1_000_000},
		})
		costUSD, _, _, _, _ := tk.LiveStats()
		if costUSD != 0.30 {
			t.Errorf("costUSD = %v, want 0.30", costUSD)
		}
	})

	t.Run("SeedTimelinePricesUsageMessages", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Pi, "zai/glm-5.3-flash", "")
		tk.Pricer = prices
		tk.SetState(taskslog.StatePurged)
		tk.SeedTimeline([]agent.Message{
			&agent.InitMessage{ReportedModel: "zai/glm-5.3-flash"},
			&agent.UsageMessage{ReportedModel: "zai/glm-5.3-flash", Usage: agent.Usage{InputTokens: 1_000_000}},
			&agent.ResultMessage{MessageType: "result", NumTurns: 1},
		})
		costUSD, _, _, _, _ := tk.LiveStats()
		if costUSD != 0.15 {
			t.Errorf("costUSD = %v, want 0.15", costUSD)
		}
	})
}

func TestRepositorySummaryRejectsReplacedInstance(t *testing.T) {
	t.Parallel()
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
	tk.SetRuntimeConnectionInfo("test-runtime:new", runtime.ConnectionTarget{}, "", "", 0)
	current := v3.DiffStat{{Path: "current.go", LinesAdded: 1}}
	tk.addParsedMessage(agent.TimedMessage{Message: &agent.DiffStatMessage{DiffStat: current}})
	for _, id := range []runtime.ID{"test-runtime:old", ""} {
		if tk.SetLiveRepositorySummary(&repo.GitSnapshot{Read: repo.NewGitRead(id), RepoStates: []v3.RepoState{{Branch: "stale"}}}) {
			t.Fatal("accepted a summary from a replaced or removed instance")
		}
	}
	read := repo.NewGitRead("test-runtime:old")
	changed, _ := tk.addParsedMessageWithGitSnapshot(agent.TimedMessage{Message: &agent.DiffStatMessage{
		DiffStat: v3.DiffStat{{Path: "old.go"}}, Repos: []v3.RepoState{{Branch: "old"}},
	}}, &repo.GitSnapshot{Read: read}, nil)
	if changed {
		t.Fatal("delayed post-tool probe updated the task summary")
	}
	if got := tk.LiveDiffStat(); !reflect.DeepEqual(got, current) {
		t.Fatalf("current stats overwritten: %+v", got)
	}
}

func TestPartialGitSummary(t *testing.T) {
	t.Parallel()
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
	tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
	target := repo.GitTarget{InstanceID: tk.RuntimeInstanceID(), Repos: []runtime.Repo{{ContainerPath: "/home/user/src/one", Branch: "branch-one"}, {ContainerPath: "/home/user/src/two", Branch: "branch-two"}}}
	tk.SetLiveRepositorySummary(&repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target,
		DiffStat:   v3.DiffStat{{Path: "one/old.go", LinesAdded: 3}, {Path: "two/old.go", LinesAdded: 7}},
		RepoStates: []v3.RepoState{{RepoIndex: 0, ChangedFiles: 1, LinesAdded: 3}, {RepoIndex: 1, ChangedFiles: 1, LinesAdded: 7}}})
	tk.SetLiveRepositorySummary(&repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, FailedRepos: []int{1},
		DiffStat: v3.DiffStat{{Path: "one/fresh.go", LinesAdded: 5}}, RepoStates: []v3.RepoState{{RepoIndex: 0, ChangedFiles: 1, LinesAdded: 5}}})
	if got := tk.LiveDiffStat(); len(got) != 2 || got[0].Path != "one/fresh.go" || got[1].Path != "two/old.go" {
		t.Fatalf("partial read hid failed repository changes: %+v", got)
	}
	states := tk.Snapshot().RepoStates
	if len(states) != 2 || states[0].Stale || !states[1].Stale || states[1].LinesAdded != 7 {
		t.Fatalf("failed repository counts or stale marker lost: %+v", states)
	}
	// A failed post-tool control persists the merged summary, so replay retains it.
	control := &agent.DiffStatMessage{MessageType: "caic_diff_stat"}
	failed := repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, FailedRepos: []int{0, 1}}
	_, _ = tk.addParsedMessageWithGitSnapshot(agent.TimedMessage{Message: control}, &failed, nil)
	restored := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "restored"}, "", "", "")
	restored.SeedTimeline([]agent.Message{control})
	if len(restored.LiveDiffStat()) != 2 || len(restored.Snapshot().RepoStates) != 2 || !restored.Snapshot().RepoStates[0].Stale {
		t.Fatal("replay lost retained stale summary")
	}
}

func TestPartialGitSummaryRecovery(t *testing.T) {
	t.Parallel()
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", "", "")
	tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
	target := repo.GitTarget{InstanceID: tk.RuntimeInstanceID(), Repos: []runtime.Repo{{ContainerPath: "/repo", Branch: "main"}}}
	failed := repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, FailedRepos: []int{0}}
	tk.SetLiveRepositorySummary(&failed)
	if states := tk.Snapshot().RepoStates; len(states) != 1 || !states[0].Stale {
		t.Fatalf("initial failure appeared clean: %+v", states)
	}
	clean := repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, RepoStates: []v3.RepoState{{RepoIndex: 0, Branch: "main"}}}
	if !tk.SetLiveRepositorySummary(&clean) {
		t.Fatal("recovery was not published")
	}
	if states := tk.Snapshot().RepoStates; len(states) != 1 || states[0].Stale {
		t.Fatalf("successful clean read did not clear stale marker: %+v", states)
	}
	if tk.SetLiveRepositorySummary(&failed) {
		t.Fatal("older failure overwrote recovery")
	}
}

func TestGitSummaryLogRoundTrip(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"compact failure", "result failure", "result clean recovery"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
			tk.Repos = []taskslog.RepoMount{{Name: "repo", Branch: "main", ContainerPath: "/repo"}}
			tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
			store := taskslog.NewStore(logtest.Logger(t), t.TempDir())
			log, err := openTaskLog(store, tk)
			if err != nil {
				t.Fatal(err)
			}
			// Dispatch owns the log even before a session is attached.
			target := tk.GitTarget()
			original := &agent.DiffStatMessage{MessageType: "caic_diff_stat", DiffStat: v3.DiffStat{{Path: "old.go", LinesAdded: 7}}, Repos: []v3.RepoState{{RepoIndex: 0, Branch: "main", ChangedFiles: 1, LinesAdded: 7, Stale: kind == "result clean recovery"}}}
			if err := log.AppendMessage(original); err != nil {
				t.Fatal(err)
			}
			tk.addMessage(t.Context(), original)
			snapshot := repo.GitSnapshot{Read: repo.NewGitRead(target.InstanceID), Target: target, FailedRepos: []int{0}}
			var message agent.Message = &agent.DiffStatMessage{MessageType: "caic_diff_stat"}
			if kind != "compact failure" {
				tk.SetState(taskslog.StateWaiting)
				message = &agent.ResultMessage{MessageType: "result"}
				// The harness connection logs native results before the host probe.
				if err := log.AppendNative([]byte(`{"t":"agent","ts":1.000,"msg":{"type":"result","subtype":"success","is_error":false,"result":"done"}}` + "\n")); err != nil {
					t.Fatal(err)
				}
				if kind == "result clean recovery" {
					snapshot.FailedRepos = nil
					snapshot.RepoStates = []v3.RepoState{{RepoIndex: 0, Branch: "main"}}
				}
			}
			changed, err := tk.addParsedMessageWithGitSnapshot(agent.TimedMessage{Message: message}, &snapshot, log)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("changed summary did not notify waiting task")
			}
			live := tk.Snapshot()
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			loaded, err := store.LoadAllReadOnly()
			if err != nil || len(loaded) != 1 {
				t.Fatalf("load = %+v, %v", loaded, err)
			}
			loaded[0].SetWireResolver(agent.Backends{harness.Claude: claudecode.New()})
			if err := loaded[0].LoadMessages(); err != nil {
				t.Fatal(err)
			}
			restored := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "restored"}, harness.Claude, "", "")
			restored.SeedTimelineParts(nil, loaded[0].Timeline)
			replay := restored.Snapshot()
			if !reflect.DeepEqual(live.DiffStat, replay.DiffStat) || !reflect.DeepEqual(live.RepoStates, replay.RepoStates) {
				t.Fatalf("log replay lost Git state: live %+v %+v; replay %+v %+v", live.DiffStat, live.RepoStates, replay.DiffStat, replay.RepoStates)
			}
			if kind == "result clean recovery" {
				if len(replay.DiffStat) != 0 || replay.RepoStates[0].Stale {
					t.Fatal("clean recovery restored old changes")
				}
			} else if len(replay.DiffStat) != 1 || !replay.RepoStates[0].Stale {
				t.Fatal("failure lost retained data or stale marker")
			}
		})
	}
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
		tk.SeedTimeline(messages)
	}
}
