# Agent Harness Protocols

## Wire Schema Drift

Runtime harness parsers ignore wire fields they do not consume. Do not add live
unknown-field warnings or `UnmarshalJSON` overflow tracking.

Use `go run ./backend/internal/cmd/check-agent-logs` to delete unsupported v1/v2 task logs and detect
schema drift. It strictly validates recent v3 task-log records against the corresponding genai DTOs and reports the
DTO and provider file to update. It also replays each log through caic's wire
and metadata parsers, as task loading does, and reports the first record each
rejects; such a record fails the task. When a wire protocol changes, update the genai
DTO and its method-to-DTO map, such as `codex.NewNotificationParams`.
It also loads each validated log as task restore does, feeds it through live
ingestion, and reports a log whose replay derives different task state at any
replayed point; `-fold-out` writes every point, one log at a time in
completion order, so two sorted runs can be diffed.
It also reports a backgrounded Claude `local_bash` task whose spawning tool use
is missing or is not `claudecode.ToolBash` or `claudecode.ToolMonitor`; classify
the new tool in `claudecode/native_subagent.go`.
It also prints the newest version each harness ran; when it is newer than the
release pinned in the genai DTO file, check that release upstream for wire
changes.

## Harness Version

Each process start records the running harness version. Claude Code
(`system/init`), Codex (the `initialize` response's `userAgent`), and OpenCode
(ACP `agentInfo.version`) report it natively. Pi and agy report none, and dsh's
`agentInfo.version` is a fixed `0.0.1`; their backends call
`agent.RecordVersion` before launch.

## Harness-Specific Evidence

Keep provider wire fields, native subagent evidence, skill-read tools, and
unexposed protocol capabilities in the relevant harness's `AGENTS.md`, beside
the parser and provider DTOs: `claudecode/`, `codex/`, `dsh/`, `opencode/`, or `pi/`.
Shared ACP mechanics belong in `acp/`; keep both ACP harness packages independent.
