// Shared MCP fixture models accepted work that survives a caller disconnect.

package agenttest

import (
	"context"

	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/mcp/mcptest"
)

// BlockingRegistry waits for Release before completing admitted calls. Started
// must have room for the expected invocations. Canceling the call does not
// revoke already-accepted work; the owning test must eventually release it.
type BlockingRegistry struct {
	mcptest.FakeRegistry

	Started chan struct{}
	Release <-chan struct{}
}

// CallTool models an accepted input operation with a separately owned lifetime.
func (r *BlockingRegistry) CallTool(ctx context.Context, p *mcp.ToolsCallParams) (mcp.RawToolResult, error) {
	r.Started <- struct{}{}
	<-r.Release
	return r.FakeRegistry.CallTool(ctx, p)
}
