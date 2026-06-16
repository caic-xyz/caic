// Pi subagent tool introspection: parses the `subagent` tool's arguments into a
// structured view so subagent orchestration is visible in the frontend.

package pi

import (
	"encoding/json"
	"strconv"
	"strings"
)

// subagentToolName is Pi's raw tool name for spawning and orchestrating
// subagents. It is normalized to the canonical "Agent" name for display (see
// normalizeToolName) but matched on the raw name to detect spawns.
const subagentToolName = "subagent"

// runningPlaceholder is Pi's sentinel progress text for a tool that is executing
// but has produced no output yet (notably the subagent tool). It carries no
// information and is suppressed from the tool-output stream.
const runningPlaceholder = "(running...)"

// subagentSpawn describes one subagent invocation parsed from a subagent tool
// call's arguments. Pi spawns subagents singly, as a parallel batch, or as a
// phased chain; each spawned agent yields one subagentSpawn.
type subagentSpawn struct {
	Agent string // Builtin or named agent, e.g. "reviewer", "planner", "worker".
	Label string // Optional human label (parallel/chain steps).
	Phase string // Optional phase name (chain steps).
	Task  string // The task prompt given to the subagent.
}

// subagentInfo is the parsed view of a subagent tool call's arguments.
type subagentInfo struct {
	Kind   string          // "single", "parallel", "chain", or "action".
	Action string          // For introspection calls: "list", "status", etc.
	Spawns []subagentSpawn // Empty for introspection (list/status) calls.
}

// subagentArgs mirrors the JSON shapes Pi's subagent tool accepts: a single
// spawn, a parallel batch (tasks[]), a phased chain (chain[]), or an
// introspection action (list/status).
type subagentArgs struct {
	subagentStep // Single spawn.

	Action string `json:"action"`

	// Parallel batch.
	Tasks []subagentStep `json:"tasks"`

	// Phased chain; each step is a single spawn or a nested parallel block.
	Chain []subagentChainStep `json:"chain"`
}

type subagentStep struct {
	Agent string `json:"agent"`
	Label string `json:"label"`
	Phase string `json:"phase"`
	Task  string `json:"task"`
}

type subagentChainStep struct {
	subagentStep

	Parallel []subagentStep `json:"parallel"`
}

// parseSubagentArgs decodes a subagent tool call's arguments into a structured
// view. It recognises the single, parallel-batch, and chain orchestration
// shapes, and the action-based introspection calls (list/status) which spawn
// no subagents.
func parseSubagentArgs(raw json.RawMessage) subagentInfo {
	var args subagentArgs
	if len(raw) == 0 || json.Unmarshal(raw, &args) != nil {
		return subagentInfo{}
	}
	spawns := args.spawns()
	switch {
	case len(args.Chain) > 0 && len(spawns) > 0:
		return subagentInfo{Kind: "chain", Spawns: spawns}
	case len(args.Tasks) > 0 && len(spawns) > 0:
		return subagentInfo{Kind: "parallel", Spawns: spawns}
	case len(spawns) > 0:
		return subagentInfo{Kind: "single", Spawns: spawns}
	case args.Action != "":
		return subagentInfo{Kind: "action", Action: args.Action}
	default:
		return subagentInfo{}
	}
}

// spawns flattens the orchestration shapes into an ordered list of subagent
// invocations. Steps with no agent (e.g. the introspection action) are dropped.
func (a *subagentArgs) spawns() []subagentSpawn {
	var out []subagentSpawn
	add := func(s subagentStep) {
		if s.Agent == "" {
			return
		}
		out = append(out, subagentSpawn(s))
	}
	switch {
	case len(a.Chain) > 0:
		for _, step := range a.Chain {
			if len(step.Parallel) > 0 {
				for _, s := range step.Parallel {
					add(s)
				}
				continue
			}
			add(step.subagentStep)
		}
	case len(a.Tasks) > 0:
		for _, s := range a.Tasks {
			add(s)
		}
	default:
		add(a.subagentStep)
	}
	return out
}

// subagentDescription summarises a subagent spawn for the live progress panel,
// e.g. "reviewer — Review the last commit" for a single spawn or
// "chain · reviewer ×3, worker" for an orchestration.
func subagentDescription(kind string, spawns []subagentSpawn) string {
	if len(spawns) == 1 {
		s := spawns[0]
		detail := s.Label
		if detail == "" {
			detail = firstLine(s.Task)
		}
		if detail == "" {
			return s.Agent
		}
		return s.Agent + " — " + detail
	}

	// Count agents by type, preserving first-seen order.
	order := make([]string, 0, len(spawns))
	counts := make(map[string]int, len(spawns))
	for _, s := range spawns {
		if _, ok := counts[s.Agent]; !ok {
			order = append(order, s.Agent)
		}
		counts[s.Agent]++
	}
	parts := make([]string, 0, len(order))
	for _, a := range order {
		if n := counts[a]; n > 1 {
			parts = append(parts, a+" ×"+strconv.Itoa(n))
		} else {
			parts = append(parts, a)
		}
	}
	return kind + " · " + strings.Join(parts, ", ")
}

// subagentStatus derives a terminal status ("completed"/"failed") from a
// subagent tool result, matching the SubagentEndMessage status vocabulary.
func subagentStatus(isError bool, resultText string) string {
	if isError || strings.HasPrefix(strings.TrimSpace(resultText), "❌") {
		return "failed"
	}
	return "completed"
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
