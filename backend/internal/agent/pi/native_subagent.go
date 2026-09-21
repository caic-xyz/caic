// Correlates Pi extension delegation calls with synchronous results and asynchronous run completions.

package pi

import (
	"encoding/json"
	"strings"

	"github.com/maruel/genai/providers/pi"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// The installed pi-subagents extension owns these fields, not core Pi RPC.
// See the version-pinned extension references in AGENTS.md. An async tool end
// acknowledges dispatch; only completions (or synchronous results) settle work.
type subagentDetails struct {
	Mode        string               `json:"mode"`
	RunID       string               `json:"runId"`
	AsyncID     string               `json:"asyncId"`
	Background  bool                 `json:"background"`
	Stopped     bool                 `json:"stopped"`
	Results     []subagentResult     `json:"results"`
	Completions []subagentCompletion `json:"completions"`
}

type subagentResult struct {
	ExitCode      *int                   `json:"exitCode"`
	Success       *bool                  `json:"success"`
	Interrupted   bool                   `json:"interrupted"`
	Stopped       bool                   `json:"stopped"`
	FinalOutput   string                 `json:"finalOutput"`
	Error         string                 `json:"error"`
	OutputState   string                 `json:"outputState"`
	ArtifactPaths *subagentArtifactPaths `json:"artifactPaths"`
}

// subagentArtifactPaths is where the extension writes a run's output. The
// extension keeps the output text in its artifact files and its wait result
// content only summarises the run, so an asynchronous completion can reference
// the artifact but not quote it.
type subagentArtifactPaths struct {
	OutputPath string `json:"outputPath"`
}

type subagentCompletion struct {
	RunID   string           `json:"runId"`
	Agent   string           `json:"agent"`
	Mode    string           `json:"mode"`
	State   string           `json:"state"`
	Success *bool            `json:"success"`
	Results []subagentResult `json:"results"`
}

// nativeSubagents folds the extension's delegation evidence into canonical
// lifecycles.
//
// The canonical identity is the extension's run ID whenever a record reports
// one, because completions only carry the run ID. A card keyed by the tool call
// ID instead would leave the restored card running and add a second card when a
// session resumes from history the wire has not seen.
// decodedRecord carries the tool execution record the native-subagent adapter
// correlates on. The canonical conversion decodes every tool start and end, and
// the adapter is the only reader of the extension details it drops, so the parser
// hands the record over instead of making the adapter decode the line again.
type decodedRecord struct {
	start       *pi.ToolExecStartEvent
	end         *pi.ToolExecEndEvent
	extensionUI *pi.ExtensionUIRequest
}

// The spawn map is allocated with the wire, created once per session, instead of
// by the first delegation that needs it.
type nativeSubagents struct {
	timeline agent.NativeSubagentTimeline
	calls    map[string]agent.NativeSubagent // tool call ID → spawn metadata, before a run ID exists
}

// parse folds one decoded record: a subagent start remembers the delegation the
// end record settles, and the extension's async-status widget settles or
// refreshes a detached run.
func (n *nativeSubagents) parse(record decodedRecord) ([]agent.Message, error) {
	switch {
	case record.start != nil:
		return n.parseStart(record.start)
	case record.end != nil:
		return n.parseEnd(record.end)
	case record.extensionUI != nil:
		return n.parseExtensionUI(record.extensionUI)
	default:
		return nil, nil
	}
}

// parseStart remembers one subagent spawn. A recognised orchestration kind proves
// delegation: batch orchestrations such as workflowScript expose no per-step
// detail, but they still produce one explicit batch card, while introspection
// actions never spawn.
func (n *nativeSubagents) parseStart(ev *pi.ToolExecStartEvent) ([]agent.Message, error) {
	if ev.ToolName != subagentToolName {
		return nil, nil
	}
	raw, err := json.Marshal(ev.Args)
	if err != nil {
		return nil, err
	}
	info := parseSubagentArgs(raw)
	if ev.ToolCallID == "" || info.Kind == "" || info.Kind == "action" {
		return nil, nil
	}
	s := agent.NativeSubagent{ToolUseID: ev.ToolCallID, Label: subagentDescription(info.Kind, info.Spawns), Status: agent.NativeSubagentStatusUnknown}
	if info.Kind == "single" {
		s.Prompt = info.Spawns[0].Task
	} else {
		s.Scope = agent.NativeSubagentScopeBatch
	}
	// The start record carries no run ID, so it cannot yet be a stable
	// identity. Remember the metadata for the end record that reports one.
	n.calls[ev.ToolCallID] = s
	return nil, nil
}

// parseEnd settles the delegation a subagent end reports, from the run details
// the extension wrote.
func (n *nativeSubagents) parseEnd(ev *pi.ToolExecEndEvent) ([]agent.Message, error) {
	if ev.ToolName != subagentToolName && ev.ToolName != "subagent_wait" && ev.ToolName != "bg_wait" {
		return nil, nil
	}
	var d subagentDetails
	if len(ev.Result.Details) > 0 {
		if err := json.Unmarshal(ev.Result.Details, &d); err != nil {
			return nil, err
		}
	}
	var out []agent.Message
	if spawned, ok := n.calls[ev.ToolCallID]; ok && ev.ToolName == subagentToolName {
		// An asynchronous run is keyed by its run ID; a synchronous or legacy
		// run without one keeps the tool call identity.
		s := spawned
		s.ID = subagentRunIdentity(d.RunID, ev.ToolCallID)
		detached := d.AsyncID != "" || d.Background
		s.Background = s.Background || detached
		if detached && !ev.IsError {
			s.Status = agent.NativeSubagentStatusRunning
		} else {
			s.Status = resultStatus(d.Results, ev.IsError, d.Stopped)
			s.Result = ev.Result.Text()
		}
		out = append(out, n.timeline.Observe(&s)...)
	}
	for _, c := range d.Completions {
		// Provider jobs may share bg_wait; mode proves this is a subagent run.
		// workflow is the installed extension's current orchestration mode;
		// single, parallel, and chain remain for historical logs.
		if c.RunID == "" || !subagentRunMode(c.Mode) {
			continue
		}
		s := agent.NativeSubagent{ID: subagentRunIdentity(c.RunID, ""), Label: c.Agent, Background: true}
		if spawned, ok := n.calls[c.RunID]; ok {
			// A completion that repeats the tool call ID can enrich the spawn.
			s = spawned
			s.ID = subagentRunIdentity(c.RunID, "")
		}
		if c.Mode != "single" {
			s.Scope = agent.NativeSubagentScopeBatch
		}
		s.Status = completionStatus(c.State, c.Success, c.Results)
		s.Result = completionResult(c.Results)
		out = append(out, n.timeline.Observe(&s)...)
	}
	return out, nil
}

// asyncWidgetKey is the installed pi-subagents extension's widget key for its
// detached-run status snapshot. The extension owns this payload; core Pi RPC
// has no typed event for it. See AGENTS.md.
const (
	asyncWidgetKey    = "subagent-async"
	asyncWidgetPrefix = "PI_SUBAGENT_ASYNC_JSON:"
	asyncSnapshotKind = "pi-subagents.async-status-snapshot"
)

// asyncSnapshot is the subset of the extension's async-status snapshot caic
// consumes: each detached run's identity, label, and reported state.
type asyncSnapshot struct {
	Kind string             `json:"kind"`
	Runs []asyncSnapshotRun `json:"runs"`
}

type asyncSnapshotRun struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	State string `json:"state"`
}

// parseExtensionUI folds the extension's async-status widget. The extension
// re-publishes the full detached-run set on every change, so this settles a run
// whose completion never arrived through a subagent_wait or bg_wait call, and
// restores cards for runs that started before the retained history.
func (n *nativeSubagents) parseExtensionUI(ev *pi.ExtensionUIRequest) ([]agent.Message, error) {
	if ev.Method != pi.UIMethodSetWidget || ev.WidgetKey != asyncWidgetKey {
		return nil, nil
	}
	var out []agent.Message
	for _, line := range ev.WidgetLines {
		payload, ok := strings.CutPrefix(line, asyncWidgetPrefix)
		if !ok {
			continue
		}
		var snap asyncSnapshot
		if err := json.Unmarshal([]byte(payload), &snap); err != nil {
			return nil, err
		}
		if snap.Kind != asyncSnapshotKind {
			continue
		}
		for i := range snap.Runs {
			run := &snap.Runs[i]
			if run.ID == "" {
				continue
			}
			s := agent.NativeSubagent{
				ID:         subagentRunIdentity(run.ID, ""),
				Label:      run.Label,
				Status:     asyncSnapshotStatus(run.State),
				Background: true,
			}
			out = append(out, n.timeline.Observe(&s)...)
		}
	}
	return out, nil
}

// subagentRunIdentity returns the canonical identity for a delegated run: the
// extension's run ID when the record reports one, otherwise the tool call ID.
func subagentRunIdentity(runID, toolCallID string) string {
	if runID != "" {
		return "pi:run:" + runID
	}
	return "pi:tool:" + toolCallID
}

// completionResult reports what the extension exposes for a finished run: the
// structured error when it failed, and otherwise a reference to the artifact that
// holds the output when the extension reported one. It never invents output text:
// the extension's own type documents that the output stays in the artifact files.
func completionResult(results []subagentResult) string {
	var errors, outputs []string
	for _, r := range results {
		if r.Error != "" {
			errors = append(errors, r.Error)
		}
		if r.FinalOutput != "" {
			outputs = append(outputs, r.FinalOutput)
		}
		if r.ArtifactPaths != nil && r.ArtifactPaths.OutputPath != "" {
			outputs = append(outputs, "Output file: "+r.ArtifactPaths.OutputPath)
		}
	}
	if len(errors) > 0 {
		return strings.Join(errors, "\n\n")
	}
	return strings.Join(outputs, "\n\n")
}

// subagentRunMode reports whether a completion describes a subagent
// orchestration run rather than an unrelated background job.
func subagentRunMode(mode string) bool {
	switch mode {
	case "single", "parallel", "chain", "workflow":
		return true
	default:
		return false
	}
}

// completionStatus maps the extension's reported run state to the canonical
// lifecycle. States the extension reports as aggregate (workflow steps) stay
// aggregate: a paused run is resumable and therefore not terminal, and an
// unrecognised state stays unknown instead of being invented.
func completionStatus(state string, success *bool, results []subagentResult) agent.NativeSubagentStatus {
	switch state {
	case "complete", "completed", "succeeded":
		return resultStatus(results, success != nil && !*success, false)
	default:
		if status, ok := runStateStatus(state); ok {
			return status
		}
		return agent.NativeSubagentStatusUnknown
	}
}

// runStateStatus maps the extension's non-completion run-state vocabulary onto
// the canonical lifecycle. It reports ok=false for a state that is not
// lifecycle evidence.
func runStateStatus(state string) (agent.NativeSubagentStatus, bool) {
	switch state {
	case "failed", "error", "errored":
		return agent.NativeSubagentStatusFailed, true
	case "interrupted", "stopped", "cancelled", "canceled":
		return agent.NativeSubagentStatusInterrupted, true
	case "paused":
		return agent.NativeSubagentStatusPaused, true
	case "running":
		return agent.NativeSubagentStatusRunning, true
	case "queued", "pending":
		// The extension reported a run that is not executing yet: it proves the
		// run exists but not that it is active.
		return agent.NativeSubagentStatusUnknown, true
	default:
		return agent.NativeSubagentStatusUnknown, false
	}
}

// asyncSnapshotStatus maps the extension's snapshot run state onto the canonical
// lifecycle. The snapshot carries no failure detail, so a terminal run settles
// the card without inventing a result; that stays a wait completion's job.
func asyncSnapshotStatus(state string) agent.NativeSubagentStatus {
	switch state {
	case "complete", "completed", "succeeded":
		return agent.NativeSubagentStatusCompleted
	default:
		if status, ok := runStateStatus(state); ok {
			return status
		}
		return agent.NativeSubagentStatusUnknown
	}
}

func resultStatus(results []subagentResult, failed, stopped bool) agent.NativeSubagentStatus {
	for _, r := range results {
		stopped = stopped || r.Interrupted || r.Stopped
		failed = failed || (r.ExitCode != nil && *r.ExitCode != 0) || (r.Success != nil && !*r.Success)
	}
	if stopped {
		return agent.NativeSubagentStatusInterrupted
	}
	if failed {
		return agent.NativeSubagentStatusFailed
	}
	return agent.NativeSubagentStatusCompleted
}
