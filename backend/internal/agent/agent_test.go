// Tests and benchmarks for the agent package, covering wire format handling and session lifecycle.

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/taskslog"

	"github.com/caic-xyz/caic/backend/internal/tokens"

	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/mcp/mcptest"
)

func TestPrepareRelay(t *testing.T) {
	// Emulate OpenSSH's remote-shell parsing, then capture the harness argv.
	dir := t.TempDir()
	for name, script := range map[string]string{
		"ssh":     "#!/bin/sh\nset -eu\ncase \"$*\" in\n*serve-attach*) shift; exec /bin/sh -c \"$*\" ;;\n*) /bin/cat >/dev/null ;;\nesac\n",
		"python3": "#!/bin/sh\nset -eu\ntest \"${PYTHONDONTWRITEBYTECODE-}\" = 1\nprintf '%s\\0' \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { //nolint:gosec // Private SSH test fixtures must be executable.
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	id := "session with spaces'; printf INJECTED; $(printf SUBSTITUTED)"
	opts := &Options{
		Logger:  slog.New(slog.DiscardHandler),
		Target:  runtime.ConnectionTarget{SSHHost: "task"},
		Dir:     "/workspace with spaces",
		Records: newTestRecords(t, t.Context(), &testLogSink{}, nil),
	}
	rp, err := PrepareRelay(t.Context(), opts, nil, []string{"claude", "--resume", id})
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.Stdin.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(rp.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.Cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	want := []string{RelayScriptPath, "serve-attach", "--dir", "/workspace with spaces", "--no-log-stdin", "--", "claude", "--resume", id}
	if got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"); !reflect.DeepEqual(got, want) {
		t.Fatalf("remote argv = %q, want %q", got, want)
	}
}

// testWire implements WireFormat for testing.
type testWire struct{}

func (testWire) EncodePrompt(p Prompt) ([]byte, error) {
	msg := struct {
		Type    string `json:"type"`
		Message struct {
			Role    string         `json:"role"`
			Content string         `json:"content"`
			Images  []v3.ImageData `json:"images,omitzero"`
		} `json:"message"`
	}{Type: "user"}
	msg.Message.Role = "user"
	msg.Message.Content = p.Text
	msg.Message.Images = p.Images
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
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
				ReportedModel: w.Model, Version: w.Version,
			}}, nil
		}
		var m SystemMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		m.MessageType = env.Type
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
	case "user":
		var m struct {
			Message struct {
				Content string         `json:"content"`
				Images  []v3.ImageData `json:"images,omitzero"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, err
		}
		return []Message{&UserInputMessage{Text: m.Message.Content, Images: m.Message.Images}}, nil
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

func (testWire) ParseInput(line []byte) ([]Message, error) { return testParseFn(line) }

func (testWire) ParseOutput(line []byte) ([]Message, error) {
	return testParseFn(line)
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type testLogSink struct {
	bytes.Buffer
}

func (s *testLogSink) AppendRecord(record PhysicalRecord) error {
	_, err := s.Write(record.Encoded)
	return err
}

func (s *testLogSink) AppendMessage(m Message) error {
	data, err := MarshalLogMessage(m)
	if err != nil {
		return err
	}
	return s.AppendRecord(PhysicalRecord{Encoded: append(data, '\n')})
}

func (*testLogSink) Close() error { return nil }

// appendRelayNativeRecord appends bytes received from a relay in the exact
// physical task-log format, matching what a live relay writes.
func appendRelayNativeRecord(log LogSink, data []byte) error {
	record, err := (&v3.NativeRecord{Type: string(taskslog.KindAgent), Time: time.Now().UTC(), Message: bytes.TrimSuffix(data, []byte{'\n'})}).MarshalJSON()
	if err != nil {
		return err
	}
	return log.AppendRecord(PhysicalRecord{Encoded: append(record, '\n')})
}

// canceledRegistry models an MCP operation that waits on another transport.
type canceledRegistry struct {
	mcptest.FakeRegistry

	started chan struct{}
}

func (r *canceledRegistry) CallTool(ctx context.Context, _ *mcp.ToolsCallParams) (mcp.RawToolResult, error) {
	close(r.started)
	<-ctx.Done()
	return mcp.RawToolResult{}, ctx.Err()
}

type closeErrorWriter struct {
	bytes.Buffer

	err error
}

func (w *closeErrorWriter) Close() error { return w.err }

type closeErrorReader struct {
	io.Reader

	err error
}

func (r *closeErrorReader) Close() error { return r.err }

func TestNewConn(t *testing.T) {
	t.Parallel()
	t.Run("SendPrompt", func(t *testing.T) {
		t.Parallel()
		sink := &testLogSink{}
		owner := newTestRecordsWithHandler(t, t.Context(), sink, &testMessageParser{}, &testRecordHandler{})
		if err := owner.AppendMessage(t.Context(), &MetaSessionMessage{SessionID: "session-1"}); err != nil {
			t.Fatal(err)
		}
		conn := NewConn(t.Context(), testLogger(), &closeErrorWriter{}, owner, nil)
		if err := conn.SendPrompt(Prompt{Text: "first"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := owner.AppendMessage(t.Context(), ContextCleared()); err != nil {
			t.Fatal(err)
		}
		if err := conn.SendPrompt(Prompt{Text: "second"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := conn.ReadMessages(io.NopCloser(strings.NewReader("")), nil); err != nil {
			t.Fatal(err)
		}
		var controls []*v3.SystemMessage
		for line := range bytes.SplitSeq(bytes.TrimSpace(sink.Bytes()), []byte{'\n'}) {
			frame, err := taskslog.DecodeFrame(line)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Kind == taskslog.KindInputDelivery {
				m, ok := frame.Control.(*v3.SystemMessage)
				if !ok {
					t.Fatalf("control = %T", frame.Control)
				}
				if frame.ProducerTime.IsZero() {
					t.Fatal("delivery has no producer time")
				}
				controls = append(controls, m)
			}
		}
		if len(controls) != 4 {
			t.Fatalf("controls = %+v", controls)
		}
		for i, session := range []string{"session-1", ""} {
			start, completed := controls[i*2], controls[i*2+1]
			if start.UUID == "" || completed.UUID != start.UUID || start.SessionID != session || completed.SessionID != session {
				t.Fatalf("attempt identity = %+v %+v", start, completed)
			}
		}
		if controls[0].UUID == controls[2].UUID {
			t.Fatal("attempt identities reused")
		}
	})
	t.Run("RecordInputFailure", func(t *testing.T) {
		t.Parallel()
		sink := &testLogSink{}
		owner := newTestRecordsWithHandler(t, t.Context(), sink, &testMessageParser{}, &testRecordHandler{})
		if err := owner.AppendMessage(t.Context(), &MetaSessionMessage{SessionID: "rejected-session"}); err != nil {
			t.Fatal(err)
		}
		conn := NewConn(t.Context(), testLogger(), &closeErrorWriter{}, owner, nil)
		err := conn.RecordInputFailure(Prompt{Text: "rejected"}, io.ErrClosedPipe)
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("failure = %v", err)
		}
		if err := conn.ReadMessages(io.NopCloser(strings.NewReader("")), nil); err != nil {
			t.Fatal(err)
		}
		parser, err := NewLogRecordParser(&testMessageParser{})
		if err != nil {
			t.Fatal(err)
		}
		var controls []*SystemMessage
		for line := range bytes.SplitSeq(bytes.TrimSpace(sink.Bytes()), []byte{'\n'}) {
			record, err := parser.ParseRecord(line)
			if err != nil {
				t.Fatal(err)
			}
			for _, timed := range record.Messages {
				if m, ok := timed.Message.(*SystemMessage); ok {
					if timed.ProducerTime.IsZero() {
						t.Fatal("missing producer timestamp")
					}
					controls = append(controls, m)
				}
			}
		}
		if len(controls) != 2 || controls[0].UUID == "" || controls[1].UUID != controls[0].UUID || controls[0].SessionID != "rejected-session" || controls[1].SessionID != "rejected-session" {
			t.Fatalf("failure identity = %+v", controls)
		}
	})
	t.Run("SendRaw", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			stdinR, stdinW := io.Pipe()
			logBuf := &testLogSink{}
			c := NewConn(t.Context(), testLogger(), stdinW, newTestRecords(t, t.Context(), logBuf, nil), nil)

			payload := []byte(`{"type":"update_environment_variables","variables":{"FOO":"bar"}}` + "\n")
			got := make([]byte, len(payload))
			var workers sync.WaitGroup
			t.Cleanup(func() { _ = stdinR.Close(); workers.Wait() })
			workers.Go(func() {
				if _, err := io.ReadFull(stdinR, got); err != nil {
					t.Errorf("read input: %v", err)
				}
			})

			if err := c.SendRaw(payload); err != nil {
				t.Fatal(err)
			}
			workers.Wait()
			if err := c.ReadMessages(io.NopCloser(strings.NewReader("")), nil); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("stdin = %q, want %q", got, payload)
			}
			var record struct {
				Type string          `json:"t"`
				Msg  json.RawMessage `json:"msg"`
			}
			if err := json.Unmarshal(logBuf.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record.Type != "input" || !bytes.Equal(record.Msg, bytes.TrimSuffix(payload, []byte{'\n'})) {
				t.Errorf("log record = %#v, want canonical input envelope for %s", record, payload)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			stdinR, stdinW := io.Pipe()
			_ = stdinR.Close()
			c := NewConn(t.Context(), testLogger(), stdinW, newTestRecords(t, t.Context(), DiscardLogSink{}, nil), nil)
			if err := c.SendRaw([]byte(`{"type":"test"}` + "\n")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("SendRaw = %v, want closed pipe", err)
			}
			if err := c.ReadMessages(io.NopCloser(strings.NewReader("")), nil); err != nil {
				t.Fatal(err)
			}
		})
	})

	// A nil registry serves no task-scoped MCP requests.
	request := `{"t":"mcp_request","id":"same-id","method":"tools/call","name":"echo","arguments":{}}` + "\n"
	t.Run("ReadMessages/error_closing_stdin", func(t *testing.T) {
		t.Parallel()
		closeErr := errors.New("close stdin")
		c := NewConn(t.Context(), testLogger(), &closeErrorWriter{err: closeErr}, newTestRecords(t, t.Context(), DiscardLogSink{}, nil), nil)
		if err := c.ReadMessages(io.NopCloser(strings.NewReader("")), nil); !errors.Is(err, closeErr) {
			t.Fatalf("ReadMessages = %v, want close error", err)
		}
	})
	t.Run("ReadMessages/error_independent_stdout_close_failure", func(t *testing.T) {
		t.Parallel()
		failure := errors.New("stdout close failed")
		records := newTestRecords(t, t.Context(), DiscardLogSink{}, nil)
		conn := NewConn(t.Context(), testLogger(), &closeErrorWriter{}, records, nil)
		stdout := &closeErrorReader{Reader: strings.NewReader(""), err: errors.Join(os.ErrClosed, failure)}
		if err := conn.ReadMessages(stdout, nil); !errors.Is(err, failure) {
			t.Fatalf("ReadMessages = %v, want independent close failure", err)
		}
	})
	t.Run("error_MCP_request_queue_overload", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		stdin, input := io.Pipe()
		stdout, output := io.Pipe()
		registry := &canceledRegistry{started: make(chan struct{})}
		c := NewConn(ctx, testLogger(), input, newTestRecords(t, ctx, DiscardLogSink{}, nil), registry)
		read := make(chan error, 1)
		var workers sync.WaitGroup
		workers.Go(func() { read <- c.ReadMessages(stdout, nil) })
		workers.Go(func() { _, _ = io.WriteString(output, strings.Repeat(request, 100)) })
		t.Cleanup(func() { cancel(); c.Cancel(); _ = output.Close(); _ = stdin.Close(); workers.Wait() })
		select {
		case err := <-read:
			if !errors.Is(err, ErrResponseQueueFull) || !errors.Is(err, ErrResponsesAborted) {
				t.Fatalf("MCP overload=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("MCP overload blocked stdout or shutdown")
		}
	})
	t.Run("blocked_registry_preserves_stdout_and_cancels", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		stdin, input := io.Pipe()
		stdout, output := io.Pipe()
		registry := &canceledRegistry{started: make(chan struct{})}
		messages := make(chan TimedMessage, 1)
		c := NewConn(ctx, testLogger(), input, newTestRecords(t, ctx, DiscardLogSink{}, messages), registry)
		read := make(chan error, 1)
		var workers sync.WaitGroup
		workers.Go(func() { read <- c.ReadMessages(stdout, nil) })
		t.Cleanup(func() { cancel(); c.Cancel(); _ = output.Close(); _ = stdin.Close(); workers.Wait() })
		workers.Go(func() {
			if _, err := io.WriteString(output, request+`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}}`+"\n"); err != nil {
				t.Errorf("feed stdout: %v", err)
			}
		})
		<-registry.started
		select {
		case parsed := <-messages:
			if m, ok := parsed.Message.(*TextMessage); !ok || m.Text != "working" {
				t.Fatalf("output=%#v", parsed.Message)
			}
		case <-time.After(time.Second):
			t.Fatal("MCP execution blocked stdout")
		}
		cancel()
		select {
		case err := <-read:
			if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrResponsesAborted) {
				t.Fatalf("cancel=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("MCP worker was not canceled and joined")
		}
	})

	t.Run("task routing and retry", func(t *testing.T) {
		t.Parallel()
		for _, task := range []string{"first", "second"} {
			registry := &mcptest.FakeRegistry{CallErr: errors.New("retry this task")}
			stdin, input := io.Pipe()
			stdout, output := io.Pipe()
			sink := &testLogSink{}
			messages := make(chan TimedMessage, 1)
			c := NewConn(t.Context(), testLogger(), input, newTestRecords(t, t.Context(), sink, messages), registry)
			read := make(chan error, 1)
			var workers sync.WaitGroup
			workers.Go(func() { read <- c.ReadMessages(stdout, nil) })
			t.Cleanup(func() { c.Cancel(); _ = output.Close(); _ = stdin.Close(); workers.Wait() })
			decoder := json.NewDecoder(stdin)
			if _, err := io.WriteString(output, request); err != nil {
				t.Fatal(err)
			}
			var response MCPResponseEnvelope
			if err := decoder.Decode(&response); err != nil {
				t.Fatal(err)
			}
			if response.ID != "same-id" || response.Error != "retry this task" {
				t.Fatalf("failed call = %+v", response)
			}
			registry.CallErr = nil
			registry.CallResult = &mcp.RawToolResult{Structured: mcp.TextOutput{Result: task}}
			if _, err := io.WriteString(output, request); err != nil {
				t.Fatal(err)
			}
			response = MCPResponseEnvelope{}
			if err := decoder.Decode(&response); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(response.Result), `"result":"`+task+`"`) || response.Error != "" {
				t.Fatalf("retried call = %+v", response)
			}
			if err := output.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-read; err != nil {
				t.Fatal(err)
			}
			if len(messages) != 0 || strings.Contains(sink.String(), `"t":"input"`) {
				t.Fatal("MCP controls forwarded or logged as inputs")
			}
		}
	})
	t.Run("unavailable registry", func(t *testing.T) {
		t.Parallel()
		stdin, input := io.Pipe()
		stdout, output := io.Pipe()
		c := NewConn(t.Context(), testLogger(), input, newTestRecords(t, t.Context(), &testLogSink{}, nil), nil)
		read := make(chan error, 1)
		var workers sync.WaitGroup
		workers.Go(func() { read <- c.ReadMessages(stdout, nil) })
		t.Cleanup(func() { c.Cancel(); _ = output.Close(); _ = stdin.Close(); workers.Wait() })
		if _, err := io.WriteString(output, request); err != nil {
			t.Fatal(err)
		}
		var response MCPResponseEnvelope
		if err := json.NewDecoder(stdin).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Error != "task-scoped MCP is unavailable" {
			t.Fatalf("unavailable response = %+v", response)
		}
		if err := output.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-read; err != nil {
			t.Fatal(err)
		}
	})
}

func TestMCPToolResultResponse(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		response, err := MCPToolResultResponse(mcp.RawToolResult{Structured: mcp.TextOutput{Result: "created"}})
		if err != nil {
			t.Fatalf("MCPToolResultResponse() error: %v", err)
		}
		if got, want := string(response.StructuredContent), `{"result":"created"}`; got != want {
			t.Errorf("StructuredContent = %s, want %s", got, want)
		}
		if got, want := response.Content[0].Text, "created"; got != want {
			t.Errorf("content text = %q, want %q", got, want)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		if _, err := MCPToolResultResponse(mcp.RawToolResult{Structured: math.Inf(1)}); err == nil {
			t.Fatal("MCPToolResultResponse() accepted a non-JSON result")
		}
	})
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
	t.Run("error_automatic_response_ends_process", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cmd := exec.CommandContext(ctx, "sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		stdin, input := io.Pipe()
		stdout, output := io.Pipe()
		c := NewConn(ctx, testLogger(), input, newTestRecords(t, ctx, DiscardLogSink{}, nil), nil)
		s := NewSession(ctx, cmd, c, stdout, testLogger())
		var workers sync.WaitGroup
		t.Cleanup(func() { cancel(); s.Cancel(); _ = output.Close(); _ = s.Wait(); workers.Wait() })
		if err := stdin.CloseWithError(io.ErrUnexpectedEOF); err != nil {
			t.Fatal(err)
		}
		workers.Go(func() {
			if _, err := io.WriteString(output, `{"t":"mcp_request","id":"request","method":"tools/list"}`+"\n"); err != nil {
				t.Errorf("feed MCP: %v", err)
			}
		})
		select {
		case <-s.Done():
			if err := s.Wait(); !errors.Is(err, ErrResponsesAborted) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("session failure = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("failed reply left the agent process or reader running")
		}
	})

	t.Parallel()
	t.Run("Wait", func(t *testing.T) {
		t.Parallel()
		// A harness usually leaves without dying by signal when its transport
		// closes. Only the session owner's own shutdown explains that status.
		start := func(t *testing.T) (*Session, context.CancelFunc, io.Closer) {
			// The process outlives the session's own shutdown request, so the
			// child reports the status it chooses instead of a signal.
			processCtx, stopProcess := context.WithCancel(context.WithoutCancel(t.Context()))
			t.Cleanup(stopProcess)
			cmd := exec.CommandContext(processCtx, "sh", "-c", "read line || exit 3")
			childStdin, err := cmd.StdinPipe()
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
			connStdin, connStdinW := io.Pipe()
			t.Cleanup(func() { _ = connStdin.Close(); _ = connStdinW.Close() })
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			s := NewSession(ctx, cmd, NewConn(ctx, testLogger(), connStdinW, newTestRecords(t, ctx, DiscardLogSink{}, make(chan TimedMessage)), nil), stdout, testLogger())
			t.Cleanup(func() { cancel(); s.Cancel(); _ = s.Wait() })
			return s, cancel, childStdin
		}
		t.Run("error/live session reports the exit status", func(t *testing.T) {
			t.Parallel()
			s, _, childStdin := start(t)
			if err := childStdin.Close(); err != nil {
				t.Fatal(err)
			}
			<-s.Done()
			if err := s.Wait(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
				t.Fatalf("session failure = %v, want the child's exit status", err)
			}
		})
		t.Run("valid/teardown does not report the exit status", func(t *testing.T) {
			t.Parallel()
			s, cancel, childStdin := start(t)
			cancel()
			if err := childStdin.Close(); err != nil {
				t.Fatal(err)
			}
			<-s.Done()
			// Cancellation is the shutdown's own doing and the runtime filters it;
			// the child's status must not be attributed to the agent.
			if err := s.Wait(); err != nil && strings.Contains(err.Error(), "agent exited") {
				t.Fatalf("teardown reported the child's exit status: %v", err)
			}
		})
	})
	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		t.Run("sentinel before EOF", func(t *testing.T) {
			t.Parallel()
			r, w := io.Pipe()
			stdout, output := io.Pipe()
			cmd := exec.CommandContext(t.Context(), "true")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := NewSession(t.Context(), cmd, NewConn(t.Context(), testLogger(), w, newTestRecords(t, t.Context(), DiscardLogSink{}, make(chan TimedMessage)), nil), stdout, testLogger())
			t.Cleanup(func() { s.Cancel(); _ = output.Close(); _ = s.Wait() })
			got := make(chan []byte, 1)
			var reader sync.WaitGroup
			reader.Go(func() { data, _ := io.ReadAll(r); got <- data })
			if err := s.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			reader.Wait()
			if data := <-got; !bytes.Equal(data, []byte{0, '\n'}) {
				t.Fatalf("stdin = %q, want stop sentinel", data)
			}
		})
		t.Run("blocked write", func(t *testing.T) {
			t.Parallel()
			r, w := io.Pipe()
			t.Cleanup(func() { _ = r.Close() })
			started := make(chan struct{})
			writer := &stopTestWriter{PipeWriter: w, started: started}
			ctx, cancel := context.WithCancel(t.Context())
			stdout, output := io.Pipe()
			cmd := exec.CommandContext(ctx, "sleep", "60")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := NewSession(ctx, cmd, NewConn(ctx, testLogger(), writer, newTestRecords(t, ctx, DiscardLogSink{}, make(chan TimedMessage)), nil), stdout, testLogger())
			t.Cleanup(func() { cancel(); _ = output.Close(); _ = s.Wait() })
			result := make(chan error, 1)
			var stopper sync.WaitGroup
			stopper.Go(func() { result <- s.Stop(ctx) })
			<-started
			cancel()
			stopper.Wait()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("Stop error = %v, want canceled", err)
			}
		})
	})
	t.Run("Lifecycle", func(t *testing.T) {
		t.Parallel()
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()
		msgCh := make(chan TimedMessage, 16)

		cmd := exec.CommandContext(t.Context(), "true")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		s := NewSession(t.Context(), cmd, NewConn(t.Context(), testLogger(), stdinW, newTestRecords(t, t.Context(), DiscardLogSink{}, msgCh), nil), stdoutR, testLogger())

		var workers sync.WaitGroup
		t.Cleanup(func() { s.Cancel(); _ = s.Wait(); workers.Wait() })

		stdinBuf := make(chan string, 1)
		workers.Go(func() {
			data, _ := io.ReadAll(stdinR)
			stdinBuf <- string(data)
		})

		prompt := Prompt{Text: "test prompt", Images: []v3.ImageData{{Data: "image", MediaType: "image/png"}}}
		if err := s.SendPrompt(prompt); err != nil {
			t.Fatal(err)
		}
		prompt.Images[0].Data = "changed"

		resultLine := `{"t":"agent","ts":1.000,"msg":{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"ok","session_id":"s","total_cost_usd":0.01,"usage":{},"uuid":"u"}}` + "\n"
		if _, err := stdoutW.Write([]byte(resultLine)); err != nil {
			t.Fatal(err)
		}

		select {
		case <-s.done:
			t.Fatal("session closed prematurely after result")
		default:
		}

		if err := s.SendPrompt(Prompt{Text: "follow-up"}); err != nil {
			t.Fatal(err)
		}

		if err := stdoutW.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Wait(); err != nil {
			t.Fatal(err)
		}

		got := <-stdinBuf
		if !strings.Contains(got, `"content":"test prompt"`) {
			t.Errorf("missing first prompt in stdin: %s", got)
		}
		if !strings.Contains(got, `"content":"follow-up"`) {
			t.Errorf("missing follow-up in stdin: %s", got)
		}

		close(msgCh)
		var count int
		var gotResult *ResultMessage
		for m := range msgCh {
			if IsInputDeliveryControl(m.Message) {
				continue
			}
			count++
			if rm, ok := m.Message.(*ResultMessage); ok {
				gotResult = rm
			}
			if count == 1 {
				input, ok := m.Message.(*UserInputMessage)
				if !ok || input.Text != "test prompt" || len(input.Images) != 1 || input.Images[0].Data != "image" {
					t.Fatalf("first message = %#v, want independent prompt copy", m.Message)
				}
			}
		}
		if count != 3 {
			t.Errorf("message count = %d, want 3", count)
		}
		if gotResult == nil {
			t.Fatal("expected ResultMessage in msgCh, got none")
		}
		if gotResult.Result != "ok" {
			t.Errorf("result = %q, want %q", gotResult.Result, "ok")
		}
	})
	t.Run("SendPrompt", func(t *testing.T) {
		t.Parallel()
		t.Run("error_failed_append_preserves_publication", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(ctx, "sleep", "60")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			messages := make(chan TimedMessage, 8)
			s := NewSession(ctx, cmd, NewConn(ctx, testLogger(), stdin, newTestRecords(t, ctx, rejectedLog{}, messages), nil), stdout, testLogger())
			t.Cleanup(func() { cancel(); s.Cancel(); _ = s.Wait() })
			if err := s.SendPrompt(Prompt{Text: "unrecorded"}); err == nil {
				t.Fatal("expected append failure")
			}
			if len(messages) != 0 {
				t.Fatalf("failed append published %d messages", len(messages))
			}
		})
		t.Run("error_interrupted_delivery_does_not_block_shutdown", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(ctx, "sleep", "60")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			reader, pipe := io.Pipe()
			started := make(chan struct{})
			writer := &stopTestWriter{PipeWriter: pipe, started: started}
			log := &testLogSink{}
			msgCh := make(chan TimedMessage, 2)
			s := NewSession(ctx, cmd, NewConn(ctx, testLogger(), writer, newTestRecords(t, ctx, log, msgCh), nil), stdout, testLogger())
			result := make(chan error, 1)
			var sends sync.WaitGroup
			sends.Go(func() { result <- s.SendPrompt(Prompt{Text: "follow up"}) })
			t.Cleanup(func() {
				cancel()
				_ = reader.Close()
				sends.Wait()
				_ = s.Wait()
			})
			<-started
			cancel()
			s.Cancel()
			// The full FIFO has no reader. Cancellation must still let the send
			// and Wait finish, with the failure preserved in the durable log.
			if err := <-result; !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("send = %v, want closed pipe", err)
			}
			_ = s.Wait()
			marker := <-msgCh
			if !IsInputDeliveryControl(marker.Message) {
				t.Fatalf("first = %T, want attempt boundary", marker.Message)
			}
			first := <-msgCh
			if _, ok := first.Message.(*UserInputMessage); !ok {
				t.Fatalf("first = %T, want attempted input", first.Message)
			}
			close(msgCh)
			// Calling SendPrompt after Wait must not touch the caller's closed
			// FIFO, even though both context and done are also ready.
			if err := s.SendPrompt(Prompt{Text: "too late"}); err == nil {
				t.Fatal("send after Wait succeeded")
			}
			parser, err := NewLogRecordParser(&testMessageParser{input: testParseFn, output: testParseFn})
			if err != nil {
				t.Fatal(err)
			}
			var order []string
			for line := range bytes.SplitSeq(bytes.TrimSpace(log.Bytes()), []byte{'\n'}) {
				record, err := parser.ParseRecord(line)
				if err != nil {
					t.Fatal(err)
				}
				for _, message := range record.Messages {
					order = append(order, message.Message.Type())
				}
			}
			if !slices.Equal(order, []string{"system", "user_input", "system"}) {
				t.Fatalf("durable order = %v, want one attempted input and failure", order)
			}
		})
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

		msgCh := make(chan TimedMessage, 16)
		s := NewSession(t.Context(), cmd, NewConn(t.Context(), log, stdin, newTestRecords(t, t.Context(), DiscardLogSink{}, msgCh), nil), stdout, log)

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

type stopTestWriter struct {
	*io.PipeWriter

	started chan struct{}
}

func (w *stopTestWriter) Write(p []byte) (int, error) {
	close(w.started)
	return w.PipeWriter.Write(p)
}

func TestRelayNativeRecordEncoding(t *testing.T) {
	t.Parallel()

	t.Run("record limit matches relay", func(t *testing.T) {
		t.Parallel()
		cmd := exec.CommandContext(t.Context(), "uv", "run", "--locked", "python", "-c",
			`import runpy; print(runpy.run_path("relay/relay_v2.py")["_MAX_ENCODED_RECORD_LEN"])`)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("relay record limit: %v: %s", err, out)
		}
		if string(out) != fmt.Sprintln(taskslog.MaxRecordSize) {
			t.Fatalf("relay limit = %s, Go limit = %d", out, taskslog.MaxRecordSize)
		}
	})

	t.Run("V3TerminatesWithLF", func(t *testing.T) {
		t.Parallel()
		log := &testLogSink{}
		if err := appendRelayNativeRecord(log, []byte(`{"type":"response"}`)); err != nil {
			t.Fatal(err)
		}
		got := log.Bytes()
		if got[len(got)-1] != '\n' || bytes.Contains(got, []byte(`\\n`)) {
			t.Fatalf("v3 record = %q, want one real LF terminator", got)
		}
		if !bytes.HasPrefix(got, []byte(`{"t":"agent","ts":`)) || !bytes.HasSuffix(got, append([]byte(`"msg":{"type":"response"}}`), '\n')) {
			t.Fatalf("v3 record = %q, want canonical input envelope", got)
		}
	})
}

func TestDirectionalNativeRecords(t *testing.T) {
	t.Parallel()
	log := &testLogSink{}
	encodedInput, err := (&v3.NativeRecord{Type: string(taskslog.KindInput), Time: time.Now().UTC(), Message: []byte(`{"method":"set_model"}`)}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.AppendRecord(PhysicalRecord{Encoded: append(encodedInput, '\n')}); err != nil {
		t.Fatal(err)
	}
	if err := appendRelayNativeRecord(log, []byte(`{"type":"message"}`)); err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(log.Bytes(), []byte{'\n'}), []byte{'\n'})
	if len(lines) != 2 || !bytes.HasPrefix(lines[0], []byte(`{"t":"input","ts":`)) || !bytes.HasPrefix(lines[1], []byte(`{"t":"agent","ts":`)) {
		t.Fatalf("directional records = %q", log.Bytes())
	}
	var parsedInput, parsedOutput []byte
	native := &testMessageParser{
		input: func(data []byte) ([]Message, error) {
			parsedInput = bytes.Clone(data)
			return []Message{&UserInputMessage{Text: "input"}}, nil
		},
		output: func(data []byte) ([]Message, error) {
			parsedOutput = bytes.Clone(data)
			return []Message{&TextMessage{Text: "output"}}, nil
		},
	}
	parser, err := NewLogRecordParser(native)
	if err != nil {
		t.Fatal(err)
	}
	input, err := parser.ParseRecord(lines[0])
	if err != nil || input.Provenance.Source == RecordSourceRelay {
		t.Fatalf("input = %#v, %v; want non-relay record", input, err)
	}
	if got, want := string(parsedInput), `{"method":"set_model"}`; got != want {
		t.Fatalf("input payload = %q, want %q", got, want)
	}
	relay, err := parser.ParseRecord(lines[1])
	if err != nil || relay.Provenance.Source != RecordSourceRelay {
		t.Fatalf("relay = %#v, %v; want relay record", relay, err)
	}
	if string(parsedOutput) != `{"type":"message"}` || string(parsedInput) != `{"method":"set_model"}` {
		t.Fatalf("input = %s, output = %s", parsedInput, parsedOutput)
	}
	var live []TimedMessage
	sink := &testLogSink{}
	owner := newTestRecordsWithHandler(t, t.Context(), sink, native, &testRecordHandler{publish: func(message TimedMessage) error {
		live = append(live, message)
		return nil
	}})
	if err := NewConn(t.Context(), testLogger(), &closeErrorWriter{}, owner, nil).ReadMessages(io.NopCloser(bytes.NewReader(log.Bytes())), nil); err != nil {
		t.Fatal(err)
	}
	want := slices.Concat(input.Messages, relay.Messages)
	if !reflect.DeepEqual(live, want) || !bytes.Equal(sink.Bytes(), log.Bytes()) {
		t.Fatalf("live = %#v, want %#v; persisted = %q", live, want, sink.Bytes())
	}
}

func TestMarshalLogMessage(t *testing.T) {
	t.Parallel()
	t.Run("CommonSource", func(t *testing.T) {
		t.Parallel()
		messages := []Message{
			&TextMessage{}, &UserInputMessage{}, ContextCleared(), InputDeliveryStarted(), InputDeliveryCompleted(), InputDeliveryFailed(errors.New("closed")), &LogMessage{},
			&StrippedEnvMessage{}, &DiffStatMessage{}, &ExitMessage{},
			&MetaSessionMessage{}, &RelayGenerationMessage{Generation: "g"}, &ModelInfoMessage{},
			&MetaResultMessage{}, &MetaPRMessage{}, &TurnCommitSnapshotMessage{}, &PendingUserActionMessage{},
		}
		source := MessageSource{ThreadID: "child", TurnID: "turn", ItemID: "item", RootThreadID: "root", AgentID: "native:child", DelegationToolUseID: "delegate"}
		for _, message := range messages {
			t.Run(fmt.Sprintf("%T", message), func(t *testing.T) {
				t.Parallel()
				*message.Provenance() = MessageSource{}
				absent, err := MarshalLogMessage(message)
				if err != nil {
					t.Fatal(err)
				}
				*message.Provenance() = source
				encoded, err := MarshalLogMessage(message)
				if err != nil {
					t.Fatal(err)
				}

				_, ok := reflect.TypeAssert[Message](reflect.New(reflect.TypeOf(message).Elem()))
				if !ok {
					t.Fatal("control does not implement Message")
				}
				frame, err := taskslog.DecodeFrame(encoded)
				if err != nil {
					t.Fatal(err)
				}
				outSource, ok := reflect.TypeAssert[MessageProvenance](reflect.ValueOf(frame.Control).Elem().FieldByName("MessageProvenance"))
				if !ok {
					t.Fatal("control has no source schema")
				}
				if outSource.Source != source {
					t.Errorf("v3 source lost: %+v", outSource.Source)
				}
				frame, err = taskslog.DecodeFrame(absent)
				if err != nil {
					t.Fatal(err)
				}
				outSource, ok = reflect.TypeAssert[MessageProvenance](reflect.ValueOf(frame.Control).Elem().FieldByName("MessageProvenance"))
				if !ok {
					t.Fatal("control has no source schema")
				}
				if !outSource.Source.IsZero() {
					t.Errorf("absent source = %+v", outSource.Source)
				}
			})
		}
	})
	t.Run("ParsedControlProvenance", func(t *testing.T) {
		t.Parallel()
		for _, message := range []Message{&MetaSessionMessage{SessionID: "session"}, ContextCleared(), InputDeliveryStarted(), InputDeliveryCompleted(), InputDeliveryFailed(errors.New("closed"))} {
			message.Provenance().AgentID = "native:child"
			encoded, err := MarshalLogMessage(message)
			if err != nil {
				t.Fatal(err)
			}
			parser, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) { return nil, nil }})
			if err != nil {
				t.Fatal(err)
			}
			record, err := parser.ParseRecord(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if len(record.Messages) != 1 || *record.Messages[0].Message.Provenance() != *message.Provenance() {
				t.Errorf("parsed %T source lost: %+v", message, record)
			}
		}
	})
	t.Run("TextSourceRoundTrip", func(t *testing.T) {
		t.Parallel()
		const withoutSource = `{"phase":"commentary","t":"text","text":"visible"}`
		message := &TextMessage{Text: "visible", Phase: "commentary"}
		encoded, err := MarshalLogMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != withoutSource {
			t.Errorf("absent source encoding = %s", encoded)
		}
		parser, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) { return nil, nil }})
		if err != nil {
			t.Fatal(err)
		}
		record, err := parser.ParseRecord(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Messages) != 1 {
			t.Fatalf("text record = %+v", record)
		}
		restored, ok := record.Messages[0].Message.(*TextMessage)
		if !ok || restored.Text != "visible" || restored.Phase != "commentary" || !restored.Source.IsZero() {
			t.Errorf("text message = %#v", record.Messages[0].Message)
		}
		for _, source := range []MessageSource{
			{ThreadID: "thread", TurnID: "turn", ItemID: "item", RootThreadID: "root"},
			{ThreadID: "thread", RootThreadID: "root"},
			{RootThreadID: "root"},
		} {
			message := &TextMessage{Text: "visible", Phase: "commentary", Source: source}
			encoded, err := MarshalLogMessage(message)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(encoded, []byte(`"source":`)) {
				t.Errorf("v3 text lost source: %s", encoded)
			}
			parser, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) { return nil, nil }})
			if err != nil {
				t.Fatal(err)
			}
			record, err := parser.ParseRecord(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if len(record.Messages) != 1 {
				t.Fatalf("v3 text record = %+v", record)
			}
			restored, ok := record.Messages[0].Message.(*TextMessage)
			if !ok || restored.Source != source || restored.Text != "visible" || restored.Phase != "commentary" {
				t.Errorf("v3 sourced text = %#v, want source %+v", record.Messages[0].Message, source)
			}
		}
	})

	t.Run("TextSourceValidation", func(t *testing.T) {
		t.Parallel()
		const sourced = `{"t":"text","text":"visible","source":{"threadID":"thread","rootThreadID":"root"}}`
		parser, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) { return nil, nil }})
		if err != nil {
			t.Fatal(err)
		}
		record, err := parser.ParseRecord([]byte(sourced))

		if err != nil || len(record.Messages) != 1 {
			t.Fatalf("v3 source rejected: %+v, %v", record, err)
		}
		message, ok := record.Messages[0].Message.(*TextMessage)
		if !ok || message.Source != (MessageSource{ThreadID: "thread", RootThreadID: "root"}) {
			t.Errorf("v3 partial source = %#v", record.Messages[0].Message)
		}
		for _, line := range []string{
			`{"t":"text","text":"visible","source":{"unknown":"value"}}`,
			`{"t":"text","text":"visible","source":{"threadID":123}}`,
			`{"t":"text","text":"visible","unknown":"value"}`,
		} {
			if _, err := parser.ParseRecord([]byte(line)); err == nil {
				t.Errorf("invalid v3 source/control accepted: %s", line)
			}
		}
	})

	t.Run("V3RelayGenerationRoundTrip", func(t *testing.T) {
		t.Parallel()
		data, err := MarshalLogMessage(&RelayGenerationMessage{
			MessageType: messageTypeRelayGeneration,
			Generation:  "generation-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		parser, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) { return nil, nil }})
		if err != nil {
			t.Fatal(err)
		}
		record, err := parser.ParseRecord(data)
		if err != nil {
			t.Fatal(err)
		}
		if !record.Control || record.Provenance.Source != RecordSourceRelay || record.Provenance.Generation != "generation-1" || len(record.Messages) != 0 {
			t.Fatalf("parsed relay generation = %#v", record)
		}
	})
}

func TestTurnCommitSnapshotReplay(t *testing.T) {
	t.Parallel()
	commit := v3.RepositoryCommit{
		RepositoryPath: "/home/user/src/repo",
		BranchName:     "caic-1",
		CommitHash:     "1111111111111111111111111111111111111111",
	}

	var snapshot []byte
	var err error

	snapshot, err = MarshalLogMessage(NewTurnCommitSnapshotMessage([]v3.RepositoryCommit{commit}, false, nil))
	if err != nil {
		t.Fatal(err)
	}

	var messages []Message
	var producerAt time.Time
	owner := newTestRecordsWithHandler(t, t.Context(), DiscardLogSink{}, &testMessageParser{output: func([]byte) ([]Message, error) {
		return nil, nil
	}}, &testRecordHandler{publish: func(parsed TimedMessage) error {
		messages = append(messages, parsed.Message)
		producerAt = parsed.ProducerTime
		return nil
	}})
	err = NewConn(t.Context(), testLogger(), &closeErrorWriter{}, owner, nil).ReadMessages(io.NopCloser(strings.NewReader(string(snapshot)+"\n")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	got, ok := messages[0].(*TurnCommitSnapshotMessage)
	if !ok {
		t.Fatalf("message = %T, want *TurnCommitSnapshotMessage", messages[0])
	}
	if producerAt.IsZero() {
		t.Fatal("snapshot has no producer timestamp")
	}
	if !reflect.DeepEqual(got.RepositoryCommits, []v3.RepositoryCommit{commit}) {
		t.Fatalf("snapshot = %+v, want commits %+v", got, []v3.RepositoryCommit{commit})
	}
}

func TestSemanticMessageReportedModelJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  Message
	}{
		{"init", &InitMessage{ReportedModel: "gpt-5.6"}},
		{"system", &SystemMessage{ReportedModel: "gpt-5.6"}},
		{"usage", &UsageMessage{ReportedModel: "gpt-5.6"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			if raw["model"] != "gpt-5.6" || raw["reported_model"] != nil {
				t.Fatalf("serialized model = %#v", raw)
			}
		})
	}
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
		for i, line := range lines {
			lines[i] = `{"t":"agent","ts":1.000,"msg":` + line + `}`
		}
		input := strings.Join(lines, "\n") + "\n"

		ch := make(chan Message, 16)
		owner := newTestRecordsWithHandler(t, t.Context(), DiscardLogSink{}, &testMessageParser{input: testParseFn, output: testParseFn}, &testRecordHandler{publish: func(m TimedMessage) error {
			ch <- m.Message
			return nil
		}})
		if err := NewConn(t.Context(), testLogger(), &closeErrorWriter{}, owner, nil).ReadMessages(io.NopCloser(strings.NewReader(input)), nil); err != nil {
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
		for i, line := range lines {
			lines[i] = `{"t":"agent","ts":1.000,"msg":` + line + `}`
		}
		input := strings.Join(lines, "\n") + "\n"

		ch := make(chan Message, 16)
		owner := newTestRecordsWithHandler(t, t.Context(), DiscardLogSink{}, &testMessageParser{input: testParseFn, output: testParseFn}, &testRecordHandler{publish: func(m TimedMessage) error {
			ch <- m.Message
			return nil
		}})
		if err := NewConn(t.Context(), testLogger(), &closeErrorWriter{}, owner, nil).ReadMessages(io.NopCloser(strings.NewReader(input)), nil); err != nil {
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
	t.Run("rejects unterminated physical records", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string

			input string
		}{
			{name: "native fragment", input: `{"type":"system","subtype":"init"}`},
			{name: "physical fragment", input: `{"t":"agent","ts":1.000,"msg":{}}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				stdinR, stdinW := io.Pipe()
				t.Cleanup(func() { _ = stdinR.Close() })
				log := &testLogSink{}
				got := make(chan TimedMessage, 1)
				conn := NewConn(t.Context(), testLogger(), stdinW, newTestRecords(t, t.Context(), log, got), nil)
				err := conn.ReadMessages(io.NopCloser(strings.NewReader(tc.input)), nil)
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("ReadMessages error = %v, want unexpected EOF", err)
				}
				if log.Len() != 0 || len(got) != 0 {
					t.Fatalf("persisted=%q dispatched=%d", log.String(), len(got))
				}
			})
		}
	})
	t.Run("physical structural corruption is not persisted", func(t *testing.T) {
		t.Parallel()
		log := &testLogSink{}
		var got []TimedMessage
		owner := newTestRecordsWithHandler(t, t.Context(), log, &testMessageParser{output: func([]byte) ([]Message, error) {
			return []Message{&TextMessage{}}, nil
		}}, &testRecordHandler{publish: func(msg TimedMessage) error {
			got = append(got, msg)
			return nil
		}})
		err := NewConn(t.Context(), testLogger(), &closeErrorWriter{}, owner, nil).ReadMessages(io.NopCloser(strings.NewReader(`{"t":"agent","time":1.000,"msg":{}}`+"\n")), nil)
		if err == nil || log.Len() != 0 || len(got) != 0 {
			t.Fatalf("err=%v persisted=%d dispatched=%d", err, log.Len(), len(got))
		}
	})
}

func TestRelayRecordReader(t *testing.T) {
	t.Parallel()
	t.Run("ReadRecord", func(t *testing.T) {
		t.Parallel()
		t.Run("rejects unterminated record without persistence", func(t *testing.T) {
			t.Parallel()
			log := &testLogSink{}
			owner := newTestRecords(t, t.Context(), log, nil)
			reader := NewRelayRecordReader(t.Context(), strings.NewReader(`{"t":"agent","ts":1.000,"msg":{"type":"system","subtype":"init"}}`), owner)

			if _, _, err := reader.ReadRecord(); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("ReadRecord error = %v, want unexpected EOF", err)
			}
			if log.Len() != 0 {
				t.Fatalf("persisted truncated record = %q", log.String())
			}
		})
		t.Run("large native record persists unchanged", func(t *testing.T) {
			t.Parallel()
			input := `{"t":"agent","ts":1.000,"msg":{"text":"` + strings.Repeat("x", 33<<20) + `"}}` + "\n"
			log := &testLogSink{}
			owner := newTestRecords(t, t.Context(), log, nil)
			reader := NewRelayRecordReader(t.Context(), strings.NewReader(input), owner)
			native, _, err := reader.ReadRecord()
			if err != nil {
				t.Fatal(err)
			}
			if string(native) != input[len(`{"t":"agent","ts":1.000,"msg":`):len(input)-2] {
				t.Fatal("large native bytes changed")
			}
			if log.String() != input {
				t.Fatal("large physical record changed")
			}
		})
	})
}

func TestReadRelayTailRecords(t *testing.T) {
	t.Parallel()
	const complete = `{"t":"agent","ts":1.000,"msg":{"type":"system","subtype":"init"}}`
	full := "old\npartial" + "\n" + complete + "\n"
	tail := "rtial\n" + complete + "\n"
	start := int64(len(full) - len(tail))
	timeline, offset, err := readRelayTailRecords(strings.NewReader(tail), start, true, "ctr", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline.RelayRecords) != 1 {
		t.Fatalf("timeline = %#v", timeline)
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
		timeline, offset, err := readRelayTailRecords(strings.NewReader("\n"+complete+"\n"), 10, false, "ctr", "")
		if err != nil || len(timeline.RelayRecords) != 1 || offset != 10+int64(len("\n"+complete+"\n")) {
			t.Fatalf("leading empty tail = timeline:%#v offset:%d err:%v", timeline, offset, err)
		}
	})
	t.Run("start at prior record LF retains following complete record", func(t *testing.T) {
		t.Parallel()
		tail := "\n" + complete + "\n"
		timeline, offset, err := readRelayTailRecords(strings.NewReader(tail), 10, true, "ctr", "")
		if err != nil || len(timeline.RelayRecords) != 1 || offset != 10+int64(len(tail)) {
			t.Fatalf("LF-boundary tail = timeline:%#v offset:%d err:%v", timeline, offset, err)
		}
	})
	t.Run("trailing fragment remains for attach", func(t *testing.T) {
		t.Parallel()
		completeRecord := complete + "\n"
		timeline, offset, err := readRelayTailRecords(strings.NewReader(completeRecord+`{"partial":true}`), 0, false, "ctr", "")
		if err != nil || len(timeline.RelayRecords) != 1 || offset != int64(len(completeRecord)) {
			t.Fatalf("trailing fragment = timeline:%#v offset:%d err:%v", timeline, offset, err)
		}
	})
	t.Run("native payload interpretation is deferred", func(t *testing.T) {
		t.Parallel()
		// Valid JSON is transferable before a provider can interpret it in its
		// complete request context. Preserve even a non-object native payload;
		// the final ordered history owns provider schema validation.
		input := `{"t":"agent","ts":1.000,"msg":["provider payload"]}` + "\n"
		timeline, _, err := readRelayTailRecords(strings.NewReader(input), 0, false, "ctr", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(timeline.RelayRecords) != 1 {
			t.Fatalf("timeline = %#v, want one empty native boundary", timeline)
		}
		if boundary := timeline.RelayRecords[0]; boundary.RelayEnd != int64(len(input)) {
			t.Fatalf("boundary = %#v, want relay end %d", boundary, len(input))
		}
		if string(timeline.Encoded) != input {
			t.Fatalf("snapshot = %q, want original bytes %q", timeline.Encoded, input)
		}
	})
	t.Run("invalid physical records are rejected", func(t *testing.T) {
		t.Parallel()
		for _, line := range []string{
			`{"t":"agent","ts":1.000,"msg":{"invalid":}}`,
			`{"t":"agent","ts":1,"msg":{}}`,
			`{"t":"relay_generation","generation":""}`,
			`{"t":"result","state":123}`,
		} {
			t.Run(line, func(t *testing.T) {
				t.Parallel()
				timeline, _, err := readRelayTailRecords(strings.NewReader(line+"\n"), 0, false, "ctr", "")
				if err == nil || len(timeline.Encoded) != 0 || len(timeline.RelayRecords) != 0 {
					t.Fatalf("snapshot = %+v, error = %v; want rejection before retention", timeline, err)
				}
			})
		}
	})
	t.Run("generation propagates to following boundaries", func(t *testing.T) {
		t.Parallel()
		marker := `{"t":"relay_generation","generation":"generation-1"}` + "\n"
		native := `{"t":"agent","ts":1.000,"msg":{"type":"ignored"}}` + "\n"
		timeline, _, err := readRelayTailRecords(strings.NewReader(marker+native), 0, false, "ctr", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(timeline.RelayRecords) != 2 {
			t.Fatalf("timeline = %#v, want marker and native boundaries ", timeline)
		}
		for i, boundary := range timeline.RelayRecords {
			if boundary.Generation != "generation-1" {
				t.Fatalf("boundary %d generation = %q, want generation-1", i, boundary.Generation)
			}
		}
	})

	t.Run("corruption and oversized records do not persist", func(t *testing.T) {
		t.Parallel()
		for _, input := range []string{
			`{"t":"agent","time":1.000,"msg":{}}` + "\n",
			`{"t":"agent","ts":1.000,"msg":"` + strings.Repeat("x", taskslog.MaxRecordSize) + `"}` + "\n",
		} {
			log := &testLogSink{}
			owner := newTestRecords(t, t.Context(), log, nil)
			reader := NewRelayRecordReader(t.Context(), strings.NewReader(input), owner)

			if _, _, err := reader.ReadRecord(); err == nil {
				t.Fatal("ReadRecord error = nil")
			}
			if log.Len() != 0 {
				t.Fatalf("persisted invalid physical record = %d bytes", log.Len())
			}
		}
	})

	t.Run("fingerprint excludes the trailing newline", func(t *testing.T) {
		t.Parallel()
		// The adoption overlap filter compares this value against fingerprints
		// stored in the task log, so the basis is one record line without its
		// newline: hashing the encoded record instead silently broke overlap
		// detection while leaving every synthetic test consistent with itself.
		line := `{"t":"agent","ts":1.000,"msg":{"type":"text","text":"hi"}}`
		timeline, _, err := readRelayTailRecords(strings.NewReader(line+"\n"), 0, false, "ctr", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(timeline.RelayRecords) != 1 {
			t.Fatalf("relay records = %d, want 1", len(timeline.RelayRecords))
		}
		got := timeline.RelayRecords[0].Fingerprint
		if want := sha256.Sum256([]byte(line)); got != want {
			t.Fatalf("fingerprint = %x, want sha256 of the line without the newline %x", got, want)
		}
		if withNewline := sha256.Sum256([]byte(line + "\n")); got == withNewline {
			t.Fatal("fingerprint hashed the newline-terminated record")
		}
	})
}

func TestRelayGenerationCommand(t *testing.T) {
	t.Parallel()
	cmd := relayGenerationCommand(t.Context(), "container")
	want := []string{"ssh", "container", "head", "-c", "4096", RelayOutputPath}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("relay generation command args = %#v, want %#v", cmd.Args, want)
	}
}

func TestLogRecordParser(t *testing.T) {
	t.Run("DecodeHistoryFrame", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			middle string
			want   float64
		}{
			{"adjacent_input", `{"t":"input","ts":100.125,"msg":{"type":"user_input","text":"hello"}}`, 100.125},
			{"intervening_record", `{"t":"log","line":"unrelated"}`, 0},
			{"child_input", `{"t":"user_input","ts":100.125,"text":"child","source":{"agentID":"child"}}`, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				parser, err := NewLogRecordParser(&testMessageParser{input: func([]byte) ([]Message, error) { return []Message{&UserInputMessage{Text: "hello"}}, nil }})
				if err != nil {
					t.Fatal(err)
				}
				var got []ParsedRecord
				for i, line := range []string{
					`{"t":"input_delivery","subtype":"input_delivery_started","session_id":"s","uuid":"i"}`,
					tc.middle,
					`{"t":"input_delivery","subtype":"input_delivery_completed","session_id":"s","uuid":"i"}`,
				} {
					frame, err := taskslog.DecodeFrame([]byte(line))
					if err != nil {
						t.Fatal(err)
					}
					records, err := parser.DecodeHistoryFrame(&frame, uint64(i+1))
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, records...)
				}
				got = append(got, parser.FlushHistory()...)
				if len(got) != 3 {
					t.Fatalf("records = %+v", got)
				}
				for _, expected := range []struct {
					index int
					id    uint64
				}{{0, 1}, {2, 3}} {
					index := expected.index
					m, ok := got[index].Messages[0].Message.(*SystemMessage)
					if !ok || m.Ts != tc.want || m.SessionID != "s" || m.UUID != "i" {
						t.Fatalf("control = %+v", m)
					}
					if got[index].ID != expected.id {
						t.Fatalf("record ID = %d", got[index].ID)
					}
					at := got[index].Messages[0].ProducerTime
					if tc.want == 0 && !at.IsZero() || tc.want != 0 && at != time.UnixMilli(100125).UTC() {
						t.Fatalf("time = %s", at)
					}
				}
			})
		}
		t.Run("unmatched_start", func(t *testing.T) {
			t.Parallel()
			parser, err := NewLogRecordParser(&testMessageParser{})
			if err != nil {
				t.Fatal(err)
			}
			frame, err := taskslog.DecodeFrame([]byte(`{"t":"input_delivery","subtype":"input_delivery_started","session_id":"","uuid":""}`))
			if err != nil {
				t.Fatal(err)
			}
			records, err := parser.DecodeHistoryFrame(&frame, 7)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 0 {
				t.Fatal("untimed start was not deferred")
			}
			records = parser.FlushHistory()
			if len(records) != 1 || records[0].ID != 7 || !records[0].Messages[0].ProducerTime.IsZero() {
				t.Fatalf("unmatched = %+v", records)
			}
			if len(parser.FlushHistory()) != 0 {
				t.Fatal("flush duplicated record")
			}
		})
	})
	t.Run("ResultCacheLifetimeBuckets", func(t *testing.T) {
		t.Parallel()
		parser, err := NewLogRecordParser(&testMessageParser{input: testParseFn, output: testParseFn})
		if err != nil {
			t.Fatal(err)
		}
		record, err := parser.ParseRecord([]byte(`{"t":"result","state":"purged","cache_creation_input_tokens":110,"cache_writes":[{"ttl_seconds":0,"tokens":10},{"ttl_seconds":3600,"tokens":100}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Messages) != 1 {
			t.Fatalf("messages = %+v", record.Messages)
		}
		m, ok := record.Messages[0].Message.(*MetaResultMessage)
		want := []v3.CacheWrite{{Tokens: 10}, {TTLSeconds: 3600, Tokens: 100}}
		if !ok || !reflect.DeepEqual(m.CacheWrites, want) {
			t.Fatalf("canonical cache writes = %+v", m)
		}
		encoded, err := MarshalLogMessage(m)
		if err != nil {
			t.Fatal(err)
		}
		var got, expected any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(`{"t":"result","state":"purged","cache_creation_input_tokens":110,"cache_writes":[{"ttl_seconds":0,"tokens":10},{"ttl_seconds":3600,"tokens":100}]}`), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("rewritten result = %s", encoded)
		}
	})
	t.Run("ChildContextDoesNotInheritRootSnapshot", func(t *testing.T) {
		t.Parallel()
		p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
			return []Message{&UsageMessage{Source: MessageSource{AgentID: "child"}}}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.ParseRecord([]byte(`{"t":"model_info","context_window":1000000}`)); err != nil {
			t.Fatal(err)
		}
		r, err := p.ParseRecord([]byte(`{"t":"agent","ts":1.000,"msg":{}}`))
		if err != nil || len(r.Messages) != 1 {
			t.Fatalf("child usage: %+v %v", r, err)
		}
		if m, ok := r.Messages[0].Message.(*UsageMessage); !ok || m.ContextWindow != 0 {
			t.Fatalf("unknown child context inherited root snapshot: %+v", r.Messages[0].Message)
		}
	})
	t.Parallel()

	t.Run("Constructor", func(t *testing.T) {
		t.Parallel()
		if _, err := NewLogRecordParser(nil); err == nil || !strings.Contains(err.Error(), "native message parser is nil") {
			t.Fatalf("nil parser error = %v", err)
		}
	})

	t.Run("ControlClassificationOnErrors", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string

			line string
		}{
			{name: "invalid metadata", line: `{"t":"caic_meta","version":"bad"}`},
			{name: "invalid model info", line: `{"t":"model_info","unexpected":true}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				p, err := NewLogRecordParser(&testMessageParser{input: testParseFn, output: testParseFn})
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

	t.Run("ContextCleared", func(t *testing.T) {
		t.Parallel()
		parser, err := NewLogRecordParser(&testMessageParser{input: testParseFn, output: testParseFn})
		if err != nil {
			t.Fatal(err)
		}
		record, err := parser.ParseRecord([]byte(`{"t":"context_cleared"}`))
		if err != nil || !record.Control || record.Provenance.Source == RecordSourceRelay || len(record.Messages) != 1 {
			t.Fatalf("context-cleared = %#v, %v", record, err)
		}
		if msg, ok := record.Messages[0].Message.(*SystemMessage); !ok || msg.Subtype != "context_cleared" {
			t.Fatalf("message = %#v", record.Messages[0].Message)
		}
	})

	t.Run("V3AgentPayloads", func(t *testing.T) {
		t.Parallel()
		var got [][]byte
		p, err := NewLogRecordParser(&testMessageParser{output: func(line []byte) ([]Message, error) {
			got = append(got, append([]byte(nil), line...))
			return []Message{&RawMessage{MessageType: "native", Raw: append([]byte(nil), line...)}}, nil
		}})
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

	t.Run("V3Rejections", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			line string
			want string
		}{
			{name: "bare_native", line: `{"type":"assistant"}`, want: "top-level type discriminator"},
			{name: "top-level type discriminator", line: `{"type":"caic_diff_stat","diff_stat":[]}`, want: "top-level type discriminator"},
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
				p, err := NewLogRecordParser(&testMessageParser{input: testParseFn, output: testParseFn})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := p.ParseRecord([]byte(tc.line)); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want substring %q", err, tc.want)
				}
			})
		}
	})

	t.Run("V3RejectsInvalidUTF8", func(t *testing.T) {
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
				p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
					called = true
					return nil, nil
				}})
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
	})

	t.Run("ModelContextSnapshot", func(t *testing.T) {
		t.Parallel()

		p, err := NewLogRecordParser(&testMessageParser{output: func(line []byte) ([]Message, error) {
			var wire struct {
				ContextWindow int `json:"context_window"`
			}
			if err := json.Unmarshal(line, &wire); err != nil {
				return nil, err
			}
			return []Message{&UsageMessage{ContextWindow: wire.ContextWindow}}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		native := func(contextWindow int) string {
			payload := fmt.Sprintf(`{"type":"usage","context_window":%d}`, contextWindow)

			return fmt.Sprintf(`{"t":"agent","ts":10.000,"msg":%s}`, payload)
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
		if record, err := p.ParseRecord([]byte(`{"t":"model_info","context_window":1000000}`)); err != nil || !record.Control || len(record.Messages) != 0 {
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

	t.Run("NativeParserRejectsNilMessagesAtomically", func(t *testing.T) {
		t.Parallel()
		action := &PendingUserActionMessage{
			MessageType: PendingUserActionMessageType,
			Action: v3.PendingUserAction{
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
				p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
					return batch, nil
				}})
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
			p, err := NewLogRecordParser(&testMessageParser{output: func([]byte) ([]Message, error) {
				return batch, nil
			}})
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

		parser, err := NewLogRecordParser(&testMessageParser{output: parseNative})
		if err != nil {
			t.Fatal(err)
		}
		mixed, err := parser.ParseRecord([]byte(`{"t":"agent","ts":123.250,"msg":{"kind":"mixed"}}`))
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
		if mixed.Provenance.Source != RecordSourceRelay {
			t.Fatal("native v3 record is not classified as relay-owned")
		}
		fresh, err := NewLogRecordParser(&testMessageParser{output: parseNative})
		if err != nil {
			t.Fatal(err)
		}
		reparsed, err := fresh.ParseRecord([]byte(`{"t":"agent","ts":123.250,"msg":{"kind":"mixed"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if reparsed.Provenance.Source != RecordSourceRelay {
			t.Fatal("reparsed native v3 record is not relay-owned")
		}
		nextRecord, err := fresh.ParseRecord([]byte(`{"t":"agent","ts":123.251,"msg":{"kind":"mixed"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if nextRecord.Provenance.Source != RecordSourceRelay {
			t.Fatal("next native v3 record is not relay-owned")
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

		empty, err := parser.ParseRecord([]byte(`{"t":"agent","ts":124.000,"msg":{"kind":"empty"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if empty.Control || len(empty.Messages) != 0 || empty.Provenance.Source != RecordSourceRelay {
			t.Fatalf("empty native record = %#v", empty)
		}

		parsed := TimedMessage{Message: &TextMessage{Text: "text"}, ProducerTime: wantTime}
		if _, ok := any(parsed).(Message); ok {
			t.Fatal("TimedMessage value implements Message")
		}
		if _, ok := any(&parsed).(Message); ok {
			t.Fatal("*TimedMessage implements Message")
		}
	})
}

// TestAttachRelaySession rejects unavailable or corrupt recorded history before
// starting SSH, including at relay offset zero.
func TestRecordsReplayRequiresAuthoritativeHistory(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"required", "missing", "empty", "not_metadata", "malformed", "partial", "canceled"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "history.jsonl")
			header := `{"t":"caic_meta","version":3,"prompt":"test","harness":"claude"}` + "\n"
			data := header
			switch name {
			case "required":
				path = ""
			case "empty":
				data = ""
			case "not_metadata":
				data = `{"t":"agent","ts":1.000,"msg":{}}` + "\n"
			case "malformed":
				data += "invalid\n"
			case "partial":
				data += `{"t":"agent","ts":1.000,"msg":{}}`
			}
			if name != "required" && name != "missing" {
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			if name == "canceled" {
				cancel()
			}
			owner := newTestRecords(t, ctx, &testLogSink{}, nil)
			err := owner.Replay(ctx, path)
			if err == nil {
				t.Fatal("invalid history was accepted")
			}

			if name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context cancellation", err)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	t.Run("Add", func(t *testing.T) {
		t.Parallel()
		u := Usage{CacheCreationInputTokens: 30, CacheTTLSeconds: 3600}
		u.Add(Usage{InputTokens: 10, CacheCreationInputTokens: 100, CacheWrites: tokens.CacheWrites{{TTL: 5 * time.Minute, Tokens: 80}, {TTL: time.Hour, Tokens: 20}}, CacheTTLSeconds: 300})
		u.Add(Usage{OutputTokens: 5, ReasoningOutputTokens: 3, CacheReadInputTokens: 40, CacheCreationInputTokens: 10})
		want := Usage{InputTokens: 10, OutputTokens: 5, ReasoningOutputTokens: 3, CacheReadInputTokens: 40, CacheCreationInputTokens: 140, CacheWrites: tokens.CacheWrites{{Tokens: 10}, {TTL: 5 * time.Minute, Tokens: 80}, {TTL: time.Hour, Tokens: 50}}}
		if !u.Equal(want) {
			t.Fatalf("aggregate = %+v, want %+v", u, want)
		}
		five, hour := u.CacheWriteBuckets().Count(5*time.Minute), u.CacheWriteBuckets().Count(time.Hour)
		if five != 80 || hour != 50 {
			t.Fatalf("write buckets = %d/%d, want 80/50", five, hour)
		}
	})
}

// testMessageParser supplies independently controlled protocol directions.
type testMessageParser struct {
	input  func([]byte) ([]Message, error)
	output func([]byte) ([]Message, error)
}

func (*testMessageParser) EncodePrompt(Prompt) ([]byte, error) { return []byte("{}\n"), nil }

func (p *testMessageParser) ParseInput(line []byte) ([]Message, error) {
	if p.input == nil {
		return nil, nil
	}
	return p.input(line)
}
func (p *testMessageParser) ParseOutput(line []byte) ([]Message, error) {
	if p.output == nil {
		return nil, nil
	}
	return p.output(line)
}

// rejectedLog models a journal that cannot accept the attempted record.
type rejectedLog struct{ DiscardLogSink }

func (rejectedLog) AppendRecord(PhysicalRecord) error { return errors.New("journal unavailable") }
func (rejectedLog) AppendMessage(Message) error       { return errors.New("journal unavailable") }

// testRecordHandler models a non-writing task fold or an explicit test observer.
type testRecordHandler struct {
	consumer context.Context
	messages chan<- TimedMessage
	publish  func(TimedMessage) error
}

func (*testRecordHandler) RecordAppended(PhysicalRecord) {}
func (*testRecordHandler) PrepareRecords(context.Context, []TimedMessage) ([]Message, error) {
	return nil, nil
}
func (h *testRecordHandler) ApplyRecords(ctx context.Context, messages []TimedMessage) error {
	for _, event := range messages {
		if h.publish != nil {
			if err := h.publish(event); err != nil {
				return err
			}
		}
		if h.messages != nil {
			select {
			case h.messages <- event:
			case <-h.consumer.Done():
				return h.consumer.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

func newTestRecords(t testing.TB, ctx context.Context, sink LogSink, messages chan<- TimedMessage) *Records {
	return newTestRecordsWithHandler(t, ctx, sink, testWire{}, &testRecordHandler{consumer: ctx, messages: messages})
}
func newTestRecordsWithHandler(t testing.TB, ctx context.Context, sink LogSink, wire WireFormat, handler RecordHandler) *Records {
	owner, err := NewRecords(context.WithoutCancel(ctx), wire, sink, handler)
	if err != nil {
		t.Fatal(err)
	}
	// Each operation asserts its terminal error; Close can repeat that failure.
	t.Cleanup(func() { _ = owner.Close() })
	return owner
}

// BenchmarkWarmHistoryReplay measures the per-reconnect host cost of replaying
// the authoritative recorded task history. Files are opened and streamed through
// the production warm path on each iteration; the wire is deliberately cheap to
// isolate record dispatch and I/O. The 128 MiB case captures long-task latency
// beyond the relay adoption snapshot bound without retaining the whole file.
func BenchmarkWarmHistoryReplay(b *testing.B) {
	for _, size := range []int64{1 << 20, 10 << 20, 128 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			stream := warmHistoryStream(b, size)
			path := filepath.Join(b.TempDir(), "history.jsonl")
			header := []byte(`{"t":"caic_meta","version":3,"prompt":"benchmark","harness":"claude"}` + "\n")
			if err := os.WriteFile(path, append(header, stream...), 0o600); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				handler := &warmHistoryObserver{}
				owner, err := NewRecords(b.Context(), &warmHistoryWire{}, DiscardLogSink{}, handler)
				if err != nil {
					b.Fatal(err)
				}
				replayErr := owner.Replay(b.Context(), path)
				closeErr := owner.Close()
				if replayErr != nil || closeErr != nil {
					b.Fatalf("replay: %v; close: %v", replayErr, closeErr)
				}
				if handler.calls != 0 {
					b.Fatalf("replay invoked %d live callbacks", handler.calls)
				}
			}
		})
	}
}

func TestRelayRecordAdmission(t *testing.T) {
	t.Parallel()
	valid := []byte("{\"t\":\"agent\",\"ts\":1.000,\"msg\":{\"text\":\"hello\"}}\n")
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"missing newline", bytes.TrimSuffix(valid, []byte("\n"))},
		{"multiple records", append(bytes.Clone(valid), valid...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sink := &testLogSink{}
			owner := newTestRecordsWithHandler(t, t.Context(), sink, &testMessageParser{}, &testRecordHandler{consumer: t.Context()})
			if result := owner.submit(t.Context(), &recordWork{op: recordRelay, data: tc.data}); result.err == nil {
				t.Fatal("invalid relay record was accepted")
			}
			if sink.Len() != 0 {
				t.Fatal("invalid relay record was persisted")
			}
		})
	}
}

// BenchmarkLiveRelayRecord measures live record dispatch for large tool output.
func BenchmarkLiveRelayRecord(b *testing.B) {
	data := fmt.Appendf(nil, "{\"t\":\"agent\",\"ts\":1.000,\"msg\":{\"text\":%q}}\n", strings.Repeat("x", 8<<20))
	owner := newTestRecordsWithHandler(b, b.Context(), DiscardLogSink{}, &testMessageParser{}, &testRecordHandler{consumer: b.Context()})
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if result := owner.submit(b.Context(), &recordWork{op: recordRelay, data: data}); result.err != nil {
			b.Fatal(result.err)
		}
	}
}

// warmHistoryStream builds a canonical v3 agent-record stream of at most size
// bytes, ending on a whole record.
func warmHistoryStream(b *testing.B, size int64) []byte {
	b.Helper()
	var buf bytes.Buffer
	line := []byte(`{"t":"agent","ts":1.000,"msg":{"type":"native_subagent","subagent":{"id":"codex:thread:test","status":"running"}}}` + "\n")
	for int64(buf.Len()+len(line)) <= size {
		buf.Write(line)
	}
	return buf.Bytes()
}

// warmHistoryWire is the cheapest possible wire format: it accepts every record,
// so the benchmark measures the record envelope and dispatch cost rather than a
// particular harness parser.
type warmHistoryWire struct{}

func (*warmHistoryWire) ParseOutput(line []byte) ([]Message, error) { return nil, nil }

func (*warmHistoryWire) EncodePrompt(Prompt) ([]byte, error) { return []byte("{}\n"), nil }

func (*warmHistoryWire) ParseInput([]byte) ([]Message, error) { return nil, nil }

// warmHistoryObserver counts every live callback; historical replay must invoke none.
type warmHistoryObserver struct{ calls int }

func (h *warmHistoryObserver) RecordAppended(PhysicalRecord) { h.calls++ }
func (h *warmHistoryObserver) PrepareRecords(context.Context, []TimedMessage) ([]Message, error) {
	h.calls++
	return nil, nil
}
func (h *warmHistoryObserver) ApplyRecords(context.Context, []TimedMessage) error {
	h.calls++
	return nil
}
