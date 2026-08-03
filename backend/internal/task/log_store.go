// LogStore owns task log segments, metadata headers, trailers, and replay writer attachment.

package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

// LogStore manages raw task JSONL logs and their companion replay writers.
type LogStore struct {
	LogDir             string
	EventReplayFactory func(logPath string, h harness.Name) (EventReplayWriter, error)
}

// Open creates a JSONL log segment and writes its metadata header.
func (s *LogStore) Open(t *Task) (io.WriteCloser, error) {
	if err := os.MkdirAll(s.LogDir, 0o750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	path := filepath.Join(s.LogDir, taskLogFileName(t))
	w, err := openTaskLogForAppend(path, t, true)
	if err != nil {
		return nil, err
	}
	if err := writeMetadataHeader(w, t); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	if err := s.attachReplay(t, path); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	return w, nil
}

// Reopen validates and opens an existing v1 JSONL log for appending without a header.
func (s *LogStore) Reopen(t *Task) (io.WriteCloser, error) {
	if s.LogDir == "" {
		return nil, errors.New("no log dir")
	}
	path := filepath.Join(s.LogDir, taskLogFileName(t))
	w, err := openTaskLogForAppend(path, t, false)
	if err != nil {
		return nil, err
	}
	if err := s.attachReplay(t, path); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	return w, nil
}

func openTaskLogForAppend(path string, t *Task, create bool) (*taskLogWriter, error) {
	cleanPath := filepath.Clean(path)
	if create {
		w, err := newTaskLogWriter(cleanPath, os.O_CREATE|os.O_EXCL|os.O_RDWR|os.O_APPEND)
		if err == nil {
			info, statErr := w.file.Stat()
			if statErr == nil {
				_, statErr = verifyPhysicalLog(path, w.file, info)
			}
			if statErr != nil {
				return nil, errors.Join(statErr, w.Close())
			}
			return w, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create log file: %w", err)
		}
	}

	w, err := newTaskLogWriter(cleanPath, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return nil, err
	}
	if err := validateRawLogAppend(w.file, w.file, path, t); err != nil {
		return nil, errors.Join(err, w.Close())
	}
	return w, nil
}

func validateRawLogAppend(source io.ReadSeeker, f *os.File, path string, t *Task) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	authority, err := scanLogAuthority(source, path)
	if err != nil {
		return fmt.Errorf("validate task log for append: %w", err)
	}
	if _, err := verifyPhysicalLog(path, f, info); err != nil {
		return fmt.Errorf("validate task log for append: %w", err)
	}
	if authority.Version != agent.LogVersionV1 {
		return fmt.Errorf("append task log: version %d requires versioned log sink", authority.Version)
	}
	if authority.Harness != t.Harness {
		return fmt.Errorf("append task log: header harness %q does not match task harness %q", authority.Harness, t.Harness)
	}
	return nil
}

// WriteResultTrailer appends a MetaResultMessage to the log writer.
func (*LogStore) WriteResultTrailer(w io.Writer, title string, res *Result) error {
	if w == nil {
		return ErrNoLog
	}
	mr := agent.MetaResultMessage{
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
	data, err := json.Marshal(mr)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

// WriteContextCleared appends a context_cleared system message to the log writer.
func (*LogStore) WriteContextCleared(w io.Writer) error {
	if w == nil {
		return ErrNoLog
	}
	msg := syntheticContextCleared()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func (s *LogStore) attachReplay(t *Task, path string) error {
	t.SetLogPath(path)
	if s.EventReplayFactory == nil {
		return nil
	}
	w, err := s.EventReplayFactory(path, t.Harness)
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

func writeMetadataHeader(w io.Writer, t *Task) error {
	repos := t.ReposSnapshot()
	metaRepos := make([]agent.MetaRepo, len(repos))
	for i, r := range repos {
		metaRepos[i] = agent.MetaRepo{Name: r.Name, BaseBranch: r.BaseBranch, Branch: r.Branch, MountedPath: r.MountedPath}
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
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal log metadata: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write log metadata: %w", err)
	}
	return nil
}
