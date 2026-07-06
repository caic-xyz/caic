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
	EventReplayFactory func(logPath string, h harness.Name) EventReplayWriter
}

// Open creates a JSONL log file and writes a metadata header as the first line.
func (s *LogStore) Open(t *Task) (io.WriteCloser, error) {
	if err := os.MkdirAll(s.LogDir, 0o750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	path := filepath.Join(s.LogDir, taskLogFileName(t))
	f, err := newTaskLogWriter(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}
	s.attachReplay(t, path)
	if err := writeMetadataHeader(f, t); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Reopen opens an existing JSONL log file for appending without writing a metadata header.
func (s *LogStore) Reopen(t *Task) (io.WriteCloser, error) {
	if s.LogDir == "" {
		return nil, errors.New("no log dir")
	}
	path := filepath.Join(s.LogDir, taskLogFileName(t))
	w, err := newTaskLogWriter(path, os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return nil, err
	}
	s.attachReplay(t, path)
	return w, nil
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

func (s *LogStore) attachReplay(t *Task, path string) {
	t.SetLogPath(path)
	if s.EventReplayFactory != nil {
		t.StartEventReplay(s.EventReplayFactory(path, t.Harness))
	}
}

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
		Tailscale:         t.Tailscale,
		USB:               t.USB,
		Display:           t.Display,
		Sudo:              t.Sudo,
		GitHubToken:       t.GitHubTokenEnabled(),
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
