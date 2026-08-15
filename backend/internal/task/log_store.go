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
func (s *LogStore) Open(t *Task) (agent.LogSink, error) {
	if err := os.MkdirAll(s.LogDir, 0o750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	path := filepath.Join(s.LogDir, taskLogFileName(t))
	w, created, err := openTaskLogForAppend(path, t, true)
	if err != nil {
		return nil, err
	}
	if created {
		w.version = agent.LogVersionV2
		if err := writeMetadataHeader(w, t); err != nil {
			return nil, errors.Join(err, w.Close())
		}
	}
	t.SetLogPath(path)
	return w, nil
}

// Reopen validates and opens an existing task log for appending without a header.
func (s *LogStore) Reopen(t *Task) (agent.LogSink, error) {
	path, err := s.reopenPath(t)
	if err != nil {
		return nil, err
	}

	w, _, err := openTaskLogForAppend(path, t, false)
	if err != nil {
		return nil, err
	}
	t.SetLogPath(path)
	return w, nil
}

func openTaskLogForAppend(path string, t *Task, create bool) (w *taskLogWriter, created bool, err error) {
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
	version, err := validateRawLogAppend(w.file, cleanPath, t)
	if err != nil {
		return nil, false, errors.Join(err, w.Close())
	}
	w.version = version
	return w, false, nil
}

// validateRawLogAppend validates the immutable header needed to safely select
// the append encoding. caic owns task-log writes, so reopening does not rescan
// historical records.
func validateRawLogAppend(f *os.File, path string, t *Task) (agent.LogVersion, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	scanner := newPhysicalLogScanner(f, path)
	if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
		return 0, fmt.Errorf("validate task log for append: %w", err)
	}
	if scanner.authority.Harness != t.Harness {
		return 0, fmt.Errorf("append task log: header harness %q does not match task harness %q", scanner.authority.Harness, t.Harness)
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

// WriteTaskResultTrailer reopens a task log, appends its terminal result, and
// closes the scoped log owner. It is for terminal and import paths without
// an active session-owned log.
func (s *LogStore) WriteTaskResultTrailer(t *Task, res *Result) error {
	log, err := s.Reopen(t)
	if err != nil {
		return err
	}
	return errors.Join(log.AppendMessage(resultTrailer(t.Title(), res)), log.Close())
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

// Compress closes log and compresses t's log only when state is non-revivable.
// Closing the live writer first ensures the compressed source contains every
// completed append. A failed compression leaves the plain source for retry.
func (s *LogStore) Compress(t *Task, log agent.LogSink, state State) error {
	if log != nil {
		if err := log.Close(); err != nil {
			return fmt.Errorf("close task log before compression: %w", err)
		}
	}
	if !state.IsTerminal() {
		return nil
	}
	path := t.LogPath()
	compressed, err := s.compressPath(path)
	if err != nil {
		return err
	}
	if compressed != path {
		t.SetLogPath(compressed)
	}
	return nil
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

// reopenPath uses a task's retained log path when available. Imported logs
// may not use the current filename convention, so that path selects the log
// for a scoped terminal append.
func (s *LogStore) reopenPath(t *Task) (string, error) {
	if path := t.LogPath(); path != "" {
		return path, nil
	}
	if s.LogDir == "" {
		return "", errors.New("no log dir")
	}
	return filepath.Join(s.LogDir, taskLogFileName(t)), nil
}

// taskLogFileName is the log file name for t: "<taskID>-<safeRepo>-<safeBranch>".
// The branch is known before the log opens in every path (the primary path
// reserves it before setup; a fork reserves and pins it before Fork), so the
// full name is available up front and stays stable for later Reopen.
func taskLogFileName(t *Task) string {
	safeRepo := ""
	safeBranch := ""
	if p := t.Primary(); p != nil {
		safeRepo = strings.ReplaceAll(p.Name, "/", "-")
		safeBranch = strings.ReplaceAll(p.Branch, "/", "-")
	}
	return t.ID.String() + "-" + safeRepo + "-" + safeBranch + ".jsonl"
}

func writeMetadataHeader(log agent.LogSink, t *Task) error {
	repos := t.ReposSnapshot()
	metaRepos := make([]agent.MetaRepo, len(repos))
	for i, r := range repos {
		metaRepos[i] = agent.MetaRepo{Name: r.Name, BaseBranch: r.BaseBranch, Branch: r.Branch, ContainerPath: r.ContainerPath}
	}
	meta := agent.MetaMessage{
		MessageType:       "caic_meta",
		Version:           int(log.LogVersion()),
		Prompt:            t.InitialPrompt.Text,
		Title:             t.Title(),
		Repos:             metaRepos,
		Harness:           t.Harness,
		Model:             t.Model,
		Effort:            t.Effort,
		StartedAt:         t.StartedAt,
		ForgeIssue:        t.ForgeIssue,
		ForkedFromTaskID:  t.ForkedFromTaskID.String(),
		Tailscale:         t.Tailscale,
		USB:               t.USB,
		Display:           t.Display,
		Sudo:              t.Sudo,
		GitHubToken:       t.GitHubTokenEnabled(),
		RuntimeName:       string(t.RuntimeName),
		BaseImage:         t.BaseImage,
		ContainerPlatform: t.ContainerPlatform,
		MaxCPUs:           t.MaxCPUs,
		CacheMounts:       metaCacheMountsFromRuntime(t.CacheMounts),
		Mounts:            metaMountsFromRuntime(t.Mounts),
	}
	if err := log.AppendMessage(&meta); err != nil {
		return fmt.Errorf("write log metadata: %w", err)
	}
	return nil
}
