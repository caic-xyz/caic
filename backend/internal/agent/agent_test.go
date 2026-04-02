package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
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
	if logW != nil {
		_, _ = logW.Write(data)
	}
	return nil
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

func TestSession(t *testing.T) {
	t.Run("Lifecycle", func(t *testing.T) {
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()

		s := &Session{
			stdin: stdinW,
			wire:  testWire{},
			done:  make(chan struct{}),
		}

		msgCh := make(chan Message, 16)

		go func() {
			defer close(s.done)
			result, parseErr := readMessages(stdoutR, msgCh, nil, testParseFn)
			s.result = result
			if parseErr != nil {
				s.err = parseErr
			} else if result == nil {
				s.err = io.ErrUnexpectedEOF
			}
		}()

		stdinBuf := make(chan string, 1)
		go func() {
			data, _ := io.ReadAll(stdinR)
			stdinBuf <- string(data)
		}()

		if err := s.Send(Prompt{Text: "test prompt"}); err != nil {
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

		if err := s.Send(Prompt{Text: "follow-up"}); err != nil {
			t.Fatal(err)
		}

		s.Close()

		got := <-stdinBuf
		if !strings.Contains(got, `"content":"test prompt"`) {
			t.Errorf("missing first prompt in stdin: %s", got)
		}
		if !strings.Contains(got, `"content":"follow-up"`) {
			t.Errorf("missing follow-up in stdin: %s", got)
		}

		_ = stdoutW.Close()

		rm, err := s.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if rm == nil {
			t.Fatal("expected result, got nil")
		}
		if rm.Result != "ok" {
			t.Errorf("result = %q, want %q", rm.Result, "ok")
		}

		close(msgCh)
		var count int
		for range msgCh {
			count++
		}
		if count != 1 {
			t.Errorf("message count = %d, want 1", count)
		}
	})
	t.Run("SendRaw", func(t *testing.T) {
		t.Run("valid", func(t *testing.T) {
			stdinR, stdinW := io.Pipe()
			var logBuf bytes.Buffer
			s := &Session{
				stdin: stdinW,
				wire:  testWire{},
				logW:  &logBuf,
				done:  make(chan struct{}),
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
			s.Close()

			got := <-stdinBuf
			if !strings.Contains(got, `"FOO":"bar"`) {
				t.Errorf("stdin missing payload: %s", got)
			}
			if !strings.Contains(logBuf.String(), `"FOO":"bar"`) {
				t.Errorf("log missing payload: %s", logBuf.String())
			}
		})
		t.Run("error", func(t *testing.T) {
			stdinR, stdinW := io.Pipe()
			_ = stdinR.Close()
			s := &Session{
				stdin: stdinW,
				wire:  testWire{},
				done:  make(chan struct{}),
			}
			if err := s.SendRaw([]byte("data\n")); err == nil {
				t.Error("expected error writing to closed pipe")
			}
		})
	})
	t.Run("CloseIdempotent", func(t *testing.T) {
		stdinR, stdinW := io.Pipe()
		go func() { _, _ = io.Copy(io.Discard, stdinR) }()
		s := &Session{
			stdin: stdinW,
			wire:  testWire{},
			done:  make(chan struct{}),
		}
		s.Close()
		s.Close()
	})
	t.Run("SignalKillNotError", func(t *testing.T) {
		cmd := exec.Command("sleep", "60")
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
		s := NewSession(cmd, stdin, stdout, msgCh, nil, testWire{}, nil)

		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}

		_, err = s.Wait()
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

func TestReadMessages(t *testing.T) {
	t.Run("FullStream", func(t *testing.T) {
		lines := []string{
			`{"type":"system","subtype":"init","cwd":"/","session_id":"s","tools":[],"model":"m","claude_code_version":"1","uuid":"u"}`,
			`{"type":"assistant","message":{"model":"m","id":"i","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{}},"session_id":"s","uuid":"u"}`,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"hi","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}`,
		}
		input := strings.Join(lines, "\n")

		ch := make(chan Message, 16)
		result, err := readMessages(strings.NewReader(input), ch, nil, testParseFn)
		close(ch)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil {
			t.Fatal("expected result, got nil")
		}
		if result.Result != "hi" {
			t.Errorf("result = %q, want %q", result.Result, "hi")
		}

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
		lines := []string{
			`{"type":"system","subtype":"init","cwd":"/","session_id":"s","tools":[],"model":"m","claude_code_version":"1","uuid":"u"}`,
			`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}}`,
			`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}}`,
			`{"type":"assistant","message":{"model":"m","id":"i","role":"assistant","content":[{"type":"text","text":"hello"}],"usage":{}},"session_id":"s","uuid":"u"}`,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"hello","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}`,
		}
		input := strings.Join(lines, "\n")

		ch := make(chan Message, 16)
		result, err := readMessages(strings.NewReader(input), ch, nil, testParseFn)
		close(ch)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil {
			t.Fatal("expected result, got nil")
		}

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
		lines := []string{
			`{"type":"system","subtype":"init","cwd":"/","session_id":"s","tools":[],"model":"m","claude_code_version":"1","uuid":"u"}`,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"ok","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}`,
		}
		input := strings.Join(lines, "\n")

		var buf bytes.Buffer
		result, err := readMessages(strings.NewReader(input), nil, &buf, testParseFn)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil {
			t.Fatal("expected result")
		}

		logged := buf.String()
		for _, line := range lines {
			if !strings.Contains(logged, line+"\n") {
				t.Errorf("log missing line: %s", line)
			}
		}
	})
}
