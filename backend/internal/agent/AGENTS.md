# Agent Harness Protocols

## Wire Schema Drift

Runtime harness parsers ignore wire fields they do not consume. Do not add live
unknown-field warnings or `UnmarshalJSON` overflow tracking.

Use `make check-agent-logs` to detect schema drift. It strictly validates recent
v2 and v3 task-log records against the corresponding genai DTOs and reports the
DTO and provider file to update. When a wire protocol changes, update both the
genai DTO and the command's type-dispatch registry.

## Harness-Specific Evidence

Keep provider wire fields, native subagent evidence, skill-read tools, and
unexposed protocol capabilities in the relevant harness's `AGENTS.md`, beside
the parser and provider DTOs: `claudecode/`, `codex/`, `opencode/`, or `pi/`.
