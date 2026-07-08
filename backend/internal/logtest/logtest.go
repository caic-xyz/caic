// Package logtest provides slog helpers for tests.
package logtest

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

var start = time.Now()

// Logger returns a debug slog logger that writes through t.Log.
func Logger(t testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(writer{t: t}, options()))
}

func options() *slog.HandlerOptions {
	return &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Int64("ms", time.Since(start).Milliseconds())
			}
			return a
		},
	}
}

type writer struct {
	t testing.TB
}

func (w writer) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}
