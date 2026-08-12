// Test log sink for recording versioned physical task-log records.

package agenttest

import (
	"bytes"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// LogSink records task-log records in memory. Version must be set to the
// physical record version represented by the test.
type LogSink struct {
	bytes.Buffer

	Version agent.LogVersion
}

// LogVersion returns the physical record version owned by the sink.
func (s *LogSink) LogVersion() agent.LogVersion { return s.Version }

// AppendNative records one complete native physical record.
func (s *LogSink) AppendNative(data []byte) error {
	_, err := s.Write(data)
	return err
}

// AppendMessage records one versioned caic control record.
func (s *LogSink) AppendMessage(m agent.Message) error {
	data, err := agent.MarshalLogMessage(s.LogVersion(), m)
	if err != nil {
		return err
	}
	return s.AppendNative(append(data, '\n'))
}

// Close releases no resources.
func (*LogSink) Close() error { return nil }

var _ agent.LogSink = (*LogSink)(nil)
