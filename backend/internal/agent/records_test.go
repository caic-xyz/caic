// Tests accepted-byte ordering, terminal interpretation failures, and derived-record publication barriers.

package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
)

func TestRecords(t *testing.T) {
	t.Parallel()
	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			t.Run("failed_append_does_not_interpret", func(t *testing.T) {
				t.Parallel()
				failure := errors.New("journal unavailable")
				sink := &rejectedRecords{failure: failure, at: 1}
				observer := &agenttest.RecordHandler{}
				parsed := 0
				wire := &agenttest.Wire{Output: func([]byte) ([]agent.Message, error) {
					parsed++
					return []agent.Message{&agent.TextMessage{Text: "unrecorded"}}, nil
				}}
				owner, err := agent.NewRecords(t.Context(), wire, sink, observer)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := owner.Close(); !errors.Is(err, failure) {
						t.Errorf("close = %v", err)
					}
				})
				reader := agent.NewRelayRecordReader(t.Context(), strings.NewReader(`{"t":"agent","ts":1.000,"msg":{}}`+"\n"), owner)
				if _, _, err := reader.ReadRecord(); !errors.Is(err, failure) {
					t.Fatalf("read error = %v", err)
				}
				if parsed != 0 || len(observer.Events()) != 0 {
					t.Fatalf("parsed=%d events=%v", parsed, observer.Events())
				}
				if err := owner.AppendMessage(t.Context(), &agent.LogMessage{Line: "later"}); !errors.Is(err, failure) {
					t.Fatalf("terminal failure = %v", err)
				}
			})
			t.Run("derived_append_retains_evidence_without_publishing_result", func(t *testing.T) {
				t.Parallel()
				failure := errors.New("derived record unavailable")
				sink := &rejectedRecords{failure: failure, at: 2}
				observer := &derivedRecords{}
				owner, err := agent.NewRecords(t.Context(), claudecode.New().NewWire(), sink, observer)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := owner.Close(); !errors.Is(err, failure) {
						t.Errorf("close = %v", err)
					}
				})
				input := `{"t":"agent","ts":1.000,"msg":{"type":"result","subtype":"success","result":"done","num_turns":1}}` + "\n"
				reader := agent.NewRelayRecordReader(t.Context(), strings.NewReader(input), owner)
				if _, _, err := reader.ReadRecord(); !errors.Is(err, failure) {
					t.Fatalf("read error = %v", err)
				}
				if sink.String() != input || len(observer.Events()) != 0 {
					t.Fatalf("evidence=%q events=%v", sink.String(), observer.Events())
				}
			})
			t.Run("interpretation_is_terminal_and_retains_exact_evidence", func(t *testing.T) {
				t.Parallel()
				var first error
				sink := &agenttest.LogSink{}
				observer := &agenttest.RecordHandler{}
				owner, err := agent.NewRecords(t.Context(), claudecode.New().NewWire(), sink, observer)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := owner.Close(); !errors.Is(err, first) {
						t.Errorf("close = %v", err)
					}
				})
				input := `{"t":"agent","ts":1.000,"msg":[]}` + "\n"
				reader := agent.NewRelayRecordReader(t.Context(), strings.NewReader(input), owner)
				_, _, first = reader.ReadRecord()
				if first == nil {
					t.Fatal("expected native schema error")
				}
				next := agent.NewRelayRecordReader(t.Context(), strings.NewReader(`{"t":"log","line":"later"}`+"\n"), owner)
				if _, _, err := next.ReadRecord(); !errors.Is(err, first) {
					t.Fatalf("next error = %v, want %v", err, first)
				}
				if sink.String() != input || len(observer.Events()) != 0 {
					t.Fatalf("evidence=%q events=%v", sink.String(), observer.Events())
				}
			})
		})
	})
	t.Run("WriteInput", func(t *testing.T) {
		t.Parallel()
		t.Run("error_failed_input_append_does_not_write_transport", func(t *testing.T) {
			t.Parallel()
			failure := errors.New("input journal unavailable")
			sink := &rejectedRecords{failure: failure, at: 1}
			observer := &agenttest.RecordHandler{}
			owner, err := agent.NewRecords(t.Context(), &agenttest.Wire{}, sink, observer)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := owner.Close(); !errors.Is(err, failure) {
					t.Errorf("close = %v", err)
				}
			})
			var output strings.Builder
			if err := owner.WriteInput(t.Context(), &output, []byte("{}\n")); !errors.Is(err, failure) {
				t.Fatalf("input error = %v", err)
			}
			if output.Len() != 0 || len(observer.Events()) != 0 {
				t.Fatalf("transport=%q events=%v", output.String(), observer.Events())
			}
		})
	})
}

type rejectedRecords struct {
	agenttest.LogSink

	failure error
	at      int
	calls   int
}

func (s *rejectedRecords) AppendRecord(record agent.PhysicalRecord) error {
	s.calls++
	if s.calls == s.at {
		return s.failure
	}
	return s.LogSink.AppendRecord(record)
}

type derivedRecords struct{ agenttest.RecordHandler }

func (*derivedRecords) PrepareRecords(context.Context, []agent.TimedMessage) ([]agent.Message, error) {
	return []agent.Message{&agent.DiffStatMessage{MessageType: "caic_diff_stat"}}, nil
}
