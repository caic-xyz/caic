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

// AppendRecord records complete encoded physical records.
func (s *LogSink) AppendRecord(record agent.PhysicalRecord) error {
	_, err := s.Write(record.Encoded)
	return err
}

// AppendMessage records one versioned caic control record.
func (s *LogSink) AppendMessage(m agent.Message) error {
	data, err := agent.MarshalLogMessage(m)
	if err != nil {
		return err
	}
	return s.AppendRecord(agent.PhysicalRecord{Encoded: append(data, '\n')})
}

// Close releases no resources.
func (*LogSink) Close() error { return nil }

var _ agent.LogSink = (*LogSink)(nil)
