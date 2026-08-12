// LogStore owns task log segments, metadata headers, trailers, and replay writer attachment.

package task

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/jsonutil"
)

// CacheProofProvider returns a fresh bounded proof for one task log.
type CacheProofProvider func(string) (CacheProof, error)

// EventReplayFactory creates a replay writer from Reopen's initial append proof
// and a task-owned provider for later cache validation.
type EventReplayFactory func(string, CacheProof, CacheProofProvider) (EventReplayWriter, error)

// LogStore manages raw task JSONL logs and their companion replay writers.
type LogStore struct {
	LogDir             string
	EventReplayFactory EventReplayFactory
}

// Open creates a JSONL log segment and writes its metadata header.
func (s *LogStore) Open(t *Task) (agent.LogSink, error) {
	if err := os.MkdirAll(s.LogDir, 0o750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	path := filepath.Join(s.LogDir, taskLogFileName(t))
	w, _, err := openTaskLogForAppend(path, t, true)
	if err != nil {
		return nil, err
	}
	w.version = t.RelayLogVersion()
	if err := writeMetadataHeader(w, t); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	var proof CacheProof
	if s.EventReplayFactory != nil {
		proof, err = t.CacheProofForLog(path)
		if err != nil {
			return nil, errors.Join(err, w.Close())
		}
	}
	if err := s.attachReplay(t, path, proof); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	return w, nil
}

// Reopen validates and opens an existing task log for appending without a header.
func (s *LogStore) Reopen(t *Task) (agent.LogSink, error) {
	path, err := s.reopenPath(t)
	if err != nil {
		return nil, err
	}

	w, proof, err := openTaskLogForAppend(path, t, false)
	if err != nil {
		return nil, err
	}
	if err := s.attachReplay(t, path, proof); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	return w, nil
}

func openTaskLogForAppend(path string, t *Task, create bool) (*taskLogWriter, CacheProof, error) {
	cleanPath := filepath.Clean(path)
	if create {
		w, err := newTaskLogWriter(cleanPath, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND)
		if err == nil {
			info, statErr := w.file.Stat()
			if statErr == nil {
				_, statErr = verifyPhysicalLog(path, w.file, info)
			}
			if statErr != nil {
				return nil, CacheProof{}, errors.Join(statErr, w.Close())
			}
			return w, CacheProof{}, nil
		}
		if !os.IsExist(err) {
			return nil, CacheProof{}, fmt.Errorf("create log file: %w", err)
		}
	}

	w, err := newTaskLogWriter(cleanPath, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return nil, CacheProof{}, err
	}
	if snapshot := t.logValidationProof(cleanPath); snapshot != nil {
		if proof, snapshotErr := validateRawLogAppendSnapshot(cleanPath, w.file, t, snapshot); snapshotErr == nil {
			w.version = proof.Version
			return w, proof, nil
		}
	}
	proof, err := validateRawLogAppend(w.file, w.file, cleanPath, t)
	if err != nil {
		return nil, CacheProof{}, errors.Join(err, w.Close())
	}
	w.version = proof.Version
	return w, proof, nil
}

// validateRawLogAppendSnapshot reuses an in-memory EOF validation only after a
// fresh bounded header and identity observation binds that proof to the open
// append descriptor and its current path.
func validateRawLogAppendSnapshot(path string, f *os.File, t *Task, snapshot *ValidatedLogSnapshot) (CacheProof, error) {
	if snapshot == nil || !snapshot.EOFValidated || snapshot.Path != filepath.Clean(path) {
		return CacheProof{}, errors.New("task log validation snapshot is stale")
	}
	info, err := f.Stat()
	if err != nil {
		return CacheProof{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return CacheProof{}, err
	}
	proof, err := cacheProofFromReader(path, &physicalLogReader{file: f, reader: f, info: info})
	if err != nil {
		return CacheProof{}, fmt.Errorf("validate task log snapshot for append: %w", err)
	}
	if proof != (CacheProof{
		Device:    snapshot.Device,
		Inode:     snapshot.Inode,
		Size:      snapshot.Size,
		ModTimeNs: snapshot.ModTimeNs,
		Version:   snapshot.Authority.Version,
		Harness:   snapshot.Authority.Harness,
		RawHeader: snapshot.RawHeader,
	}) {
		return CacheProof{}, errors.New("task log validation snapshot is stale")
	}
	if proof.Harness != t.Harness {
		return CacheProof{}, fmt.Errorf("append task log: header harness %q does not match task harness %q", proof.Harness, t.Harness)
	}
	return proof, nil
}

func validateRawLogAppend(source io.ReadSeeker, f *os.File, path string, t *Task) (CacheProof, error) {
	info, err := f.Stat()
	if err != nil {
		return CacheProof{}, err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return CacheProof{}, err
	}
	scanner := newPhysicalLogScanner(source, path)
	if _, err := scanner.ReadHeader(&jsonutil.FieldWarner{}); err != nil {
		return CacheProof{}, fmt.Errorf("validate task log for append: %w", err)
	}
	for scanner.Scan() {
	}
	if err := scanner.Err(); err != nil {
		return CacheProof{}, fmt.Errorf("validate task log for append: %w", err)
	}
	stableInfo, err := verifyPhysicalLog(path, f, info)
	if err != nil {
		return CacheProof{}, fmt.Errorf("validate task log for append: %w", err)
	}
	if scanner.authority.Harness != t.Harness {
		return CacheProof{}, fmt.Errorf("append task log: header harness %q does not match task harness %q", scanner.authority.Harness, t.Harness)
	}
	identity := physicalFileIdentityFromFile(f, stableInfo)
	if !identity.Valid {
		return CacheProof{}, fmt.Errorf("task log has no stable physical identity: %s", path)
	}
	return CacheProof{Device: identity.Device, Inode: identity.Inode, Size: stableInfo.Size(), ModTimeNs: stableInfo.ModTime().UnixNano(), Version: scanner.authority.Version, Harness: scanner.authority.Harness, RawHeader: string(scanner.headerRaw)}, nil
}

// WriteResultTrailer appends a MetaResultMessage to an active task log.
func (*LogStore) WriteResultTrailer(log agent.LogSink, title string, res *Result) error {
	if log == nil {
		return ErrNoLog
	}
	return log.AppendMessage(resultTrailer(title, res))
}

// WriteTaskResultTrailer reopens a task log, appends its terminal result, and
// closes the scoped log owner. It is for terminal and adoption paths without
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

// reopenPath uses the task's previously validated path when available. Logs
// loaded during adoption may not use the current filename convention, so the
// validated path is the authoritative identity for a scoped terminal append.
func (s *LogStore) reopenPath(t *Task) (string, error) {
	if path := t.LogPath(); path != "" {
		return path, nil
	}
	if s.LogDir == "" {
		return "", errors.New("no log dir")
	}
	return filepath.Join(s.LogDir, taskLogFileName(t)), nil
}

func (s *LogStore) attachReplay(t *Task, path string, proof CacheProof) error {
	t.SetLogPath(path)
	if s.EventReplayFactory == nil {
		return nil
	}
	w, err := s.EventReplayFactory(path, proof, t.CacheProofForLog)
	if err != nil {
		return err
	}
	t.StartEventReplay(w)
	return nil
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
		Version:           1,
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
