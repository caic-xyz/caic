# Multi-Agent Abstraction Plan for CAIC

## Completed: Backend Interface + Claude Subpackage (Step 1)

The `agent.Backend` interface is extracted and the Claude-specific code lives in `agent/claude/`.

- **`agent/backend.go`** — `Backend` interface with `Start`, `AttachRelay`, `ReadRelayOutput`, `ParseMessage`, `Harness`
- **`agent/claude/claude.go`** — Claude Code backend: launches `claude -p` via relay, parses Claude NDJSON, writes Claude stdin format
- **`agent/agent.go`** — Shared infrastructure: `Session` (with pluggable `WriteFn`), `ParseMessage`, `Options`, relay utilities
- **`WriteFn` on `Session`** instead of `WritePrompt` on `Backend` — avoids changing `task.Task.SendInput` or storing the backend on the task
- **Relay utilities stay in `agent/`** — `DeployRelay`, `IsRelayRunning`, `HasRelayDir`, `ReadPlan` are process-agnostic

## Completed: Harness Field + Backend Registry (Step 2)

The `harness` field is plumbed end-to-end from API request through task lifecycle, logs, and frontend display.

### What was done

- **`agent/types.go`** — `type Harness string` (defined type) with `agent.Claude` constant. `DiffFileStat`, `DiffStat` structs. `MetaMessage.Harness agent.Harness` (required, validated). The `agent` package has **no dependency on `dto`**.
- **`agent/backend.go`** — `Backend.Harness()` returns `agent.Harness`.
- **`dto/types.go`** — `type Harness string` (defined type). `HarnessClaude` constant. Duplicated `DiffFileStat`/`DiffStat` structs. Values must match `agent.Harness` constants.
- **`dto/validate.go`** — `CreateTaskReq.Validate()` requires non-empty `harness`.
- **`task/task.go`** — `Task.Harness agent.Harness`. No `dto` import.
- **`task/runner.go`** — `Runner.Backends map[agent.Harness]agent.Backend`. `runner.backend(name)` is a direct map lookup (no fallback). No `dto` import.
- **`task/diffstat.go`** — `ParseDiffNumstat` returns `agent.DiffStat`. No `dto` import.
- **`task/load.go`** — `LoadedTask.Harness agent.Harness`.
- **`server/server.go`** — Boundary conversions: `agent.Harness(req.Harness)` inbound, `dto.Harness(e.task.Harness)` outbound, `toDTODiffStat()` for DiffStat.
- **`server/eventconv.go`** — `toDTODiffStat()` conversion helper.
- **Frontend** — `createTask` passes `harness: "claude"`. `TaskItemSummary` shows harness name when not "claude".

### Key design decisions

- **`type Harness string`** (defined type in both `agent` and `dto`) — provides compile-time type safety. Named "Harness" because it identifies the CLI harness (Claude Code, Gemini CLI, opencode), not the model.
- **`agent.Harness` / `dto.Harness`** — both defined types, explicit conversions at the server boundary: `agent.Harness(req.Harness)` inbound, `dto.Harness(task.Harness)` outbound.
- **`agent.DiffFileStat` / `dto.DiffFileStat`** — duplicated struct types. `agent` owns the canonical definition; `dto` owns the API/frontend definition. Conversion at server boundary.
- **Required everywhere, no fallback** — `harness` field is mandatory in API requests and JSONL logs. `runner.backend()` does a direct map lookup.
- **`Backends` map on `Runner`** (not on `Server`) — keeps backend selection close to agent launch code.
- **`ParseMessage` in `loadLogFile` still uses `agent.ParseMessage`** (Claude format). When a new harness is added, `loadLogFile` will need per-harness dispatch. Deferred.

### Current file layout

```
agent/
  agent.go       — Session (writeFn-pluggable), Options, ParseMessage, readMessages, relay utils
  backend.go     — Backend interface (Harness() returns Harness)
  types.go       — Harness/Claude, DiffFileStat/DiffStat, Message types
  agent_test.go  — Session/parse tests
  claude/
    claude.go       — Backend impl (Harness() → agent.Claude)
    claude_test.go
  gemini/
    gemini.go       — Backend impl (Harness() → agent.Gemini)
    record.go       — Gemini stream-json record types
    parse.go        — ParseMessage: Gemini records → agent.Message
  relay/
    embed.go     — relay.py embed
    relay.py

task/
  task.go        — Task struct (Harness agent.Harness); no dto import
  runner.go      — Runner with Backends map[agent.Harness]agent.Backend; no dto import
  diffstat.go    — ParseDiffNumstat → agent.DiffStat; no dto import
  load.go        — LoadedTask (Harness agent.Harness), loadLogFile

server/
  server.go      — agent.Harness↔dto.Harness boundary conversions, SSE handlers
  eventconv.go   — agent.Message → dto.ClaudeEventMessage (Claude raw stream)
  genericconv.go — agent.Message → dto.EventMessage (backend-neutral stream)

server/dto/
  types.go       — type Harness string, HarnessClaude, DiffFileStat/DiffStat (duplicated)
  validate.go    — CreateTaskReq.Validate() (harness required)
  events.go      — EventMessage (generic) + ClaudeEventMessage (Claude raw) + shared sub-types
```

## Feasibility: Gemini CLI

**Verdict: Yes, feasible.** Gemini CLI supports:
- Headless mode: `gemini -p "prompt"` (same pattern as Claude's `-p`)
- Streaming JSON: `--output-format stream-json` (NDJSON, same concept)
- Session resume: `--resume [UUID]`
- Auto-approve: `--yolo` (equivalent to `--dangerously-skip-permissions`)
- Model selection: `-m gemini-2.5-flash` etc.

**Key differences** that affect the abstraction:
1. **Wire format differs** — Gemini's `stream-json` events have a different schema than Claude Code's NDJSON messages (different type names, different content block structure)
2. **No SDK** — Gemini CLI is the only programmatic interface (same as Claude Code in practice: both are subprocess-based)
3. **Tool names differ** — `read_file` vs `Read`, `run_shell_command` vs `Bash`, etc.
4. **Result/stats schema differs** — Gemini reports `stats.models`/`stats.tools` vs Claude's `ResultMessage` with `total_cost_usd`/`usage`
5. **Session management** — Gemini stores sessions in `~/.gemini/tmp/`, Claude in its own location. Both support `--resume`.

## Completed: Gemini Backend (Step 3)

The Gemini CLI backend is implemented in `agent/gemini/`:

- **`gemini.go`** — `Backend` impl: `Start`, `AttachRelay`, `ReadRelayOutput`, `ParseMessage`, `WritePrompt`, `Harness() → "gemini"`. Launches via `gemini -p --output-format stream-json --yolo`.
- **`record.go`** — Typed records for Gemini's stream-json NDJSON: `InitRecord`, `MessageRecord`, `ToolUseRecord`, `ToolResultRecord`, `ResultRecord`. All embed `Overflow` for forward compatibility (unknown fields logged as warnings).
- **`parse.go`** — `ParseMessage` translates Gemini records → `agent.Message`. Tool name mapping (`read_file`→`Read`, `run_shell_command`→`Bash`, etc.).
- **`unknown.go`** — `Overflow` type and helpers for forward-compatible JSON parsing.
- **`parse_test.go`** / **`record_test.go`** — Full test coverage for parsing and record types.
- **`task/load.go`** — `parseFnForHarness()` dispatches to `agentgemini.ParseMessage` for Gemini logs.
- **`task/runner.go`** — `initDefaults` registers both `claude` and `gemini` backends.

## Completed: Frontend Harness Selector (Step 4)

- **`GET /api/v1/harnesses`** endpoint returns available harnesses from `Runner.Backends`.
- **`dto/types.go`** — `HarnessJSON` type with `Name` field.
- **`dto/routes.go`** — `listHarnesses` route registered.
- **`App.tsx`** — Fetches harnesses on mount. Shows a harness selector dropdown when 2+ harnesses are available. `createTask` passes `selectedHarness()` instead of hardcoded `"claude"`.
- **`TaskItemSummary`** — Already displays harness name for non-claude tasks.

## Completed: Rename `ClaudeCodeVersion` → `AgentVersion` (Step 5)

- **`task/task.go`** — `Task.AgentVersion` (was `ClaudeCodeVersion`).
- **`dto/events.go`** — `EventInit.AgentVersion` with JSON tag `"agentVersion"`.
- **`dto/types.go`** — `TaskJSON.AgentVersion` with JSON tag `"agentVersion"`.
- **`server/server.go`** — `toJSON` maps `AgentVersion`.
- **`server/eventconv.go`** — `convertMessage` init case maps `AgentVersion`.
- **Frontend** — `TaskItemSummary`, `TaskList`, `TaskView` all use `agentVersion`.
- **Tests** — `eventconv_test.go`, `server_test.go` updated.
- **Note:** `agent/types.go` `SystemInitMessage.Version` JSON tag remains `"claude_code_version"` because that is Claude Code's wire format. The Gemini backend constructs `SystemInitMessage` directly in Go, so the JSON tag only matters for Claude.

## Risk Assessment

- **Relay compatibility.** The relay.py is process-agnostic (stdin/stdout bridge), so it works with Gemini CLI unchanged.
- **Cost tracking.** Gemini has a free tier (no cost). `ResultMessage.TotalCostUSD` is 0 for Gemini.
- **Session resume.** Gemini's `--resume` works differently (local file-based sessions). Need to verify it works in a container where `~/.gemini/` persists.
- **Gemini CLI `--yolo` flag conflict.** When `--yolo` is a positional arg (e.g. via alias), `-p` fails with "Cannot use both a positional prompt and the --prompt (-p) flag together". The relay launches gemini directly (not via alias), so this is not an issue in practice.

## Non-Goals (for now)
- MCP server integration (both CLIs support it, but not needed for the core abstraction)
- Multiple simultaneous backends in a single task
- Backend-specific settings UI (model lists, API key management)
