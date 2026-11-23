// Package testlog gives each test a logger that writes to its own output.

package testlog

import (
	"log/slog"
	"testing"
)

// New returns a debug-level logger over t.Output(), so its lines show beside the
// test that wrote them and a goroutine that logs after the test ends fails it.
func New(t testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
}
