// Tests for the agent package, covering wire format handling and session lifecycle.

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testWire implements WireFormat for testing.
type testWire struct{}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type testLogSink struct {
	bytes.Buffer

	Version LogVersion
}

func (s *testLogSink) LogVersion() LogVersion { return s.Version }

func (s *testLogSink) AppendNative(data []byte) error {
	_, err := s.Write(data)
	return err
}

func (s *testLogSink) AppendMessage(m Message) error {
	data, err := MarshalLogMessage(s.LogVersion(), m)
	if err != nil {
		return err
	}
	return s.AppendNative(append(data, '\n'))
}

func (*testLogSink) Close() error { return nil }

func (testWire) WritePrompt(w io.Writer, p Prompt, log LogSink) error {
	msg := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{Type: "user"}
	msg.Message.Role = "user"
	msg.Message.Content = p.Text
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return err
	}
	return AppendNativeRecord(log, LogVersionV1, data)
}

// testParseFn is a minimal Claude-format parser for testing. It avoids
// importing the claude sub-package (which would create an import cycle).
func testParseFn(line []byte) ([]Message, error) {
	var env struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	switch env.Type {
	case "system":
		if env.Subtype == "init" {
			var w struct {
				SessionID string   `json:"session_id"`
				Cwd       string   `json:"cwd"`
				Tools     []string `json:"tools"`
				Model     string   `json:"model"`
				Version   string   `json:"claude_code_version"`
			}
			if err := json.Unmarshal(line, &w); err != nil {
				return nil, err
			}
			return []Message{&InitMessage{
				SessionID: w.SessionID, Cwd: w.Cwd, Tools: w.Tools,
				Model: w.Model, Version: w.Version,
			}}, nil
		}
		var m SystemMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []Message{&m}, nil
	case "assistant":
		var w struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		var msgs []Message
		for _, c := range w.Message.Content {
			if c.Type == "text" && c.Text != "" {
				msgs = append(msgs, &TextMessage{Text: c.Text})
			}
		}
		if len(msgs) == 0 {
			msgs = append(msgs, &RawMessage{MessageType: "assistant", Raw: append([]byte(nil), line...)})
		}
		return msgs, nil
	case "result":
		var m ResultMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []Message{&m}, nil
	case "stream_event":
		var w struct {
			Event struct {
				Type  string `json:"type"`
				Delta *struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			} `json:"event"`
		}
		if err := json.Unmarshal(line, &w); err != nil {
			return nil, err
		}
		if w.Event.Type == "content_block_delta" && w.Event.Delta != nil && w.Event.Delta.Type == "text_delta" && w.Event.Delta.Text != "" {
			return []Message{&TextDeltaMessage{Text: w.Event.Delta.Text}}, nil
		}
		return []Message{&RawMessage{MessageType: "stream_event", Raw: append([]byte(nil), line...)}}, nil
	default:
		return []Message{&RawMessage{MessageType: env.Type, Raw: append([]byte(nil), line...)}}, nil
	}
}

func (testWire) ParseMessage(line []byte) ([]Message, error) {
	return testParseFn(line)
}

func TestExitMessage(t *testing.T) {
	t.Parallel()
	t.Run("valid_uses_relay_error", func(t *testing.T) {
		t.Parallel()
		exit := &ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}
		if got := exit.ExitError(); got != "Unknown option: --approve" {
			t.Errorf("ExitError = %q, want relay stderr", got)
		}
	})
	t.Run("valid_falls_back_to_exit_code", func(t *testing.T) {
		t.Parallel()
		exit := &ExitMessage{ExitCode: 2}
		if got := exit.ExitError(); got != "agent subprocess exited with code 2" {
			t.Errorf("ExitError = %q, want exit-code diagnostic", got)
		}
	})
}

func TestSession(t *testing.T) {
	t.Parallel()
	t.Run("Lifecycle", func(t *testing.T) {
		t.Parallel()
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()

		s := &Session{
			Conn: NewConn(t.Context(), testLogger(), stdinW, DiscardLogSink{Version: LogVersionV1}, testWire{}),
			done: make(chan struct{}),
		}

		msgCh := make(chan Message, 16)

		go func() {
			defer close(s.done)
			if parseErr := DefaultReadMessages(t.Context(), testLogger(), stdoutR, func(m ParsedMessage) { msgCh <- m.Message }, DiscardLogSink{Version: LogVersionV1}, LogVersionV1, testParseFn); parseErr != nil {
				s.err = parseErr
			}
		}()

		stdinBuf := make(chan string, 1)
		go func() {
			data, _ := io.ReadAll(stdinR)
			stdinBuf <- string(data)
		}()

		if err := s.SendPrompt(Prompt{Text: "test prompt"}); err != nil {
			t.Fatal(err)
		}

		resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"ok","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}` + "\n"
		if _, err := stdoutW.Write([]byte(resultLine)); err != nil {
			t.Fatal(err)
		}

		select {
		case <-s.done:
			t.Fatal("session closed prematurely after result")
		case <-time.After(50 * time.Millisecond):
		}

		if err := s.SendPrompt(Prompt{Text: "follow-up"}); err != nil {
			t.Fatal(err)
		}

		_ = s.Close()

		got := <-stdinBuf
		if !strings.Contains(got, `"content":"test prompt"`) {
			t.Errorf("missing first prompt in stdin: %s", got)
		}
		if !strings.Contains(got, `"content":"follow-up"`) {
			t.Errorf("missing follow-up in stdin: %s", got)
		}

		_ = stdoutW.Close()

		if err := s.Wait(); err != nil {
			t.Fatal(err)
		}

		close(msgCh)
		var count int
		var gotResult *ResultMessage
		for m := range msgCh {
			count++
			if rm, ok := m.(*ResultMessage); ok {
				gotResult = rm
			}
		}
		if count != 1 {
			t.Errorf("message count = %d, want 1", count)
		}
		if gotResult == nil {
			t.Fatal("expected ResultMessage in msgCh, got none")
		}
		if gotResult.Result != "ok" {
			t.Errorf("result = %q, want %q", gotResult.Result, "ok")
		}
	})
	t.Run("PlainTextWritePrompt", func(t *testing.T) {
		t.Parallel()
		var stdin bytes.Buffer
		log := &testLogSink{Version: LogVersionV2}
		if err := PlainTextWritePrompt(&stdin, Prompt{Text: "hello"}, log); err != nil {
			t.Fatal(err)
		}
		if got := stdin.String(); got != "hello\n" {
			t.Errorf("stdin = %q, want hello\\n", got)
		}
		parser, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) { return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		record, err := parser.ParseRecord(bytes.TrimSuffix(log.Bytes(), []byte{'\n'}))
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Messages) != 1 {
			t.Fatalf("messages = %d, want 1", len(record.Messages))
		}
		input, ok := record.Messages[0].Message.(*UserInputMessage)
		if !ok || input.Text != "hello" {
			t.Errorf("message = %#v, want user input hello", record.Messages[0].Message)
		}
	})

	t.Run("SendRaw", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			stdinR, stdinW := io.Pipe()
			logBuf := &testLogSink{Version: LogVersionV2}
			s := &Session{
				Conn: NewConn(t.Context(), testLogger(), stdinW, logBuf, testWire{}),
				done: make(chan struct{}),
			}

			stdinBuf := make(chan string, 1)
			go func() {
				data, _ := io.ReadAll(stdinR)
				stdinBuf <- string(data)
			}()

			payload := []byte(`{"type":"update_environment_variables","variables":{"FOO":"bar"}}` + "\n")
			if err := s.SendRaw(payload); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()

			got := <-stdinBuf
			if !strings.Contains(got, `"FOO":"bar"`) {
				t.Errorf("stdin missing payload: %s", got)
			}
			var record struct {
				Type string          `json:"t"`
				Msg  json.RawMessage `json:"msg"`
			}
			if err := json.Unmarshal(logBuf.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record.Type != "agent" || !bytes.Equal(record.Msg, bytes.TrimSuffix(payload, []byte{'\n'})) {
				t.Errorf("log record = %#v, want canonical agent envelope for %s", record, payload)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			stdinR, stdinW := io.Pipe()
			_ = stdinR.Close()
			s := &Session{
				Conn: NewConn(t.Context(), testLogger(), stdinW, DiscardLogSink{Version: LogVersionV1}, testWire{}),
				done: make(chan struct{}),
			}
			if err := s.SendRaw([]byte("data\n")); err == nil {
				t.Error("expected error writing to closed pipe")
			}
		})
	})
	t.Run("CloseIdempotent", func(t *testing.T) {
		t.Parallel()
		stdinR, stdinW := io.Pipe()
		go func() { _, _ = io.Copy(io.Discard, stdinR) }()
		s := &Session{
			Conn: NewConn(t.Context(), testLogger(), stdinW, DiscardLogSink{Version: LogVersionV1}, testWire{}),
			done: make(chan struct{}),
		}
		_ = s.Close()
	})
	t.Run("SignalKillNotError", func(t *testing.T) {
		t.Parallel()
		cmd := exec.CommandContext(t.Context(), "sleep", "60")
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

		var logBuf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logBuf, nil))

		msgCh := make(chan ParsedMessage, 16)
		s := NewSession(t.Context(), cmd, NewConn(t.Context(), log, stdin, DiscardLogSink{Version: LogVersionV1}, testWire{}), stdout, msgCh, log)

		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}

		err = s.Wait()
		if err == nil {
			t.Fatal("expected error from killed process")
		}
		if !strings.Contains(err.Error(), "signal: killed") {
			t.Fatalf("expected 'signal: killed' in error, got: %v", err)
		}

		logOutput := logBuf.String()
		if strings.Contains(logOutput, "level=ERROR") {
			t.Errorf("killed process should not produce ERROR log:\n%s", logOutput)
		}
	})
}

func TestAppendNativeRecord(t *testing.T) {
	t.Parallel()

	t.Run("V2TerminatesWithLF", func(t *testing.T) {
		t.Parallel()
		log := &testLogSink{Version: LogVersionV2}
		if err := AppendNativeRecord(log, LogVersionV2, []byte(`{"type":"response"}`)); err != nil {
			t.Fatal(err)
		}
		got := log.Bytes()
		if got[len(got)-1] != '\n' || bytes.Contains(got, []byte(`\\n`)) {
			t.Fatalf("v2 record = %q, want one real LF terminator", got)
		}
		if !bytes.HasPrefix(got, []byte(`{"t":"agent","ts":`)) || !bytes.HasSuffix(got, append([]byte(`"msg":{"type":"response"}}`), '\n')) {
			t.Fatalf("v2 record = %q, want canonical agent envelope", got)
		}
	})
}

func TestWriteMetaSession(t *testing.T) {
	t.Parallel()

	t.Run("VersionWithoutSession", func(t *testing.T) {
		t.Parallel()
		buf := &testLogSink{Version: LogVersionV1}
		if err := WriteMetaSession(buf, &InitMessage{Model: "m", Version: "1.2.3"}); err != nil {
			t.Fatal(err)
		}
		var got MetaSessionMessage
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
			t.Fatal(err)
		}
		if got.MessageType != "caic_session" || got.SessionID != "" || got.Model != "m" || got.AgentVersion != "1.2.3" {
			t.Fatalf("MetaSessionMessage = %+v", got)
		}
	})

	t.Run("EmptyIgnored", func(t *testing.T) {
		t.Parallel()
		buf := &testLogSink{Version: LogVersionV2}
		if err := WriteMetaSession(buf, &InitMessage{}); err != nil {
			t.Fatal(err)
		}
		if buf.Len() != 0 {
			t.Fatalf("buffer length = %d, want 0", buf.Len())
		}
	})
}

func TestReadMessages(t *testing.T) {
	t.Parallel()
	t.Run("FullStream", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			`{"type":"system","subtype":"init","cwd":"/","session_id":"s","tools":[],"model":"m","claude_code_version":"1","uuid":"u"}`,
			`{"type":"assistant","message":{"model":"m","id":"i","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{}},"session_id":"s","uuid":"u"}`,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"hi","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}`,
		}
		input := strings.Join(lines, "\n") + "\n"

		ch := make(chan Message, 16)
		if err := DefaultReadMessages(t.Context(), testLogger(), strings.NewReader(input), func(m ParsedMessage) { ch <- m.Message }, DiscardLogSink{Version: LogVersionV1}, LogVersionV1, testParseFn); err != nil {
			t.Fatal(err)
		}
		close(ch)

		// init(1) + text(1) + result(1) = 3
		var count int
		for range ch {
			count++
		}
		if count != 3 {
			t.Errorf("message count = %d, want 3", count)
		}
	})
	t.Run("StreamWithPartialMessages", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			`{"type":"system","subtype":"init","cwd":"/","session_id":"s","tools":[],"model":"m","claude_code_version":"1","uuid":"u"}`,
			`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}}`,
			`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}}`,
			`{"type":"assistant","message":{"model":"m","id":"i","role":"assistant","content":[{"type":"text","text":"hello"}],"usage":{}},"session_id":"s","uuid":"u"}`,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"hello","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}`,
		}
		input := strings.Join(lines, "\n") + "\n"

		ch := make(chan Message, 16)
		if err := DefaultReadMessages(t.Context(), testLogger(), strings.NewReader(input), func(m ParsedMessage) { ch <- m.Message }, DiscardLogSink{Version: LogVersionV1}, LogVersionV1, testParseFn); err != nil {
			t.Fatal(err)
		}
		close(ch)

		var msgs []Message
		for m := range ch {
			msgs = append(msgs, m)
		}
		// init(1) + 2 deltas + text(1) + result(1) = 5
		if len(msgs) != 5 {
			t.Errorf("message count = %d, want 5", len(msgs))
		}
		if _, ok := msgs[1].(*TextDeltaMessage); !ok {
			t.Errorf("msgs[1] is %T, want *TextDeltaMessage", msgs[1])
		}
		if _, ok := msgs[2].(*TextDeltaMessage); !ok {
			t.Errorf("msgs[2] is %T, want *TextDeltaMessage", msgs[2])
		}
	})
	t.Run("LogWriter", func(t *testing.T) {
		t.Parallel()
		lines := []string{
			`{"type":"system","subtype":"init","cwd":"/","session_id":"s","tools":[],"model":"m","claude_code_version":"1","uuid":"u"}`,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"ok","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}`,
		}
		input := strings.Join(lines, "\n") + "\n"

		buf := &testLogSink{Version: LogVersionV1}
		if err := DefaultReadMessages(t.Context(), testLogger(), strings.NewReader(input), func(ParsedMessage) {}, buf, LogVersionV1, testParseFn); err != nil {
			t.Fatal(err)
		}

		logged := buf.String()
		for _, line := range lines {
			if !strings.Contains(logged, line+"\n") {
				t.Errorf("log missing line: %s", line)
			}
		}
	})
	t.Run("rejects unterminated physical records", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			version LogVersion
			input   string
		}{
			{name: "v1", version: LogVersionV1, input: `{"type":"system","subtype":"init"}`},
			{name: "v2", version: LogVersionV2, input: `{"t":"agent","ts":1.000,"msg":{}}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				stdinR, stdinW := io.Pipe()
				t.Cleanup(func() { _ = stdinR.Close() })
				var log bytes.Buffer
				got := make(chan ParsedMessage, 1)
				conn := NewConn(t.Context(), testLogger(), stdinW, &testLogSink{Version: tc.version}, testWire{})
				err := conn.ReadMessages(strings.NewReader(tc.input), got)
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("ReadMessages error = %v, want unexpected EOF", err)
				}
				if log.Len() != 0 || len(got) != 0 {
					t.Fatalf("persisted=%q dispatched=%d", log.String(), len(got))
				}
			})
		}
	})
	t.Run("v2 structural corruption is not persisted", func(t *testing.T) {
		t.Parallel()
		var log bytes.Buffer
		var got []ParsedMessage
		err := DefaultReadMessages(t.Context(), testLogger(), strings.NewReader(`{"t":"agent","time":1.000,"msg":{}}`+"\n"), func(msg ParsedMessage) {
			got = append(got, msg)
		}, &testLogSink{Version: LogVersionV2}, LogVersionV2, func([]byte) ([]Message, error) {
			return []Message{&TextMessage{}}, nil
		})
		if err == nil || log.Len() != 0 || len(got) != 0 {
			t.Fatalf("err=%v persisted=%d dispatched=%d", err, log.Len(), len(got))
		}
	})
}

func TestRelayRecordReader(t *testing.T) {
	t.Parallel()
	t.Run("rejects unterminated record without persistence", func(t *testing.T) {
		t.Parallel()
		var log bytes.Buffer
		reader, err := NewRelayRecordReader(strings.NewReader(`{"type":"system","subtype":"init"}`), LogVersionV1, &testLogSink{Version: LogVersionV1})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := reader.ReadRecord(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("ReadRecord error = %v, want unexpected EOF", err)
		}
		if log.Len() != 0 {
			t.Fatalf("persisted truncated record = %q", log.String())
		}
	})
}

func TestReadRelayTailRecords(t *testing.T) {
	t.Parallel()
	const complete = `{"type":"system","subtype":"init"}`
	full := "old\npartial" + "\n" + complete + "\n"
	tail := "rtial\n" + complete + "\n"
	start := int64(len(full) - len(tail))
	parser, err := NewLogRecordParser(LogVersionV1, func([]byte) ([]Message, error) {
		return []Message{&TextMessage{Text: "tail"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs, offset, err := readRelayTailRecords(strings.NewReader(tail), parser, start, true, "ctr")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %#v", msgs)
	}
	msg, ok := msgs[0].Message.(*TextMessage)
	if !ok || msg.Text != "tail" {
		t.Fatalf("messages = %#v", msgs)
	}
	if offset != int64(len(full)) {
		t.Fatalf("offset = %d, want snapshot boundary %d", offset, len(full))
	}
	if got := full[offset:]; got != "" {
		t.Fatalf("attach remainder before append = %q, want empty", got)
	}
	appended := `{"type":"system","subtype":"status"}` + "\n"
	if got := (full + appended)[offset:]; got != appended {
		t.Fatalf("attach remainder after append = %q, want only appended %q", got, appended)
	}
	if !tailNeedsLeadingSkip(start, 'x') || tailNeedsLeadingSkip(start, '\n') || tailNeedsLeadingSkip(0, 'x') {
		t.Fatal("tail boundary classification is wrong")
	}
	t.Run("leading empty record is not a partial fragment", func(t *testing.T) {
		t.Parallel()
		parser, err := NewLogRecordParser(LogVersionV1, func([]byte) ([]Message, error) {
			return []Message{&TextMessage{Text: "complete"}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		msgs, offset, err := readRelayTailRecords(strings.NewReader("\n"+complete+"\n"), parser, 10, false, "ctr")
		if err != nil || len(msgs) != 1 || offset != 10+int64(len("\n"+complete+"\n")) {
			t.Fatalf("leading empty tail = msgs:%#v offset:%d err:%v", msgs, offset, err)
		}
	})
	t.Run("start at prior record LF retains following complete record", func(t *testing.T) {
		t.Parallel()
		parser, err := NewLogRecordParser(LogVersionV1, func([]byte) ([]Message, error) {
			return []Message{&TextMessage{Text: "complete"}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tail := "\n" + complete + "\n"
		msgs, offset, err := readRelayTailRecords(strings.NewReader(tail), parser, 10, true, "ctr")
		if err != nil || len(msgs) != 1 || offset != 10+int64(len(tail)) {
			t.Fatalf("LF-boundary tail = msgs:%#v offset:%d err:%v", msgs, offset, err)
		}
	})
	t.Run("trailing fragment remains for attach", func(t *testing.T) {
		t.Parallel()
		parser, err := NewLogRecordParser(LogVersionV1, func([]byte) ([]Message, error) {
			return []Message{&TextMessage{Text: "complete"}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		completeRecord := complete + "\n"
		msgs, offset, err := readRelayTailRecords(strings.NewReader(completeRecord+`{"partial":true}`), parser, 0, false, "ctr")
		if err != nil || len(msgs) != 1 || offset != int64(len(completeRecord)) {
			t.Fatalf("trailing fragment = msgs:%#v offset:%d err:%v", msgs, offset, err)
		}
	})
	t.Run("v2 corruption and oversized records do not persist", func(t *testing.T) {
		t.Parallel()
		for _, input := range []string{
			`{"t":"agent","time":1.000,"msg":{}}` + "\n",
			`{"t":"agent","ts":1.000,"msg":"` + strings.Repeat("x", maxNDJSONRecordLen) + `"}` + "\n",
		} {
			var log bytes.Buffer
			reader, err := NewRelayRecordReader(strings.NewReader(input), LogVersionV2, &testLogSink{Version: LogVersionV2})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := reader.ReadRecord(); err == nil {
				t.Fatal("ReadRecord error = nil")
			}
			if log.Len() != 0 {
				t.Fatalf("persisted invalid v2 = %d bytes", log.Len())
			}
		}
	})
}

func TestYieldMessages(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"system","subtype":"init","cwd":"/","session_id":"s","tools":[],"model":"m","claude_code_version":"1","uuid":"u"}`,
		`{"type":"assistant","message":{"model":"m","id":"i","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{}},"session_id":"s","uuid":"u"}`,
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"hi","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}`,
	}
	collect := func(content string, skipFirst bool) ([]Message, error) {
		var msgs []Message
		parser, err := NewLogRecordParser(LogVersionV1, testParseFn)
		if err != nil {
			return nil, err
		}
		for m, e := range yieldMessages(strings.NewReader(content), parser, skipFirst, "ctr") {
			if e != nil {
				return msgs, e
			}
			msgs = append(msgs, m.Message)
		}
		return msgs, nil
	}

	t.Run("Full", func(t *testing.T) {
		t.Parallel()
		msgs, err := collect(strings.Join(lines, "\n")+"\n", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 3 {
			t.Errorf("message count = %d, want 3", len(msgs))
		}
	})

	t.Run("SkipFirstPartialLine", func(t *testing.T) {
		t.Parallel()
		// Simulate a tail that cut the first record mid-line: the partial
		// fragment must be dropped, the remaining valid records kept.
		msgs, err := collect(`ssage":{"model"...truncated`+"\n"+strings.Join(lines[1:], "\n")+"\n", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Errorf("message count = %d, want 2", len(msgs))
		}
	})

	t.Run("SkipFirstPriorRecordLF", func(t *testing.T) {
		t.Parallel()
		// A tail can start at the LF ending the prior record. Consume that
		// empty physical record as the partial fragment, retaining every
		// following complete record.
		msgs, err := collect("\n"+strings.Join(lines, "\n")+"\n", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != len(lines) {
			t.Errorf("message count = %d, want %d", len(msgs), len(lines))
		}
	})
}

func TestLogRecordParser(t *testing.T) {
	t.Parallel()

	t.Run("Constructor", func(t *testing.T) {
		t.Parallel()
		if _, err := NewLogRecordParser(LogVersion(3), testParseFn); err == nil || !strings.Contains(err.Error(), "unsupported log version 3") {
			t.Fatalf("unknown version error = %v", err)
		}
		if _, err := NewLogRecordParser(LogVersionV1, nil); err == nil || !strings.Contains(err.Error(), "native message parser is nil") {
			t.Fatalf("nil parser error = %v", err)
		}
	})

	t.Run("ControlVocabulary", func(t *testing.T) {
		t.Parallel()
		pairs := []struct {
			name string
			v1   string
			v2   string
		}{
			{
				name: "diff_stat",
				v1:   `{"type":"caic_diff_stat","diff_stat":[{"path":"a.go","added":2,"deleted":1}],"ts":12.5}`,
				v2:   `{"t":"diff_stat","diff_stat":[{"path":"a.go","added":2,"deleted":1}],"ts":12.5}`,
			},
			{
				name: "exit",
				v1:   `{"type":"caic_exit","exit_code":2,"cmd":["agent"],"error":"failed","ts":13}`,
				v2:   `{"t":"exit","exit_code":2,"cmd":["agent"],"error":"failed","ts":13}`,
			},
			{
				name: "stripped_env",
				v1:   `{"type":"caic_stripped_env","variables":{"TOKEN":"secret"}}`,
				v2:   `{"t":"stripped_env","variables":{"TOKEN":"secret"}}`,
			},
			{
				name: "session",
				v1:   `{"type":"caic_session","session_id":"s1","model":"m1","agent_version":"1.2.3"}`,
				v2:   `{"t":"session","session_id":"s1","model":"m1","agent_version":"1.2.3"}`,
			},
			{
				name: "pr",
				v1:   `{"type":"caic_pr","forge_owner":"o","forge_repo":"r","forge_pr":7}`,
				v2:   `{"t":"pr","forge_owner":"o","forge_repo":"r","forge_pr":7}`,
			},
			{
				name: "result",
				v1:   `{"type":"caic_result","state":"purged","title":"done","cost_usd":1.5,"num_turns":2}`,
				v2:   `{"t":"result","state":"purged","title":"done","cost_usd":1.5,"num_turns":2}`,
			},
			{
				name: "pending_user_action",
				v1:   `{"type":"caic_pending_user_action","action":{"kind":"ask_user_question","request_id":"r1","tool_use_id":"t1"}}`,
				v2:   `{"t":"pending_user_action","action":{"kind":"ask_user_question","request_id":"r1","tool_use_id":"t1"}}`,
			},
			{
				name: "provisioning_log",
				v1:   `{"type":"caic_log","line":"starting"}`,
				v2:   `{"t":"log","line":"starting"}`,
			},
		}
		for _, tc := range pairs {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				v1, err := NewLogRecordParser(LogVersionV1, testParseFn)
				if err != nil {
					t.Fatal(err)
				}
				v2, err := NewLogRecordParser(LogVersionV2, testParseFn)
				if err != nil {
					t.Fatal(err)
				}
				gotV1, err := v1.ParseRecord([]byte(tc.v1))
				if err != nil {
					t.Fatal(err)
				}
				gotV2, err := v2.ParseRecord([]byte(tc.v2))
				if err != nil {
					t.Fatal(err)
				}
				if !gotV1.Control || !gotV2.Control {
					t.Fatalf("control classification = %t, %t; want true", gotV1.Control, gotV2.Control)
				}
				if !reflect.DeepEqual(gotV1.Messages, gotV2.Messages) {
					t.Fatalf("v1 = %#v, v2 = %#v", gotV1.Messages, gotV2.Messages)
				}
				if len(gotV1.Messages) != 1 || !gotV1.Messages[0].ProducerTime.IsZero() || !gotV2.Messages[0].ProducerTime.IsZero() {
					t.Fatalf("control producer times = %v, %v; want zero", gotV1.Messages, gotV2.Messages)
				}
			})
		}
	})

	t.Run("ControlClassificationOnErrors", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			version LogVersion
			line    string
		}{
			{name: "v1", version: LogVersionV1, line: `{"type":"caic_meta","version":"bad"}`},
			{name: "v2", version: LogVersionV2, line: `{"t":"model_info","unexpected":true}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				p, err := NewLogRecordParser(tc.version, testParseFn)
				if err != nil {
					t.Fatal(err)
				}
				record, err := p.ParseRecord([]byte(tc.line))
				if err == nil || !record.Control {
					t.Fatalf("record = %#v, err = %v; want classified control error", record, err)
				}
			})
		}
	})

	t.Run("HeadersAndLegacySession", func(t *testing.T) {
		t.Parallel()
		for _, version := range []LogVersion{LogVersionV1, LogVersionV2} {
			p, err := NewLogRecordParser(version, testParseFn)
			if err != nil {
				t.Fatal(err)
			}
			discriminator := "type"
			if version == LogVersionV2 {
				discriminator = "t"
			}
			line := fmt.Sprintf(`{%q:"caic_meta","version":%d,"prompt":"p","repos":[],"harness":"claude"}`, discriminator, version)
			record, err := p.ParseRecord([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			if !record.Control || len(record.Messages) != 1 {
				t.Fatalf("version %d record = %#v", version, record)
			}
			meta, ok := record.Messages[0].Message.(*MetaMessage)
			if !ok || meta.Version != int(version) || !record.Messages[0].ProducerTime.IsZero() {
				t.Fatalf("version %d meta = %#v", version, record.Messages[0])
			}
		}

		v2, err := NewLogRecordParser(LogVersionV2, testParseFn)
		if err != nil {
			t.Fatal(err)
		}
		wrongVersion := []byte(`{"t":"caic_meta","version":1,"prompt":"p","repos":[],"harness":"claude"}`)
		if _, err := v2.ParseRecord(wrongVersion); err == nil || !strings.Contains(err.Error(), "does not match parser version 2") {
			t.Fatalf("mismatched header error = %v", err)
		}

		p, err := NewLogRecordParser(LogVersionV1, testParseFn)
		if err != nil {
			t.Fatal(err)
		}
		record, err := p.ParseRecord([]byte(`{"type":"caic_init","session_id":"legacy","model":"m","version":"0.9"}`))
		if err != nil {
			t.Fatal(err)
		}
		want := []ParsedMessage{{Message: &InitMessage{SessionID: "legacy", Model: "m", Version: "0.9"}}}
		if !record.Control || !reflect.DeepEqual(record.Messages, want) {
			t.Fatalf("legacy record = %#v, want messages %#v", record, want)
		}
	})

	t.Run("ContextCleared", func(t *testing.T) {
		t.Parallel()
		v1, err := NewLogRecordParser(LogVersionV1, testParseFn)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := NewLogRecordParser(LogVersionV2, testParseFn)
		if err != nil {
			t.Fatal(err)
		}
		gotV1, err := v1.ParseRecord([]byte(`{"type":"system","subtype":"context_cleared"}`))
		if err != nil {
			t.Fatal(err)
		}
		gotV2, err := v2.ParseRecord([]byte(`{"t":"context_cleared"}`))
		if err != nil {
			t.Fatal(err)
		}
		if gotV1.Control || !gotV2.Control || !reflect.DeepEqual(gotV1.Messages, gotV2.Messages) {
			t.Fatalf("v1 = %#v, v2 = %#v", gotV1, gotV2)
		}
	})

	t.Run("V1NativeFallback", func(t *testing.T) {
		t.Parallel()
		var got []byte
		p, err := NewLogRecordParser(LogVersionV1, func(line []byte) ([]Message, error) {
			got = append([]byte(nil), line...)
			return []Message{&RawMessage{MessageType: "native", Raw: append([]byte(nil), line...)}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range [][]byte{
			[]byte(`{"type":"caic_future","value":1}`),
			[]byte(`not-json`),
		} {
			record, err := p.ParseRecord(line)
			if err != nil {
				t.Fatal(err)
			}
			if record.Control || !bytes.Equal(got, line) || len(record.Messages) != 1 {
				t.Fatalf("native input = %s, record = %#v", got, record)
			}
		}
	})

	t.Run("V2AgentPayloads", func(t *testing.T) {
		t.Parallel()
		var got [][]byte
		p, err := NewLogRecordParser(LogVersionV2, func(line []byte) ([]Message, error) {
			got = append(got, append([]byte(nil), line...))
			return []Message{&RawMessage{MessageType: "native", Raw: append([]byte(nil), line...)}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		payloads := []string{`{"type":"native","value":1}`, `42`, `true`, `"text"`}
		for _, payload := range payloads {
			line := fmt.Sprintf(`{"t":"agent","ts":123.500,"msg":%s}`, payload)
			record, err := p.ParseRecord([]byte(line))
			if err != nil {
				t.Fatalf("payload %s: %v", payload, err)
			}
			if record.Control || len(record.Messages) != 1 {
				t.Fatalf("payload %s record = %#v", payload, record)
			}
		}
		if len(got) != len(payloads) {
			t.Fatalf("native calls = %d, want %d", len(got), len(payloads))
		}
		for i := range payloads {
			if string(got[i]) != payloads[i] {
				t.Errorf("payload %d = %s, want %s", i, got[i], payloads[i])
			}
		}
	})

	t.Run("V2Rejections", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			line string
			want string
		}{
			{name: "bare_native", line: `{"type":"assistant"}`, want: "top-level type discriminator"},
			{name: "prefixed_v1", line: `{"type":"caic_diff_stat","diff_stat":[]}`, want: "top-level type discriminator"},
			{name: "unknown", line: `{"t":"future"}`, want: `unknown top-level t "future"`},
			{name: "missing_t", line: `{"value":1}`, want: "missing top-level t"},
			{name: "malformed_json", line: `{`, want: "decode control discriminator"},
			{name: "missing_ts", line: `{"t":"agent","msg":{"type":"assistant"}}`, want: "noncanonical envelope"},
			{name: "invalid_ts_type", line: `{"t":"agent","ts":"now","msg":{"type":"assistant"}}`, want: "invalid ts"},
			{name: "zero_ts", line: `{"t":"agent","ts":0.000,"msg":{"type":"assistant"}}`, want: "timestamp must be positive"},
			{name: "out_of_range_ts", line: `{"t":"agent","ts":9223372036854775807.000,"msg":{"type":"assistant"}}`, want: "outside the supported Unix time range"},
			{name: "missing_msg", line: `{"t":"agent","ts":1.000}`, want: "invalid timestamp delimiter"},
			{name: "null_msg", line: `{"t":"agent","ts":1.000,"msg":null}`, want: "null msg"},
			{name: "invalid_msg", line: `{"t":"agent","ts":1.000,"msg":{"x":}}`, want: "invalid or noncanonical msg envelope"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				p, err := NewLogRecordParser(LogVersionV2, testParseFn)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.ParseRecord([]byte(tc.line)); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want substring %q", err, tc.want)
				}
			})
		}
	})

	t.Run("V2RejectsInvalidUTF8", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			line []byte
		}{
			{
				name: "envelope",
				line: []byte{'{', '"', 't', '"', ':', '"', 'a', 'g', 0xff, 'e', 'n', 't', '"', '}'},
			},
			{
				name: "msg",
				line: append(append([]byte(`{"t":"agent","ts":1.000,"msg":{"text":"`), 0xff), []byte(`"}}`)...),
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				called := false
				p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
					called = true
					return nil, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.ParseRecord(tc.line); err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
					t.Fatalf("error = %v, want invalid UTF-8", err)
				}
				if called {
					t.Fatal("native parser called for invalid UTF-8")
				}
			})
		}

		legacy := []byte{'n', 'a', 't', 'i', 'v', 'e', 0xff}
		var got []byte
		v1, err := NewLogRecordParser(LogVersionV1, func(line []byte) ([]Message, error) {
			got = append([]byte(nil), line...)
			return []Message{&RawMessage{MessageType: "native", Raw: got}}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v1.ParseRecord(legacy); err != nil {
			t.Fatalf("v1 invalid UTF-8 fallback: %v", err)
		}
		if !bytes.Equal(got, legacy) {
			t.Fatalf("v1 native input = %v, want %v", got, legacy)
		}
	})

	t.Run("ModelContextSnapshot", func(t *testing.T) {
		t.Parallel()
		for _, version := range []LogVersion{LogVersionV1, LogVersionV2} {
			t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
				t.Parallel()
				p, err := NewLogRecordParser(version, func(line []byte) ([]Message, error) {
					var wire struct {
						ContextWindow int `json:"context_window"`
					}
					if err := json.Unmarshal(line, &wire); err != nil {
						return nil, err
					}
					return []Message{&UsageMessage{ContextWindow: wire.ContextWindow}}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				native := func(contextWindow int) string {
					payload := fmt.Sprintf(`{"type":"usage","context_window":%d}`, contextWindow)
					if version == LogVersionV1 {
						return payload
					}
					return fmt.Sprintf(`{"t":"agent","ts":10.000,"msg":%s}`, payload)
				}
				infoDiscriminator := "type"
				infoType := "caic_model_info"
				if version == LogVersionV2 {
					infoDiscriminator = "t"
					infoType = "model_info"
				}

				before, err := p.ParseRecord([]byte(native(0)))
				if err != nil {
					t.Fatal(err)
				}
				beforeUsage, ok := before.Messages[0].Message.(*UsageMessage)
				if !ok {
					t.Fatalf("pre-snapshot message = %T, want *UsageMessage", before.Messages[0].Message)
				}
				if beforeUsage.ContextWindow != 0 {
					t.Fatalf("pre-snapshot context = %d", beforeUsage.ContextWindow)
				}
				if record, err := p.ParseRecord(fmt.Appendf(nil, `{%q:%q,"context_window":1000000}`, infoDiscriminator, infoType)); err != nil || !record.Control || len(record.Messages) != 0 {
					t.Fatalf("model info record = %#v, err = %v", record, err)
				}
				after, err := p.ParseRecord([]byte(native(0)))
				if err != nil {
					t.Fatal(err)
				}
				afterUsage, ok := after.Messages[0].Message.(*UsageMessage)
				if !ok {
					t.Fatalf("post-snapshot message = %T, want *UsageMessage", after.Messages[0].Message)
				}
				if afterUsage.ContextWindow != 1000000 {
					t.Fatalf("snapshot context = %d", afterUsage.ContextWindow)
				}
				nativeValue, err := p.ParseRecord([]byte(native(200000)))
				if err != nil {
					t.Fatal(err)
				}
				nativeUsage, ok := nativeValue.Messages[0].Message.(*UsageMessage)
				if !ok {
					t.Fatalf("native-context message = %T, want *UsageMessage", nativeValue.Messages[0].Message)
				}
				if nativeUsage.ContextWindow != 200000 {
					t.Fatalf("native context = %d", nativeUsage.ContextWindow)
				}
			})
		}
	})

	t.Run("NativeParserRejectsNilMessagesAtomically", func(t *testing.T) {
		t.Parallel()
		action := &PendingUserActionMessage{
			MessageType: PendingUserActionMessageType,
			Action: PendingUserAction{
				Kind: PendingUserActionAskUserQuestion, RequestID: "r1", ToolUseID: "t1",
			},
		}
		agentLine := []byte(`{"t":"agent","ts":1.000,"msg":{"type":"native"}}`)

		for _, tc := range []struct {
			name    string
			invalid Message
		}{
			{name: "nil_interface", invalid: nil},
			{name: "typed_nil_pending", invalid: (*PendingUserActionMessage)(nil)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				batch := []Message{action, tc.invalid}
				p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
					return batch, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.ParseRecord(agentLine); err == nil || !strings.Contains(err.Error(), "native message 1 is nil") {
					t.Fatalf("error = %v, want nil message error", err)
				}
				batch = []Message{action}
				record, err := p.ParseRecord(agentLine)
				if err != nil {
					t.Fatal(err)
				}
				if record.Control || len(record.Messages) != 1 || record.Messages[0].Message != action {
					t.Fatalf("pending state changed by rejected batch: %#v", record)
				}
			})
		}

		t.Run("typed_nil_usage", func(t *testing.T) {
			t.Parallel()
			usage := &UsageMessage{}
			batch := []Message{usage, (*UsageMessage)(nil)}
			p, err := NewLogRecordParser(LogVersionV2, func([]byte) ([]Message, error) {
				return batch, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.ParseRecord([]byte(`{"t":"model_info","context_window":1000000}`)); err != nil {
				t.Fatal(err)
			}
			if _, err := p.ParseRecord(agentLine); err == nil || !strings.Contains(err.Error(), "native message 1 is nil") {
				t.Fatalf("error = %v, want nil message error", err)
			}
			if usage.ContextWindow != 0 {
				t.Fatalf("usage mutated by rejected batch: %#v", usage)
			}
			validUsage := &UsageMessage{}
			batch = []Message{validUsage}
			record, err := p.ParseRecord(agentLine)
			if err != nil {
				t.Fatal(err)
			}
			if record.Control || len(record.Messages) != 1 || validUsage.ContextWindow != 1000000 {
				t.Fatalf("context state after rejected batch = %#v", record)
			}
		})
	})

	t.Run("ProducerMetadata", func(t *testing.T) {
		t.Parallel()
		parseNative := func(line []byte) ([]Message, error) {
			var wire struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(line, &wire); err != nil {
				return nil, err
			}
			switch wire.Kind {
			case "empty":
				return nil, nil
			case "mixed":
				return []Message{
					&TextMessage{Text: "before"},
					&ToolUseMessage{ToolUseID: "t", Name: "Read"},
					&TextMessage{Text: "between"},
					&ToolResultMessage{ToolUseID: "t"},
					&TextMessage{Text: "after"},
				}, nil
			default:
				return []Message{&TextMessage{Text: "text"}}, nil
			}
		}

		v2, err := NewLogRecordParser(LogVersionV2, parseNative)
		if err != nil {
			t.Fatal(err)
		}
		mixed, err := v2.ParseRecord([]byte(`{"t":"agent","ts":123.250,"msg":{"kind":"mixed"}}`))
		if err != nil {
			t.Fatal(err)
		}
		wantTypes := []any{
			(*TextMessage)(nil),
			(*ToolUseMessage)(nil),
			(*TextMessage)(nil),
			(*ToolResultMessage)(nil),
			(*TextMessage)(nil),
		}
		if mixed.Control || len(mixed.Messages) != len(wantTypes) {
			t.Fatalf("mixed record = %#v", mixed)
		}
		wantTime := time.Unix(123, 250_000_000).UTC()
		for i, want := range wantTypes {
			if reflect.TypeOf(mixed.Messages[i].Message) != reflect.TypeOf(want) {
				t.Fatalf("mixed[%d] = %T, want %T", i, mixed.Messages[i].Message, want)
			}
			if !mixed.Messages[i].ProducerTime.Equal(wantTime) {
				t.Fatalf("mixed[%d] producer time = %v, want %v", i, mixed.Messages[i].ProducerTime, wantTime)
			}
		}
		switch mixed.Messages[1].Message.(type) {
		case *ToolUseMessage:
		default:
			t.Fatalf("unwrapped message = %T, want *ToolUseMessage", mixed.Messages[1].Message)
		}

		v1, err := NewLogRecordParser(LogVersionV1, parseNative)
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := v1.ParseRecord([]byte(`{"kind":"mixed"}`))
		if err != nil {
			t.Fatal(err)
		}
		if legacy.Control || len(legacy.Messages) != len(wantTypes) {
			t.Fatalf("v1 record = %#v", legacy)
		}
		for i, msg := range legacy.Messages {
			if msg.Message == nil || !msg.ProducerTime.IsZero() {
				t.Fatalf("v1 message %d = %#v, want non-nil message and zero producer time", i, msg)
			}
		}

		empty, err := v2.ParseRecord([]byte(`{"t":"agent","ts":124.000,"msg":{"kind":"empty"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if empty.Control || len(empty.Messages) != 0 {
			t.Fatalf("empty native record = %#v", empty)
		}

		parsed := ParsedMessage{Message: &TextMessage{Text: "text"}, ProducerTime: wantTime}
		if _, ok := any(parsed).(Message); ok {
			t.Fatal("ParsedMessage value implements Message")
		}
		if _, ok := any(&parsed).(Message); ok {
			t.Fatal("*ParsedMessage implements Message")
		}
	})
}
