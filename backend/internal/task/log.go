// Serialized task log writer, metadata append, and compression helpers.

package task

import (
	"bytes"
	"crypto/sha256"
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

// compressLogFile atomically replaces a plain task log with a verified zstd
// copy. It is intentionally usable by live terminal cleanup, which has no
// retained semantic snapshot.
func compressLogFile(path string) (string, error) {
	compressed, _, err := compressLogFileWithSnapshot(path, nil)
	return compressed, err
}

// compressionFileOps isolates the rename transaction for focused failure tests.
type compressionFileOps struct {
	stat   func(string) (os.FileInfo, error)
	rename func(string, string) error
	remove func(string) error
}

var standardCompressionFileOps = compressionFileOps{
	stat:   os.Stat,
	rename: os.Rename,
	remove: os.Remove,
}

// compressLogFileWithSnapshot validates a complete temporary zstd stream and,
// when available, its semantic authority before removing the plain source.
func compressLogFileWithSnapshot(path string, source *ValidatedLogSnapshot) (string, *ValidatedLogSnapshot, error) {
	return compressLogFileWithSnapshotAndOps(path, source, standardCompressionFileOps)
}

func compressLogFileWithSnapshotAndOps(path string, source *ValidatedLogSnapshot, ops compressionFileOps) (string, *ValidatedLogSnapshot, error) {
	if path == "" || isLogCompressed(path) {
		return path, source, nil
	}
	if !strings.HasSuffix(path, logPlainExt) {
		return "", nil, fmt.Errorf("not a task log path: %s", path)
	}
	info, err := ops.stat(filepath.Clean(path))
	if err != nil {
		return "", nil, err
	}
	dst := compressedLogPath(path)
	tmp := compressedLogTempPath(path)
	if err := ops.remove(tmp); err != nil && !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("remove stale temp compressed log: %w", err)
	}

	in, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", nil, err
	}
	out, err := os.OpenFile(filepath.Clean(tmp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, errors.Join(err, in.Close())
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		return "", nil, errors.Join(err, in.Close(), out.Close())
	}
	var sourceSize int64
	var sourceHash []byte
	var copyErr error
	if source == nil {
		digest := sha256.New()
		sourceSize, copyErr = io.Copy(io.MultiWriter(enc, digest), in)
		sourceHash = digest.Sum(nil)
	} else {
		sourceSize, copyErr = io.Copy(enc, in)
	}
	closeErr := errors.Join(enc.Close(), out.Close(), in.Close())
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = ops.remove(tmp)
		return "", nil, err
	}
	if err := verifyUnchangedCompressedLogSource(path, info, ops.stat, "while compressing"); err != nil {
		_ = ops.remove(tmp)
		return "", nil, err
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		_ = ops.remove(tmp)
		return "", nil, fmt.Errorf("set compressed log mtime: %w", err)
	}
	var compressedSnapshot *ValidatedLogSnapshot
	if source == nil {
		if err := validateCompressedLogCopy(tmp, sourceSize, sourceHash); err != nil {
			_ = ops.remove(tmp)
			return "", nil, fmt.Errorf("validate temporary compressed log: %w", err)
		}
	} else {
		compressedSnapshot, err = rebindCompressedValidatedSnapshotAsCompressed(tmp, source)
		if err != nil {
			_ = ops.remove(tmp)
			return "", nil, fmt.Errorf("validate temporary compressed task log: %w", err)
		}
		if !source.ContentDigestValid || compressedSnapshot.ContentSize != source.ContentSize || compressedSnapshot.ContentDigest != source.ContentDigest {
			_ = ops.remove(tmp)
			return "", nil, errors.New("temporary compressed log content differs from source snapshot")
		}
	}
	tmpInfo, err := ops.stat(tmp)
	if err != nil {
		_ = ops.remove(tmp)
		return "", nil, fmt.Errorf("stat temporary compressed log: %w", err)
	}
	// Validate the source immediately before publication so ordinary source
	// changes cannot leave a compressed replacement visible.
	if err := verifyUnchangedCompressedLogSource(path, info, ops.stat, "before replacing source"); err != nil {
		_ = ops.remove(tmp)
		return "", nil, err
	}
	if err := ops.rename(tmp, dst); err != nil {
		_ = ops.remove(tmp)
		return "", nil, fmt.Errorf("rename compressed log: %w", err)
	}
	if err := validatePromotedCompressedLog(dst, tmpInfo, ops.stat); err != nil {
		return "", nil, errors.Join(err, removePromotedCompressedLog(dst, tmpInfo, ops))
	}
	// A source replacement can still race the rename. Once publication has
	// happened, remove only the file whose identity was recorded for tmp.
	if err := verifyUnchangedCompressedLogSource(path, info, ops.stat, "before replacing source"); err != nil {
		return "", nil, errors.Join(err, removePromotedCompressedLog(dst, tmpInfo, ops))
	}
	if err := ops.remove(path); err != nil {
		return "", nil, errors.Join(fmt.Errorf("remove plain log after compression: %w", err), removePromotedCompressedLog(dst, tmpInfo, ops))
	}
	if compressedSnapshot != nil {
		compressedSnapshot.Path = filepath.Clean(dst)
	}
	return dst, compressedSnapshot, nil
}

func verifyUnchangedCompressedLogSource(path string, expected os.FileInfo, stat func(string) (os.FileInfo, error), stage string) error {
	current, err := stat(filepath.Clean(path))
	if err != nil {
		return err
	}
	if !os.SameFile(expected, current) || current.Size() != expected.Size() || current.ModTime().UnixNano() != expected.ModTime().UnixNano() {
		return fmt.Errorf("task log changed %s: %s", stage, path)
	}
	return nil
}

func validatePromotedCompressedLog(path string, temporary os.FileInfo, stat func(string) (os.FileInfo, error)) error {
	current, err := stat(path)
	if err != nil {
		return fmt.Errorf("stat promoted compressed log: %w", err)
	}
	if !os.SameFile(temporary, current) {
		return fmt.Errorf("promoted compressed log identity changed: %s", path)
	}
	return nil
}

// removePromotedCompressedLog removes a failed transaction's destination only
// after proving it still has the temporary file's recorded identity.
func removePromotedCompressedLog(path string, temporary os.FileInfo, ops compressionFileOps) error {
	if err := validatePromotedCompressedLog(path, temporary, ops.stat); err != nil {
		return err
	}
	if err := ops.remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove promoted compressed log: %w", err)
	}
	return nil
}

// validateCompressedLogCopy proves that the temporary compressed descriptor
// reaches zstd EOF and exactly reproduces the bytes copied from the source.
func validateCompressedLogCopy(path string, wantSize int64, wantHash []byte) (retErr error) {
	r, err := openCompressedLogReader(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := r.Close(); retErr == nil {
			retErr = closeErr
		}
	}()
	info, err := r.file.Stat()
	if err != nil {
		return err
	}
	digest := sha256.New()
	size, err := io.Copy(digest, r)
	if err != nil {
		return err
	}
	if size != wantSize || !bytes.Equal(digest.Sum(nil), wantHash) {
		return errors.New("compressed log content differs from source")
	}
	_, err = verifyPhysicalLog(path, r.file, info)
	return err
}

// compressibleLogState reports whether a task state has no resumable log use.
func compressibleLogState(s State) bool {
	return s == StatePurged || s == StateFailed
}

// CompressTerminalLogs compresses loaded non-revivable task logs and updates
// their paths so later lazy reads use the compressed files.
func CompressTerminalLogs(logs []*LoadedTask) error {
	var errs []error
	for _, lt := range logs {
		if lt == nil || !compressibleLogState(lt.State) {
			continue
		}
		if isLogCompressed(lt.path) {
			continue
		}
		snapshot := lt.ValidatedSnapshot()
		if snapshot == nil || !validatedSnapshotMatchesFile(snapshot, lt.path) {
			errs = append(errs, fmt.Errorf("task log %s has no current validated snapshot", lt.path))
			continue
		}
		compressed, compressedSnapshot, err := compressLogFileWithSnapshot(lt.path, snapshot)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lt.path = compressed
		lt.LogSize = compressedSnapshot.Size
		lt.setValidatedSnapshot(compressedSnapshot)
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

// openCompressedLogReader opens path as zstd regardless of its filename.
func openCompressedLogReader(path string) (*zstdReadCloser, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	d, err := zstd.NewReader(f)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &zstdReadCloser{dec: d, file: f}, nil
}
