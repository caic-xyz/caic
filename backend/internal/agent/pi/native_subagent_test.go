// Tests Pi subagent-extension lifecycle mapping without a live harness.

package pi

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// TestSubagentCompletionStatus pins how the extension's reported run states map
// onto the canonical lifecycle. `paused` is the state a detached workflow run
// reports while it awaits attention, which the extension can resume, so it must
// not be presented as a failure or as still running.
func TestSubagentCompletionStatus(t *testing.T) {
	t.Parallel()
	failed := false
	succeeded := true
	exitZero := 0
	exitOne := 1
	for _, test := range []struct {
		name string
		c    subagentCompletion
		want agent.NativeSubagentStatus
	}{
		{"complete", subagentCompletion{State: "complete", Success: &succeeded}, agent.NativeSubagentStatusCompleted},
		{"completed without success flag", subagentCompletion{State: "completed"}, agent.NativeSubagentStatusCompleted},
		{"complete with aggregate failure", subagentCompletion{State: "complete", Success: &failed}, agent.NativeSubagentStatusFailed},
		{"child failure", subagentCompletion{State: "complete", Success: &succeeded, Results: []subagentResult{{Success: &failed}}}, agent.NativeSubagentStatusFailed},
		{"child exit code", subagentCompletion{State: "complete", Success: &succeeded, Results: []subagentResult{{ExitCode: &exitOne}}}, agent.NativeSubagentStatusFailed},
		{"child exit zero", subagentCompletion{State: "complete", Success: &succeeded, Results: []subagentResult{{ExitCode: &exitZero}}}, agent.NativeSubagentStatusCompleted},
		{"child interruption", subagentCompletion{State: "complete", Success: &succeeded, Results: []subagentResult{{Interrupted: true}}}, agent.NativeSubagentStatusInterrupted},
		{"failed", subagentCompletion{State: "failed"}, agent.NativeSubagentStatusFailed},
		{"errored", subagentCompletion{State: "errored"}, agent.NativeSubagentStatusFailed},
		{"stopped", subagentCompletion{State: "stopped"}, agent.NativeSubagentStatusInterrupted},
		{"cancelled", subagentCompletion{State: "cancelled"}, agent.NativeSubagentStatusInterrupted},
		{"paused", subagentCompletion{State: "paused"}, agent.NativeSubagentStatusPaused},
		{"running", subagentCompletion{State: "running"}, agent.NativeSubagentStatusRunning},
		{"queued", subagentCompletion{State: "queued"}, agent.NativeSubagentStatusUnknown},
		{"pending", subagentCompletion{State: "pending"}, agent.NativeSubagentStatusUnknown},
		{"unrecognized", subagentCompletion{State: "reticulating"}, agent.NativeSubagentStatusUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := completionStatus(test.c.State, test.c.Success, test.c.Results); got != test.want {
				t.Fatalf("completionStatus(%+v) = %q, want %q", test.c, got, test.want)
			}
		})
	}
}

// TestSubagentPausedRunStopsCountingAsActive pins the recorded detached-run
// shape: a paused completion settles the card out of the active count, so a
// workflow that stops to await attention cannot stick an active agent.
func TestSubagentPausedRunStopsCountingAsActive(t *testing.T) {
	t.Parallel()
	wire := New("", nil).NewWire()
	feed := func(line string) []agent.NativeSubagent {
		msgs, err := wire.ParseMessage([]byte(line))
		if err != nil {
			t.Fatalf("ParseMessage(%s): %v", line, err)
		}
		var out []agent.NativeSubagent
		for _, message := range msgs {
			if native, ok := message.(*agent.NativeSubagentMessage); ok {
				out = append(out, native.Subagent)
			}
		}
		return out
	}
	feed(`{"type":"tool_execution_start","toolCallId":"c1","toolName":"subagent","args":{"workflowScript":"const r = await runs.run('implement', {agent:'worker', task:'do work'});"}}`)
	feed(`{"type":"tool_execution_end","toolCallId":"c1","toolName":"subagent","result":{"content":[{"type":"text","text":"Async workflow [test-run]"}],"details":{"mode":"workflow","runId":"test-run","asyncId":"test-run","results":[]}},"isError":false}`)
	settled := feed(`{"type":"tool_execution_end","toolCallId":"c2","toolName":"subagent_wait","result":{"content":[{"type":"text","text":"Waited for run test-run; done. Outcome: 1 paused."}],"details":{"mode":"management","results":[],"completions":[{"runId":"test-run","agent":"workflow","mode":"workflow","state":"paused","success":false,"results":[{"agent":"worker","runId":"test-run","success":false,"outputState":"present"}]}]}},"isError":false}`)
	if len(settled) != 1 || settled[0].Status != agent.NativeSubagentStatusPaused || settled[0].Scope != agent.NativeSubagentScopeBatch {
		t.Fatalf("settled = %#v, want one paused batch card", settled)
	}
	if settled[0].ID != "pi:run:test-run" {
		t.Fatalf("identity = %q, want the run card settled by the wait", settled[0].ID)
	}
	var timeline agent.NativeSubagentTimeline
	for i := range settled {
		timeline.Apply(&settled[i])
	}
	if timeline.ActiveCount() != 0 {
		t.Fatalf("active = %d, want 0 for a paused run", timeline.ActiveCount())
	}
}

// TestSubagentCompletionResult pins what an asynchronous completion can report.
// The installed extension's completion type carries the structured error and
// artifact paths, not the output text, so the card must never claim more.
func TestSubagentCompletionResult(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		results []subagentResult
		want    string
	}{
		{"artifact reference", []subagentResult{{OutputState: "present", ArtifactPaths: &subagentArtifactPaths{OutputPath: "/workspace/out.md"}}}, "Output file: /workspace/out.md"},
		{"structured error wins", []subagentResult{{Error: "child crashed", ArtifactPaths: &subagentArtifactPaths{OutputPath: "/workspace/out.md"}}}, "child crashed"},
		{"multiple errors", []subagentResult{{Error: "one"}, {Error: "two"}}, "one\n\ntwo"},
		{"inline output from a synchronous run", []subagentResult{{FinalOutput: "the joke"}}, "the joke"},
		{"nothing reported", []subagentResult{{OutputState: "missing"}}, ""},
		{"no children", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := completionResult(test.results); got != test.want {
				t.Fatalf("completionResult = %q, want %q", got, test.want)
			}
		})
	}
}

// TestSubagentManagementNeverSpawns pins that introspection and provider jobs
// shared with bg_wait never produce native activity.
func TestSubagentManagementNeverSpawns(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"subagent","args":{"action":"list"}}`,
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"subagent","result":{"content":[{"type":"text","text":"Executable agents"}],"details":{"mode":"management","results":[]}},"isError":false}`,
		`{"type":"tool_execution_end","toolCallId":"c2","toolName":"subagent_wait","result":{"content":[{"type":"text","text":"No active run matched"}],"details":{"mode":"management","results":[]}},"isError":false}`,
		`{"type":"tool_execution_end","toolCallId":"c3","toolName":"bg_wait","result":{"content":[{"type":"text","text":"bash job done"}],"details":{"mode":"management","results":[],"completions":[{"runId":"job-1","agent":"bash","mode":"background","state":"complete","success":true,"results":[]}]}},"isError":false}`,
	} {
		wire := New("", nil).NewWire()
		msgs, err := wire.ParseMessage([]byte(line))
		if err != nil {
			t.Fatalf("ParseMessage(%s): %v", line, err)
		}
		for _, message := range msgs {
			if native, ok := message.(*agent.NativeSubagentMessage); ok {
				t.Fatalf("%s produced native subagent %#v, want none", line, native.Subagent)
			}
		}
	}
}
