// Compressed task log metadata summary cache for fast startup reloads.

package task

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
)

const (
	logSummaryVersion = 1
	logSummaryExt     = ".taskmeta.json"
)

// logSummary is the on-disk sidecar for compressed task-log metadata.
//
// It records enough log identity to reject stale caches. When the version,
// byte size, and mtime match the current zstd log, startup can reconstruct
// LoadedTask metadata without streaming the whole compressed file.
type logSummary struct {
	Version  int               `json:"v"`
	LogSize  int64             `json:"logSize"`
	LogModNs int64             `json:"logModNs"`
	Task     loadedTaskSummary `json:"task"`
}

type loadedTaskSummary struct {
	TaskID            string              `json:"taskID,omitempty"`
	Prompt            string              `json:"prompt,omitempty"`
	Title             string              `json:"title,omitempty"`
	Repos             []repoMountSummary  `json:"repos,omitempty"`
	Harness           harness.Name        `json:"harness,omitempty"`
	StartedAt         time.Time           `json:"startedAt,omitzero"`
	LastStateUpdateAt time.Time           `json:"lastStateUpdateAt,omitzero"`
	State             string              `json:"state,omitempty"`
	ForgeIssue        int                 `json:"forgeIssue,omitempty"`
	ForgeOwner        string              `json:"forgeOwner,omitempty"`
	ForgeRepo         string              `json:"forgeRepo,omitempty"`
	ForgePR           int                 `json:"forgePR,omitempty"`
	Tailscale         bool                `json:"tailscale,omitempty"`
	USB               bool                `json:"usb,omitempty"`
	Display           bool                `json:"display,omitempty"`
	Sudo              bool                `json:"sudo,omitempty"`
	GitHubToken       bool                `json:"gitHubToken,omitempty"`
	BaseImage         string              `json:"baseImage,omitempty"`
	ContainerPlatform string              `json:"containerPlatform,omitempty"`
	MaxCPUs           int                 `json:"maxCPUs,omitempty"`
	CacheMounts       []cacheMountSummary `json:"cacheMounts,omitempty"`
	Mounts            []mountSummary      `json:"mounts,omitempty"`
	Model             string              `json:"model,omitempty"`
	Effort            string              `json:"effort,omitempty"`
	SessionID         string              `json:"sessionID,omitempty"`
	AgentVersion      string              `json:"agentVersion,omitempty"`
	Result            *resultSummary      `json:"result,omitempty"`
}

type resultSummary struct {
	State       string         `json:"state,omitempty"`
	DiffStat    agent.DiffStat `json:"diffStat,omitempty"`
	CostUSD     float64        `json:"costUSD,omitempty"`
	DurationNs  int64          `json:"durationNs,omitempty"`
	NumTurns    int            `json:"numTurns,omitempty"`
	Usage       agent.Usage    `json:"usage,omitzero"`
	AgentResult string         `json:"agentResult,omitempty"`
	Err         string         `json:"err,omitempty"`
}

type repoMountSummary struct {
	Name        string `json:"name,omitempty"`
	BaseBranch  string `json:"baseBranch,omitempty"`
	Branch      string `json:"branch,omitempty"`
	GitRoot     string `json:"gitRoot,omitempty"`
	MountedPath string `json:"mountedPath,omitempty"`
}

type cacheMountSummary struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	HostPath    string `json:"hostPath,omitempty"`
	MountPath   string `json:"mountPath,omitempty"`
	ReadOnly    bool   `json:"readOnly,omitempty"`
	Shallow     bool   `json:"shallow,omitempty"`
}

type mountSummary struct {
	HostPath  string `json:"hostPath,omitempty"`
	MountPath string `json:"mountPath,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

func logSummaryPath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), trimLogExt(filepath.Base(logPath))+logSummaryExt)
}

// loadLogSummary returns a LoadedTask reconstructed from a fresh sidecar.
//
// Missing, invalid, or stale summaries are cache misses. Callers then fall back
// to scanning the compressed log and rewriting the sidecar.
func loadLogSummary(logPath string) (*LoadedTask, bool) {
	info, err := os.Stat(filepath.Clean(logPath))
	if err != nil {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Clean(logSummaryPath(logPath)))
	if err != nil {
		return nil, false
	}
	var summary logSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		slog.Warn("task log summary: invalid cache", "path", logSummaryPath(logPath), "err", err)
		return nil, false
	}
	if summary.Version != logSummaryVersion || summary.LogSize != info.Size() || summary.LogModNs != info.ModTime().UnixNano() {
		return nil, false
	}
	return summary.Task.toLoadedTask(logPath, info.Size()), true
}

func storeLogSummary(lt *LoadedTask) error {
	if lt == nil || lt.path == "" {
		return nil
	}
	info, err := os.Stat(filepath.Clean(lt.path))
	if err != nil {
		return err
	}
	summary := logSummary{
		Version:  logSummaryVersion,
		LogSize:  info.Size(),
		LogModNs: info.ModTime().UnixNano(),
		Task:     loadedTaskSummaryFrom(lt),
	}
	data, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	path := logSummaryPath(lt.path)
	tmp := path + ".tmp"
	if err := os.WriteFile(filepath.Clean(tmp), data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}

func loadedTaskSummaryFrom(lt *LoadedTask) loadedTaskSummary {
	return loadedTaskSummary{
		TaskID:            lt.TaskID,
		Prompt:            lt.Prompt,
		Title:             lt.Title,
		Repos:             repoMountSummaries(lt.Repos),
		Harness:           lt.Harness,
		StartedAt:         lt.StartedAt,
		LastStateUpdateAt: lt.LastStateUpdateAt,
		State:             lt.State.String(),
		ForgeIssue:        lt.ForgeIssue,
		ForgeOwner:        lt.ForgeOwner,
		ForgeRepo:         lt.ForgeRepo,
		ForgePR:           lt.ForgePR,
		Tailscale:         lt.Tailscale,
		USB:               lt.USB,
		Display:           lt.Display,
		Sudo:              lt.Sudo,
		GitHubToken:       lt.GitHubToken,
		BaseImage:         lt.BaseImage,
		ContainerPlatform: lt.ContainerPlatform,
		MaxCPUs:           lt.MaxCPUs,
		CacheMounts:       cacheMountSummaries(lt.CacheMounts),
		Mounts:            mountSummaries(lt.Mounts),
		Model:             lt.Model,
		Effort:            lt.Effort,
		SessionID:         lt.SessionID,
		AgentVersion:      lt.AgentVersion,
		Result:            resultSummaryFrom(lt.Result),
	}
}

func (s *loadedTaskSummary) toLoadedTask(path string, size int64) *LoadedTask {
	return &LoadedTask{
		path:              path,
		TaskID:            s.TaskID,
		Prompt:            s.Prompt,
		Title:             s.Title,
		Repos:             repoMountsFromSummaries(s.Repos),
		Harness:           s.Harness,
		StartedAt:         s.StartedAt,
		LastStateUpdateAt: s.LastStateUpdateAt,
		State:             parseState(s.State),
		ForgeIssue:        s.ForgeIssue,
		ForgeOwner:        s.ForgeOwner,
		ForgeRepo:         s.ForgeRepo,
		ForgePR:           s.ForgePR,
		Tailscale:         s.Tailscale,
		USB:               s.USB,
		Display:           s.Display,
		Sudo:              s.Sudo,
		GitHubToken:       s.GitHubToken,
		BaseImage:         s.BaseImage,
		ContainerPlatform: s.ContainerPlatform,
		MaxCPUs:           s.MaxCPUs,
		CacheMounts:       cacheMountsFromSummaries(s.CacheMounts),
		Mounts:            mountsFromSummaries(s.Mounts),
		Model:             s.Model,
		Effort:            s.Effort,
		SessionID:         s.SessionID,
		AgentVersion:      s.AgentVersion,
		LogSize:           size,
		Result:            s.Result.toResult(),
	}
}

func repoMountSummaries(repos []RepoMount) []repoMountSummary {
	if len(repos) == 0 {
		return nil
	}
	out := make([]repoMountSummary, len(repos))
	for i, r := range repos {
		out[i] = repoMountSummary(r)
	}
	return out
}

func repoMountsFromSummaries(repos []repoMountSummary) []RepoMount {
	if len(repos) == 0 {
		return nil
	}
	out := make([]RepoMount, len(repos))
	for i, r := range repos {
		out[i] = RepoMount(r)
	}
	return out
}

func cacheMountSummaries(mounts []runtime.CacheMount) []cacheMountSummary {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]cacheMountSummary, len(mounts))
	for i, m := range mounts {
		out[i] = cacheMountSummary{
			Name:        m.Name,
			Description: m.Description,
			HostPath:    m.HostPath,
			MountPath:   m.MountPath,
			ReadOnly:    m.ReadOnly,
			Shallow:     m.Shallow,
		}
	}
	return out
}

func cacheMountsFromSummaries(mounts []cacheMountSummary) []runtime.CacheMount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]runtime.CacheMount, len(mounts))
	for i, m := range mounts {
		out[i] = runtime.CacheMount{
			Name:        m.Name,
			Description: m.Description,
			HostPath:    m.HostPath,
			MountPath:   m.MountPath,
			ReadOnly:    m.ReadOnly,
			Shallow:     m.Shallow,
		}
	}
	return out
}

func mountSummaries(mounts []runtime.Mount) []mountSummary {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]mountSummary, len(mounts))
	for i, m := range mounts {
		out[i] = mountSummary{
			HostPath:  m.HostPath,
			MountPath: m.MountPath,
			ReadOnly:  m.ReadOnly,
		}
	}
	return out
}

func mountsFromSummaries(mounts []mountSummary) []runtime.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]runtime.Mount, len(mounts))
	for i, m := range mounts {
		out[i] = runtime.Mount{
			HostPath:  m.HostPath,
			MountPath: m.MountPath,
			ReadOnly:  m.ReadOnly,
		}
	}
	return out
}

func resultSummaryFrom(r *Result) *resultSummary {
	if r == nil {
		return nil
	}
	summary := &resultSummary{
		State:       r.State.String(),
		DiffStat:    r.DiffStat,
		CostUSD:     r.CostUSD,
		DurationNs:  int64(r.Duration),
		NumTurns:    r.NumTurns,
		Usage:       r.Usage,
		AgentResult: r.AgentResult,
	}
	if r.Err != nil {
		summary.Err = r.Err.Error()
	}
	return summary
}

func (s *resultSummary) toResult() *Result {
	if s == nil {
		return nil
	}
	result := &Result{
		State:       parseState(s.State),
		DiffStat:    s.DiffStat,
		CostUSD:     s.CostUSD,
		Duration:    time.Duration(s.DurationNs),
		NumTurns:    s.NumTurns,
		Usage:       s.Usage,
		AgentResult: s.AgentResult,
	}
	if s.Err != "" {
		result.Err = errors.New(s.Err)
	}
	return result
}
