// Tests raw-record projection ranges, shared recovery, cancellation, and cold/warm history costs.

package history

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

type parsingSpy struct {
	agent.WireFormat

	spy *resolverSpy
}

func (s *parsingSpy) ParseMessage(data []byte) ([]agent.Message, error) {
	call := s.spy.calls.Add(1)
	if block := s.spy.block; block != nil && call == block.at {
		close(block.started)
		<-block.release
	}
	return s.WireFormat.ParseMessage(data)
}

type resolverSpy struct {
	backends agent.Backends
	calls    atomic.Int64
	block    *parseBlock
}

func (s *resolverSpy) ResolveWire(h harness.Name) (agent.WireFormat, error) {
	wire, err := s.backends.ResolveWire(h)
	if err != nil {
		return nil, err
	}
	return &parsingSpy{WireFormat: wire, spy: s}, nil
}

type parseBlock struct {
	at      int64
	started chan struct{}
	release <-chan struct{}
}

type pausedWriter struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
	data    bytes.Buffer
}

func (w *pausedWriter) Write(data []byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
		<-w.release
	})
	return w.data.Write(data)
}

const claudeHeader = `{"t":"caic_meta","version":3,"harness":"claude","prompt":"history"}`
const twoTexts = `{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}}}`
const noOutput = `{"t":"agent","ts":2.000,"msg":{"type":"unknown_history_record"}}`
const delta = `{"t":"agent","ts":3.000,"msg":{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"next"}}}}`

func TestUserInputObservationRecovery(t *testing.T) {
	t.Parallel()
	path := writeFixture(t, claudeHeader, `{"t":"user_input","text":"unknown"}`, `{"t":"user_input","ts":1234.567,"text":"timed"}`)
	for attempt := range 2 {
		source, err := NewSource(t.Context(), path, backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var times []int64
		for event, err := range source.Events(t.Context(), snapshot, 1, snapshot.Through) {
			if err != nil {
				t.Fatal(err)
			}
			if event.Message.UserInput != nil {
				times = append(times, event.Message.Ts)
			}
		}
		if !slices.Equal(times, []int64{0, 1234567}) {
			t.Fatalf("cold recovery %d timestamps = %v", attempt, times)
		}
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFixture(t testing.TB, lines ...string) string {
	path := filepath.Join(t.TempDir(), "task.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendFixture(t testing.TB, path, data string) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatal(err)
	}
}

func newFixtureSource(t testing.TB, path string) (*Source, *resolverSpy) {
	resolver := &resolverSpy{backends: backends.Default("", nil)}
	source, err := NewSource(t.Context(), path, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	return source, resolver
}

func TestSource(t *testing.T) {
	t.Parallel()
	t.Run("ExitAfterInputAttempt", func(t *testing.T) {
		t.Parallel()
		for _, outcome := range []string{"unresolved", "failed", "completed", "partial_output", "result_before_outcome", "historical"} {
			t.Run(outcome, func(t *testing.T) {
				t.Parallel()
				path := writeFixture(t, claudeHeader, `{"t":"agent","ts":1.000,"msg":{"type":"result","is_error":false,"result":"done"}}`)
				source, _ := newFixtureSource(t, path)
				if _, err := source.Refresh(t.Context()); err != nil {
					t.Fatal(err)
				}
				var messages []agent.Message
				if outcome != "historical" {
					messages = append(messages, agent.InputDeliveryStarted())
				}
				messages = append(messages, &agent.UserInputMessage{Text: "attempt"})
				switch outcome {
				case "failed":
					messages = append(messages, agent.InputDeliveryFailed(io.ErrClosedPipe))
				case "completed":
					messages = append(messages, agent.InputDeliveryCompleted())
				case "partial_output":
					messages = append(messages, &agent.TextDeltaMessage{Text: "working"}, agent.InputDeliveryFailed(io.ErrClosedPipe))
				case "result_before_outcome":
					messages = append(messages, &agent.ResultMessage{Result: "done again"}, agent.InputDeliveryCompleted())
				}
				want := ""
				if outcome == "completed" || outcome == "partial_output" || outcome == "historical" {
					want = "termination artifact"
				}
				messages = append(messages, &agent.ExitMessage{ExitCode: 1, Error: "termination artifact"})
				for _, m := range messages {
					switch m.(type) {
					case *agent.TextDeltaMessage:
						appendFixture(t, path, delta+"\n")
					case *agent.ResultMessage:
						appendFixture(t, path, `{"t":"agent","ts":4.000,"msg":{"type":"result","is_error":false,"result":"done again"}}`+"\n")
					default:
						encoded, err := agent.MarshalLogMessage(agent.LogVersionV3, m)
						if err != nil {
							t.Fatal(err)
						}
						appendFixture(t, path, string(encoded)+"\n")
					}
					// Each record can be the current end of a growing log. Recovery
					// cannot read ahead to decide the attempt's eventual outcome.
					if _, err := source.Refresh(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				snapshot, err := source.Refresh(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				got := ""
				for event, err := range source.Events(t.Context(), snapshot, 1, snapshot.Through) {
					if err != nil {
						t.Fatal(err)
					}
					if event.Message.Error != nil {
						got = event.Message.Error.Err
					}
				}
				if got != want {
					t.Fatalf("stored exit = %q, want %q", got, want)
				}
			})
		}
	})
	t.Run("TurnCosts", testSourceTurnCosts)
	t.Run("physical identities and immutable partial ranges", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader, noOutput, twoTexts)
		source, spy := newFixtureSource(t, path)
		before, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if before.Through != 3 {
			t.Fatalf("through = %d, want three physical records", before.Through)
		}
		body, err := readRange(source, t.Context(), before, 1, 3)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(body, "\n")
		if len(lines) != 5 || lines[0] != "[" || !strings.HasPrefix(lines[2], ",") || lines[3] != "]" || lines[4] != "" {
			t.Fatalf("history is not one output per line: %q", body)
		}
		var events []Event
		if err := json.Unmarshal([]byte(body), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[0].RecordID != 3 || events[0].Index != 0 || events[1].RecordID != 3 || events[1].Index != 1 || events[0].Message.Text.Text != "first" || events[1].Message.Text.Text != "second" {
			t.Fatalf("record expansion = %s", body)
		}
		appendFixture(t, path, delta[:len(delta)/2])
		partial, err := source.Refresh(t.Context())
		if err != nil || partial != before {
			t.Fatalf("partial raw line advanced snapshot: %+v, %v", partial, err)
		}
		appendFixture(t, path, delta[len(delta)/2:]+"\n")
		after, err := source.Refresh(t.Context())
		if err != nil || after.Through != 4 {
			t.Fatalf("append snapshot = %+v, %v", after, err)
		}
		again, err := readRange(source, t.Context(), after, 1, 3)
		if err != nil || again != body {
			t.Fatalf("published range changed: %s, %v", again, err)
		}
		if calls := spy.calls.Load(); calls != 3 {
			t.Fatalf("native parse calls = %d, want one per complete native record", calls)
		}
		if err := os.Truncate(path, 0); err != nil {
			t.Fatal(err)
		}
		cached, err := readRange(source, t.Context(), before, 1, 3)
		if err != nil || cached != body {
			t.Fatalf("range read depended on raw log: %s, %v", cached, err)
		}
		if _, err := source.Refresh(t.Context()); err == nil {
			t.Fatal("raw truncation was accepted")
		}
	})
	t.Run("ordered ownership survives incremental reads", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t,
			`{"t":"caic_meta","version":3,"harness":"codex","prompt":"history"}`,
			`{"t":"agent","ts":1.000,"msg":{"method":"thread/started","params":{"thread":{"id":"root"}}}}`)
		source, _ := newFixtureSource(t, path)
		if _, err := source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		appendFixture(t, path, `{"t":"agent","ts":2.000,"msg":{"method":"thread/started","params":{"thread":{"id":"child","parentThreadId":"root"}}}}`+"\n"+
			`{"t":"agent","ts":3.000,"msg":{"method":"item/completed","params":{"threadId":"child","turnId":"turn","item":{"id":"answer","type":"agentMessage","text":"child output"}}}}`+"\n")
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for event, err := range source.Events(t.Context(), snapshot, 4, 4) {
			if err != nil {
				t.Fatal(err)
			}
			if event.Message.Kind == v1.EventKindText {
				found = true
				origin := agent.MessageSource(event.Message.Source)
				if !origin.IsChild() || origin.RootThreadID != "root" || origin.ThreadID != "child" {
					t.Fatalf("lost earlier ownership: %+v", origin)
				}
			}
		}
		if !found {
			t.Fatal("child output missing")
		}
	})
	t.Run("API expansion preserves raw record identity", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t,
			`{"t":"caic_meta","version":3,"harness":"codex","prompt":"history"}`,
			`{"t":"agent","ts":1.000,"msg":{"method":"thread/started","params":{"thread":{"id":"root"}}}}`,
			`{"t":"agent","ts":2.000,"msg":{"method":"item/started","params":{"threadId":"root","turnId":"turn","item":{"id":"read","type":"commandExecution","command":"cat /home/user/.agents/skills/review/SKILL.md /home/user/.agents/skills/code-quality/SKILL.md","cwd":"/repo","status":"inProgress"}}}}`,
			`{"t":"agent","ts":3.000,"msg":{"method":"item/completed","params":{"threadId":"root","turnId":"turn","item":{"id":"read","type":"commandExecution","status":"completed","exitCode":0}}}}`)
		source, _ := newFixtureSource(t, path)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var kinds []v1.EventKind
		for event, err := range source.Events(t.Context(), snapshot, 4, 4) {
			if err != nil {
				t.Fatal(err)
			}
			if event.RecordID != 4 || event.Index != uint32(len(kinds)) { //nolint:gosec // The fixture emits three outputs.
				t.Fatalf("wrong expanded identity: %d/%d", event.RecordID, event.Index)
			}
			kinds = append(kinds, event.Message.Kind)
		}
		if !slices.Equal(kinds, []v1.EventKind{v1.EventKindToolResult, v1.EventKindSkillLoad, v1.EventKindSkillLoad}) {
			t.Fatalf("expanded result kinds = %v", kinds)
		}
	})
	t.Run("shared reconstruction and bounded range iterators", func(t *testing.T) {
		t.Parallel()
		lines := make([]string, 1, 801)
		lines[0] = claudeHeader
		for range 800 {
			lines = append(lines, delta)
		}
		path := writeFixture(t, lines...)
		source, spy := newFixtureSource(t, path)
		var wg sync.WaitGroup
		for range 12 {
			wg.Go(func() {
				if snapshot, err := source.Refresh(t.Context()); err != nil || snapshot.Through != 801 {
					t.Errorf("shared refresh = %+v, %v", snapshot, err)
				}
			})
		}
		wg.Wait()
		if spy.calls.Load() != 800 {
			t.Fatalf("concurrent readers replayed raw prefix: %d calls", spy.calls.Load())
		}
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var forward []Event
		for event, err := range source.Events(t.Context(), snapshot, 245, 535) {
			if err != nil {
				t.Fatal(err)
			}
			forward = append(forward, event)
		}
		if len(forward) != 291 || forward[0].RecordID != 245 || forward[len(forward)-1].RecordID != 535 {
			t.Fatalf("range traversal lost identity or ordering: %d events", len(forward))
		}
		if spy.calls.Load() != 800 {
			t.Fatal("projected range traversal reparsed raw history")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := readRange(source, ctx, snapshot, 1, 256); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled range = %v", err)
		}
	})
	t.Run("invalid ranges", func(t *testing.T) {
		t.Parallel()
		source, _ := newFixtureSource(t, writeFixture(t, claudeHeader, twoTexts))
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, span := range [][2]uint64{{0, 1}, {2, 1}, {1, 3}, {1, 257}} {
			if _, err := readRange(source, t.Context(), snapshot, span[0], span[1]); err == nil {
				t.Fatalf("invalid range accepted: %v", span)
			}
		}
	})
	t.Run("large records and stopping traversal", func(t *testing.T) {
		t.Parallel()
		text := strings.Repeat("large content ", 100_000)
		record := fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}}`, text)
		path := writeFixture(t, claudeHeader, record, delta)
		source, spy := newFixtureSource(t, path)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for event, err := range source.Events(t.Context(), snapshot, 1, 3) {
			if err != nil {
				t.Fatal(err)
			}
			if event.RecordID != 2 || event.Message.Text == nil || len(event.Message.Text.Text) != len(text) {
				t.Fatalf("large record lost content or identity: raw=%d", event.RecordID)
			}
			seen++
			break
		}
		if seen != 1 || spy.calls.Load() != 2 {
			t.Fatal("early traversal termination reparsed history")
		}
		if _, err := source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("spool cleanup", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader, twoTexts)
		source, err := NewSource(t.Context(), path, backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		name := source.spool.Name()
		if _, err := source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("projection spool survived Close: %v", err)
		}
		if _, err := source.Refresh(t.Context()); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("closed source refreshed: %v", err)
		}
	})
	t.Run("child completion cannot hide a relay exit", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader,
			`{"t":"agent","ts":1.000,"msg":{"type":"result","subtype":"success","parent_tool_use_id":"child","result":"child success"}}`,
			`{"t":"exit","exit_code":1,"error":"relay failed"}`)
		source, _ := newFixtureSource(t, path)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		childResult, exit := false, false
		for event, err := range source.Events(t.Context(), snapshot, 1, snapshot.Through) {
			if err != nil {
				t.Fatal(err)
			}
			origin := agent.MessageSource(event.Message.Source)
			if event.Message.Result != nil && origin.IsChild() {
				childResult = true
			}
			if event.Message.Error != nil && event.Message.Error.Err == "relay failed" {
				exit = true
			}
		}
		if !childResult || !exit {
			t.Fatalf("child result=%v, visible relay exit=%v", childResult, exit)
		}
	})
	t.Run("chunk boundary equivalence and reconstruction", func(t *testing.T) {
		t.Parallel()
		lines := make([]string, 254, 260)
		lines[0] = claudeHeader
		for i := 1; i < len(lines); i++ {
			lines[i] = noOutput
		}
		lines = append(lines,
			`{"t":"agent","ts":2.000,"msg":{"type":"assistant","message":{"content":[{"type":"tool_use","id":"crossing","name":"Bash","input":{"command":"echo done"}}]}}}`,
			delta,
			`{"t":"agent","ts":4.000,"msg":{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"crossing","content":"done"}]}}}`,
			twoTexts,
			`{"t":"agent","ts":5.000,"msg":{"type":"result","subtype":"success","total_cost_usd":1.5,"usage":{"input_tokens":100,"output_tokens":20}}}`)
		path := writeFixture(t, lines...)
		source, _ := newFixtureSource(t, path)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		firstChunk, err := readRange(source, t.Context(), snapshot, 1, 256)
		if err != nil {
			t.Fatal(err)
		}
		lastChunk, err := readRange(source, t.Context(), snapshot, 257, snapshot.Through)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := taskslog.LoadHistorySource(path)
		if err != nil {
			t.Fatal(err)
		}
		loaded.SetWireResolver(backends.Default("", nil))
		tracker := apiconv.NewToolTimingTracker(loaded.Harness, loaded.RequestedModel, nil)
		var expected []v1.EventMessage
		for parsed, err := range loaded.StreamMessages(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			expected = append(expected, tracker.ConvertMessage(parsed.Message, parsed.ProducerTime)...)
		}
		var actual []v1.EventMessage
		for event, err := range source.Events(t.Context(), snapshot, 1, snapshot.Through) {
			if err != nil {
				t.Fatal(err)
			}
			actual = append(actual, event.Message)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatal("chunked ordered projection differs from canonical conversion across tool/delta/final boundary")
		}
		appendFixture(t, path, `{"t":"context_cleared"}`+"\n"+twoTexts+"\n")
		if _, err := source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		rebuilt, _ := newFixtureSource(t, path)
		rebuiltSnapshot, err := rebuilt.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range []struct {
			first, last uint64
			body        string
		}{{1, 256, firstChunk}, {257, snapshot.Through, lastChunk}} {
			body, err := readRange(rebuilt, t.Context(), rebuiltSnapshot, chunk.first, chunk.last)
			if err != nil || body != chunk.body {
				t.Fatalf("published range changed after compaction/reconstruction: %v", err)
			}
		}
	})
	t.Run("reads during refresh preserve old snapshot", func(t *testing.T) {
		t.Parallel()
		path := writeFixture(t, claudeHeader, twoTexts)
		source, spy := newFixtureSource(t, path)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		before, err := readRange(source, t.Context(), snapshot, 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		releaseContext, release := context.WithCancel(t.Context())
		t.Cleanup(release)
		spy.block = &parseBlock{at: 2, started: make(chan struct{}), release: releaseContext.Done()}
		appendFixture(t, path, delta+"\n")
		refreshDone := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Go(func() { _, err := source.Refresh(t.Context()); refreshDone <- err })
		<-spy.block.started
		readDone := make(chan error, 1)
		wg.Go(func() {
			body, err := readRange(source, t.Context(), snapshot, 1, 2)
			if err == nil && body != before {
				err = errors.New("old snapshot bytes changed during refresh")
			}
			readDone <- err
		})
		release()
		wg.Wait()
		if err := errors.Join(<-refreshDone, <-readDone); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("streamed old ranges stay consistent while suffixes refresh", func(t *testing.T) {
		t.Parallel()
		text := strings.Repeat("prior response ", 8_000)
		record := fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}}`, text)
		path := writeFixture(t, claudeHeader, record)
		source, _ := newFixtureSource(t, path)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		before, err := readRange(source, t.Context(), snapshot, 1, snapshot.Through)
		if err != nil {
			t.Fatal(err)
		}
		releaseContext, release := context.WithCancel(t.Context())
		t.Cleanup(release)
		writer := &pausedWriter{started: make(chan struct{}), release: releaseContext.Done()}
		done := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			done <- source.WriteSlice(t.Context(), snapshot, v1.HistorySlice{First: Position{RecordID: 1}, Last: Position{RecordID: snapshot.Through, Index: RecordEndIndex}}, writer)
		})
		<-writer.started
		appendFixture(t, path, delta+"\n")
		refreshCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		after, err := source.Refresh(refreshCtx)
		release()
		wg.Wait()
		if err != nil || after.Through != 3 {
			t.Fatalf("blocked history writer stalled suffix refresh: through=%d, %v", after.Through, err)
		}
		if err := <-done; err != nil || writer.data.String() != before {
			t.Fatalf("concurrent append changed streamed snapshot: %v", err)
		}
	})
}

func TestSourceSpoolProcessExit(t *testing.T) {
	if path := os.Getenv("CAIC_SPOOL_EXIT_FIXTURE"); path != "" {
		source, err := NewSource(t.Context(), path, backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		fmt.Println(source.spool.Name())
		os.Exit(0) // Exercise operating-system cleanup without Source.Close.
	}
	t.Parallel()
	path := writeFixture(t, claudeHeader, twoTexts)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSourceSpoolProcessExit$") //nolint:gosec // Runs this test binary returned by os.Executable, not caller input.
	cmd.Env = append(os.Environ(), "CAIC_SPOOL_EXIT_FIXTURE="+path)
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSpace(string(output))
	if filepath.Dir(name) != filepath.Dir(path) {
		t.Fatal("projection spool was not colocated with the owned log")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := root.Stat(filepath.Base(name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("process exit orphaned its projection spool: %v", err)
	}
}

func TestSourceControls(t *testing.T) {
	t.Parallel()
	path := writeFixture(t, claudeHeader,
		`{"t":"session","session_id":"session-1","model":"claude-sonnet-4-6","reported_effort":"high"}`,
		`{"t":"user_input","text":"follow up","images":[{"media_type":"image/png","data":"aGk="}]}`,
		`{"t":"text","text":"plain response"}`)
	source, _ := newFixtureSource(t, path)
	snapshot, err := source.Refresh(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for event, err := range source.Events(t.Context(), snapshot, 1, snapshot.Through) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want exactly init/input/text", len(events))
	}
	init := events[0].Message.Init
	if events[0].RecordID != 2 || init == nil || init.SessionID != "session-1" || init.ReportedModel != "claude-sonnet-4-6" || init.ReportedEffort != "high" {
		t.Fatalf("missing addressable session: %+v", events[0])
	}
	input := events[1].Message.UserInput
	if input == nil || input.Text != "follow up" || len(input.Images) != 1 {
		t.Fatalf("missing user input/images: %+v", events[1])
	}
	if events[2].Message.Text == nil || events[2].Message.Text.Text != "plain response" {
		t.Fatalf("missing plain text: %+v", events[2])
	}
}

func assertSourcePoisoned(t *testing.T, source *Source, snapshot Snapshot, want error) {
	if _, err := readRange(source, t.Context(), snapshot, 1, 1); !errors.Is(err, want) {
		t.Fatalf("ReadRange after failure: %v", err)
	}
	var output bytes.Buffer
	if err := source.WriteSlice(t.Context(), snapshot, v1.HistorySlice{First: Position{RecordID: 1}, Last: Position{RecordID: 1, Index: RecordEndIndex}}, &output); !errors.Is(err, want) {
		t.Fatalf("WriteSlice after failure: %v", err)
	}
	for _, bounds := range [][2]uint64{{1, snapshot.Through}, {1, 0}} {
		for _, sequence := range []func(func(Event, error) bool){source.Events(t.Context(), snapshot, bounds[0], bounds[1])} {
			failed := false
			for _, err := range sequence {
				if !errors.Is(err, want) {
					t.Fatalf("iterator after failure: %v", err)
				}
				failed = true
			}
			if !failed {
				t.Fatal("iterator silently accepted poisoned source")
			}
		}
	}
}

func TestSourceStickyFailure(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{"truncate", `{"t":"agent","ts":1,"msg":`, `{"t":"caic_meta","version":3,"harness":"codex"}`} {
		t.Run(suffix, func(t *testing.T) {
			t.Parallel()
			path := writeFixture(t, claudeHeader, twoTexts)
			source, _ := newFixtureSource(t, path)
			snapshot, err := source.Refresh(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if suffix == "truncate" {
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			} else {
				appendFixture(t, path, suffix+"\n")
			}
			_, err = source.Refresh(t.Context())
			if err == nil {
				t.Fatal("accepted corrupt history")
			}
			assertSourcePoisoned(t, source, snapshot, err)
		})
	}
	t.Run("failed boundary frame does not publish offset", func(t *testing.T) {
		t.Parallel()
		lines := append([]string{claudeHeader}, slices.Repeat([]string{noOutput}, 255)...)
		path := writeFixture(t, lines...)
		resolver := agent.Backends{harness.Claude: &agenttest.FakeBackend{WireFactory: func() agent.WireFormat {
			return &agenttest.Wire{Parse: func(data []byte) ([]agent.Message, error) {
				if bytes.Contains(data, []byte("invalid_cost")) {
					return []agent.Message{&agent.ResultMessage{TotalCostUSD: math.NaN()}}, nil
				}
				return nil, nil
			}}
		}}}
		source, err := NewSource(t.Context(), path, resolver)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		end := source.end
		offsets := len(source.starts)
		appendFixture(t, path, `{"t":"agent","ts":1,"msg":{"type":"invalid_cost"}}`+"\n")
		_, err = source.Refresh(t.Context())
		if err == nil {
			t.Fatal("accepted unencodable native cost")
		}
		if source.through != 256 || len(source.starts) != offsets || source.end != end {
			t.Fatal("failed frame published an offset or progress")
		}
		assertSourcePoisoned(t, source, snapshot, err)
	})
}

func BenchmarkSource(b *testing.B) {
	const turns = 5_000
	lines := make([]string, 1, 1+turns*10)
	lines[0] = claudeHeader
	output := strings.Repeat("tool output line\n", 256)
	large := strings.Repeat("large output line\n", 64<<10)
	for turn := range turns {
		id := fmt.Sprintf("tool-%d", turn)
		result := output
		if turn%1_000 == 0 {
			result = large
		}
		lines = append(lines,
			fmt.Sprintf(`{"t":"agent","ts":1.000,"msg":{"type":"user","message":{"role":"user","content":[{"type":"text","text":%q}]}}}`, fmt.Sprintf("Follow-up turn %d", turn)),
			fmt.Sprintf(`{"t":"agent","ts":2.000,"msg":{"type":"assistant","message":{"content":[{"type":"thinking","thinking":%q}]}}}`, "Inspect the affected files and validate their behavior."),
			fmt.Sprintf(`{"t":"agent","ts":3.000,"msg":{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"git diff --stat"}}]}}}`, id),
			delta,
			fmt.Sprintf(`{"t":"agent","ts":4.000,"msg":{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}}`, id, result),
			delta,
			fmt.Sprintf(`{"t":"agent","ts":5.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":%q}],"usage":{"input_tokens":10000,"output_tokens":800,"cache_read_input_tokens":3000}}}}`, "The requested changes are implemented. "+strings.Repeat("Validation passed. ", 24)),
			fmt.Sprintf(`{"t":"agent","ts":6.000,"msg":{"type":"result","subtype":"success","total_cost_usd":%.3f,"num_turns":1,"usage":{"input_tokens":10000,"output_tokens":800}}}`, float64(turn+1)*0.002),
			`{"t":"agent","ts":7.000,"msg":{"type":"system","subtype":"compact_boundary"}}`,
			noOutput)
	}
	path := writeFixture(b, lines...)
	encoded, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		b.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		b.Fatal(err)
	}
	compressed := path + ".zst"
	if err := os.WriteFile(filepath.Clean(compressed), encoder.EncodeAll(encoded, nil), 0o600); err != nil { //nolint:gosec // Compressed fixture path belongs to this test's private temporary directory.
		b.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		b.Fatal(err)
	}
	b.Run("Compressed", func(b *testing.B) { benchmarkSource(b, compressed, lines[1:]) })
	// Cycle through complete varied turns, including their large output records.
	benchmarkSource(b, path, lines[1:])
}

func benchmarkSource(b *testing.B, path string, appends []string) {
	resolver := backends.Default("", nil)
	b.Run("RawReplayBaseline", func(b *testing.B) {
		loaded, err := taskslog.LoadHistorySource(path)
		if err != nil {
			b.Fatal(err)
		}
		loaded.SetWireResolver(resolver)
		b.ReportAllocs()
		for b.Loop() {
			tracker := apiconv.NewToolTimingTracker(harness.Claude, "", nil)
			for message, err := range loaded.StreamMessages(b.Context()) {
				if err != nil {
					b.Fatal(err)
				}
				tracker.ConvertMessage(message.Message, message.ProducerTime)
			}
		}
	})
	b.Run("ColdProjection", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			source, err := NewSource(b.Context(), path, resolver)
			if err != nil {
				b.Fatal(err)
			}
			_, refreshErr := source.Refresh(b.Context())
			if err := errors.Join(refreshErr, source.Close()); err != nil {
				b.Fatal(err)
			}
		}
	})
	source, _ := newFixtureSource(b, path)
	snapshot, err := source.Refresh(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	b.Run("WarmOldChunk", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := readRange(source, b.Context(), snapshot, 1, 256); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("WarmRefresh", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := source.Refresh(b.Context()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("WarmManifest", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, _, _, err := source.Sections(b.Context(), "history"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("WarmCurrentSection", func(b *testing.B) {
		_, sections, _, err := source.Sections(b.Context(), "history")
		if err != nil {
			b.Fatal(err)
		}
		body := sections[len(sections)-1].Body
		if body == nil {
			b.Fatal("missing current body")
		}
		b.ReportAllocs()
		for b.Loop() {
			if err := source.WriteSlice(b.Context(), snapshot, *body, io.Discard); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, project := range []bool{false, true} {
		name := "IncrementalRawBaseline"
		if project {
			name = "IncrementalProjection"
		}
		b.Run(name, func(b *testing.B) {
			path := writeFixture(b, claudeHeader)
			f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := f.Close(); err != nil {
					b.Error(err)
				}
			})
			if project {
				source, _ := newFixtureSource(b, path)
				if _, err := source.Refresh(b.Context()); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				at := 0
				for b.Loop() {
					if _, err := f.WriteString(appends[at%len(appends)] + "\n"); err != nil {
						b.Fatal(err)
					}
					at++
					if _, err := source.Refresh(b.Context()); err != nil {
						b.Fatal(err)
					}
				}
				return
			}
			cursor, err := taskslog.OpenRecordCursor(path, resolver)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					b.Error(err)
				}
			})
			for _, err := range cursor.Read(b.Context()) {
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			at := 0
			for b.Loop() {
				if _, err := f.WriteString(appends[at%len(appends)] + "\n"); err != nil {
					b.Fatal(err)
				}
				at++
				for _, err := range cursor.Read(b.Context()) {
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// TestSourceLocalChunkSizes measures physical-record-count chunks from recent
// plain production logs without retaining or printing conversation contents.
// Run with CAIC_HISTORY_LOG_DIRECTORY=$HOME/.cache/caic/tasks go test
// ./backend/internal/server/history -run '^TestSourceLocalChunkSizes$' -v.
// Local production data is optional and is excluded from normal benchmarks.
func TestSourceLocalChunkSizes(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("CAIC_HISTORY_LOG_DIRECTORY")
	if dir == "" {
		t.Skip("set CAIC_HISTORY_LOG_DIRECTORY for local physical-record measurements")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := dataset.Close(); err != nil {
			t.Error(err)
		}
	})
	type sizes struct {
		raw  []int64
		gzip []int64
	}
	counts := []uint64{128, 256, 512}
	measurements := make(map[uint64]*sizes)
	for _, count := range counts {
		measurements[count] = &sizes{}
	}
	files := 0
	var records uint64
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().Before(time.Now().Add(-48 * time.Hour)) {
			continue
		}
		input, err := dataset.Open(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		// Keep source datasets read-only. A bounded file-size snapshot also
		// prevents concurrent production appends changing the measured prefix.
		copyPath := filepath.Join(t.TempDir(), "task.jsonl")
		copyFile, err := os.OpenFile(filepath.Clean(copyPath), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = input.Close()
			t.Fatal(err)
		}
		_, copyErr := io.CopyN(copyFile, input, info.Size())
		if err := errors.Join(copyErr, input.Close(), copyFile.Close()); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		source, err := NewSource(t.Context(), copyPath, backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			_ = source.Close()
			t.Fatal(err)
		}
		files++
		records += snapshot.Through
		t.Logf("local log=%d harness=%s raw records=%d input bytes=%d spool bytes=%d cold projection=%s", files, source.cursor.Harness, snapshot.Through, info.Size(), source.end, time.Since(started))
		for _, count := range counts {
			for first := uint64(1); first+count-1 <= snapshot.Through; first += count {
				last := first + count - 1
				var body strings.Builder
				body.WriteByte('[')
				comma := false
				for pos := first; pos <= last; {
					end := min(last, ((pos-1)/RecordsPerChunk+1)*RecordsPerChunk)
					part, err := readRange(source, t.Context(), snapshot, pos, end)
					if err != nil {
						_ = source.Close()
						t.Fatal(err)
					}
					if len(part) > 2 {
						if comma {
							body.WriteByte(',')
						}
						body.WriteString(part[1 : len(part)-1])
						comma = true
					}
					pos = end + 1
				}
				body.WriteByte(']')
				var compressed bytes.Buffer
				zip, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := zip.Write([]byte(body.String()))
				if err := errors.Join(writeErr, zip.Close()); err != nil {
					t.Fatal(err)
				}
				measurements[count].raw = append(measurements[count].raw, int64(body.Len()))
				measurements[count].gzip = append(measurements[count].gzip, int64(compressed.Len()))
			}
		}
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("recent plain logs=%d physical records=%d", files, records)
	for _, count := range counts {
		m := measurements[count]
		slices.Sort(m.raw)
		slices.Sort(m.gzip)
		if len(m.raw) == 0 {
			t.Fatalf("no complete %d-record chunks", count)
		}
		median := (len(m.raw) - 1) / 2
		p99 := (len(m.raw)*99+99)/100 - 1
		t.Logf("records/chunk=%d chunks=%d JSON median=%d p99=%d max=%d gzip1 median=%d p99=%d max=%d bytes", count, len(m.raw), m.raw[median], m.raw[p99], m.raw[len(m.raw)-1], m.gzip[median], m.gzip[p99], m.gzip[len(m.gzip)-1])
	}
}

// readRange materializes test output through the same builder path used by the
// cold/warm benchmark baseline; production callers stream through WriteSlice.
func readRange(s *Source, ctx context.Context, snapshot Snapshot, first, last uint64) (string, error) {
	var result strings.Builder
	if err := s.WriteSlice(ctx, snapshot, v1.HistorySlice{First: Position{RecordID: first}, Last: Position{RecordID: last, Index: RecordEndIndex}}, &result); err != nil {
		return "", err
	}
	return result.String(), nil
}

func TestSourceCompressed(t *testing.T) {
	t.Parallel()
	lines := []string{claudeHeader, twoTexts, noOutput, `{"t":"agent","ts":4.000,"msg":{"type":"result","subtype":"success","total_cost_usd":1.25,"usage":{"input_tokens":42}}}`}
	plain := writeFixture(t, lines...)
	encoded, err := os.ReadFile(filepath.Clean(plain))
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := plain + ".zst"
	compressedBytes := encoder.EncodeAll(encoded, nil)
	if err := os.WriteFile(filepath.Clean(compressed), compressedBytes, 0o600); err != nil { //nolint:gosec // Compressed fixture path belongs to this test's private temporary directory.
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	var previous string
	var previousCosts []TurnEstimate
	for _, path := range []string{plain, compressed} {
		source, _ := newFixtureSource(t, path)
		snapshot, err := source.Refresh(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Through != 4 {
			t.Fatalf("physical records: %+v", snapshot)
		}
		output, err := readRange(source, t.Context(), snapshot, 1, 4)
		if err != nil {
			t.Fatal(err)
		}
		var costs []TurnEstimate
		for cost, err := range source.TurnCosts(t.Context(), snapshot, 4, nil) {
			if err != nil {
				t.Fatal(err)
			}
			costs = append(costs, cost)
		}
		if previous != "" && (output != previous || !reflect.DeepEqual(costs, previousCosts)) {
			t.Fatal("compressed projection differs in messages, positions or prices")
		}
		if len(costs) != 1 || costs[0].Cost.USD != 1.25 {
			t.Fatalf("pricing: %+v", costs)
		}
		previous, previousCosts = output, costs
		again, err := source.Refresh(t.Context())
		if err != nil || again != snapshot {
			t.Fatalf("settled refresh: %+v %v", again, err)
		}
	}
	t.Run("error/truncated compressed stream", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "truncated.jsonl.zst")
		if err := os.WriteFile(path, compressedBytes[:len(compressedBytes)-4], 0o600); err != nil { //nolint:gosec // Path is generated inside this test's private temporary directory.
			t.Fatal(err)
		}
		source, err := NewSource(t.Context(), path, backends.Default("", nil))
		if err != nil {
			return
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Error(err)
			}
		})
		if _, err := source.Refresh(t.Context()); err == nil {
			t.Fatal("truncated compression stream accepted")
		}
	})
}
