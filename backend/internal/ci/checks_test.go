// Tests for CI check run evaluation logic.

package ci

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

func TestEvaluateCheckRuns(t *testing.T) {
	t.Parallel()
	t.Run("all done success", func(t *testing.T) {
		t.Parallel()
		runs := []forge.CheckRun{
			{Name: "build", Status: forge.CheckRunStatusCompleted, Conclusion: forge.CheckRunConclusionSuccess},
			{Name: "test", Status: forge.CheckRunStatusCompleted, Conclusion: forge.CheckRunConclusionSuccess},
		}
		result, done := EvaluateCheckRuns("o", "r", runs)
		if !done {
			t.Fatal("expected done")
		}
		if result.Status != "success" {
			t.Errorf("got status %q, want success", result.Status)
		}
		if len(result.Checks) != 2 {
			t.Fatalf("got %d checks, want 2", len(result.Checks))
		}
	})

	t.Run("not done returns checks", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		runs := []forge.CheckRun{
			{Name: "build", Status: forge.CheckRunStatusCompleted, Conclusion: forge.CheckRunConclusionSuccess, StartedAt: now, CompletedAt: now.Add(time.Minute)},
			{Name: "test", Status: forge.CheckRunStatusInProgress, StartedAt: now},
		}
		result, done := EvaluateCheckRuns("o", "r", runs)
		if done {
			t.Fatal("expected not done")
		}
		if len(result.Checks) != 2 {
			t.Fatalf("got %d checks, want 2", len(result.Checks))
		}
		if result.Checks[0].Status != "completed" {
			t.Errorf("check 0 status = %q, want completed", result.Checks[0].Status)
		}
		if result.Checks[1].Status != "in_progress" {
			t.Errorf("check 1 status = %q, want in_progress", result.Checks[1].Status)
		}
		if result.Checks[1].StartedAt.IsZero() {
			t.Error("check 1 startedAt should be set")
		}
		if result.Checks[0].CompletedAt.IsZero() {
			t.Error("check 0 completedAt should be set")
		}
	})
}

func TestInterimCIStatus(t *testing.T) {
	t.Parallel()
	t.Run("all pending returns pending", func(t *testing.T) {
		t.Parallel()
		runs := []forge.CheckRun{
			{Name: "build", Status: forge.CheckRunStatusQueued},
			{Name: "test", Status: forge.CheckRunStatusInProgress},
		}
		status := InterimCIStatus(runs)
		if status != forge.CIStatusPending {
			t.Errorf("got %q, want %q", status, forge.CIStatusPending)
		}
	})

	t.Run("one failure among pending returns failure", func(t *testing.T) {
		t.Parallel()
		runs := []forge.CheckRun{
			{Name: "build", Status: forge.CheckRunStatusCompleted, Conclusion: forge.CheckRunConclusionFailure},
			{Name: "test", Status: forge.CheckRunStatusInProgress},
		}
		status := InterimCIStatus(runs)
		if status != forge.CIStatusFailure {
			t.Errorf("got %q, want %q", status, forge.CIStatusFailure)
		}
	})

	t.Run("success and pending returns pending", func(t *testing.T) {
		t.Parallel()
		runs := []forge.CheckRun{
			{Name: "build", Status: forge.CheckRunStatusCompleted, Conclusion: forge.CheckRunConclusionSuccess},
			{Name: "test", Status: forge.CheckRunStatusQueued},
		}
		status := InterimCIStatus(runs)
		if status != forge.CIStatusPending {
			t.Errorf("got %q, want %q", status, forge.CIStatusPending)
		}
	})

	t.Run("cancelled among pending returns failure", func(t *testing.T) {
		t.Parallel()
		runs := []forge.CheckRun{
			{Name: "build", Status: forge.CheckRunStatusCompleted, Conclusion: forge.CheckRunConclusionCancelled},
			{Name: "test", Status: forge.CheckRunStatusQueued},
		}
		status := InterimCIStatus(runs)
		if status != forge.CIStatusFailure {
			t.Errorf("got %q, want %q", status, forge.CIStatusFailure)
		}
	})
}
