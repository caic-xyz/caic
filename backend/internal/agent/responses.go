// Bounded automatic protocol replies keep stdout independent of blocked stdin.

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	maxPendingResponses     = 64
	maxPendingResponseBytes = 4 << 20
)

// ErrResponseQueueFull means the bounded automatic-reply budget was exhausted.
var ErrResponseQueueFull = errors.New("automatic response queue capacity exceeded")

// ErrResponsesAborted means session termination or a write failure prevented
// delivery of one or more automatic protocol replies.
var ErrResponsesAborted = errors.New("automatic responses aborted")

// responseWork holds an encoded reply or an MCP invocation in separate bounded
// lanes. A long registry call cannot hold up automatic permission responses.
type responseWork struct {
	data    []byte
	request *MCPRequest
}

func (w *responseWork) size() int {
	if w.request != nil {
		return len(w.request.ID) + len(w.request.Method) + len(w.request.Name) + len(w.request.Arguments)
	}
	return len(w.data)
}

// responseQueue bounds queued and in-flight work together. Saturation is a
// session failure, never backpressure on stdout or a silently discarded reply.
// Each lane has the same count/byte budget. MCP results move into the reply
// lane before stdin delivery, so both queues stay independently bounded.
type responseQueue struct {
	items   chan responseWork
	mu      sync.Mutex
	count   int
	bytes   int
	stopped bool
}

func (q *responseQueue) enqueue(w responseWork) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return ErrResponsesAborted
	}
	n := w.size()
	if q.count == maxPendingResponses || n > maxPendingResponseBytes-q.bytes {
		return ErrResponseQueueFull
	}
	q.count++
	q.bytes += n
	if w.request != nil {
		cp := *w.request
		cp.Arguments = bytes.Clone(cp.Arguments)
		w.request = &cp
	} else {
		w.data = bytes.Clone(w.data)
	}
	q.items <- w
	return nil
}

func (q *responseQueue) run(ctx context.Context, c *conn) error {
	for {
		if ctx.Err() != nil {
			q.mu.Lock()
			q.stopped = true
			n := q.count
			q.mu.Unlock()
			if n > 0 {
				return fmt.Errorf("%w: %d pending: %w", ErrResponsesAborted, n, ctx.Err())
			}
			return nil
		}
		select {
		case <-ctx.Done():
			continue
		case w := <-q.items:
			data := w.data
			err := ctx.Err()
			if err == nil && len(data) > maxPendingResponseBytes {
				err = ErrResponseQueueFull
			}
			if err == nil {
				err = c.SendRaw(data)
			}
			q.mu.Lock()
			q.count--
			q.bytes -= w.size()
			if err != nil {
				q.stopped = true
			}
			q.mu.Unlock()
			if err != nil {
				return errors.Join(ErrResponsesAborted, fmt.Errorf("deliver automatic response: %w", err))
			}
		}
	}
}

// runMCP serializes registry requests independently of the stdin reply writer.
// Production registries come from the manager's MCPExecutor: ending this stream
// cancels its wait, while already-admitted calls retain their manager-owned slot.
func (q *responseQueue) runMCP(ctx context.Context, c *conn, replies *responseQueue) error {
	for {
		if ctx.Err() != nil {
			q.mu.Lock()
			q.stopped = true
			n := q.count
			q.mu.Unlock()
			if n > 0 {
				return fmt.Errorf("%w: %d MCP requests pending: %w", ErrResponsesAborted, n, ctx.Err())
			}
			return nil
		}
		select {
		case <-ctx.Done():
			continue
		case w := <-q.items:
			result, callErr := MCPRequestResult(ctx, c.mcp, *w.request)
			data, err := MCPResponseBytes(w.request.ID, result, callErr)
			if err == nil {
				err = ctx.Err()
			}
			if err == nil {
				err = replies.enqueue(responseWork{data: data})
			}
			q.mu.Lock()
			q.count--
			q.bytes -= w.size()
			if err != nil {
				q.stopped = true
			}
			q.mu.Unlock()
			if err != nil {
				return errors.Join(ErrResponsesAborted, fmt.Errorf("execute MCP response: %w", err))
			}
		}
	}
}

// ContinuedReader retains a pipe's cancellation boundary after a handshake has
// buffered some output. Its owner closes Closer when message reading ends.
type ContinuedReader struct {
	io.Reader
	io.Closer
}
