// Serializes durable task records, ordered provider interpretation, and publication through one bounded owner.

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/maruel/ksid"
)

// RecordHandler applies accepted records to a production consumer. Preparation
// may return derived host controls; it must not publish or reenter Records.
// ApplyRecords must not append records or perform transport I/O.
type RecordHandler interface {
	RecordAppended(record PhysicalRecord)
	PrepareRecords(ctx context.Context, messages []TimedMessage) ([]Message, error)
	ApplyRecords(ctx context.Context, messages []TimedMessage) error
}

// RecordIntent prepares host records at their ordered admission point and applies
// associated runtime-only state after every required append succeeds.
// Neither method may reenter Records or perform transport I/O.
type RecordIntent interface {
	PrepareRecords(ctx context.Context) ([]Message, error)
	ApplyRecords(ctx context.Context) error
}

// Records owns one wire, its ordered parser, and the physical sink. Producers
// submit bounded work and join before Close; Close drains admission and closes
// the sink exactly once. A failed append or interpretation poisons this owner.
type Records struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	wire    WireFormat
	sink    LogSink
	parser  *LogRecordParser
	handler RecordHandler
	queue   chan *recordWork
	done    chan struct{}
	workers sync.WaitGroup
	err     error // published by done
}

// NewRecords transfers wire and sink ownership to a bounded ingestion worker.
// handler supplies the runtime fold or the recorder's observation policy.
func NewRecords(ctx context.Context, wire WireFormat, sink LogSink, handler RecordHandler) (*Records, error) {
	if wire == nil || sink == nil || handler == nil {
		return nil, errors.New("wire, sink, and record handler are required")
	}
	parser, err := NewLogRecordParser(wire)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancelCause(ctx)
	r := &Records{ctx: lifetime, cancel: cancel, wire: wire, sink: sink, parser: parser, handler: handler, queue: make(chan *recordWork, 256), done: make(chan struct{})}
	r.workers.Go(r.run)
	return r, nil
}

// Done closes when admission stops through cancellation or an ingestion failure.
// Producers must join before the lifetime owner calls Close.
func (r *Records) Done() <-chan struct{} { return r.ctx.Done() }

// Close drains accepted work and joins the worker before closing its sink.
// It is called once by the lifetime owner, after every producer has joined.
func (r *Records) Close() error {
	r.queue <- &recordWork{op: recordClose}
	<-r.done
	r.workers.Wait()
	return r.err
}

// AppendMessage commits one host control before applying its effects.
func (r *Records) AppendMessage(ctx context.Context, m Message) error {
	return r.AppendMessages(ctx, []Message{m})
}

// AppendMessages commits a coherent host batch before applying any effects.
func (r *Records) AppendMessages(ctx context.Context, m []Message) error {
	return r.submit(ctx, &recordWork{op: recordHost, messages: m}).err
}

// CommitIntent runs a host admission and its accepted effects in stream order.
func (r *Records) CommitIntent(ctx context.Context, intent RecordIntent) error {
	return r.submit(ctx, &recordWork{op: recordIntent, intent: intent}).err
}

// WriteInput commits complete encoded provider input before writing transport.
// The transport write runs outside the ingestion worker.
func (r *Records) WriteInput(ctx context.Context, w io.Writer, data []byte) error {
	result := r.submit(ctx, &recordWork{op: recordInput, data: data})
	if result.err != nil {
		return result.err
	}
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

// Replay reconstructs this owner's parser from a fixed physical history
// snapshot, without republishing historical records or invoking live effects.
// It must complete before transport attachment or other producers start.
func (r *Records) Replay(ctx context.Context, path string) error {
	return r.submit(ctx, &recordWork{op: recordReplay, text: path, ctx: ctx}).err
}

func (r *Records) submit(ctx context.Context, w *recordWork) recordResult {
	if err := ctx.Err(); err != nil {
		return recordResult{err: err}
	}
	if err := context.Cause(r.ctx); err != nil { //nolint:contextcheck // The owner intentionally outlives request cancellation.
		return recordResult{err: err}
	}
	w.ack = make(chan recordResult, 1)
	select {
	case r.queue <- w:
	case <-ctx.Done():
		return recordResult{err: ctx.Err()}
	case <-r.ctx.Done():
		return recordResult{err: context.Cause(r.ctx)} //nolint:contextcheck // Admission observes the independent owner lifetime.
	}
	// Once admitted, ACK is authoritative even when the caller is canceled.
	// Worker preparation uses the independently cancelable owner lifetime.
	return <-w.ack
}

func (r *Records) run() {
	defer close(r.done)
	var failure error
	for w := range r.queue {
		if w.op == recordClose {
			r.cancel(errors.New("record owner closed"))
			r.err = errors.Join(failure, r.sink.Close())
			return
		}
		if failure == nil {
			failure = context.Cause(r.ctx)
		}
		if failure != nil {
			w.ack <- recordResult{err: failure}
			continue
		}
		result := r.process(w)
		if result.err != nil && !result.nonfatal {
			failure = result.err
			r.cancel(failure)
		}
		w.ack <- result
	}
}

func (r *Records) process(w *recordWork) recordResult {
	if w.op == recordReplay {
		return recordResult{err: r.replay(w.ctx, w.text)}
	}
	if w.op == recordIntent {
		messages, err := w.intent.PrepareRecords(r.ctx)
		if err != nil {
			return recordResult{err: err, nonfatal: true}
		}
		w.messages = messages
	}
	var encoded []byte
	var operationErr error
	var delivery inputDelivery
	var err error
	switch w.op { //nolint:exhaustive // Replay and close are handled before encoding.
	case recordHost, recordIntent:
		encoded, err = marshalRecordMessages(w.messages)
	case recordRelay:
		// Relay records have the task-log size limit and retain their exact bytes
		// for persistence and replay.
		encoded = w.data
	case recordInput:
		encoded, err = marshalInputRecords(w.data)
	case recordReply:
		delivery = r.newInputDelivery()
		encoded, err = startedInputRecords(&delivery, w.data)
	case recordPrompt:
		delivery = r.newInputDelivery()
		data := w.data
		err = w.failure
		if err == nil && data == nil {
			data, err = r.wire.EncodePrompt(w.prompt)
		}
		if err != nil {
			operationErr = err
			encoded, err = marshalRecordMessages([]Message{delivery.stamp(InputDeliveryStarted()), &UserInputMessage{Text: w.prompt.Text, Images: w.prompt.Images, Ts: float64(time.Now().UnixMilli()) / 1000}, delivery.stamp(InputDeliveryFailed(operationErr))})
			break
		}
		encoded, err = startedInputRecords(&delivery, data)
		w.data = data
	case recordCompact:
		encoder, ok := r.wire.(CompactCommand)
		if !ok {
			return recordResult{err: errors.New("compact not supported by this backend"), nonfatal: true}
		}
		w.data, err = encoder.EncodeCompact(w.text)
		if err != nil {
			// Encoding rejects a client request before any bytes are accepted.
			// Keep the record owner available for subsequent input.
			return recordResult{err: err, nonfatal: true}
		}
		encoded, err = marshalInputRecords(w.data)
	}
	if err != nil {
		return recordResult{err: err}
	}
	var frames []taskslog.Frame
	if w.op == recordRelay {
		if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
			return recordResult{err: io.ErrUnexpectedEOF}
		}
		frame, err := taskslog.DecodeFrame(encoded[:len(encoded)-1])
		if err != nil {
			return recordResult{err: err}
		}
		frames = append(frames, frame)
	} else {
		frames, err = validateRecordBatch(encoded)
		if err != nil {
			return recordResult{err: err}
		}
	}
	physical := PhysicalRecord{Encoded: encoded}
	if w.op == recordRelay {
		physical.Provenance = FrameProvenance(&frames[0])
	}
	if len(encoded) > 0 {
		if err = r.sink.AppendRecord(physical); err != nil {
			return recordResult{err: fmt.Errorf("append task records: %w", err)}
		}
		r.handler.RecordAppended(physical)
	}
	var messages []TimedMessage
	var observed []TimedMessage
	for i := range frames {
		parsed, decodeErr := r.parser.DecodeFrame(&frames[i])
		if decodeErr != nil {
			return recordResult{err: fmt.Errorf("interpret accepted task record: %w", decodeErr)}
		}
		observed = append(observed, parsed.Messages...)
		for _, msg := range parsed.Messages {
			if w.disposition != nil {
				handled, dispatchErr := w.disposition(msg)
				if dispatchErr != nil {
					return recordResult{err: dispatchErr}
				}
				if handled {
					continue
				}
			}
			if _, bridge := msg.Message.(*MCPRequestMessage); !bridge {
				messages = append(messages, msg)
			}
		}
	}
	derived, err := r.handler.PrepareRecords(r.ctx, messages)
	if err != nil {
		return recordResult{err: err}
	}
	if len(derived) > 0 {
		data, marshalErr := marshalRecordMessages(derived)
		if marshalErr != nil {
			return recordResult{err: marshalErr}
		}
		extra, frameErr := validateRecordBatch(data)
		if frameErr != nil {
			return recordResult{err: frameErr}
		}
		if err = r.sink.AppendRecord(PhysicalRecord{Encoded: data}); err != nil {
			return recordResult{err: err}
		}
		for i := range extra {
			parsed, decodeErr := r.parser.DecodeFrame(&extra[i])
			if decodeErr != nil {
				return recordResult{err: decodeErr}
			}
			messages = append(messages, parsed.Messages...)
		}
	}
	if err = r.handler.ApplyRecords(r.ctx, messages); err != nil {
		return recordResult{err: err}
	}
	if w.op == recordIntent {
		if err := w.intent.ApplyRecords(r.ctx); err != nil {
			return recordResult{err: err}
		}
	}
	result := recordResult{delivery: delivery, messages: observed, data: w.data, err: operationErr, nonfatal: operationErr != nil}
	if w.op == recordRelay {
		result.data = nil
		if w.returnNative && frames[0].Kind == taskslog.KindAgent {
			result.data = bytes.Clone(frames[0].Native)
		}
	}
	return result
}

func (r *Records) replay(ctx context.Context, path string) (retErr error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			return io.ErrUnexpectedEOF
		}
	}
	scanner := taskslog.NewScanner(io.NewSectionReader(f, 0, info.Size()), path)
	if _, err := scanner.ReadHeader(); err != nil {
		return err
	}
	if _, err := r.parser.DecodeFrame(&scanner.HeaderFrame); err != nil {
		return err
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := r.parser.DecodeFrame(scanner.Frame()); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (r *Records) newInputDelivery() inputDelivery {
	return inputDelivery{sessionID: r.parser.session.SessionID, uuid: ksid.NewID().String()}
}

type recordOperation uint8

const (
	recordHost recordOperation = iota
	recordRelay
	recordInput
	recordReply
	recordPrompt
	recordCompact
	recordReplay
	recordClose
	recordIntent
)

type recordWork struct {
	ctx          context.Context
	intent       RecordIntent
	op           recordOperation
	data         []byte
	messages     []Message
	prompt       Prompt
	failure      error
	text         string
	returnNative bool
	disposition  func(TimedMessage) (bool, error)
	ack          chan recordResult
}
type recordResult struct {
	delivery inputDelivery
	data     []byte
	messages []TimedMessage
	err      error
	nonfatal bool
}

// inputDelivery is the identity captured at one ordered prompt or reply
// admission. Transport completion retains it even if the active native session
// changes.
type inputDelivery struct {
	sessionID string
	uuid      string
}

func (d *inputDelivery) stamp(m *SystemMessage) *SystemMessage {
	m.SessionID, m.UUID = d.sessionID, d.uuid
	return m
}

// startedInputRecords encodes a delivery start immediately followed by the
// native input it identifies.
func startedInputRecords(d *inputDelivery, data []byte) ([]byte, error) {
	encoded, err := marshalRecordMessages([]Message{d.stamp(InputDeliveryStarted())})
	if err != nil {
		return nil, err
	}
	native, err := marshalInputRecords(data)
	if err != nil {
		return nil, err
	}
	return append(encoded, native...), nil
}

func marshalRecordMessages(messages []Message) ([]byte, error) {
	var out []byte
	for _, msg := range messages {
		data, err := MarshalLogMessage(msg)
		if err != nil {
			return nil, err
		}
		out = append(out, data...)
		out = append(out, '\n')
	}
	return out, nil
}

func marshalInputRecords(data []byte) ([]byte, error) {
	var out []byte
	for {
		line, rest, err := nextInputRecord(data)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		data = rest
		if len(line) == 1 {
			continue
		}
		encoded, err := (&v3.NativeRecord{Type: string(taskslog.KindInput), Time: time.Now().UTC(), Message: line[:len(line)-1]}).MarshalJSON()
		if err != nil {
			return nil, err
		}
		out = append(out, encoded...)
		out = append(out, '\n')
	}
}

func validateRecordBatch(data []byte) ([]taskslog.Frame, error) {
	var frames []taskslog.Frame
	for {
		line, rest, err := nextInputRecord(data)
		if errors.Is(err, io.EOF) {
			return frames, nil
		}
		if err != nil {
			return nil, err
		}
		data = rest
		frame, err := taskslog.DecodeFrame(line[:len(line)-1])
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	}
}

// nextInputRecord borrows a complete physical line from immutable in-memory input.
func nextInputRecord(data []byte) (line, rest []byte, err error) {
	if len(data) == 0 {
		return nil, nil, io.EOF
	}
	search := data
	if len(search) > taskslog.MaxRecordSize {
		search = search[:taskslog.MaxRecordSize]
	}
	end := bytes.IndexByte(search, '\n')
	if end < 0 {
		if len(data) >= taskslog.MaxRecordSize {
			return nil, nil, fmt.Errorf("NDJSON record must be smaller than %d bytes", taskslog.MaxRecordSize)
		}
		return nil, nil, io.ErrUnexpectedEOF
	}
	if end+1 >= taskslog.MaxRecordSize {
		return nil, nil, fmt.Errorf("NDJSON record must be smaller than %d bytes", taskslog.MaxRecordSize)
	}
	return data[:end+1], data[end+1:], nil
}
