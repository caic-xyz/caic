// Package sse writes deadline-bounded Server-Sent Event response frames.

package sse

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

const writeTimeout = 5 * time.Second

// Stream writes SSE frames and bounds each client write.
type Stream struct {
	w          http.ResponseWriter
	controller *http.ResponseController
}

// New constructs a Stream for w.
func New(w http.ResponseWriter) *Stream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &Stream{w: w, controller: http.NewResponseController(w)}
}

// Writef writes an SSE frame. Call Flush to deliver buffered frames.
func (s *Stream) Writef(format string, args ...any) error {
	if err := s.BeginWrite(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, format, args...); err != nil {
		return errors.Join(fmt.Errorf("write SSE frame: %w", err), s.clearDeadline())
	}
	return nil
}

// BeginWrite starts a bounded write sequence. Call Flush after its frames.
func (s *Stream) BeginWrite() error {
	return s.setDeadline(time.Now().Add(writeTimeout))
}

// ClearWriteDeadline ends a write sequence that failed before Flush.
func (s *Stream) ClearWriteDeadline() error {
	return s.clearDeadline()
}

// Flush delivers pending frames and clears their write deadline.
func (s *Stream) Flush() error {
	flushErr := s.controller.Flush()
	deadlineErr := s.clearDeadline()
	if flushErr != nil {
		return errors.Join(fmt.Errorf("flush SSE stream: %w", flushErr), deadlineErr)
	}
	return deadlineErr
}

// KeepAlive writes and flushes an SSE comment heartbeat.
func (s *Stream) KeepAlive() error {
	if err := s.Writef(": keepalive\n\n"); err != nil {
		return err
	}
	return s.Flush()
}

// Data writes and flushes an SSE data frame.
func (s *Stream) Data(data []byte) error {
	if err := s.Writef("data: %s\n\n", data); err != nil {
		return err
	}
	return s.Flush()
}

func (s *Stream) setDeadline(deadline time.Time) error {
	if err := s.controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("set SSE write deadline: %w", err)
	}
	return nil
}

func (s *Stream) clearDeadline() error {
	return s.setDeadline(time.Time{})
}
