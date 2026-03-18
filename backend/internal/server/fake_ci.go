// Fake CI simulation for e2e tests: sets a PR and cycles checks to success.
//go:build e2e

package server

import (
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// maybeFakeCI polls until the task reaches a non-running state, then sets a
// fake PR and transitions CI from pending to success with progressive check
// completion.
func (s *Server) maybeFakeCI(t *task.Task) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		switch t.GetState() {
		case task.StateWaiting, task.StateAsking, task.StateHasPlan:
			goto ready
		case task.StatePurged, task.StateFailed:
			return
		default:
		}
	}
ready:
	t.SetPR("fake-owner", "fake-repo", 1)
	now := time.Now()
	checks := []forge.Check{
		{Name: "build", Owner: "fake-owner", Repo: "fake-repo", RunID: 1, JobID: 1, Status: forge.CheckRunStatusInProgress, QueuedAt: now, StartedAt: now},
		{Name: "test", Owner: "fake-owner", Repo: "fake-repo", RunID: 1, JobID: 2, Status: forge.CheckRunStatusQueued, QueuedAt: now},
		{Name: "lint", Owner: "fake-owner", Repo: "fake-repo", RunID: 1, JobID: 3, Status: forge.CheckRunStatusQueued, QueuedAt: now},
	}
	t.SetCIStatus(forge.CIStatusPending, checks)
	for i := range checks {
		select {
		case <-time.After(time.Second):
		case <-s.ctx.Done():
			return
		}
		checks[i].Status = forge.CheckRunStatusCompleted
		checks[i].Conclusion = forge.CheckRunConclusionSuccess
		checks[i].CompletedAt = time.Now()
		if i+1 < len(checks) {
			checks[i+1].Status = forge.CheckRunStatusInProgress
			checks[i+1].StartedAt = time.Now()
		}
		t.SetCIStatus(forge.CIStatusPending, checks)
	}
	t.SetCIStatus(forge.CIStatusSuccess, checks)
}
