package agent

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestWriteMessageLog(t *testing.T) {
	var buf bytes.Buffer
	var logBuf bytes.Buffer
	if err := writeMessage(&buf, "hello", &logBuf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != logBuf.String() {
		t.Errorf("stdin and log differ:\nstdin: %q\nlog:   %q", buf.String(), logBuf.String())
	}
	if !strings.Contains(buf.String(), `"content":"hello"`) {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestSessionLifecycle(t *testing.T) {
	// Simulate a session using pipes instead of a real process.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	s := &Session{
		stdin: stdinW,
		logW:  nil,
		done:  make(chan struct{}),
	}

	msgCh := make(chan Message, 16)

	// Simulate the readMessages goroutine.
	go func() {
		defer close(s.done)
		result, parseErr := readMessages(stdoutR, msgCh, nil)
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

func TestParsePgrepOutput(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantSID   string
		wantFound bool
	}{
		{
			name:      "Empty",
			input:     "",
			wantSID:   "",
			wantFound: false,
		},
		{
			name:      "NoResume",
			input:     "12345 claude -p --input-format stream-json\n",
			wantSID:   "",
			wantFound: true,
		},
		{
			name:      "WithResume",
			input:     "12345 claude -p --resume abc-123 --input-format stream-json\n",
			wantSID:   "abc-123",
			wantFound: true,
		},
		{
			name:      "ResumeAtEnd",
			input:     "12345 claude -p --resume sess-99\n",
			wantSID:   "sess-99",
			wantFound: true,
		},
		{
			name:      "MultipleLines",
			input:     "111 something\n222 claude -p --resume my-id --verbose\n",
			wantSID:   "my-id",
			wantFound: true,
		},
		{
			name:      "MultipleLinesNoResume",
			input:     "111 something\n222 claude -p --verbose\n",
			wantSID:   "",
			wantFound: true,
		},
		{
			name:      "WhitespaceOnly",
			input:     "   \n  \n",
			wantSID:   "",
			wantFound: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sid, found := parsePgrepOutput(tc.input)
			if sid != tc.wantSID {
				t.Errorf("sessionID = %q, want %q", sid, tc.wantSID)
			}
			if found != tc.wantFound {
				t.Errorf("found = %v, want %v", found, tc.wantFound)
			}
		})
	}
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
		result, err := readMessages(strings.NewReader(input), ch, nil)
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
		result, err := readMessages(strings.NewReader(input), nil, &buf)
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
