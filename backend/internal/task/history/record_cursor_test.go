// Tests semantic control preservation and descriptor-owned authority in incremental physical history.

package history

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestRecordCursor(t *testing.T) {
	t.Parallel()
	t.Run("Read", func(t *testing.T) {
		t.Parallel()
		t.Run("delivery timestamps and early stop", func(t *testing.T) {
			t.Parallel()
			for _, compressed := range []bool{false, true} {
				t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
					t.Parallel()
					path := writePhysicalTestLog(t, compressed,
						`{"t":"caic_meta","version":3,"harness":"dsh","prompt":"test"}`,
						`{"t":"input_delivery","subtype":"input_delivery_started","session_id":"s","uuid":"i"}`,
						`{"t":"input","ts":100.125,"msg":{"jsonrpc":"2.0","id":17,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"test"}]}}}`,
						`{"t":"input_delivery","subtype":"input_delivery_completed","session_id":"s","uuid":"i"}`)
					before, err := os.ReadFile(path) //nolint:gosec // The path is a fixture created beneath t.TempDir.
					if err != nil {
						t.Fatal(err)
					}
					resolver := backends.Default("", nil)
					cursor, err := OpenRecordCursor(path, resolver)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := cursor.Close(); err != nil {
							t.Error(err)
						}
					})
					var got []agent.TimedMessage
					for record, err := range cursor.Read(t.Context()) {
						if err != nil {
							t.Fatal(err)
						}
						got = append(got, record.Messages...)
						if record.ID == 2 {
							break
						}
					}
					var remaining []uint64
					for record, err := range cursor.Read(t.Context()) {
						if err != nil {
							t.Fatal(err)
						}
						remaining = append(remaining, record.ID)
						got = append(got, record.Messages...)
					}
					if !slices.Equal(remaining, []uint64{3, 4}) {
						t.Fatalf("remaining physical IDs = %v", remaining)
					}
					loaded, err := loadSemanticTask(path, resolver)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, loaded.Timeline) || len(got) != 4 {
						t.Fatalf("cursor/timeline mismatch: %#v %#v", got, loaded.Timeline)
					}
					for _, i := range []int{0, 3} {
						m, ok := got[i].Message.(*agent.SystemMessage)
						if !ok || m.Ts != 100.125 || !got[i].ProducerTime.Equal(time.UnixMilli(100125)) {
							t.Fatalf("delivery time = %+v", got[i])
						}
					}
					after, err := os.ReadFile(path) //nolint:gosec // Compare the owned temporary fixture after read-only loading.
					if err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(before, after) {
						t.Fatal("historical log changed")
					}
				})
			}
		})
		t.Run("delivery pending across polling and cancellation", func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, false,
				`{"t":"caic_meta","version":3,"harness":"claude","prompt":"test"}`,
				`{"t":"input_delivery","subtype":"input_delivery_started","session_id":"s","uuid":"i"}`)
			cursor, err := OpenRecordCursor(path, backends.Default("", nil))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			var ids []uint64
			for record, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, record.ID)
			}
			if !slices.Equal(ids, []uint64{1}) {
				t.Fatalf("pending IDs = %v", ids)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			for _, err := range cursor.Read(ctx) {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation = %v", err)
				}
			}
			writer, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := writer.WriteString(`{"t":"user_input","ts":42.125,"text":"later"}` + "\n")
			if err := errors.Join(writeErr, writer.Close()); err != nil {
				t.Fatal(err)
			}
			var times []time.Time
			ids = nil
			for record, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, record.ID)
				for _, timed := range record.Messages {
					times = append(times, timed.ProducerTime)
				}
			}
			if !slices.Equal(ids, []uint64{2, 3}) || len(times) != 2 || !times[0].Equal(time.UnixMilli(42125)) || !times[1].Equal(time.UnixMilli(42125)) {
				t.Fatalf("appended records = %v %v", ids, times)
			}
		})
		t.Run("cancellation between buffered delivery records", func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, false,
				`{"t":"caic_meta","version":3,"harness":"claude","prompt":"test"}`,
				`{"t":"input_delivery","subtype":"input_delivery_started","session_id":"s","uuid":"i"}`,
				`{"t":"user_input","ts":42.125,"text":"accepted"}`)
			cursor, err := OpenRecordCursor(path, backends.Default("", nil))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			var ids []uint64
			var stopped bool
			for record, err := range cursor.Read(ctx) {
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					stopped = true
					break
				}
				ids = append(ids, record.ID)
				if record.ID == 2 {
					cancel()
				}
			}
			cancel()
			if !stopped || !slices.Equal(ids, []uint64{1, 2}) {
				t.Fatalf("canceled records = %v, stopped=%t", ids, stopped)
			}
			ids = nil
			for record, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, record.ID)
			}
			if !slices.Equal(ids, []uint64{3}) {
				t.Fatalf("resumed records = %v", ids)
			}
		})
		t.Run("sealed unmatched delivery retains unknown time", func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, true,
				`{"t":"caic_meta","version":3,"harness":"claude","prompt":"test"}`,
				`{"t":"input_delivery","subtype":"input_delivery_started","session_id":"","uuid":""}`)
			cursor, err := OpenRecordCursor(path, backends.Default("", nil))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			var ids []uint64
			var messages []agent.TimedMessage
			for record, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, record.ID)
				messages = append(messages, record.Messages...)
			}
			if !slices.Equal(ids, []uint64{1, 2}) || len(messages) != 1 || !messages[0].ProducerTime.IsZero() {
				t.Fatalf("unmatched records=%v %+v", ids, messages)
			}
			loaded, err := loadSemanticTask(path, backends.Default("", nil))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(messages, loaded.Timeline) {
				t.Fatalf("unmatched cursor/full mismatch: %+v %+v", messages, loaded.Timeline)
			}
		})
		t.Run("controls", func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, false,
				`{"t":"caic_meta","version":3,"harness":"claude","prompt":"original"}`,
				`{"t":"session","session_id":"session-1","model":"claude-sonnet-4-6","reported_effort":"high","agent_version":"1.2"}`,
				`{"t":"user_input","text":"follow up","images":[{"media_type":"image/png","data":"aGk="}]}`,
				`{"t":"text","text":"plain response"}`)
			resolver := backends.Default("", nil)
			cursor, err := OpenRecordCursor(path, resolver)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			var conversation []agent.TimedMessage
			var session *agent.MetaSessionMessage
			for record, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				for _, message := range record.Messages {
					if m, ok := message.Message.(*agent.MetaSessionMessage); ok {
						session = m
					}
					conversation = append(conversation, message)
				}
			}
			loaded, err := loadSemanticTask(path, resolver)
			if err != nil {
				t.Fatal(err)
			}
			if session == nil || session.SessionID != loaded.SessionID || session.ReportedModel != loaded.ReportedModel || session.ReportedEffort != loaded.ReportedEffort {
				t.Fatalf("session context missing or differs from semantic loading: %+v", session)
			}
			if len(conversation) != 3 || !reflect.DeepEqual(conversation, loaded.Timeline) {
				t.Fatalf("conversation controls differ from semantic loading: %#v", conversation)
			}
			input, ok := conversation[1].Message.(*agent.UserInputMessage)
			if !ok || input.Text != "follow up" || len(input.Images) != 1 || input.Images[0].Data != "aGk=" {
				t.Fatalf("input/images not preserved: %#v", conversation[0].Message)
			}
		})
		t.Run("compressed EOF", func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, true, `{"t":"caic_meta","version":3,"harness":"claude","prompt":"test"}`, `{"t":"text","text":"answer"}`)
			cursor, err := OpenRecordCursor(path, backends.Default("", nil))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			count := 0
			for _, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				count++
			}
			if count != 2 {
				t.Fatalf("read %d physical records", count)
			}
			for _, err := range cursor.Read(t.Context()) {
				t.Fatalf("settled read produced another record: %v", err)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Run("pending delivery precedes terminal decode failure", func(t *testing.T) {
				t.Parallel()
				for _, stopEarly := range []bool{false, true} {
					t.Run(fmt.Sprintf("stopEarly=%t", stopEarly), func(t *testing.T) {
						t.Parallel()
						path := writePhysicalTestLog(t, false,
							`{"t":"caic_meta","version":3,"harness":"opencode","prompt":"test"}`,
							`{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":17,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"test"}]}}}`,
							`{"t":"input_delivery","subtype":"input_delivery_started","session_id":"s","uuid":"i"}`,
							`{"t":"agent","ts":2.000,"msg":{"jsonrpc":"2.0","id":17,"result":{"stopReason":42}}}`)
						cursor, err := OpenRecordCursor(path, backends.Default("", nil))
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := cursor.Close(); err != nil {
								t.Error(err)
							}
						})
						var ids []uint64
						var failure error
						for record, err := range cursor.Read(t.Context()) {
							if err != nil {
								failure = err
								break
							}
							ids = append(ids, record.ID)
							if record.ID == 3 {
								if len(record.Messages) != 1 || !record.Messages[0].ProducerTime.IsZero() {
									t.Fatalf("pending = %+v", record)
								}
								if stopEarly {
									break
								}
							}
						}
						if !slices.Equal(ids, []uint64{1, 2, 3}) {
							t.Fatalf("valid records before failure = %v", ids)
						}
						if stopEarly {
							for record, err := range cursor.Read(t.Context()) {
								if err == nil || record.ID != 0 {
									t.Fatalf("resumed error = %+v %v", record, err)
								}
								failure = err
							}
						}
						if failure == nil {
							t.Fatal("missing terminal decode error")
						}
						for _, err := range cursor.Read(t.Context()) {
							if !errors.Is(err, failure) {
								t.Fatalf("terminal error changed: %v", err)
							}
						}
					})
				}
			})
			for _, compressed := range []bool{false, true} {
				t.Run(fmt.Sprintf("terminal native decode compressed=%t", compressed), func(t *testing.T) {
					t.Parallel()
					path := writePhysicalTestLog(t, compressed,
						`{"t":"caic_meta","version":3,"harness":"opencode","prompt":"test"}`,
						`{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":17,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"test"}]}}}`,
						`{"t":"agent","ts":2.000,"msg":{"jsonrpc":"2.0","id":17,"result":{"stopReason":42}}}`,
						`{"t":"text","text":"must not resume"}`)
					cursor, err := OpenRecordCursor(path, backends.Default("", nil))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := cursor.Close(); err != nil {
							t.Error(err)
						}
					})
					var failure error
					var ids []uint64
					for record, err := range cursor.Read(t.Context()) {
						if err != nil {
							failure = err
							continue
						}
						ids = append(ids, record.ID)
					}
					if failure == nil || !slices.Equal(ids, []uint64{1, 2}) {
						t.Fatalf("records=%v, error=%v", ids, failure)
					}
					for range 2 {
						count := 0
						for record, err := range cursor.Read(t.Context()) {
							count++
							if !errors.Is(err, failure) || record.ID != 0 || len(record.Messages) != 0 {
								t.Fatalf("continued poisoned decoder: record=%+v, error=%v, want %v", record, err, failure)
							}
						}
						if count != 1 {
							t.Fatalf("terminal read yielded %d errors", count)
						}
					}
				})
			}
		})
		t.Run("valid/cancellation and plain EOF retain correlation", func(t *testing.T) {
			t.Parallel()
			path := writePhysicalTestLog(t, false,
				`{"t":"caic_meta","version":3,"harness":"opencode","prompt":"test"}`,
				`{"t":"input","ts":1.000,"msg":{"jsonrpc":"2.0","id":17,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"test"}]}}}`)
			cursor, err := OpenRecordCursor(path, backends.Default("", nil))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			var cancellation error
			for _, err := range cursor.Read(ctx) {
				cancellation = err
			}
			if !errors.Is(cancellation, context.Canceled) {
				t.Fatalf("canceled read = %v", cancellation)
			}
			var ids []uint64
			for record, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, record.ID)
			}
			if !slices.Equal(ids, []uint64{1, 2}) {
				t.Fatalf("records after cancellation = %v", ids)
			}
			for _, err := range cursor.Read(t.Context()) {
				t.Fatalf("EOF poll produced a record: %v", err)
			}
			writer, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := writer.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := writer.WriteString(`{"t":"agent","ts":2.000,"msg":{"jsonrpc":"2.0","id":17,"error":{"code":-32000,"message":"rejected"}}}` + "\n"); err != nil {
				t.Fatal(err)
			}
			var results []string
			for record, err := range cursor.Read(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				if record.ID != 3 {
					t.Fatalf("appended physical ID = %d", record.ID)
				}
				for _, message := range record.Messages {
					if result, ok := message.Message.(*agent.ResultMessage); ok {
						results = append(results, result.Result)
					}
				}
			}
			if !slices.Equal(results, []string{"rejected"}) {
				t.Fatalf("lost ordered input state across polling: %v", results)
			}
		})
	})
}

func TestOpenRecordCursor(t *testing.T) {
	t.Parallel()
	const header = `{"t":"caic_meta","version":3,"harness":"claude","prompt":"test"}`
	const text = `{"t":"text","text":"hello"}`
	t.Run("path replaced during resolution retains validated descriptor", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("Windows does not permit replacement of this open log descriptor")
		}
		path := writePhysicalTestLog(t, false, header, text)
		replacement := writePhysicalTestLog(t, false, `{"t":"text","text":"foreign"}`)
		resolver := newTestParserResolver(func(harness.Name) (agent.WireFormat, error) {
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
			return &agenttest.Wire{Output: func([]byte) ([]agent.Message, error) { return nil, nil }}, nil
		})
		cursor, err := OpenRecordCursor(path, resolver)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := cursor.Close(); err != nil {
				t.Error(err)
			}
		})
		var count int
		for record, err := range cursor.Read(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			count++
			if record.ID == 2 {
				if len(record.Messages) != 1 {
					t.Fatal("missing retained descriptor text")
				}
				text, ok := record.Messages[0].Message.(*agent.TextMessage)
				if !ok || text.Text != "hello" {
					t.Fatal("read unvalidated replacement descriptor")
				}
			}
		}
		if count != 2 {
			t.Fatalf("read %d records from replaced path", count)
		}
	})
	t.Run("missing leading header", func(t *testing.T) {
		t.Parallel()
		path := writePhysicalTestLog(t, false, text)
		if cursor, err := OpenRecordCursor(path, backends.Default("", nil)); err == nil {
			_ = cursor.Close()
			t.Fatal("accepted missing authority")
		}
	})
	t.Run("changed during wire resolution", func(t *testing.T) {
		t.Parallel()
		path := writePhysicalTestLog(t, false, header, text)
		resolver := newTestParserResolver(func(harness.Name) (agent.WireFormat, error) {
			if err := os.WriteFile(path, []byte(text+"\n"+strings.Repeat(" ", len(header))), 0o600); err != nil {
				t.Fatal(err)
			}
			return &agenttest.Wire{Output: func([]byte) ([]agent.Message, error) { return nil, nil }}, nil
		})
		cursor, err := OpenRecordCursor(path, resolver)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := cursor.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, err := range cursor.Read(t.Context()) {
			if err != nil {
				return
			}
		}
		t.Fatal("accepted changed leading authority")
	})
	t.Run("matching segment", func(t *testing.T) {
		t.Parallel()
		cursor, err := OpenRecordCursor(writePhysicalTestLog(t, false, header, text, header), backends.Default("", nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := cursor.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, err := range cursor.Read(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, segment := range []string{`{"t":"caic_meta","version":3,"harness":"codex"}`, `{"t":"caic_meta","version":3,"harness":"claude"}`} {
		t.Run(segment, func(t *testing.T) {
			t.Parallel()
			cursor, err := OpenRecordCursor(writePhysicalTestLog(t, false, header, text, segment), backends.Default("", nil))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			var readErr error
			for _, err := range cursor.Read(t.Context()) {
				if err != nil {
					readErr = err
				}
			}
			if readErr == nil {
				t.Fatalf("segment authority result: %v", readErr)
			}
		})
	}
}
