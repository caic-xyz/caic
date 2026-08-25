// Task metadata conversion helpers.

package task

import (
	"slices"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func metaCacheMountsFromRuntime(in []runtime.CacheMount) []agent.MetaCacheMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]agent.MetaCacheMount, len(in))
	for i, m := range in {
		out[i] = agent.MetaCacheMount{
			Name:          m.Name,
			Description:   m.Description,
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
			Shallow:       m.Shallow,
		}
	}
	return out
}

func metaMountsFromRuntime(in []runtime.Mount) []agent.MetaMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]agent.MetaMount, len(in))
	for i, m := range in {
		out[i] = agent.MetaMount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
		}
	}
	return out
}

// terminalLogSummary projects the live task and its bounded terminal result
// into the same LoadedTask shape used by startup. Completion already has these
// values in memory, so persisting this projection avoids decoding the newly
// compressed message body on the next process start.
func (t *Task) terminalLogSummary(version agent.LogVersion, res *taskslog.Result) *taskslog.LoadedTask {
	snapshot := t.Snapshot()
	// GitRoot is process-local checkout state and is absent from the durable
	// log header. Do not let the fast completion path persist a stale host path
	// that a normal log scan would never reconstruct.
	for i := range snapshot.Repos {
		snapshot.Repos[i].GitRoot = ""
	}
	return &taskslog.LoadedTask{
		TaskID:            t.ID.String(),
		Prompt:            t.InitialPrompt.Text,
		Title:             snapshot.Title,
		Repos:             snapshot.Repos,
		LogVersion:        version,
		Harness:           t.Harness,
		StartedAt:         t.StartedAt,
		LastStateUpdateAt: snapshot.StateUpdatedAt,
		State:             res.State,
		ForgeIssue:        snapshot.ForgeIssue,
		ForkedFromTaskID:  t.ForkedFromTaskID.String(),
		ForgeOwner:        snapshot.ForgeOwner,
		ForgeRepo:         snapshot.ForgeRepo,
		ForgePR:           snapshot.ForgePR,
		Tailscale:         snapshot.Tailscale,
		USB:               snapshot.USB,
		Display:           snapshot.Display,
		Sudo:              snapshot.Sudo,
		GitHubToken:       snapshot.GitHubToken,
		RuntimeName:       snapshot.RuntimeName,
		BaseImage:         t.BaseImage,
		ContainerPlatform: t.ContainerPlatform,
		MaxCPUs:           t.MaxCPUs,
		CacheMounts:       slices.Clone(t.CacheMounts),
		Mounts:            slices.Clone(t.Mounts),
		Model:             snapshot.Model,
		Effort:            t.Effort,
		SessionID:         snapshot.SessionID,
		AgentVersion:      snapshot.AgentVersion,
		DiffCreated:       t.DiffCreated(),
		LastTrailer:       res,
	}
}
