// Tests bounded MCP ownership across canceled streams, reconnects, and shutdown.

package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/mcp/mcptest"
)

func TestMCPExecutor(t *testing.T) {
	t.Parallel()
	t.Run("Wrap/canceled_streams_retain_capacity_until_completion", func(t *testing.T) {
		t.Parallel()
		ctx, cancelOwner := context.WithCancel(t.Context())
		release := make(chan struct{})
		registry := &agenttest.BlockingRegistry{Started: make(chan struct{}, 64), Release: release}
		executor := agent.NewMCPExecutor(ctx)
		var workers sync.WaitGroup
		t.Cleanup(func() { close(release); cancelOwner(); executor.Close(); workers.Wait() })
		params := &mcp.ToolsCallParams{Name: "echo", Arguments: []byte(`{}`)}
		// A fresh wrapper models a reconnected stream. Its canceled wait must
		// not free a slot still occupied by accepted input elsewhere.
		for range 63 {
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan error, 1)
			wrapped := executor.Wrap(registry)
			workers.Go(func() { _, err := wrapped.CallTool(ctx, params); result <- err })
			<-registry.Started
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("stream cancellation=%v", err)
			}
		}
		completed := make(chan error, 1)
		workers.Go(func() { _, err := executor.Wrap(registry).CallTool(t.Context(), params); completed <- err })
		<-registry.Started
		if _, err := executor.Wrap(mcptest.FakeRegistry{}).Tools(t.Context()); !errors.Is(err, agent.ErrResponseQueueFull) {
			t.Fatalf("reconnect reset occupied capacity: %v", err)
		}
		for range 64 {
			release <- struct{}{}
		}
		if err := <-completed; err != nil {
			t.Fatal(err)
		}
		if _, err := executor.Wrap(mcptest.FakeRegistry{}).Tools(t.Context()); err != nil {
			t.Fatalf("completed call retained capacity: %v", err)
		}
		cancelOwner()
		if _, err := executor.Wrap(mcptest.FakeRegistry{}).Tools(t.Context()); !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown admission=%v", err)
		}
	})
	t.Run("Wrap/error_arguments_exceed_budget", func(t *testing.T) {
		t.Parallel()
		executor := agent.NewMCPExecutor(t.Context())
		t.Cleanup(executor.Close)
		if _, err := executor.Wrap(mcptest.FakeRegistry{}).CallTool(t.Context(), &mcp.ToolsCallParams{Name: "echo", Arguments: []byte(strings.Repeat("x", 4<<20))}); !errors.Is(err, agent.ErrResponseQueueFull) {
			t.Fatalf("oversized arguments=%v", err)
		}
	})
}
