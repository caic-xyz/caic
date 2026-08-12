// Package tasktest provides shared test doubles for task package seams.
package tasktest

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// FakeEventReplayWriter is a task.EventReplayWriter test double.
type FakeEventReplayWriter struct {
	Messages       []agent.Message
	ParsedMessages []agent.ParsedMessage
	Commits        []string

	CommitErr error
	WriteErr  error
}

// WriteMessage records msg.
func (f *FakeEventReplayWriter) WriteMessage(_ context.Context, msg agent.ParsedMessage) error {
	f.ParsedMessages = append(f.ParsedMessages, msg)
	f.Messages = append(f.Messages, msg.Message)
	return f.WriteErr
}

// Commit records logPath and returns CommitErr.
func (f *FakeEventReplayWriter) Commit(_ context.Context, logPath string) error {
	f.Commits = append(f.Commits, logPath)
	return f.CommitErr
}
