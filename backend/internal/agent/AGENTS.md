# Agent Harness Protocols

## Wire Schema Drift

Runtime harness parsers are deliberately forward-compatible: they ignore wire
fields they do not consume. Do not add live unknown-field warnings or
`UnmarshalJSON` overflow tracking.

Use `make check-agent-logs` to detect schema drift. It strictly validates recent
v2 task-log records against the corresponding genai DTOs and reports the DTO and
provider file to update. When a wire protocol changes, update both the genai DTO
and the command's type-dispatch registry.
