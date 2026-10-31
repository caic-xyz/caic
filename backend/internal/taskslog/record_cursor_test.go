// Tests semantic control preservation and descriptor-owned authority in incremental physical history.

package taskslog

import (
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestRecordCursorControls(t *testing.T) {
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
}

func TestRecordCursorAuthority(t *testing.T) {
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
		resolver := newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
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
		resolver := newTestWireResolver(func(harness.Name) (agent.WireFormat, error) {
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
	for _, segment := range []string{header, `{"t":"caic_meta","version":3,"harness":"codex"}`, `{"t":"caic_meta","version":3,"harness":"claude"}`} {
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
			if (readErr == nil) != (segment == header) {
				t.Fatalf("segment authority result: %v", readErr)
			}
		})
	}
}

func TestRecordCursorCompressedEOF(t *testing.T) {
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
	stream, ok := cursor.compressed.(*zstdReadCloser)
	if !ok {
		t.Fatal("compressed cursor has no streaming decoder")
	}
	file := stream.file
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
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("completed compressed descriptor retained: %v", err)
	}
	if cursor.scanner != nil || cursor.parser != nil || cursor.compressed != nil {
		t.Fatal("completed compressed parsing buffers retained")
	}
	for _, err := range cursor.Read(t.Context()) {
		t.Fatalf("settled read produced another record: %v", err)
	}
}
