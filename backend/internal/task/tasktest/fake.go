// Package tasktest provides shared test doubles for task package seams.
package tasktest

import "github.com/caic-xyz/caic/backend/internal/agent"

// FakeEventReplayWriter is a task.EventReplayWriter test double.
type FakeEventReplayWriter struct {
	Messages []agent.Message
	Commits  []string

	CommitErr error
}

// WriteMessage records msg.
func (f *FakeEventReplayWriter) WriteMessage(msg agent.Message) {
	f.Messages = append(f.Messages, msg)
}

// Commit records logPath and returns CommitErr.
func (f *FakeEventReplayWriter) Commit(logPath string) error {
	f.Commits = append(f.Commits, logPath)
	return f.CommitErr
}
