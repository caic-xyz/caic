// Test-only parser resolvers adapt focused parser callbacks for task-log tests.

package history

import (
	"errors"
	"path/filepath"

	"github.com/caic-xyz/caic/backend/internal/taskslog"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

type testParserResolver struct {
	resolve func(harness.Name) (agent.WireFormat, error)
}

func newTestParserResolver(resolve func(harness.Name) (agent.WireFormat, error)) testParserResolver {
	return testParserResolver{resolve: resolve}
}

func (r testParserResolver) ResolveParser(h harness.Name) (agent.MessageParser, error) {
	parse, err := r.resolve(h)
	if err != nil {
		return nil, err
	}
	if parse == nil {
		return nil, errors.New("native message parser is nil")
	}
	return parse, nil
}

func (r testParserResolver) ResolveMetadataParser(h harness.Name) (agent.MessageParser, error) {
	return r.ResolveParser(h)
}

// loadMessagesForTest reconstructs through the same configured source as production.
func loadMessagesForTest(lt *LoadedTask, resolver ParserResolver) error {
	source, err := NewStore(testLogger(), taskslog.NewStore(filepath.Dir(lt.LogPath())), resolver).LoadSource(lt.LogPath())
	if err != nil {
		return err
	}
	if err := source.LoadMessages(); err != nil {
		return err
	}
	*lt = *source
	return nil
}
