// Manager-owned bounded MCP calls outlive canceled agent streams without losing capacity accounting.

package agent

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/maruel/gomode/mcp"
)

// MCPExecutor owns admitted registry calls independently of any agent stream.
// At most 64 calls and 4 MiB of arguments remain queued or executing.
// Canceled callers stop waiting, while accepted work and its bounded budget
// remain owned until the actual registry call completes. Close cancels the
// owner context, rejects new work, and joins all admitted calls.
type MCPExecutor struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	count   int
	bytes   int
	workers sync.WaitGroup
}

// NewMCPExecutor creates one shared budget for a task manager's lifetime.
func NewMCPExecutor(ctx context.Context) *MCPExecutor {
	ctx, cancel := context.WithCancel(ctx)
	return &MCPExecutor{ctx: ctx, cancel: cancel}
}

// Wrap preserves the registry capability and gives its tools/list and tools/call
// operations an owned, cancelable wait. Reconnects must reuse this executor.
func (e *MCPExecutor) Wrap(registry mcp.Registry) mcp.Registry {
	return executorRegistry{Registry: registry, executor: e}
}

// Close rejects admission before waiting, including races with a new stream.
// The manager cancels its session contexts before joining this executor, so a
// registry operation awaiting accepted input can finish when its target closes.
func (e *MCPExecutor) Close() {
	e.mu.Lock()
	e.closed = true
	e.cancel()
	e.mu.Unlock()
	e.workers.Wait()
}

func (e *MCPExecutor) run[T any](ctx context.Context, size int, call func(context.Context) (T, error)) (T, error) {
	var zero T
	e.mu.Lock()
	if e.closed || e.ctx.Err() != nil {
		e.mu.Unlock()
		return zero, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		e.mu.Unlock()
		return zero, err
	}
	if e.count == maxPendingResponses || size > maxPendingResponseBytes-e.bytes {
		e.mu.Unlock()
		return zero, fmt.Errorf("MCP execution budget: %w", ErrResponseQueueFull)
	}
	e.count++
	e.bytes += size
	done := make(chan mcpCallResult[T], 1)
	// Admission and WaitGroup.Go share the close gate: Close cannot Wait
	// before this work is registered, even when the caller disconnects now.
	e.workers.Go(func() {
		callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		stop := context.AfterFunc(e.ctx, cancel) //nolint:contextcheck // Manager shutdown cancels work that retains caller identity, independently of its stream.
		value, err := call(callCtx)
		stop()
		cancel()
		e.mu.Lock()
		e.count--
		e.bytes -= size
		e.mu.Unlock()
		done <- mcpCallResult[T]{value: value, err: err}
	})
	e.mu.Unlock()
	select {
	case result := <-done:
		return result.value, result.err
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-e.ctx.Done():
		return zero, e.ctx.Err()
	}
}

type mcpCallResult[T any] struct {
	value T
	err   error
}

type executorRegistry struct {
	mcp.Registry

	executor *MCPExecutor
}

func (r executorRegistry) Tools(ctx context.Context) ([]mcp.ToolDescriptor, error) {
	return r.executor.run(ctx, 0, r.Registry.Tools)
}

func (r executorRegistry) CallTool(ctx context.Context, params *mcp.ToolsCallParams) (mcp.RawToolResult, error) {
	size := len(params.Name) + len(params.Arguments) + len(params.InputResponses) + len(params.RequestState)
	if size > maxPendingResponseBytes {
		return mcp.RawToolResult{}, fmt.Errorf("MCP execution budget: %w", ErrResponseQueueFull)
	}
	cp := *params
	cp.Arguments = bytes.Clone(params.Arguments)
	cp.InputResponses = bytes.Clone(params.InputResponses)
	return r.executor.run(ctx, size, func(ctx context.Context) (mcp.RawToolResult, error) { return r.Registry.CallTool(ctx, &cp) })
}
