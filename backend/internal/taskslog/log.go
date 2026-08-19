// Package taskslog owns task-log persistence, compression, and replay loading.

package taskslog

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// ErrNoLog reports that a task has no active or persisted log to append to.
var ErrNoLog = errors.New("no task log")

const (
	logPlainExt      = ".jsonl"
	logCompressedExt = ".jsonl.zst"
)

// IsLogName reports whether name is a caic task log filename.
func IsLogName(name string) bool {
	return strings.HasSuffix(name, logPlainExt) || strings.HasSuffix(name, logCompressedExt)
}

// taskLogWriter owns serialized physical appends to one task log. Callers
// provide either complete native records or semantic caic controls; raw file
// writes never escape this type.
type taskLogWriter struct {
	mu      sync.Mutex
	file    *os.File
	version agent.LogVersion
	closed  bool
}

// newTaskLogWriter opens a serialized append writer for an active task log.
func newTaskLogWriter(path string, flags int) (*taskLogWriter, error) {
	f, err := os.OpenFile(path, flags, 0o600) //nolint:gosec // path is derived from task log directory and task ID.
	if err != nil {
		return nil, err
	}
	return &taskLogWriter{file: f, version: agent.LogVersionV1}, nil
}

// LogVersion returns the physical record version owned by the task log.
func (w *taskLogWriter) LogVersion() agent.LogVersion { return w.version }

// AppendNative appends one complete encoded native physical record.
func (w *taskLogWriter) AppendNative(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return os.ErrClosed
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return errors.New("task log native record is not LF-terminated")
	}
	n, err := w.file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

// AppendMessage encodes and appends one backend-owned control record.
func (w *taskLogWriter) AppendMessage(m agent.Message) error {
	data, err := agent.MarshalLogMessage(w.version, m)
	if err != nil {
		return err
	}
	return w.AppendNative(append(data, '\n'))
}

// Close closes the underlying task log file once.
func (w *taskLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}

// isLogCompressed reports whether path names a zstd-compressed task log.
func isLogCompressed(path string) bool {
	return strings.HasSuffix(path, logCompressedExt)
}

// trimLogExt removes either task log extension from name.
func trimLogExt(name string) string {
	if base, ok := strings.CutSuffix(name, logCompressedExt); ok {
		return base
	}
	return strings.TrimSuffix(name, logPlainExt)
}

// compressedLogPath returns the zstd path for a task log path.
func compressedLogPath(path string) string {
	if isLogCompressed(path) {
		return path
	}
	return path + ".zst"
}

// compressedLogTempPath returns a temporary compressed path outside the task-log namespace.
func compressedLogTempPath(path string) string {
	return compressedLogPath(path) + ".tmp"
}

// openLogReader opens a plain or zstd-compressed task log for reading.
func openLogReader(path string) (io.ReadCloser, error) {
	if isLogCompressed(path) {
		return openCompressedLogReader(path)
	}
	return os.Open(filepath.Clean(path))
}

var zstdDecodeDummy = strings.NewReader("")

var zstdDecoderPool = sync.Pool{New: func() any {
	decoder, err := zstd.NewReader(zstdDecodeDummy)
	if err != nil {
		panic(err)
	}
	return decoder
}}

type zstdReadCloser struct {
	dec  *zstd.Decoder
	file *os.File
}

// Read decompresses bytes from the wrapped zstd task log.
func (r *zstdReadCloser) Read(p []byte) (int, error) {
	return r.dec.Read(p)
}

// Close releases the zstd decoder and closes the backing file.
func (r *zstdReadCloser) Close() error {
	// Reset detaches the file before returning the reusable decoder to the pool.
	if err := r.dec.Reset(nil); err != nil {
		return errors.Join(err, r.file.Close())
	}
	zstdDecoderPool.Put(r.dec)
	return r.file.Close()
}

// openCompressedLogReader opens path as zstd regardless of its filename.
func openCompressedLogReader(path string) (*zstdReadCloser, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	d, ok := zstdDecoderPool.Get().(*zstd.Decoder)
	if !ok {
		panic("taskslog zstd decoder pool contains an invalid value")
	}
	if err := d.Reset(f); err != nil {
		zstdDecoderPool.Put(d)
		return nil, errors.Join(err, f.Close())
	}
	return &zstdReadCloser{dec: d, file: f}, nil
}
