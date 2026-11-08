// Package agent owns coding-agent sessions, directional history, ordered inputs, and bounded protocol replies.
// Backend implementations live in sub-packages (e.g. agent/claudecode).
//
// # Ordered recording
//
// Each execution creates Records before provisioning. Its bounded worker owns
// the wire, journal, ordered interpretation, and publication. Transport readers
// commit exact physical records before the provider sees them. Input delivery
// commits the attempted command before a separate bounded worker writes stdin.
// Runtime handlers return derived controls before publishing a completed turn;
// they never write through a competing journal or reenter the owner.
//
// Conn owns transport readers, input delivery, and bounded automatic replies.
// Provider interceptors return response bytes without writing transport.
//
// # Relay shutdown protocol
//
// Each agent runs inside a container behind a relay daemon (relay.py) that
// survives SSH disconnects. Graceful shutdown uses a null-byte (\x00)
// sentinel written to stdin:
//
// Flow 1 — One task is purged (user action or container death):
//
//	Server calls Checkout.Cleanup → Session.Stop writes \x00\n then closes
//	stdin → attach_client forwards sentinel through Unix socket, sees stdin
//	EOF, exits → _client_reader sets shutdown_event → _shutdown_watchdog
//	closes proc.stdin, sends SIGINT, escalates to SIGTERM/SIGKILL →
//	reader_thread sees stdout EOF → server kills container.
//
// Flow 2 — Backend restarts (upgrade, crash):
//
//	SSH connections are severed → attach_client sees stdin EOF and disconnects
//	(no \x00 sent) → relay daemon + agent keep running → on restart, server
//	discovers the container during task import, reconciles output.jsonl into
//	the task log, restores the fresh wire from recorded directional history,
//	and calls relay.py attach --offset N to reconnect.
package agent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/taskslog"

	"github.com/maruel/gomode/mcp"

	"github.com/caic-xyz/caic/backend/internal/agent/relay"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// Prompt bundles user text with optional images for a single interaction.
type Prompt struct {
	Text   string         `json:"text"`
	Images []v3.ImageData `json:"images,omitempty,omitzero"`
}

// Options configures an agent session launch.
type Options struct {
	Logger          *slog.Logger // Non-nil session logger.
	Target          runtime.ConnectionTarget
	Dir             string // Working directory inside the runtime.
	Model           string // Model alias ("opus", "sonnet", "haiku", "fable") or full ID. Empty = default.
	Effort          string // Thinking effort (e.g. "low", "medium", "high", "max"). Empty = default.
	InitialPrompt   Prompt // Initial prompt; never mutated after creation.
	ResumeSessionID string
	RelayOffset     int64 // Byte offset into relay output.jsonl for AttachRelay.
	// PendingUserActions is the restored user-facing work that still needs
	// input after AttachRelay reconnects. It must not contain backend-only
	// protocol state such as keepalive, auto-allow, or environment control
	// messages.
	PendingUserActions []v3.PendingUserAction
	Records            *Records     // Required ordered record owner; created before any startup output.
	StripEnv           []string     // Env var names for relay to strip from subprocess and emit as caic_stripped_env.
	MCP                mcp.Registry // Optional task-scoped CAIC MCP registry.
}

// MCPRequest is one task-local MCP bridge request.
type MCPRequest struct {
	ID        string          `json:"id"`
	Method    mcp.Method      `json:"method"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// MCPTextContent is one text block in a relayed MCP tool result.
type MCPTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MCPToolResponse is the MCP-compatible result returned through the task relay.
type MCPToolResponse struct {
	Content           []MCPTextContent `json:"content"`
	IsError           bool             `json:"isError"`
	StructuredContent json.RawMessage  `json:"structuredContent,omitempty"`
}

// MCPResponseEnvelope carries one task-local MCP response through the relay.
type MCPResponseEnvelope struct {
	Type   string          `json:"t"`
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// MCPToolsListResponse is the result of a task-local MCP tools/list request.
type MCPToolsListResponse struct {
	Tools []mcp.ToolDescriptor `json:"tools"`
}

// mcpRelayEnvelope identifies a task-local MCP relay control record.
type mcpRelayEnvelope struct {
	Type string `json:"t"`
}

// WireFormat defines the wire protocol for a backend's stdin/stdout
// communication. Implementations must pair prompt encoding and MessageParser
// for the same protocol.
type WireFormat interface {
	MessageParser

	// EncodePrompt returns one complete LF-terminated provider command.
	EncodePrompt(p Prompt) ([]byte, error)
}

// MessageParser decodes both directions of one ordered provider protocol.
// Methods run synchronously; their input may alias scanner-owned memory and
// must not be retained after returning. One line can yield multiple messages.
type MessageParser interface {
	// ParseInput decodes a recorded command sent to the provider.
	ParseInput(line []byte) ([]Message, error)
	// ParseOutput decodes a line received from the provider.
	ParseOutput(line []byte) ([]Message, error)
}

// timedParser optionally receives the producer time of the record it is about
// to decode. A parser whose messages carry durations must use it instead of the
// wall clock, so decoding recorded history reproduces the live projection.
type timedParser interface {
	SetProducerTime(at time.Time)
}

// RequestedEffort is an optional interface for WireFormat implementations that
// encode the requested reasoning effort into their outgoing requests.
type RequestedEffort interface {
	SetRequestedEffort(effort string)
}

// ApplyRequestedEffort records effort on wire when its backend encodes one and
// returns wire unchanged otherwise.
func ApplyRequestedEffort(wire WireFormat, effort string) WireFormat {
	if effortWire, ok := wire.(RequestedEffort); ok {
		effortWire.SetRequestedEffort(effort)
	}
	return wire
}

// CompactCommand is an optional interface for WireFormat implementations that
// support context compaction. Encoding errors reject the request before recording
// or writing native input; the connection remains usable for subsequent requests.
type CompactCommand interface {
	EncodeCompact(instructions string) ([]byte, error)
}

// ErrCompactInstructionsUnsupported reports that native compaction cannot honor
// custom instructions. The request is rejected without sending a native command.
var ErrCompactInstructionsUnsupported = errors.New("custom compaction instructions are not supported")

// MessageInterceptor handles provider controls on the stdout reader. It must
// never write stdin: returned response bytes are delivered by the reader's
// bounded response worker. handled suppresses normal message publication.
type MessageInterceptor interface {
	HandleMessage(m Message) (response []byte, handled bool, err error)
}

// Conn handles wire-format I/O for a single agent session. It is safe for
// concurrent use. Backends can wrap a Conn to intercept messages (e.g.
// injecting control commands after initialization).
type Conn interface {
	// SendPrompt delivers a user prompt with durable delivery outcomes. A nil
	// data uses the wire encoder; non-nil data is already provider-encoded.
	SendPrompt(p Prompt, data []byte) error
	// RecordInputFailure makes a failed attempt durable when a wrapper rejected
	// it before the connection's regular prompt writer ran.
	RecordInputFailure(p Prompt, err error) error
	// SendRaw writes pre-encoded NDJSON bytes to the agent's stdin.
	SendRaw(data []byte) error
	// SendCompact sends a compact/context-reduction command.
	SendCompact(instructions string) error
	// ReadMessages runs the message read loop: reads NDJSON lines from r,
	// commits them through Records, and publishes the resulting messages.
	// It owns r and closes both pipes before joining its workers.
	// It must run exactly once per connection.
	// A nil interceptor leaves provider messages unchanged.
	ReadMessages(r io.ReadCloser, intercept MessageInterceptor) error
	// SendStop sends the null-byte sentinel to trigger graceful agent shutdown.
	// Cancel interrupts a blocked SendStop through the message reader.
	SendStop(ctx context.Context)
	// Cancel requests message-reader shutdown. ReadMessages owns closing the
	// pipes and reports their close errors when it returns.
	Cancel()
}

// conn is the default Conn implementation.
type conn struct {
	ctx         context.Context
	logger      *slog.Logger
	stdin       io.WriteCloser
	records     *Records
	mcp         mcp.Registry
	interrupted <-chan struct{}
	cancelRead  context.CancelFunc
	inputs      chan *inputWork
	inputDone   chan struct{}
	workers     sync.WaitGroup
}

func (c *conn) SendPrompt(p Prompt, data []byte) error {
	return c.sendInput(c.ctx, &inputWork{op: inputPrompt, prompt: p, data: data})
}

func (c *conn) RecordInputFailure(p Prompt, err error) error {
	if _, ok := errors.AsType[*inputDeliveryError](err); ok {
		return err
	}
	result := c.records.submit(c.ctx, &recordWork{op: recordPrompt, prompt: p, failure: err})
	if !errors.Is(result.err, err) {
		return &inputDeliveryError{Err: errors.Join(err, result.err)}
	}
	return &inputDeliveryError{Err: result.err}
}

func (c *conn) SendRaw(data []byte) error {
	return c.sendInput(c.ctx, &inputWork{op: inputRaw, data: bytes.Clone(data)})
}

func (c *conn) SendCompact(instructions string) error {
	return c.sendInput(c.ctx, &inputWork{op: inputCompact, text: instructions})
}

// ReadMessages owns the transport pipes and joins bounded input/reply workers.
// The record owner outlives this transport and is closed by its runtime owner.
func (c *conn) ReadMessages(r io.ReadCloser, intercept MessageInterceptor) error {
	ctx, cancel := context.WithCancel(c.ctx)
	q := responseQueue{items: make(chan responseWork, maxPendingResponses)}
	requests := responseQueue{items: make(chan responseWork, maxPendingResponses)}
	reads := make(chan error, 1)
	replies := make(chan error, 1)
	mcpDone := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() { replies <- q.run(ctx, c) })
	workers.Go(func() { mcpDone <- requests.runMCP(ctx, c, &q) })
	workers.Go(func() {
		reader := bufio.NewReaderSize(r, 1<<20)
		for {
			data, err := readNDJSONRecord(reader)
			if errors.Is(err, io.EOF) {
				reads <- nil
				return
			}
			if err != nil {
				reads <- err
				return
			}
			if len(data) == 1 {
				continue
			}
			result := c.records.submit(ctx, &recordWork{op: recordRelay, data: data, disposition: func(parsed TimedMessage) (bool, error) {
				if request, ok := parsed.Message.(*MCPRequestMessage); ok {
					err := requests.enqueue(responseWork{request: &MCPRequest{ID: request.ID, Method: mcp.Method(request.Method), Name: request.Name, Arguments: request.Arguments}})
					return true, err
				}
				if intercept == nil {
					return false, nil
				}
				response, handled, err := intercept.HandleMessage(parsed.Message)
				if err != nil {
					return false, err
				}
				if len(response) > 0 {
					err = q.enqueue(responseWork{data: response})
				}
				return handled, err
			}})
			if result.err != nil {
				reads <- result.err
				return
			}
		}
	})
	var readErr, replyErr, mcpErr error
	readEnded, replyEnded, mcpEnded := false, false, false
	select {
	case readErr = <-reads:
		readEnded = true
	case replyErr = <-replies:
		replyEnded = true
	case mcpErr = <-mcpDone:
		mcpEnded = true
	case <-c.interrupted:
	case <-c.records.Done():
		readErr = context.Cause(c.records.ctx)
	}
	q.mu.Lock()
	q.stopped = true
	q.mu.Unlock()
	requests.mu.Lock()
	requests.stopped = true
	requests.mu.Unlock()
	cancel()
	c.cancelRead()
	closeErr := c.stdin.Close()
	readCloseErr := r.Close()
	workers.Wait()
	c.workers.Wait()
	if !readEnded {
		err := <-reads
		if !expectedStreamEnd(err) {
			readErr = errors.Join(readErr, err)
		}
	}
	if !replyEnded {
		replyErr = <-replies
	}
	if !mcpEnded {
		mcpErr = <-mcpDone
	}
	if expectedStreamEnd(readCloseErr) {
		readCloseErr = nil
	}
	return errors.Join(readErr, replyErr, mcpErr, c.ctx.Err(), closeErr, readCloseErr)
}

func (c *conn) SendStop(ctx context.Context) { _ = c.sendInput(ctx, &inputWork{op: inputStop}) }

func (c *conn) Cancel() { c.cancelRead() }

func (c *conn) sendInput(ctx context.Context, w *inputWork) error {
	w.ack = make(chan error, 1)
	select {
	case c.inputs <- w:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.interrupted:
		return errors.New("agent input stopped")
	}
	select {
	case err := <-w.ack:
		return err
	case <-c.inputDone:
		return errors.New("agent input stopped")
	}
}

func (c *conn) writeInputs() {
	defer close(c.inputDone)
	for {
		select {
		case <-c.interrupted:
			return
		case w := <-c.inputs:
			var result recordResult
			switch w.op {
			case inputPrompt:
				result = c.records.submit(c.ctx, &recordWork{op: recordPrompt, prompt: w.prompt, data: w.data})
			case inputRaw:
				result.data = w.data
				if !isMCPResponse(w.data) {
					result = c.records.submit(c.ctx, &recordWork{op: recordInput, data: w.data})
					result.data = w.data
				}
			case inputCompact:
				result = c.records.submit(c.ctx, &recordWork{op: recordCompact, text: w.text})
			case inputStop:
				result.data = []byte{0, '\n'}
			}
			err := result.err
			if err == nil {
				var n int
				n, err = c.stdin.Write(result.data)
				if err == nil && n != len(result.data) {
					err = io.ErrShortWrite
				}
				if w.op == inputPrompt {
					outcome := InputDeliveryCompleted()
					if err != nil {
						outcome = InputDeliveryFailed(err)
					}
					err = errors.Join(err, c.records.AppendMessage(context.WithoutCancel(c.ctx), result.delivery.stamp(outcome)))
				}
			}
			if w.op == inputPrompt && err != nil {
				err = &inputDeliveryError{Err: err}
			}
			w.ack <- err
		}
	}
}

// NewConn creates a transport over the task-owned physical log authority.
//
// Closing stdin must unblock a pending write so session shutdown can finish.
// log and records must be non-nil. Records owns the persistence policy. A nil
// registry serves no task-scoped MCP requests.
func NewConn(ctx context.Context, log *slog.Logger, stdin io.WriteCloser, records *Records, registry mcp.Registry) Conn {
	if log == nil || records == nil {
		panic("logger and records are required")
	}
	interruption, cancel := context.WithCancel(ctx)
	c := &conn{ctx: ctx, logger: log, stdin: stdin, records: records, mcp: registry, interrupted: interruption.Done(), cancelRead: cancel, inputs: make(chan *inputWork, 64), inputDone: make(chan struct{})}
	c.workers.Go(c.writeInputs)
	return c
}

type inputOperation uint8

const (
	inputPrompt inputOperation = iota
	inputRaw
	inputCompact
	inputStop
)

type inputWork struct {
	op     inputOperation
	prompt Prompt
	data   []byte
	text   string
	ack    chan error
}

type inputDeliveryError struct{ Err error }

func (e *inputDeliveryError) Error() string { return e.Err.Error() }
func (e *inputDeliveryError) Unwrap() error { return e.Err }

// MCPRequestResult handles one task-scoped MCP request and returns its bridge
// response payload. Callers must encode and send that payload to the relay.
func MCPRequestResult(ctx context.Context, registry mcp.Registry, req MCPRequest) (any, error) {
	if registry == nil {
		return nil, errors.New("task-scoped MCP is unavailable")
	}
	if req.Method == mcp.MethodToolsList {
		tools, err := registry.Tools(ctx)
		return MCPToolsListResponse{Tools: tools}, err
	}
	if req.Method == mcp.MethodToolsCall {
		result, err := registry.CallTool(ctx, &mcp.ToolsCallParams{Name: req.Name, Arguments: req.Arguments})
		if err != nil {
			return nil, err
		}
		return MCPToolResultResponse(result)
	}
	return nil, fmt.Errorf("unsupported MCP method: %s", req.Method)
}

// MCPToolResultResponse translates a registry result to the standard MCP tool
// response shape expected by the local relay clients, using a result summary
// as text content when the structured result supplies one.
func MCPToolResultResponse(result mcp.RawToolResult) (MCPToolResponse, error) {
	data, err := json.Marshal(result.Structured)
	if err != nil {
		return MCPToolResponse{}, err
	}
	text := string(data)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err == nil {
		if summary, ok := fields["result"]; ok {
			var value *string
			if err := json.Unmarshal(summary, &value); err == nil && value != nil {
				text = *value
			}
		}
	}
	response := MCPToolResponse{
		Content: []MCPTextContent{{Type: "text", Text: text}},
		IsError: result.IsError,
	}
	if !result.IsError {
		response.StructuredContent = data
	}
	return response, nil
}

// MCPResponseBytes encodes one bridge result for delivery to the relay.
func MCPResponseBytes(id string, result any, err error) ([]byte, error) {
	response := MCPResponseEnvelope{Type: "mcp_response", ID: id}
	if result != nil {
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return nil, marshalErr
		}
		response.Result = encoded
	}
	if err != nil {
		response.Error = err.Error()
	}
	data, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		return nil, marshalErr
	}
	return append(data, '\n'), nil
}

func isMCPResponse(data []byte) bool {
	var envelope mcpRelayEnvelope
	return json.Unmarshal(data, &envelope) == nil && envelope.Type == "mcp_response"
}

// Session manages a running agent process. It embeds Conn for wire I/O.
type Session struct {
	Conn

	ctx context.Context

	cmd     *exec.Cmd
	log     *slog.Logger
	done    chan struct{} // closed when readMessages goroutine exits
	err     error
	workers sync.WaitGroup

	inputMu     sync.Mutex // Serializes prompt attempts and joins sends before session completion.
	inputClosed bool       // Protected by inputMu; Wait prevents any later publications.
}

// NewSession creates a Session from an already-started command. Messages read
// from stdout are parsed and forwarded to msgCh through the Conn's
// ReadMessages method.
//
// A background goroutine reads stdout until EOF, then waits for the process to
// exit. The done channel is closed when both are complete. Callers should use
// Done() to detect session end and Wait() to retrieve the error.
//
// Parse and process failures are joined. A signal exit caused by local
// cancellation or cleanup after a parse failure is an expected shutdown effect.
func NewSession(ctx context.Context, cmd *exec.Cmd, c Conn, stdout io.ReadCloser, log *slog.Logger) *Session {
	if log == nil {
		panic("logger is required")
	}
	s := &Session{
		Conn: c,
		ctx:  ctx,
		cmd:  cmd,
		log:  log,
		done: make(chan struct{}),
	}

	s.workers.Go(func() {
		defer close(s.done)
		intercept, _ := c.(MessageInterceptor)
		parseErr := s.ReadMessages(stdout, intercept)
		if parseErr != nil {
			// A failed automatic response ends the session, including when the
			// harness keeps running after its input/output transport is closed.
			_ = cmd.Process.Kill()
		}
		waitErr := cmd.Wait()
		if waitErr != nil && (ctx.Err() != nil || parseErr != nil) {
			// The session owner closed the transport. A signal, or the nonzero
			// status a child reports because its input closed, describes that
			// teardown rather than an agent failure.
			waitErr = nil
		}
		if parseErr != nil {
			s.err = fmt.Errorf("parse: %w", parseErr)
		}
		if waitErr != nil {
			s.err = errors.Join(s.err, fmt.Errorf("agent exited: %w", waitErr))
		}
		switch {
		case parseErr != nil:
			log.ErrorContext(ctx, "session parse error", "err", parseErr)
		case waitErr != nil:
			// Signal-based exits (SIGKILL, SIGTERM) are expected when
			// containers are purged. Log at Info, not Error.
			if isSignalExit(waitErr) {
				log.InfoContext(ctx, "session killed", "err", waitErr)
			} else {
				log.WarnContext(ctx, "session exit error", "err", waitErr)
			}
		default:
			log.InfoContext(ctx, "session done")
		}
	})

	return s
}

// StartRelay is a convenience that calls PrepareRelay, creates a default Conn,
// and sends the initial prompt.
func StartRelay(ctx context.Context, opts *Options, agentArgs []string) (*Session, error) {
	rp, err := PrepareRelay(ctx, opts, nil, agentArgs)
	if err != nil {
		return nil, err
	}
	return StartSession(ctx, rp, NewConn(ctx, opts.Logger, rp.Stdin, opts.Records, nil), opts)
}

// StartSession creates a Session from a RelayProcess and Conn, sends the
// initial prompt if present, and returns the session.
func StartSession(ctx context.Context, rp *RelayProcess, c Conn, opts *Options) (*Session, error) {
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	if opts.Logger == nil {
		return nil, errors.New("opts.Logger is required")
	}
	log := opts.Logger.With("target", sshHost)
	s := NewSession(ctx, rp.Cmd, c, rp.Stdout, log)
	if opts.InitialPrompt.Text != "" || len(opts.InitialPrompt.Images) > 0 {
		if err := s.SendPrompt(opts.InitialPrompt); err != nil {
			s.Cancel()
			return nil, fmt.Errorf("write prompt: %w", errors.Join(err, s.Wait()))
		}
	}
	return s, nil
}

// AttachRelaySession attaches at the verified relay offset. The caller must
// finish authoritative history replay into opts.Records before attachment.
// The caller verifies connectivity through the session lifecycle.
// Backends may pass wrap to intercept the default Conn before message reading
// starts.
func AttachRelaySession(ctx context.Context, opts *Options, wrap func(Conn) (Conn, error)) (*Session, error) {
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	if opts.Logger == nil {
		return nil, errors.New("opts.Logger is required")
	}
	sshArgs := []string{
		sshHost, "python3", RelayScriptPath, "attach",
		"--offset", strconv.FormatInt(opts.RelayOffset, 10),
	}
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // args are not user-controlled.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("stdout pipe: %w", err), stdin.Close())
	}
	c := NewConn(ctx, opts.Logger, stdin, opts.Records, opts.MCP)
	if wrap != nil {
		wrapped, err := wrap(c)
		if err != nil {
			c.Cancel()
			return nil, errors.Join(err, stdin.Close(), stdout.Close())
		}
		c = wrapped
	}
	cmd.Stderr = &SlogWriter{Context: ctx, Logger: opts.Logger, Prefix: "relay attach", Container: sshHost}
	if err := cmd.Start(); err != nil {
		c.Cancel()
		// exec.Cmd closes the pipes it created when Start fails.
		return nil, fmt.Errorf("attach relay: %w", err)
	}

	log := opts.Logger.With("target", sshHost)
	return NewSession(ctx, cmd, c, stdout, log), nil
}

// SendPrompt publishes a user attempt before a harness can respond, then
// delivers it. Input and output share one FIFO; stdout drainage never waits for
// the stdin write. A transport failure is both durable and visible in that FIFO.
func (s *Session) SendPrompt(p Prompt) error {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if s.inputClosed {
		return errors.New("agent session exited")
	}
	// A completed session must never select a ready send over done: callers
	// may already have joined the record producer after Wait.
	select {
	case <-s.done:
		return errors.New("agent session exited")
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
	}
	if err := s.Conn.SendPrompt(p, nil); err != nil {
		return s.RecordInputFailure(p, err)
	}
	return nil
}

// Stop sends the null-byte sentinel, closes stdin, and waits for the agent
// process to exit or the context to expire. Returns nil on clean exit, the
// process exit error on abnormal exit, or the context error on timeout.
//
// Closing stdin after the sentinel is critical: the attach_client's main
// thread is blocked on stdin.read1(). Without the close, attach_client never
// exits, SSH never exits, and cmd.Wait() never returns — deadlocking Stop.
//
// The sentinel must be written explicitly rather than inferred from stdin EOF
// in the attach client, because EOF also occurs on SSH drops and backend
// restarts where the container should keep running.
func (s *Session) Stop(ctx context.Context) error {
	s.log.Debug("session: sending stop sentinel")
	stopDone := make(chan struct{})
	var stopWork sync.WaitGroup
	stopWork.Go(func() {
		defer close(stopDone)
		s.SendStop(ctx)
	})
	select {
	case <-stopDone:
	case <-ctx.Done():
	}
	// Close stdin so attach_client sees EOF and exits. The sentinel is
	// already in the pipe buffer; the OS delivers it before the EOF.
	s.Cancel()
	stopWork.Wait()
	s.log.Debug("session: waiting for process exit")
	select {
	case <-s.done:
		err := s.Wait()
		s.log.Debug("session: process exited", "err", err)
		return err
	case <-ctx.Done():
		s.log.Debug("session: context timeout while waiting for process exit", "err", ctx.Err())
		return ctx.Err()
	}
}

// Done returns a channel that is closed when the agent process exits.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// Wait blocks until the agent process exits.
func (s *Session) Wait() error {
	<-s.done
	s.workers.Wait()
	// Join the final failure record of a send
	// interrupted by closing stdin before allowing that closure.
	s.inputMu.Lock()
	s.inputClosed = true
	s.inputMu.Unlock()
	return s.err
}

// RelayRecordReader reads one physical relay record at a time. Agent payloads
// are returned as their unchanged native JSON; caic controls are returned only
// as parsed controls and never presented as native handshake data. Startup
// and continued reads share the same required record owner.
type RelayRecordReader struct {
	ctx     context.Context
	r       *bufio.Reader
	records *Records
}

// NewRelayRecordReader reads through the same owner used by the continued session.
func NewRelayRecordReader(ctx context.Context, r io.Reader, records *Records) *RelayRecordReader {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 1<<20)
	}
	return &RelayRecordReader{ctx: ctx, r: br, records: records}
}

// Reader returns the buffered continuation after the committed startup records.
func (r *RelayRecordReader) Reader() *bufio.Reader { return r.r }

// ReadRecord commits exact bytes before returning native envelopes and messages.
func (r *RelayRecordReader) ReadRecord() ([]byte, []TimedMessage, error) {
	for {
		data, err := readNDJSONRecord(r.r)
		if err != nil {
			return nil, nil, err
		}
		if len(data) == 1 {
			continue
		}
		result := r.records.submit(r.ctx, &recordWork{op: recordRelay, data: data})
		return result.data, result.messages, result.err
	}
}

func messageIsNil(msg Message) bool {
	if msg == nil {
		return true
	}
	value := reflect.ValueOf(msg)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Relay paths inside the container.
const (
	RelayDir                    = "/tmp/caic-relay"
	RelayScriptPath             = RelayDir + "/relay.py"
	RelaySockPath               = RelayDir + "/relay.sock"
	RelayOutputPath             = RelayDir + "/output.jsonl"
	RelayLogPath                = RelayDir + "/relay.log"
	ClaudeCodeCaicMCPConfigPath = RelayDir + "/caic-mcp.json"
	PiCaicMCPExtensionPath      = RelayDir + "/caic-mcp.ts"
)

// DeployRelay uploads the maintained relay script into the runtime target. Idempotent.
func DeployRelay(ctx context.Context, target runtime.ConnectionTarget) error {
	if target.SSHHost == "" {
		return errors.New("agent connection target missing SSH host")
	}
	// SSH concatenates remote args with spaces and passes them to the login
	// shell, so a single string works correctly as a shell command.
	cmd := exec.CommandContext(ctx, "ssh", target.SSHHost, //nolint:gosec // target is not user-controlled
		"mkdir -p "+RelayDir+" && cat > "+RelayScriptPath)
	cmd.Stdin = bytes.NewReader(relay.ScriptV2)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy relay: %w: %s", err, out)
	}
	return nil
}

// WidgetPluginDir is the container path where the widget plugin is deployed.
const WidgetPluginDir = RelayDir + "/widget-plugin"

// DeployEmbeddedDir writes all files from an embed.FS to a target directory
// in the container via a single SSH + tar invocation. Idempotent.
func DeployEmbeddedDir(ctx context.Context, container string, fsys fs.FS, targetDir string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return readErr
		}
		if writeErr := tw.WriteHeader(&tar.Header{
			Name: path,
			Mode: 0o644,
			Size: int64(len(data)),
		}); writeErr != nil {
			return writeErr
		}
		_, writeErr := tw.Write(data)
		return writeErr
	}); err != nil {
		return fmt.Errorf("build tar: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	cmd := exec.CommandContext(ctx, "ssh", container, //nolint:gosec // container is not user-controlled
		"mkdir -p "+targetDir+" && tar xf - -C "+targetDir)
	cmd.Stdin = &buf
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("deploy %s: %w: %s", targetDir, err, out)
	}
	return nil
}

// StopRelay sends the relay shutdown sentinel through a fresh attachment and
// waits for the attach client to exit. It terminates the persistent relay and
// its agent subprocess without treating an SSH disconnect as a shutdown.
func StopRelay(ctx context.Context, target runtime.ConnectionTarget) error {
	if target.SSHHost == "" {
		return errors.New("agent connection target missing SSH host")
	}
	cmd := sshCmd(ctx, target.SSHHost, "python3", RelayScriptPath, "attach")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("relay shutdown stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relay shutdown attach: %w", err)
	}
	_, writeErr := stdin.Write([]byte{0, '\n'})
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	return errors.Join(writeErr, closeErr, waitErr)
}

// CleanRelayState removes the relay state directory in the container so that
// a subsequent StartRelay begins with a clean output.jsonl. Used by fork to
// prevent the source task's message history from leaking into the forked task.
func CleanRelayState(ctx context.Context, container string) error {
	cmd := exec.CommandContext(ctx, "ssh", container, "rm", "-rf", RelayDir) //nolint:gosec // container is not user-controlled
	return cmd.Run()
}

// RelayStatus checks relay socket + PID liveness and returns diagnostic detail.
func RelayStatus(ctx context.Context, container string) (alive bool, detail string, err error) {
	pidPath := RelayDir + "/pid"
	check := fmt.Sprintf(
		`sock=0; [ -S %[1]s ] && sock=1; `+
			`pid=""; [ -f %[2]s ] && pid=$(cat %[2]s 2>/dev/null); `+
			`killok=0; if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then killok=1; fi; `+
			`echo "sock=$sock pid=$pid kill=$killok"; `+
			`[ "$sock" -eq 1 ] && [ "$killok" -eq 1 ]`,
		RelaySockPath, pidPath)
	cmd := sshCmd(ctx, container, "sh", "-c", check)
	out, err := cmd.CombinedOutput()
	detail = strings.TrimSpace(string(out))
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() != 0 {
			return false, detail, nil
		}
		return false, detail, fmt.Errorf("test relay: %w", err)
	}
	return true, detail, nil
}

// IsRelayRunning checks whether the relay socket exists in the container.
func IsRelayRunning(ctx context.Context, container string) (bool, error) {
	alive, _, err := RelayStatus(ctx, container)
	return alive, err
}

// ReadRelayLog reads the last maxBytes of the relay daemon's log file from the
// container. Returns empty string on any error (missing file, SSH failure).
func ReadRelayLog(ctx context.Context, container string, maxBytes int) string {
	// Use tail -c to cap the output; the log can be large after long sessions.
	cmd := sshCmd(ctx, container, "tail", "-c", strconv.Itoa(maxBytes), RelayLogPath)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ReadPlan reads a plan file from the container by invoking relay.py read-plan
// over SSH. If planFile is non-empty, that specific file is read; otherwise the
// most recently modified .md file in ~/.claude/plans/ is used.
func ReadPlan(ctx context.Context, container, planFile string) (string, error) {
	if container == "" {
		return "", errors.New("read plan: container is required")
	}
	args := []string{container, "python3", RelayScriptPath, "read-plan"}
	if planFile != "" {
		args = append(args, planFile)
	}
	cmd := sshCmd(ctx, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read plan: %w", err)
	}
	return string(out), nil
}

// SlogWriter is an io.Writer that logs each line via slog.Warn. It is used
// as cmd.Stderr for SSH relay subprocesses across all backends.
type SlogWriter struct {
	Context   context.Context
	Logger    *slog.Logger
	Prefix    string
	Container string

	buf []byte
}

// Write buffers p until complete lines can be logged.
func (w *SlogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimSpace(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			w.Logger.WarnContext(w.Context, "stderr", "src", w.Prefix, "ctr", w.Container, "line", line)
		}
	}
	return len(p), nil
}

// RelayProcess holds the started relay SSH command and its I/O pipes.
type RelayProcess struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
}

// ShellQuote preserves one argument through a POSIX shell without expansion.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// PrepareRelay deploys the relay script and starts the SSH serve-attach
// process. relayArgs configure the relay; agentArgs start the harness after
// the relay's command separator. The caller creates a Conn and Session from
// the returned process.
func PrepareRelay(ctx context.Context, opts *Options, relayArgs, agentArgs []string) (*RelayProcess, error) {
	if opts.Logger == nil {
		return nil, errors.New("opts.Logger is required")
	}
	if opts.Dir == "" {
		return nil, errors.New("opts.Dir is required")
	}
	sshHost := opts.Target.SSHHost
	if sshHost == "" {
		return nil, errors.New("agent connection target missing SSH host")
	}
	tStart := time.Now()
	if err := DeployRelay(ctx, opts.Target); err != nil {
		return nil, err
	}
	opts.Logger.DebugContext(ctx, "startup", "phase", "deploy_relay", "target", sshHost, "dur", time.Since(tStart))

	sshArgs := make([]string, 0, 10+len(relayArgs)+2*len(opts.StripEnv)+len(agentArgs))
	sshArgs = append(sshArgs, sshHost, "PYTHONDONTWRITEBYTECODE=1", "python3", RelayScriptPath, "serve-attach", "--dir", opts.Dir, "--no-log-stdin")
	sshArgs = append(sshArgs, relayArgs...)
	for _, key := range opts.StripEnv {
		sshArgs = append(sshArgs, "--strip-env", key)
	}
	sshArgs = append(sshArgs, "--")
	sshArgs = append(sshArgs, agentArgs...)

	// OpenSSH joins remote arguments and passes them through the login shell.
	// Keep the fixed environment assignment as shell syntax; quote the command
	// and its arguments so opaque session IDs remain one argv value.
	for i := 2; i < len(sshArgs); i++ {
		sshArgs[i] = ShellQuote(sshArgs[i])
	}
	opts.Logger.DebugContext(ctx, "relay", "msg", "launch", "target", sshHost, "args", agentArgs)
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...) //nolint:gosec // remote arguments are shell-quoted; target is server-owned.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = &SlogWriter{Context: ctx, Logger: opts.Logger, Prefix: "relay serve-attach", Container: sshHost}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start relay: %w", err)
	}
	opts.Logger.InfoContext(ctx, "startup", "phase", "relay_started", "target", sshHost, "dur", time.Since(tStart))
	return &RelayProcess{Cmd: cmd, Stdin: stdin, Stdout: stdout}, nil
}

// sshTimeoutArgs are the default SSH options for relay operations.
var sshTimeoutArgs = []string{"-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2"}

// sshCmd creates an exec.Cmd with standard timeout args prepended.
func sshCmd(ctx context.Context, extraArgs ...string) *exec.Cmd {
	args := make([]string, 0, len(sshTimeoutArgs)+len(extraArgs))
	args = append(args, sshTimeoutArgs...)
	args = append(args, extraArgs...)
	return exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // args are not user-controlled.
}

// RelayOutputSize returns the byte size of the relay output.jsonl in the
// container. This is fast (single stat call) and avoids transferring the file.
func RelayOutputSize(ctx context.Context, container string) (int64, error) {
	cmd := sshCmd(ctx, container, "stat", "-c", "%s", RelayOutputPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("stat relay output: %w", err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse relay size %q: %w", string(out), err)
	}
	return n, nil
}

// ReadRelayTail reads only the tail of the relay output.jsonl from the
// container and returns validated physical records plus the total file size (for
// RelayOffset). Memory and transfer are bounded by maxBytes; validated v3
// records are retained so adoption can persist output produced while offline.
// Provider interpretation follows physical reconciliation with local history.
func ReadRelayTail(ctx context.Context, container string, maxBytes int64) (timeline RelaySnapshot, size int64, err error) {
	generation, err := relayOutputGeneration(ctx, container)
	if err != nil {
		return RelaySnapshot{}, 0, err
	}
	size, err = RelayOutputSize(ctx, container)
	if err != nil {
		return RelaySnapshot{}, 0, err
	}
	timeline, offset, err := readRelaySnapshot(ctx, container, maxBytes, size, generation)
	if err != nil {
		return timeline, offset, err
	}
	currentGeneration, err := relayOutputGeneration(ctx, container)
	if err != nil {
		return timeline, offset, err
	}
	if currentGeneration != generation {
		return RelaySnapshot{}, 0, errors.New("relay output generation changed during snapshot")
	}
	return timeline, offset, nil
}

// readRelaySnapshot validates the relay byte range that
// relaySnapshotCommand transfers for size bytes and returns the physical snapshot
// plus the physical offset the read stopped at. generation is the relay output
// generation observed before the read; the caller owns that round trip so it can
// also verify the generation did not change.
func readRelaySnapshot(ctx context.Context, container string, maxBytes, size int64, generation string) (timeline RelaySnapshot, offset int64, err error) {
	cmd, start := relaySnapshotCommand(ctx, container, maxBytes, size)
	skipFirst, err := relayTailNeedsLeadingSkip(ctx, container, start)
	if err != nil {
		return RelaySnapshot{}, start, err
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return RelaySnapshot{}, start, fmt.Errorf("relay stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return RelaySnapshot{}, start, fmt.Errorf("start relay read: %w", err)
	}
	timeline, offset, readErr := readRelayTailRecords(pipe, start, skipFirst, container, generation)
	if readErr != nil {
		_ = cmd.Wait()
		return timeline, offset, readErr
	}
	if err := cmd.Wait(); err != nil {
		return timeline, offset, fmt.Errorf("relay read: %w", err)
	}
	return timeline, offset, nil
}

func relayOutputGeneration(ctx context.Context, container string) (string, error) {
	out, err := relayGenerationCommand(ctx, container).Output()
	if err != nil {
		return "", fmt.Errorf("read relay generation: %w", err)
	}
	newline := bytes.IndexByte(out, '\n')
	if newline < 0 {
		return "", nil
	}
	out = out[:newline]
	var record struct {
		Type       string `json:"t"`
		Generation string `json:"generation"`
	}
	if !json.Valid(out) {
		return "", nil
	}
	if err := json.Unmarshal(out, &record); err != nil {
		return "", fmt.Errorf("decode relay generation: %w", err)
	}
	if record.Type != string(taskslog.KindRelayGeneration) {
		return "", nil
	}
	if record.Generation == "" {
		return "", errors.New("relay generation record is empty")
	}
	return record.Generation, nil
}

func relayGenerationCommand(ctx context.Context, container string) *exec.Cmd {
	return exec.CommandContext(ctx, "ssh", container, "head", "-c", "4096", RelayOutputPath) //nolint:gosec // container is not user-controlled.
}

// readRelayTailRecords parses one stat-bounded relay snapshot and returns the
// exact physical offset consumed, including a skipped partial tail record.
func readRelayTailRecords(r io.Reader, start int64, skipFirst bool, src, generation string) (timeline RelaySnapshot, offset int64, err error) {
	reader := bufio.NewReaderSize(r, 1<<20)
	offset = start
	currentGeneration := generation
	for {
		record, readErr := readNDJSONRecord(reader)
		offset += int64(len(record))
		if errors.Is(readErr, io.EOF) {
			return timeline, offset, nil
		}
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			return timeline, offset - int64(len(record)), nil
		}
		if readErr != nil {
			return timeline, offset, fmt.Errorf("read relay record: %w", readErr)
		}
		line := record[:len(record)-1]
		if skipFirst {
			skipFirst = false
			continue
		}
		if len(line) == 0 {
			continue
		}
		frame, parseErr := taskslog.DecodeFrame(line)
		parsed := ParsedRecord{Provenance: FrameProvenance(&frame)}
		if parseErr != nil {
			return timeline, offset, fmt.Errorf("parse relay record from %s: %w", src, parseErr)
		}
		if parsed.Provenance.Generation != "" {
			currentGeneration = parsed.Provenance.Generation
		}
		timeline.Encoded = append(timeline.Encoded, record...)
		if parsed.Provenance.Source == RecordSourceRelay {
			timeline.RelayRecords = append(timeline.RelayRecords, RelayRecordBoundary{
				Generation:  currentGeneration,
				RelayEnd:    offset,
				ByteEnd:     len(timeline.Encoded),
				Fingerprint: sha256.Sum256(line),
			})
		}
	}
}

func tailNeedsLeadingSkip(start int64, previous byte) bool {
	return start > 0 && previous != '\n'
}

// relayTailNeedsLeadingSkip reports whether a tail begins mid-record.
func relayTailNeedsLeadingSkip(ctx context.Context, container string, start int64) (bool, error) {
	if start == 0 {
		return false, nil
	}
	command := fmt.Sprintf("dd if=%s bs=1 skip=%d count=1 status=none", RelayOutputPath, start-1)
	out, err := sshCmd(ctx, container, command).Output()
	if err != nil {
		return false, fmt.Errorf("read relay tail boundary: %w", err)
	}
	if len(out) != 1 {
		return false, fmt.Errorf("read relay tail boundary: got %d bytes", len(out))
	}
	return tailNeedsLeadingSkip(start, out[0]), nil
}

// relaySnapshotCommand bounds a relay read to the size observed by stat. The
// returned start is the physical output-file offset of the transferred bytes.
func relaySnapshotCommand(ctx context.Context, container string, tailBytes, size int64) (cmd *exec.Cmd, start int64) {
	if tailBytes > 0 && size > tailBytes {
		start := size - tailBytes
		command := fmt.Sprintf("head -c %d %s | tail -c %d", size, RelayOutputPath, tailBytes)
		return sshCmd(ctx, container, command), start
	}
	command := fmt.Sprintf("head -c %d %s", size, RelayOutputPath)
	return sshCmd(ctx, container, command), 0
}

// readNDJSONRecord reads one LF-terminated physical record without inventing a
// delimiter. The task-log limit includes the physical LF terminator.
func readNDJSONRecord(r *bufio.Reader) ([]byte, error) {
	var record []byte
	for {
		fragment, err := r.ReadSlice('\n')
		record = append(record, fragment...)
		if len(record) >= taskslog.MaxRecordSize {
			return record, fmt.Errorf("NDJSON record must be smaller than %d bytes", taskslog.MaxRecordSize)
		}
		switch {
		case err == nil:
			return record, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(record) == 0 {
				return nil, io.EOF
			}
			return record, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// isSignalExit reports whether err indicates the process was killed by a
// signal (e.g. SIGKILL from container purge).
func isSignalExit(err error) bool {
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return false
	}
	// On Unix, ExitCode() returns -1 when the process was killed by a signal.
	// ProcessState.Sys() returns syscall.WaitStatus with signal details.
	if exitErr.ExitCode() == -1 {
		return true
	}
	// Also check for specific signals via os.ProcessState.
	if ps := exitErr.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(interface{ Signal() os.Signal }); ok {
			return ws.Signal() != nil
		}
	}
	return false
}

// expectedStreamEnd recognizes only shutdown leaves. A joined independent read
// or close failure must remain visible even when another branch was canceled.
func expectedStreamEnd(err error) bool {
	if err == nil {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !expectedStreamEnd(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := wrapped.Unwrap()
		return child != nil && expectedStreamEnd(child)
	}
	return errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled)
}
