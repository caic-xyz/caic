# Agent Harness Protocols

## Wire Schema Drift

Runtime harness parsers ignore wire fields they do not consume. Do not add live
unknown-field warnings or `UnmarshalJSON` overflow tracking.

Use `go run ./backend/internal/cmd/check-agent-logs` to delete unsupported v1/v2 task logs and detect
schema drift. It strictly validates recent v3 task-log records against the corresponding genai DTOs and reports the
DTO and provider file to update. When a wire protocol changes, update the genai
DTO and its method-to-DTO map, such as `codex.NewNotificationParams`.

## Harness Version

Each process start records the running harness version. Claude Code
(`system/init`), Codex (the `initialize` response's `userAgent`), and ACP
(`agentInfo.version`) report it natively. Harnesses that do not, Pi and agy, call
`agent.RecordVersion` before launch.

## Harness-Specific Evidence

Keep provider wire fields, native subagent evidence, skill-read tools, and
unexposed protocol capabilities in the relevant harness's `AGENTS.md`, beside
the parser and provider DTOs: `claudecode/`, `codex/`, `dsh/`, `opencode/`, or `pi/`.
Shared ACP mechanics belong in `acp/`; keep both ACP harness packages independent.
