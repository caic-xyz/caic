// Owns execution recording from provisioning through producer drain and final physical close.

package taskrun

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/task"
	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// Run owns an execution's ordered records before and after transport startup.
// Its creator joins startup; the expected-identity claimant joins all producers
// and closes recording once. Task.recordingMu serializes those claims.
type Run struct {
	ctx             context.Context
	parent          context.Context
	cancel          context.CancelCauseFunc
	records         *agent.Records
	runtime         *AgentRuntime
	task            *Task
	startupDone     chan struct{}
	workMu          sync.Mutex
	work            sync.WaitGroup
	workSealed      bool
	pendingMutating map[agent.ToolKey]struct{}
	prepared        *repo.GitSnapshot
}

// RecordAppended advances the remote cursor only after physical acceptance.
func (r *Run) RecordAppended(record agent.PhysicalRecord) {
	if record.Provenance.Source != agent.RecordSourceRelay {
		return
	}
	t := r.task
	t.mu.Lock()
	if record.Provenance.Generation != "" {
		t.RelayOffset = 0
	}
	t.RelayOffset += int64(len(record.Encoded))
	t.mu.Unlock()
}

// PrepareRecords completes derived Git work at the finished turn's barrier.
// No task mutation or publication occurs before its controls have appended.
func (r *Run) PrepareRecords(_ context.Context, messages []agent.TimedMessage) ([]agent.Message, error) {
	r.prepared = nil
	var derived []agent.Message
	runtime := r.runtime
	t := r.task
	target := t.GitTarget()
	for _, parsed := range messages {
		switch msg := parsed.Message.(type) {
		case *agent.ToolUseMessage:
			if _, ok := mutatingTools[msg.Name]; ok {
				r.pendingMutating[agent.ToolKeyForSource(msg.Provenance(), msg.ToolUseID)] = struct{}{}
			}
		case *agent.ToolResultMessage:
			key := agent.ToolKeyForSource(msg.Provenance(), msg.ToolUseID)
			if _, ok := r.pendingMutating[key]; !ok {
				continue
			}
			delete(r.pendingMutating, key)
			if runtime.Runtimes == nil || runtime.Checkout == nil {
				continue
			}
			snapshot, _ := runtime.Checkout.DiffStatAndRepoStates(r.ctx, runtime.Log, runtime.Runtimes, target) //nolint:contextcheck // Git preparation must stop with its execution, while Records survives to preserve final evidence.
			if err := r.ctx.Err(); err != nil {
				return nil, err
			}
			r.prepared = &snapshot
		case *agent.ResultMessage:
			if msg.Source.IsChild() || runtime.Runtimes == nil || runtime.Checkout == nil {
				continue
			}
			previous := t.latestCommitSnapshot()
			var baseline []v3.RepositoryCommit
			if previous != nil {
				baseline = previous.RepositoryCommits
			}
			snapshot, commits, change, _ := runtime.Checkout.TurnSnapshot(r.ctx, runtime.Log, runtime.Runtimes, target, baseline) //nolint:contextcheck // Git preparation must stop with its execution, while Records survives to preserve final evidence.
			if err := r.ctx.Err(); err != nil {
				return nil, err
			}
			r.prepared = &snapshot
			if len(commits) > 0 {
				derived = append(derived, agent.NewTurnCommitSnapshotMessage(commits, false, change))
			}
			t.mu.Lock()
			if snapshot.Read.InstanceID != "" && snapshot.Read.InstanceID == t.runtimeInstanceID && snapshot.Read.NewerThan(t.lastGitRead) {
				msg.DiffStat, _ = snapshot.Summary(t.liveDiffStat, t.liveRepoStates)
			}
			t.mu.Unlock()
		}
	}
	if r.prepared != nil {
		t.mu.Lock()
		valid := r.prepared.Read.InstanceID != "" && r.prepared.Read.InstanceID == t.runtimeInstanceID && r.prepared.Read.NewerThan(t.lastGitRead)
		stats, states := r.prepared.Summary(t.liveDiffStat, t.liveRepoStates)
		changed := !slices.Equal(t.liveDiffStat, stats) || len(states) > 0 && !slices.Equal(t.liveRepoStates, states)
		t.mu.Unlock()
		if valid && changed {
			derived = append([]agent.Message{agent.NewDiffStatMessage(stats, states)}, derived...)
		} else if !valid {
			r.prepared = nil
		}
	}
	return derived, nil
}

// ApplyRecords folds only committed messages and starts owned title work.
func (r *Run) ApplyRecords(ctx context.Context, messages []agent.TimedMessage) error {
	t := r.task
	changed := false
	title := false
	for _, parsed := range messages {
		changed = t.addParsedMessage(parsed) || changed
		if _, ok := parsed.Message.(*agent.ResultMessage); ok && !parsed.Message.Provenance().IsChild() {
			title = true
		}
	}
	if r.prepared != nil {
		t.mu.Lock()
		t.lastGitRead = r.prepared.Read
		t.mu.Unlock()
		r.prepared = nil
	}
	if changed {
		r.runtime.NotifyTaskChange()
	}
	if title && t.titleRefreshDue() {
		r.workMu.Lock()
		if !r.workSealed {
			r.work.Go(func() { t.generateTitle(r.ctx, r.runtime.Log, r, r.runtime.NotifyTaskChange) })
		}
		r.workMu.Unlock()
	}
	_ = ctx
	return nil
}

// joinTitles places a barrier after all joined external producers. The owner
// seals title admission before Wait, while remaining available for generated
// titles to submit their final metadata intents.
func (r *Run) joinTitles() error {
	r.workMu.Lock()
	sealed := r.workSealed
	r.workMu.Unlock()
	var err error
	if !sealed {
		err = r.records.CommitIntent(context.WithoutCancel(r.ctx), &titleSealIntent{run: r})
		// A poisoned owner cannot execute the barrier. Seal under the same
		// admission mutex so even its final callback cannot race Wait.
		r.workMu.Lock()
		r.workSealed = true
		r.workMu.Unlock()
	}
	r.work.Wait()
	return err
}

func (r *Run) close() error {
	joinErr := r.joinTitles()
	r.cancel(context.Canceled)
	return errors.Join(joinErr, r.records.Close())
}

type titleSealIntent struct{ run *Run }

func (*titleSealIntent) PrepareRecords(context.Context) ([]agent.Message, error) {
	return nil, nil
}

func (i *titleSealIntent) ApplyRecords(context.Context) error {
	i.run.workMu.Lock()
	i.run.workSealed = true
	i.run.workMu.Unlock()
	return nil
}

type repositoryIntent struct {
	task     *Task
	read     *repo.GitSnapshot
	accepted bool
	changed  bool
}

func (i *repositoryIntent) PrepareRecords(context.Context) ([]agent.Message, error) {
	t := i.task
	t.mu.Lock()
	defer t.mu.Unlock()
	i.accepted = i.read.Read.InstanceID != "" && i.read.Read.InstanceID == t.runtimeInstanceID && i.read.Read.NewerThan(t.lastGitRead)
	if !i.accepted {
		return nil, nil
	}
	stats, states := i.read.Summary(t.liveDiffStat, t.liveRepoStates)
	i.changed = !slices.Equal(t.liveDiffStat, stats) || len(states) > 0 && !slices.Equal(t.liveRepoStates, states)
	if !i.changed {
		return nil, nil
	}
	return []agent.Message{agent.NewDiffStatMessage(stats, states)}, nil
}
func (i *repositoryIntent) ApplyRecords(context.Context) error {
	if i.accepted {
		i.task.mu.Lock()
		i.task.lastGitRead = i.read.Read
		i.task.mu.Unlock()
	}
	return nil
}

type lifecycleIntent struct {
	runtime   *AgentRuntime
	task      *Task
	persisted task.State
	state     task.State
	allowed   []task.State
	previous  task.State
	accepted  bool
}

func (i *lifecycleIntent) PrepareRecords(context.Context) ([]agent.Message, error) {
	t := i.task
	t.mu.Lock()
	i.previous = t.state
	i.accepted = slices.Contains(i.allowed, t.state)
	title := t.title
	t.mu.Unlock()
	if !i.accepted {
		return nil, nil
	}
	return []agent.Message{taskhistory.ResultMessage(title, &task.Result{State: i.persisted})}, nil
}
func (i *lifecycleIntent) ApplyRecords(context.Context) error {
	if i.accepted {
		i.task.SetState(i.state)
		if i.previous != i.state {
			i.runtime.NotifyTaskChange()
		}
	}
	return nil
}

var _ agent.RecordHandler = (*Run)(nil)
var _ agent.RecordIntent = (*repositoryIntent)(nil)
var _ agent.RecordIntent = (*lifecycleIntent)(nil)

// titleIntent orders derived runtime-only title metadata. Its durability remains
// the task's normal terminal trailer; no transcript event represents a title.
type titleIntent struct {
	task       *Task
	generation uint64
	title      string
	notify     func()
	accepted   bool
}

func (i *titleIntent) PrepareRecords(context.Context) ([]agent.Message, error) {
	i.task.mu.Lock()
	i.accepted = i.generation == i.task.titleGeneration
	i.task.mu.Unlock()
	return nil, nil
}
func (i *titleIntent) ApplyRecords(ctx context.Context) error {
	i.task.mu.Lock()
	i.accepted = i.accepted && i.generation == i.task.titleGeneration && ctx.Err() == nil
	changed := false
	if i.accepted {
		previous := i.task.title
		i.task.setTitleLocked(i.title)
		changed = previous != i.task.title
	}
	i.task.mu.Unlock()
	if changed {
		i.notify()
	}
	return nil
}
