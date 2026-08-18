// LogStore owns task log segments, metadata headers, trailers, and terminal compression.

package task

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// LogStore manages raw task JSONL logs.
type LogStore struct {
	LogDir string
}

// Open creates a JSONL log segment with a v2 metadata header, or reopens an
// existing segment without rewriting its header.
func (s *LogStore) Open(path string, header *agent.MetaMessage) (agent.LogSink, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	w, created, err := openTaskLogForAppend(path, header, true)
	if err != nil {
		return nil, err
	}
	if created {
		w.version = agent.LogVersionV2
		if err := writeMetadataHeader(w, header); err != nil {
			return nil, errors.Join(err, w.Close())
		}
	}
	return w, nil
}

// Reopen validates and opens an existing task log for appending without a header.
func (s *LogStore) Reopen(path string, header *agent.MetaMessage) (agent.LogSink, error) {
	w, _, err := openTaskLogForAppend(path, header, false)
	if err != nil {
		return nil, err
	}
	return w, nil
}

func openTaskLogForAppend(path string, header *agent.MetaMessage, create bool) (w *taskLogWriter, created bool, err error) {
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
	version, err := validateRawLogAppend(w.file, cleanPath, header)
	if err != nil {
		return nil, false, errors.Join(err, w.Close())
	}
	w.version = version
	return w, false, nil
}

// validateRawLogAppend validates the immutable header needed to safely select
// the append encoding. caic owns task-log writes, so reopening does not rescan
// historical records.
func validateRawLogAppend(f *os.File, path string, header *agent.MetaMessage) (agent.LogVersion, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	scanner := newPhysicalLogScanner(f, path)
	if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
		return 0, fmt.Errorf("validate task log for append: %w", err)
	}
	if scanner.authority.Harness != header.Harness {
		return 0, fmt.Errorf("append task log: header harness %q does not match task harness %q", scanner.authority.Harness, header.Harness)
	}
	if err := scanner.authority.Version.Validate(); err != nil {
		return 0, fmt.Errorf("validate task log for append: %w", err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return 0, err
	}
	return scanner.authority.Version, nil
}

// WriteResultTrailer appends a MetaResultMessage to an active task log.
func (*LogStore) WriteResultTrailer(log agent.LogSink, title string, res *Result) error {
	if log == nil {
		return ErrNoLog
	}
	return log.AppendMessage(resultTrailer(title, res))
}

func resultTrailer(title string, res *Result) *agent.MetaResultMessage {
	mr := &agent.MetaResultMessage{
		MessageType:              "caic_result",
		State:                    res.State.String(),
		Title:                    title,
		CostUSD:                  res.CostUSD,
		Duration:                 res.Duration.Seconds(),
		NumTurns:                 res.NumTurns,
		InputTokens:              res.Usage.InputTokens,
		OutputTokens:             res.Usage.OutputTokens,
		CacheCreationInputTokens: res.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     res.Usage.CacheReadInputTokens,
		ReasoningOutputTokens:    res.Usage.ReasoningOutputTokens,
		DiffStat:                 res.DiffStat,
		AgentResult:              res.AgentResult,
	}
	if res.Err != nil {
		mr.Error = res.Err.Error()
	}
	return mr
}

// WriteContextCleared appends a context_cleared system message to the task log.
func (*LogStore) WriteContextCleared(log agent.LogSink) error {
	if log == nil {
		return ErrNoLog
	}
	return log.AppendMessage(syntheticContextCleared())
}

// Compress closes log and compresses path only when state is non-revivable.
// Closing the live writer first ensures the compressed source contains every
// completed append. A failed compression leaves the plain source for retry.
func (s *LogStore) Compress(path string, log agent.LogSink, state State) (string, error) {
	if log != nil {
		if err := log.Close(); err != nil {
			return path, fmt.Errorf("close task log before compression: %w", err)
		}
	}
	if !state.IsTerminal() {
		return path, nil
	}
	return s.compressPath(path)
}

// CompressTerminalLogs compresses loaded non-revivable task logs during startup.
// Plain sources take precedence over interrupted plain-and-compressed pairs, so
// a retry always compresses the authoritative plain source.
func (s *LogStore) CompressTerminalLogs(logs []*LoadedTask) error {
	var errs []error
	for _, lt := range logs {
		if lt == nil || !lt.State.IsTerminal() || isLogCompressed(lt.path) {
			continue
		}
		compressed, err := s.compressPath(lt.path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lt.path = compressed
		info, err := os.Stat(compressed)
		if err != nil {
			errs = append(errs, fmt.Errorf("stat compressed task log: %w", err))
			continue
		}
		lt.LogSize = info.Size()
	}
	return errors.Join(errs...)
}

// compressPath atomically replaces a plain terminal task log with its zstd
// representation. A failed compression leaves the source log intact.
func (*LogStore) compressPath(path string) (string, error) {
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
		return "", errors.Join(
			fmt.Errorf("remove plain log after compression: %w", err),
			os.Remove(dst),
		)
	}
	return dst, nil
}

func writeMetadataHeader(log agent.LogSink, header *agent.MetaMessage) error {
	meta := *header
	meta.Version = int(log.LogVersion())
	if err := log.AppendMessage(&meta); err != nil {
		return fmt.Errorf("write log metadata: %w", err)
	}
	return nil
}
