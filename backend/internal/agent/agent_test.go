package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// testWire implements WireFormat for testing.
type testWire struct{}

func (testWire) WritePrompt(w io.Writer, prompt string, logW io.Writer) error {
	msg := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}{Type: "user"}
	msg.Message.Role = "user"
	msg.Message.Content = prompt
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

func (testWire) ParseMessage(line []byte) (Message, error) {
	return ParseMessage(line)
}

func TestSessionLifecycle(t *testing.T) {
	// Simulate a session using pipes instead of a real process.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	s := &Session{
		stdin: stdinW,
		wire:  testWire{},
		done:  make(chan struct{}),
	}

	msgCh := make(chan Message, 16)

	// Simulate the readMessages goroutine.
	go func() {
		defer close(s.done)
		result, parseErr := readMessages(stdoutR, msgCh, nil, ParseMessage)
		s.result = result
		if parseErr != nil {
			s.err = parseErr
		} else if result == nil {
			s.err = io.ErrUnexpectedEOF
		}
	}()

	// Drain stdin in background (io.Pipe is synchronous).
	stdinBuf := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(stdinR)
		stdinBuf <- string(data)
	}()

	// Send a prompt.
	if err := s.Send("test prompt"); err != nil {
		t.Fatal(err)
	}

	// Write a result message to stdout.
	resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"ok","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}` + "\n"
	if _, err := stdoutW.Write([]byte(resultLine)); err != nil {
		t.Fatal(err)
	}

	// Session should NOT be done yet — stdin is still open.
	select {
	case <-s.done:
		t.Fatal("session closed prematurely after result")
	case <-time.After(50 * time.Millisecond):
		// Expected: session stays alive.
	}

	// Send a second message (multi-turn).
	if err := s.Send("follow-up"); err != nil {
		t.Fatal(err)
	}

	// Now close stdin so the process can exit.
	s.Close()

	got := <-stdinBuf
	if !strings.Contains(got, `"content":"test prompt"`) {
		t.Errorf("missing first prompt in stdin: %s", got)
	}
	if !strings.Contains(got, `"content":"follow-up"`) {
		t.Errorf("missing follow-up in stdin: %s", got)
	}

	// Close stdout to simulate process exit.
	_ = stdoutW.Close()

	// Wait for session to finish.
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

	// Verify messages were dispatched.
	close(msgCh)
	var count int
	for range msgCh {
		count++
	}
	if count != 1 {
		t.Errorf("message count = %d, want 1", count)
	}
}

func TestSessionClose(t *testing.T) {
	_, stdinW := io.Pipe()
	s := &Session{
		stdin: stdinW,
		wire:  testWire{},
		done:  make(chan struct{}),
	}
	// Close should be idempotent.
	s.Close()
	s.Close()
}

func TestParseMessage(t *testing.T) {
	t.Run("SystemInit", func(t *testing.T) {
		line := `{"type":"system","subtype":"init","cwd":"/home/user","session_id":"abc-123","tools":["Bash","Read"],"model":"claude-opus-4-6","claude_code_version":"2.1.34","uuid":"uuid-1"}`
		msg, err := ParseMessage([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := msg.(*SystemInitMessage)
		if !ok {
			t.Fatalf("got %T, want *SystemInitMessage", msg)
		}
		if m.Model != "claude-opus-4-6" {
			t.Errorf("model = %q, want %q", m.Model, "claude-opus-4-6")
		}
		if len(m.Tools) != 2 {
			t.Errorf("tools = %v, want 2 items", m.Tools)
		}
	})
	t.Run("Assistant", func(t *testing.T) {
		line := `{"type":"assistant","message":{"model":"claude-opus-4-6","id":"msg_01","role":"assistant","content":[{"type":"text","text":"hello world"}],"usage":{"input_tokens":10,"output_tokens":5}},"session_id":"abc","uuid":"u1"}`
		msg, err := ParseMessage([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := msg.(*AssistantMessage)
		if !ok {
			t.Fatalf("got %T, want *AssistantMessage", msg)
		}
		text := TextFromAssistant(m)
		if text != "hello world" {
			t.Errorf("text = %q, want %q", text, "hello world")
		}
	})
	t.Run("Result", func(t *testing.T) {
		line := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"duration_api_ms":1200,"num_turns":3,"result":"done","session_id":"abc","total_cost_usd":0.05,"usage":{"input_tokens":100,"output_tokens":50},"uuid":"u2"}`
		msg, err := ParseMessage([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := msg.(*ResultMessage)
		if !ok {
			t.Fatalf("got %T, want *ResultMessage", msg)
		}
		if m.Subtype != "success" {
			t.Errorf("subtype = %q, want %q", m.Subtype, "success")
		}
		if m.TotalCostUSD != 0.05 {
			t.Errorf("cost = %f, want 0.05", m.TotalCostUSD)
		}
		if m.NumTurns != 3 {
			t.Errorf("turns = %d, want 3", m.NumTurns)
		}
	})
	t.Run("RawFallback", func(t *testing.T) {
		line := `{"type":"stream_event","event":{"type":"message_start"}}`
		msg, err := ParseMessage([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := msg.(*RawMessage)
		if !ok {
			t.Fatalf("got %T, want *RawMessage", msg)
		}
		if m.Type() != "stream_event" {
			t.Errorf("type = %q, want %q", m.Type(), "stream_event")
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
		result, err := readMessages(strings.NewReader(input), ch, nil, ParseMessage)
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

		var count int
		for range ch {
			count++
		}
		if count != 3 {
			t.Errorf("message count = %d, want 3", count)
		}
	})
	t.Run("LogWriter", func(t *testing.T) {
		lines := []string{
			`{"type":"system","subtype":"init","cwd":"/","session_id":"s","tools":[],"model":"m","claude_code_version":"1","uuid":"u"}`,
			`{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"ok","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}`,
		}
		input := strings.Join(lines, "\n")

		var buf bytes.Buffer
		result, err := readMessages(strings.NewReader(input), nil, &buf, ParseMessage)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil {
			t.Fatal("expected result")
		}

		// Each input line should appear in the log, newline-terminated.
		logged := buf.String()
		for _, line := range lines {
			if !strings.Contains(logged, line+"\n") {
				t.Errorf("log missing line: %s", line)
			}
		}
	})
}
