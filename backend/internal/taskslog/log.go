// Package taskslog owns task-log persistence, compression, and replay loading.

package taskslog

import (
	"bytes"
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

// zstdDecodeDummy is a minimal valid zstd frame used only to construct a pooled
// decoder; pooled decoders are reset to a real log before they are read.
var zstdDecodeDummy = buildZstdDummyFrame()

// buildZstdDummyFrame builds the tiny frame used to seed pooled decoders.
func buildZstdDummyFrame() []byte {
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf)
	_, _ = w.Write([]byte("x"))
	_ = w.Close()
	return buf.Bytes()
}

// zstdDecoderPool reuses zstd decoders across log reads. A decoder's window
// buffer is sized to the log and is otherwise re-allocated (and zeroed) on
// every open, which dominated cold-start CPU; pooling resets each decoder to
// the next file instead, so the window is allocated once per worker and reused.
var zstdDecoderPool = sync.Pool{
	New: func() any {
		d, err := zstd.NewReader(bytes.NewReader(zstdDecodeDummy))
		if err != nil {
			panic(err)
		}
		return d
	},
}

type zstdReadCloser struct {
	dec  *zstd.Decoder
	file *os.File
}

// Read decompresses bytes from the wrapped zstd task log.
func (r *zstdReadCloser) Read(p []byte) (int, error) {
	return r.dec.Read(p)
}

// Close detaches the stream and returns the zstd decoder to the shared pool,
// then closes the backing file. The detach matters: a read that stops before
// EOF leaves the decoder's async goroutines parked on the stream; Reset(nil)
// drains them so a pooled (or later GC-dropped) decoder cannot leak
// goroutines and their window buffers.
func (r *zstdReadCloser) Close() error {
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
	got := zstdDecoderPool.Get()
	d, ok := got.(*zstd.Decoder)
	if !ok {
		panic("taskslog: zstdDecoderPool entry is not a zstd.Decoder")
	}
	if err := d.Reset(f); err != nil {
		zstdDecoderPool.Put(got)
		return nil, errors.Join(err, f.Close())
	}
	return &zstdReadCloser{dec: d, file: f}, nil
}
