// Package logtest provides lifecycle-safe slog helpers for tests.
package logtest

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

var start = time.Now()

// Logger returns a debug slog logger that writes through t.Log.
func Logger(t testing.TB) *slog.Logger {
	w := &writer{t: t}
	t.Cleanup(w.close)
	return slog.New(slog.NewTextHandler(w, options()))
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

	mu   sync.Mutex
	done bool
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return len(p), nil
	}
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

func (w *writer) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
}
