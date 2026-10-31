// Store owns physical task-log initialization, byte appenders, queue sidecars, and compression.

package taskslog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/taskslog/data/inputqueue"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/klauspost/compress/zstd"
)

// Writer accepts exact LF-terminated records or batches. Success permits the
// caller's cursor advancement, without implying fsync or stronger delivery.
type Writer interface {
	Append(encoded []byte) error
	Close() error
}

// Store owns one physical journal directory and serializes compression.
type Store struct {
	LogDir     string
	compressMu sync.Mutex
}

// NewStore creates a physical store; the directory is created on first write.
func NewStore(logDir string) *Store { return &Store{LogDir: filepath.Clean(logDir)} }

// Open creates a JSONL log segment with a v3 metadata header, or reopens an
// existing segment without rewriting its header. name must be a base
// filename (no path separators or "."/".." elements); Open resolves it under
// LogDir and returns the resolved path.
func (s *Store) Open(name string, header *v3.MetaMessage) (Writer, string, error) {
	path, err := s.resolveName(name)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(s.LogDir, 0o750); err != nil {
		return nil, "", fmt.Errorf("create log dir: %w", err)
	}
	w, created, err := openTaskLogForAppend(path, header, true)
	if err != nil {
		return nil, "", err
	}
	if created {
		if err := writeMetadataHeader(w, header); err != nil {
			return nil, "", errors.Join(err, discardNewLog(w, path))
		}
	}
	return w, path, nil
}

// Initialize exclusively creates and closes a new log with its metadata header.
// Errors remove the newly created file. Existing files are never reopened or
// modified. The caller owns Rollback until publication succeeds.
func (s *Store) Initialize(name string, header *v3.MetaMessage) (*PreparedLog, error) {
	path, err := s.resolveName(name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.LogDir, 0o750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	w, err := newTaskLogWriter(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND)
	if err != nil {
		return nil, fmt.Errorf("create task log: %w", err)
	}
	info, err := w.file.Stat()
	if err != nil {
		return nil, errors.Join(err, discardNewLog(w, path))
	}
	if err := writeMetadataHeader(w, header); err != nil {
		return nil, errors.Join(err, discardNewLog(w, path))
	}
	if err := w.Close(); err != nil {
		return nil, errors.Join(err, removeOwnedLog(path, info))
	}
	return &PreparedLog{Path: path, info: info}, nil
}

func discardNewLog(w *taskLogWriter, path string) error {
	info, err := w.file.Stat()
	closeErr := w.Close()
	if err != nil {
		return errors.Join(err, closeErr)
	}
	return errors.Join(closeErr, removeOwnedLog(path, info))
}

func removeOwnedLog(path string, info os.FileInfo) error {
	current, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return nil
	}
	return os.Remove(path)
}

// Reopen validates and opens an existing task log for appending without a
// header. name must be a base filename (no path separators or "."/".."
// elements); Reopen resolves it under LogDir and returns the resolved path.
// A compressed log is restored to a validated plain log before appending.
func (s *Store) Reopen(name string, header *v3.MetaMessage) (Writer, string, error) {
	path, err := s.resolveName(name)
	if err != nil {
		return nil, "", err
	}
	if IsCompressed(path) {
		s.compressMu.Lock()
		defer s.compressMu.Unlock()
		plain := strings.TrimSuffix(path, ".zst")
		if _, err := os.Stat(plain); errors.Is(err, os.ErrNotExist) {
			if err := restorePlainLog(path, plain, header); err != nil {
				return nil, "", err
			}
		} else if err != nil {
			return nil, "", err
		}
		w, _, err := openTaskLogForAppend(plain, header, false)
		if err != nil {
			return nil, "", err
		}
		// The plain log is authoritative, including after an interrupted
		// restore. Remove the archive only after validating the plain header.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", errors.Join(err, w.Close())
		}
		return w, plain, nil
	}
	w, _, err := openTaskLogForAppend(path, header, false)
	if err != nil {
		return nil, "", err
	}
	return w, path, nil
}

// restorePlainLog publishes a complete, validated decoded log. A decode or
// header error leaves the archive intact and never publishes a partial log.
func restorePlainLog(path, plain string, header *v3.MetaMessage) (err error) {
	in, err := OpenReader(path)
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(plain), ".restore-task-log-*")
	if err != nil {
		return errors.Join(err, in.Close())
	}
	tmp := out.Name()
	defer func() {
		if removeErr := os.Remove(tmp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	_, copyErr := io.Copy(out, in)
	readErr := errors.Join(copyErr, in.Close())
	if readErr != nil {
		return errors.Join(readErr, out.Close())
	}
	validationErr := validateRawLogAppend(out, tmp, header)
	if err := errors.Join(validationErr, out.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp, plain); err != nil {
		return fmt.Errorf("restore plain task log: %w", err)
	}
	return nil
}

func openTaskLogForAppend(path string, header *v3.MetaMessage, create bool) (w *taskLogWriter, created bool, err error) {
	cleanPath := filepath.Clean(path)
	if create {
		w, err = newTaskLogWriter(cleanPath, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND)
		if err == nil {
			return w, true, nil
		}
		if !os.IsExist(err) {
			return nil, false, fmt.Errorf("create log file: %w", err)
		}
	}
	w, err = newTaskLogWriter(cleanPath, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return nil, false, err
	}
	err = validateRawLogAppend(w.file, cleanPath, header)
	if err != nil {
		return nil, false, errors.Join(err, w.Close())
	}
	return w, false, nil
}

// validateRawLogAppend validates the immutable header needed to safely select
// the append encoding. caic owns task-log writes, so reopening does not rescan
// historical records.
func validateRawLogAppend(f *os.File, path string, header *v3.MetaMessage) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	scanner := NewScanner(f, path)
	if _, err := scanner.ReadHeader(); err != nil {
		return fmt.Errorf("validate task log for append: %w", err)
	}
	if scanner.Harness != header.Harness {
		return fmt.Errorf("append task log: header harness %q does not match task harness %q", scanner.Harness, header.Harness)
	}

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	return nil
}

// LoadInputQueue reads pending prompts; an absent file is an empty queue.
func (s *Store) LoadInputQueue() ([]inputqueue.Entry, error) {
	f, err := os.Open(filepath.Join(s.LogDir, "input-queue.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	token, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("read input queue: %w", err)
	}
	if token != json.Delim('[') {
		return nil, errors.New("input queue must be an array")
	}
	var out []inputqueue.Entry
	for dec.More() {
		var entry inputqueue.Entry
		if err := dec.Decode(&entry); err != nil {
			return nil, fmt.Errorf("read queued prompt: %w", err)
		}
		out = append(out, entry)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("close input queue: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected trailing input queue data")
	}
	return out, nil
}

// SaveInputQueue commits a complete replacement before callers publish it.
func (s *Store) SaveInputQueue(entries []inputqueue.Entry) error {
	if err := os.MkdirAll(s.LogDir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.LogDir, ".input-queue-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.WriteString("[\n")
	enc := json.NewEncoder(f)
	for i, q := range entries {
		if err != nil {
			break
		}
		if i > 0 {
			_, err = f.WriteString(",\n")
			if err != nil {
				break
			}
		}
		err = enc.Encode(q)
	}
	if err == nil {
		_, err = f.WriteString("]\n")
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(s.LogDir, "input-queue.json"))
}

// Compress atomically replaces a plain terminal task log with its zstd
// representation. A failed compression leaves the source log intact. Calls
// for the same path are serialized by compressMu, so a concurrent compressor
// either observes the compressed result and returns it, or sees the plain
// source gone with the compressed sibling present and adopts it.
func (s *Store) Compress(path string) (string, error) {
	s.compressMu.Lock()
	defer s.compressMu.Unlock()
	if path == "" || IsCompressed(path) {
		return path, nil
	}
	if !strings.HasSuffix(path, logPlainExt) {
		return "", fmt.Errorf("not a task log path: %s", path)
	}
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		if os.IsNotExist(err) {
			// The plain source vanished after the scan. If the compressed
			// sibling exists the log was already settled by its owner (for
			// example the per-task terminal path); adopt it instead of
			// reporting a spurious compression failure.
			if _, statErr := os.Stat(compressedLogPath(path)); statErr == nil {
				return compressedLogPath(path), nil
			}
		}
		return "", err
	}
	tmp := compressedLogTempPath(path)
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
	// Preserve the source mtime: the settled age cutoff and the per-repo
	// newest-N cap rank by mtime, so the compressed log must measure as old as
	// its settlement, not as new as its compression.
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("set compressed log mtime: %w", err)
	}
	dst := compressedLogPath(path)
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename compressed log: %w", err)
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			// The plain source is gone; the compressed log is the settled
			// result, so report it as success instead of deleting it.
			return dst, nil
		}
		return "", errors.Join(
			fmt.Errorf("remove plain log after compression: %w", err),
			os.Remove(dst),
		)
	}
	return dst, nil
}

// resolveName validates name is a plain task-log filename and joins it under
// LogDir, denying any path that escapes it (separators, "..", or absolute
// paths).
func (s *Store) resolveName(name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("task log name must be a base filename, got %q", name)
	}
	return filepath.Join(s.LogDir, name), nil
}

// PreparedLog is an initialized, closed task log ready for publication.
type PreparedLog struct {
	Path string

	info os.FileInfo
}

// Rollback removes this unpublished log, preserving a replacement file.
func (p *PreparedLog) Rollback() error {
	return removeOwnedLog(p.Path, p.info)
}

// EncodeHeader encodes and validates the canonical v3 physical header without LF.
func EncodeHeader(header *v3.MetaMessage) ([]byte, error) {
	meta := *header
	meta.Version = 3
	meta.MessageType = "caic_meta"
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	_, err = DecodeFrame(encoded)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	encoded, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func writeMetadataHeader(log Writer, header *v3.MetaMessage) error {
	encoded, err := EncodeHeader(header)
	if err != nil {
		return err
	}
	if err := log.Append(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write log metadata: %w", err)
	}
	return nil
}
