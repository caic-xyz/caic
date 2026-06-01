// Serialized task log writer and metadata append helpers.

package task

import (
	"errors"
	"os"
	"sync"
)

// ErrNoLog reports that a task has no active or persisted log to append to.
var ErrNoLog = errors.New("no task log")

// taskLogWriter is needed for now because task log ownership is currently
// split across several components: agent.Conn writes raw harness output, Runner
// writes lifecycle metadata, Task.WriteToLog writes task metadata, harnesses
// write synthetic records through opts.LogW, and adoption restores enough state
// to write metadata again later.
//
// That shared ownership means the active log writer must at least provide a
// clear concurrency contract: JSONL appends from session output and server-side
// metadata are serialized through one writer, and close is idempotent across
// converging lifecycle paths. The cleaner design is a single TaskLog/LogStore
// owner with explicit append methods instead of handing out raw io.WriteClosers.
type taskLogWriter struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
}

func newTaskLogWriter(path string, flags int) (*taskLogWriter, error) {
	f, err := os.OpenFile(path, flags, 0o600) //nolint:gosec // path is derived from task log directory and task ID.
	if err != nil {
		return nil, err
	}
	return &taskLogWriter{file: f}, nil
}

func (w *taskLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	return w.file.Write(p)
}

func (w *taskLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}
