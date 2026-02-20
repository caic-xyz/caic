package agent

import (
	"bytes"
	"encoding/json"
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

func (testWire) ParseMessage(line []byte) (Message, error) {
	return ParseMessage(line)
}

func TestSession(t *testing.T) {
	t.Run("Lifecycle", func(t *testing.T) {
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
		if err := s.Send(Prompt{Text: "test prompt"}); err != nil {
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
		if err := s.Send(Prompt{Text: "follow-up"}); err != nil {
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
		// A killed process (e.g. container termination) must not produce
		// ERROR-level logs — it's expected behavior.
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

		// Capture log output to verify log level.
		var logBuf bytes.Buffer
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
		defer slog.SetDefault(oldDefault)

		msgCh := make(chan Message, 16)
		s := NewSession(cmd, stdin, stdout, msgCh, nil, testWire{}, nil)

		// Kill the process (simulates container termination via SIGKILL).
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

		// Signal-killed sessions must NOT produce ERROR-level logs.
		logOutput := logBuf.String()
		if strings.Contains(logOutput, "level=ERROR") {
			t.Errorf("killed process should not produce ERROR log:\n%s", logOutput)
		}
	})
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
	t.Run("StreamEventTextDelta", func(t *testing.T) {
		line := `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}}`
		msg, err := ParseMessage([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := msg.(*StreamEvent)
		if !ok {
			t.Fatalf("got %T, want *StreamEvent", msg)
		}
		if m.Type() != "stream_event" {
			t.Errorf("type = %q, want %q", m.Type(), "stream_event")
		}
		if m.Event.Type != "content_block_delta" {
			t.Errorf("event.type = %q, want %q", m.Event.Type, "content_block_delta")
		}
		if m.Event.Index != 0 {
			t.Errorf("event.index = %d, want 0", m.Event.Index)
		}
		if m.Event.Delta == nil {
			t.Fatal("event.delta is nil")
		}
		if m.Event.Delta.Type != "text_delta" {
			t.Errorf("delta.type = %q, want %q", m.Event.Delta.Type, "text_delta")
		}
		if m.Event.Delta.Text != "Hello" {
			t.Errorf("delta.text = %q, want %q", m.Event.Delta.Text, "Hello")
		}
	})
	t.Run("StreamEventMessageStart", func(t *testing.T) {
		line := `{"type":"stream_event","event":{"type":"message_start"}}`
		msg, err := ParseMessage([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := msg.(*StreamEvent)
		if !ok {
			t.Fatalf("got %T, want *StreamEvent", msg)
		}
		if m.Event.Type != "message_start" {
			t.Errorf("event.type = %q, want %q", m.Event.Type, "message_start")
		}
		if m.Event.Delta != nil {
			t.Errorf("delta should be nil for message_start")
		}
	})
	t.Run("DiffStat", func(t *testing.T) {
		line := `{"type":"caic_diff_stat","diff_stat":[{"path":"main.go","added":10,"deleted":3},{"path":"img.png","added":0,"deleted":0,"binary":true}]}`
		msg, err := ParseMessage([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := msg.(*DiffStatMessage)
		if !ok {
			t.Fatalf("got %T, want *DiffStatMessage", msg)
		}
		if m.Type() != "caic_diff_stat" {
			t.Errorf("type = %q, want %q", m.Type(), "caic_diff_stat")
		}
		if len(m.DiffStat) != 2 {
			t.Fatalf("diff_stat len = %d, want 2", len(m.DiffStat))
		}
		if m.DiffStat[0].Path != "main.go" || m.DiffStat[0].Added != 10 || m.DiffStat[0].Deleted != 3 {
			t.Errorf("diff_stat[0] = %+v", m.DiffStat[0])
		}
		if !m.DiffStat[1].Binary {
			t.Errorf("diff_stat[1].binary = false, want true")
		}
	})
	t.Run("RawFallback", func(t *testing.T) {
		line := `{"type":"tool_progress","data":"some progress"}`
		msg, err := ParseMessage([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := msg.(*RawMessage)
		if !ok {
			t.Fatalf("got %T, want *RawMessage", msg)
		}
		if m.Type() != "tool_progress" {
			t.Errorf("type = %q, want %q", m.Type(), "tool_progress")
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
		result, err := readMessages(strings.NewReader(input), ch, nil, ParseMessage)
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
		// init + 2 stream_event + assistant + result = 5
		if len(msgs) != 5 {
			t.Errorf("message count = %d, want 5", len(msgs))
		}
		// Verify stream events are parsed as StreamEvent.
		if _, ok := msgs[1].(*StreamEvent); !ok {
			t.Errorf("msgs[1] is %T, want *StreamEvent", msgs[1])
		}
		if _, ok := msgs[2].(*StreamEvent); !ok {
			t.Errorf("msgs[2] is %T, want *StreamEvent", msgs[2])
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
