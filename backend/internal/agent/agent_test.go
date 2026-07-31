// Tests for the agent package, covering wire format handling and session lifecycle.

package agent

import (
	"bytes"
	"encoding/json"
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

func (testWire) WritePrompt(w io.Writer, p Prompt, logW io.Writer) error {
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
	_, err = logW.Write(data)
	return err
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
			Conn: NewConn(stdinW, io.Discard, testWire{}),
			done: make(chan struct{}),
		}

		msgCh := make(chan Message, 16)

		go func() {
			defer close(s.done)
			if parseErr := DefaultReadMessages(stdoutR, func(m Message) { msgCh <- m }, io.Discard, testParseFn); parseErr != nil {
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
	t.Run("SendRaw", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			stdinR, stdinW := io.Pipe()
			var logBuf bytes.Buffer
			s := &Session{
				Conn: NewConn(stdinW, &logBuf, testWire{}),
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
			if !strings.Contains(logBuf.String(), `"FOO":"bar"`) {
				t.Errorf("log missing payload: %s", logBuf.String())
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			stdinR, stdinW := io.Pipe()
			_ = stdinR.Close()
			s := &Session{
				Conn: NewConn(stdinW, io.Discard, testWire{}),
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
			Conn: NewConn(stdinW, io.Discard, testWire{}),
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
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
		defer slog.SetDefault(oldDefault)

		msgCh := make(chan Message, 16)
		s := NewSession(cmd, NewConn(stdin, io.Discard, testWire{}), stdout, msgCh, nil)

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

func TestWriteMetaSession(t *testing.T) {
	t.Parallel()

	t.Run("VersionWithoutSession", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		if err := WriteMetaSession(&buf, &InitMessage{Model: "m", Version: "1.2.3"}); err != nil {
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
		var buf bytes.Buffer
		if err := WriteMetaSession(&buf, &InitMessage{}); err != nil {
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
		input := strings.Join(lines, "\n")

		ch := make(chan Message, 16)
		if err := DefaultReadMessages(strings.NewReader(input), func(m Message) { ch <- m }, io.Discard, testParseFn); err != nil {
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
		input := strings.Join(lines, "\n")

		ch := make(chan Message, 16)
		if err := DefaultReadMessages(strings.NewReader(input), func(m Message) { ch <- m }, io.Discard, testParseFn); err != nil {
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
		input := strings.Join(lines, "\n")

		var buf bytes.Buffer
		if err := DefaultReadMessages(strings.NewReader(input), func(Message) {}, &buf, testParseFn); err != nil {
			t.Fatal(err)
		}

		logged := buf.String()
		for _, line := range lines {
			if !strings.Contains(logged, line+"\n") {
				t.Errorf("log missing line: %s", line)
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
		for m, e := range yieldMessages(strings.NewReader(content), testParseFn, skipFirst, "ctr") {
			if e != nil {
				return msgs, e
			}
			msgs = append(msgs, m)
		}
		return msgs, nil
	}

	t.Run("Full", func(t *testing.T) {
		t.Parallel()
		msgs, err := collect(strings.Join(lines, "\n"), false)
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
		msgs, err := collect(`ssage":{"model"...truncated`+"\n"+strings.Join(lines[1:], "\n"), true)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 {
			t.Errorf("message count = %d, want 2", len(msgs))
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
		var zero LogRecordParser
		if _, err := zero.ParseRecord([]byte(`{"type":"assistant"}`)); err == nil || !strings.Contains(err.Error(), "unsupported log version 0") {
			t.Fatalf("zero parser error = %v", err)
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
				v2:   `{"type":"diff_stat","diff_stat":[{"path":"a.go","added":2,"deleted":1}],"ts":12.5}`,
			},
			{
				name: "exit",
				v1:   `{"type":"caic_exit","exit_code":2,"cmd":["agent"],"error":"failed","ts":13}`,
				v2:   `{"type":"exit","exit_code":2,"cmd":["agent"],"error":"failed","ts":13}`,
			},
			{
				name: "stripped_env",
				v1:   `{"type":"caic_stripped_env","variables":{"TOKEN":"secret"}}`,
				v2:   `{"type":"stripped_env","variables":{"TOKEN":"secret"}}`,
			},
			{
				name: "session",
				v1:   `{"type":"caic_session","session_id":"s1","model":"m1","agent_version":"1.2.3"}`,
				v2:   `{"type":"session","session_id":"s1","model":"m1","agent_version":"1.2.3"}`,
			},
			{
				name: "pr",
				v1:   `{"type":"caic_pr","forge_owner":"o","forge_repo":"r","forge_pr":7}`,
				v2:   `{"type":"pr","forge_owner":"o","forge_repo":"r","forge_pr":7}`,
			},
			{
				name: "result",
				v1:   `{"type":"caic_result","state":"purged","title":"done","cost_usd":1.5,"num_turns":2}`,
				v2:   `{"type":"result","state":"purged","title":"done","cost_usd":1.5,"num_turns":2}`,
			},
			{
				name: "pending_user_action",
				v1:   `{"type":"caic_pending_user_action","action":{"kind":"ask_user_question","request_id":"r1","tool_use_id":"t1"}}`,
				v2:   `{"type":"pending_user_action","action":{"kind":"ask_user_question","request_id":"r1","tool_use_id":"t1"}}`,
			},
			{
				name: "provisioning_log",
				v1:   `{"type":"caic_log","line":"starting"}`,
				v2:   `{"type":"log","line":"starting"}`,
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
				if !reflect.DeepEqual(gotV1, gotV2) {
					t.Fatalf("v1 = %#v, v2 = %#v", gotV1, gotV2)
				}
				if len(gotV1) != 1 || !gotV1[0].ProducerTime.IsZero() || !gotV2[0].ProducerTime.IsZero() {
					t.Fatalf("control producer times = %v, %v; want zero", gotV1, gotV2)
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
			line := fmt.Sprintf(`{"type":"caic_meta","version":%d,"prompt":"p","repos":[],"harness":"claude"}`, version)
			msgs, err := p.ParseRecord([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("version %d message count = %d", version, len(msgs))
			}
			meta, ok := msgs[0].Message.(*MetaMessage)
			if !ok || meta.Version != int(version) || !msgs[0].ProducerTime.IsZero() {
				t.Fatalf("version %d meta = %#v", version, msgs[0])
			}
		}

		v2, err := NewLogRecordParser(LogVersionV2, testParseFn)
		if err != nil {
			t.Fatal(err)
		}
		wrongVersion := []byte(`{"type":"caic_meta","version":1,"prompt":"p","repos":[],"harness":"claude"}`)
		if _, err := v2.ParseRecord(wrongVersion); err == nil || !strings.Contains(err.Error(), "does not match parser version 2") {
			t.Fatalf("mismatched header error = %v", err)
		}

		p, err := NewLogRecordParser(LogVersionV1, testParseFn)
		if err != nil {
			t.Fatal(err)
		}
		msgs, err := p.ParseRecord([]byte(`{"type":"caic_init","session_id":"legacy","model":"m","version":"0.9"}`))
		if err != nil {
			t.Fatal(err)
		}
		want := []ParsedMessage{{Message: &InitMessage{SessionID: "legacy", Model: "m", Version: "0.9"}}}
		if !reflect.DeepEqual(msgs, want) {
			t.Fatalf("legacy session = %#v, want %#v", msgs, want)
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
		gotV2, err := v2.ParseRecord([]byte(`{"type":"context_cleared"}`))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotV1, gotV2) {
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
			msgs, err := p.ParseRecord(line)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, line) || len(msgs) != 1 {
				t.Fatalf("native input = %s, messages = %#v", got, msgs)
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
			line := fmt.Sprintf(`{"type":"agent","ts":123.5,"msg":%s}`, payload)
			msgs, err := p.ParseRecord([]byte(line))
			if err != nil {
				t.Fatalf("payload %s: %v", payload, err)
			}
			if len(msgs) != 1 {
				t.Fatalf("payload %s message count = %d", payload, len(msgs))
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
			{name: "bare_native", line: `{"type":"assistant"}`, want: `unknown top-level type "assistant"`},
			{name: "prefixed_v1", line: `{"type":"caic_diff_stat","diff_stat":[]}`, want: `unknown top-level type "caic_diff_stat"`},
			{name: "unknown", line: `{"type":"future"}`, want: `unknown top-level type "future"`},
			{name: "missing_type", line: `{"value":1}`, want: "missing top-level type"},
			{name: "malformed_json", line: `{`, want: "decode envelope"},
			{name: "missing_ts", line: `{"type":"agent","msg":{"type":"assistant"}}`, want: "missing ts"},
			{name: "invalid_ts_type", line: `{"type":"agent","ts":"now","msg":{"type":"assistant"}}`, want: "cannot unmarshal string"},
			{name: "zero_ts", line: `{"type":"agent","ts":0,"msg":{"type":"assistant"}}`, want: "must be finite and positive"},
			{name: "out_of_range_ts", line: `{"type":"agent","ts":1e40,"msg":{"type":"assistant"}}`, want: "out of range"},
			{name: "missing_msg", line: `{"type":"agent","ts":1}`, want: "missing msg"},
			{name: "null_msg", line: `{"type":"agent","ts":1,"msg":null}`, want: "null msg"},
			{name: "invalid_msg", line: `{"type":"agent","ts":1,"msg":}`, want: "decode envelope"},
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
				line: []byte{'{', '"', 't', 'y', 'p', 'e', '"', ':', '"', 'a', 'g', 0xff, 'e', 'n', 't', '"', '}'},
			},
			{
				name: "msg",
				line: append(append([]byte(`{"type":"agent","ts":1,"msg":{"text":"`), 0xff), []byte(`"}}`)...),
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
					return fmt.Sprintf(`{"type":"agent","ts":10,"msg":%s}`, payload)
				}
				infoType := "caic_model_info"
				if version == LogVersionV2 {
					infoType = "model_info"
				}

				before, err := p.ParseRecord([]byte(native(0)))
				if err != nil {
					t.Fatal(err)
				}
				beforeUsage, ok := before[0].Message.(*UsageMessage)
				if !ok {
					t.Fatalf("pre-snapshot message = %T, want *UsageMessage", before[0].Message)
				}
				if beforeUsage.ContextWindow != 0 {
					t.Fatalf("pre-snapshot context = %d", beforeUsage.ContextWindow)
				}
				if msgs, err := p.ParseRecord(fmt.Appendf(nil, `{"type":%q,"context_window":1000000}`, infoType)); err != nil || len(msgs) != 0 {
					t.Fatalf("model info messages = %#v, err = %v", msgs, err)
				}
				after, err := p.ParseRecord([]byte(native(0)))
				if err != nil {
					t.Fatal(err)
				}
				afterUsage, ok := after[0].Message.(*UsageMessage)
				if !ok {
					t.Fatalf("post-snapshot message = %T, want *UsageMessage", after[0].Message)
				}
				if afterUsage.ContextWindow != 1000000 {
					t.Fatalf("snapshot context = %d", afterUsage.ContextWindow)
				}
				nativeValue, err := p.ParseRecord([]byte(native(200000)))
				if err != nil {
					t.Fatal(err)
				}
				nativeUsage, ok := nativeValue[0].Message.(*UsageMessage)
				if !ok {
					t.Fatalf("native-context message = %T, want *UsageMessage", nativeValue[0].Message)
				}
				if nativeUsage.ContextWindow != 200000 {
					t.Fatalf("native context = %d", nativeUsage.ContextWindow)
				}
			})
		}
	})

	t.Run("PendingActionDeduplication", func(t *testing.T) {
		t.Parallel()
		for _, version := range []LogVersion{LogVersionV1, LogVersionV2} {
			t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
				t.Parallel()
				p, err := NewLogRecordParser(version, func(line []byte) ([]Message, error) {
					var native PendingUserActionMessage
					if err := json.Unmarshal(line, &native); err != nil {
						return nil, err
					}
					native.MessageType = PendingUserActionMessageType
					return []Message{&native}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				nativeLine := func(action PendingUserAction) string {
					data, err := json.Marshal(PendingUserActionMessage{MessageType: "native_pending", Action: action})
					if err != nil {
						t.Fatal(err)
					}
					if version == LogVersionV1 {
						return string(data)
					}
					return fmt.Sprintf(`{"type":"agent","ts":11,"msg":%s}`, data)
				}
				topLine := func(action PendingUserAction) string {
					typ := PendingUserActionMessageType
					if version == LogVersionV2 {
						typ = "pending_user_action"
					}
					data, err := json.Marshal(PendingUserActionMessage{MessageType: typ, Action: action})
					if err != nil {
						t.Fatal(err)
					}
					return string(data)
				}

				first := PendingUserAction{
					Kind: PendingUserActionAskUserQuestion, RequestID: "r1", ToolUseID: "t1",
					Ask: PendingAskAction{Questions: []AskQuestion{{Question: "first", Options: []AskOption{}}}},
				}
				duplicate := ClonePendingUserAction(first)
				duplicate.Ask.Questions[0].Question = "duplicate"
				second := PendingUserAction{Kind: PendingUserActionAskUserQuestion, RequestID: "r2", ToolUseID: "t2"}
				third := PendingUserAction{Kind: PendingUserActionAskUserQuestion, RequestID: "r3", ToolUseID: "t3"}

				var got []Message
				for _, line := range []string{
					nativeLine(first), topLine(duplicate),
					topLine(second), nativeLine(second),
					topLine(third), nativeLine(third),
				} {
					msgs, err := p.ParseRecord([]byte(line))
					if err != nil {
						t.Fatal(err)
					}
					for _, msg := range msgs {
						got = append(got, msg.Message)
					}
				}
				if len(got) != 3 {
					t.Fatalf("message count = %d, want 3: %#v", len(got), got)
				}
				firstMsg, firstOK := got[0].(*PendingUserActionMessage)
				secondMsg, secondOK := got[1].(*PendingUserActionMessage)
				thirdMsg, thirdOK := got[2].(*PendingUserActionMessage)
				if !firstOK || !secondOK || !thirdOK {
					t.Fatalf("pending message types = %T, %T, %T", got[0], got[1], got[2])
				}
				if firstMsg.Action.Ask.Questions[0].Question != "first" {
					t.Fatalf("first occurrence not preserved: %#v", got[0])
				}
				if secondMsg.Action.RequestID != "r2" || thirdMsg.Action.RequestID != "r3" {
					t.Fatalf("order = %#v", got)
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
		agentLine := []byte(`{"type":"agent","ts":1,"msg":{"type":"native"}}`)

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
				msgs, err := p.ParseRecord(agentLine)
				if err != nil {
					t.Fatal(err)
				}
				if len(msgs) != 1 || msgs[0].Message != action {
					t.Fatalf("pending state changed by rejected batch: %#v", msgs)
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
			if _, err := p.ParseRecord([]byte(`{"type":"model_info","context_window":1000000}`)); err != nil {
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
			msgs, err := p.ParseRecord(agentLine)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 || validUsage.ContextWindow != 1000000 {
				t.Fatalf("context state after rejected batch = %#v", msgs)
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
		mixed, err := v2.ParseRecord([]byte(`{"type":"agent","ts":123.25,"msg":{"kind":"mixed"}}`))
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
		if len(mixed) != len(wantTypes) {
			t.Fatalf("mixed messages = %#v", mixed)
		}
		wantTime := time.Unix(123, 250_000_000).UTC()
		for i, want := range wantTypes {
			if reflect.TypeOf(mixed[i].Message) != reflect.TypeOf(want) {
				t.Fatalf("mixed[%d] = %T, want %T", i, mixed[i].Message, want)
			}
			if !mixed[i].ProducerTime.Equal(wantTime) {
				t.Fatalf("mixed[%d] producer time = %v, want %v", i, mixed[i].ProducerTime, wantTime)
			}
		}
		switch mixed[1].Message.(type) {
		case *ToolUseMessage:
		default:
			t.Fatalf("unwrapped message = %T, want *ToolUseMessage", mixed[1].Message)
		}

		v1, err := NewLogRecordParser(LogVersionV1, parseNative)
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := v1.ParseRecord([]byte(`{"kind":"mixed"}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(legacy) != len(wantTypes) {
			t.Fatalf("v1 messages = %#v", legacy)
		}
		for i, msg := range legacy {
			if msg.Message == nil || !msg.ProducerTime.IsZero() {
				t.Fatalf("v1 message %d = %#v, want non-nil message and zero producer time", i, msg)
			}
		}

		empty, err := v2.ParseRecord([]byte(`{"type":"agent","ts":124,"msg":{"kind":"empty"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(empty) != 0 {
			t.Fatalf("empty native result = %#v", empty)
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
