// Test log sink for recording physical v3 task-log records.

package agenttest

import (
	"bytes"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// LogSink records v3 task-log records in memory.
type LogSink struct {
	bytes.Buffer
}

// AppendNative records one complete native physical record.
func (s *LogSink) AppendNative(data []byte) error {
	_, err := s.Write(data)
	return err
}

// AppendMessage records one versioned caic control record.
func (s *LogSink) AppendMessage(m agent.Message) error {
	data, err := agent.MarshalLogMessage(m)
	if err != nil {
		return err
	}
	return s.AppendNative(append(data, '\n'))
}

// Close releases no resources.
func (*LogSink) Close() error { return nil }

var _ agent.LogSink = (*LogSink)(nil)
