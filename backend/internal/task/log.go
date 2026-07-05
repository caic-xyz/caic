// Serialized task log writer, metadata append, and compression helpers.

package task

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
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

// taskLogWriter is needed for now because task log ownership is currently
// split across several components: agent.Conn writes raw harness output, RepoWorkspace
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

// newTaskLogWriter opens a serialized append writer for an active task log.
func newTaskLogWriter(path string, flags int) (*taskLogWriter, error) {
	f, err := os.OpenFile(path, flags, 0o600) //nolint:gosec // path is derived from task log directory and task ID.
	if err != nil {
		return nil, err
	}
	return &taskLogWriter{file: f}, nil
}

// Write serializes writes to the underlying task log file.
func (w *taskLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	return w.file.Write(p)
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

// openLogReader opens a plain or zstd-compressed task log for reading.
func openLogReader(path string) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	if !isLogCompressed(path) {
		return f, nil
	}
	d, err := zstd.NewReader(f)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &zstdReadCloser{dec: d, file: f}, nil
}

// compressLogFile atomically replaces a plain task log with a zstd copy.
func compressLogFile(path string) (string, error) {
	if path == "" || isLogCompressed(path) {
		return path, nil
	}
	if !strings.HasSuffix(path, logPlainExt) {
		return "", fmt.Errorf("not a task log path: %s", path)
	}
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	dst := compressedLogPath(path)
	tmp := dst + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove stale temp compressed log: %w", err)
	}

	in, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	out, err := os.OpenFile(filepath.Clean(tmp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", errors.Join(err, in.Close())
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		return "", errors.Join(err, in.Close(), out.Close())
	}

	_, copyErr := io.Copy(enc, in)
	closeErr := errors.Join(enc.Close(), out.Close(), in.Close())
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("set compressed log mtime: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename compressed log: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("remove plain log after compression: %w", err)
	}
	return dst, nil
}

// compressibleLogState reports whether a task state has no resumable log use.
func compressibleLogState(s State) bool {
	return s == StatePurged || s == StateFailed
}

// compressLogIfDone compresses the task log after a terminal non-revivable state.
func (t *Task) compressLogIfDone(s State) error {
	if !compressibleLogState(s) {
		return nil
	}
	t.mu.Lock()
	path := t.logPath
	t.mu.Unlock()
	compressed, err := compressLogFile(path)
	if err != nil {
		return err
	}
	if compressed != path {
		t.SetLogPath(compressed)
	}
	return nil
}

// CompressTerminalLogs compresses loaded non-revivable task logs and updates
// their paths so later lazy reads use the compressed files.
func CompressTerminalLogs(logs []*LoadedTask) error {
	var errs []error
	for _, lt := range logs {
		if lt == nil || !compressibleLogState(lt.State) {
			continue
		}
		compressed, err := compressLogFile(lt.path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lt.path = compressed
		if info, statErr := os.Stat(compressed); statErr == nil {
			lt.LogSize = info.Size()
		}
		if err := storeLogSummary(lt); err != nil {
			errs = append(errs, fmt.Errorf("write task log summary: %w", err))
		}
	}
	return errors.Join(errs...)
}

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
	r.dec.Close()
	return r.file.Close()
}
