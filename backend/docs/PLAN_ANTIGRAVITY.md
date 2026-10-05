# Antigravity runs through caic

Verify selectable Antigravity tasks through the normal task lifecycle with task-scoped MCP.

## Phase 1 — default-selection: selected Antigravity tasks complete the normal lifecycle

- **Scope:** real task-lifecycle verification, including container stop/revive and conversation resume.
- **Preserve:** [backend boundary](../internal/agents/antigravity/AGENTS.md).
- **Verify:** provisioning, live relay, and task MCP checks pass through the normal task lifecycle using the default registry; stopped containers revive with their conversation and task MCP restored.
- **Blocked:** the broad runtime smoke gate fails during md fork ownership restoration on dangling pnpm store symlinks. Repair that path or explicitly waive the unrelated failure.
