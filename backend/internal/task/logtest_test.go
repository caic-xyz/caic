// Test task-log sinks for task package tests.

package task

import (
	"bytes"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

type testLogSink struct {
	bytes.Buffer

	version agent.LogVersion
}

func (s *testLogSink) AppendNative(data []byte) error {
	_, err := s.Write(data)
	return err
}

func (s *testLogSink) AppendMessage(m agent.Message) error {
	version := s.version
	if version == 0 {
		version = agent.LogVersionV1
	}
	data, err := agent.MarshalLogMessage(version, m)
	if err != nil {
		return err
	}
	return s.AppendNative(append(data, '\n'))
}

func (*testLogSink) Close() error { return nil }
