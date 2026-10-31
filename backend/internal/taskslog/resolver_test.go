// Test-only wire resolvers adapt focused parser callbacks for task-log tests.

package taskslog

import (
	"errors"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

type testWireResolver struct {
	resolve func(harness.Name) (agent.WireFormat, error)
}

func newTestWireResolver(resolve func(harness.Name) (agent.WireFormat, error)) testWireResolver {
	return testWireResolver{resolve: resolve}
}

func (r testWireResolver) ResolveWire(h harness.Name) (agent.WireFormat, error) {
	parse, err := r.resolve(h)
	if err != nil {
		return nil, err
	}
	if parse == nil {
		return nil, errors.New("native message parser is nil")
	}
	return parse, nil
}
