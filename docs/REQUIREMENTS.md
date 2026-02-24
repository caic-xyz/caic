# caic - Requirements

Coding Agents in Containers: manage multiple coding agents in parallel.

## Overview

caic orchestrates multiple Claude Code agents running in isolated
[md](https://github.com/caic-xyz/md) containers. Each task gets its own
container+branch, Claude runs inside via SSH with streaming JSON, and results
are pulled back and serialized into the upstream repository.

## Definitions

- **Task**: a user-supplied description of work to do (bug fix, feature, etc.).
- **Container**: an md container (`md-<repo>-<branch>`) providing an isolated
  git clone, accessible via SSH.
- **Agent**: a Claude Code process running inside a container, communicating via
  the streaming JSON protocol over SSH.

## Functional Requirements

### FR-1: Task Submission

- User provides a task description via CLI.
- caic creates a new git branch from the current HEAD (e.g.,
  `caic/<timestamp>-<slug>`).
- caic starts an md container for that branch (`md start --no-ssh`).

### FR-2: Agent Execution

- caic runs Claude Code inside the container over SSH:
  ```
  ssh <container> claude -p \
    --output-format stream-json --verbose \
    --dangerously-skip-permissions \
    --max-turns <N> \
    "<task description>"
  ```
- Reads stdout as NDJSON, parsing each line into typed messages.
- Relevant message types:
  - `system` (subtype `init`): session metadata.
  - `assistant`: model responses and tool-use requests.
  - `user`: tool results (executed by Claude Code internally).
  - `result`: terminal message indicating success or error.
- The agent is considered done when a `result` message is received.

### FR-3: Result Reporting

- On completion, caic runs `md diff --stat` to summarize what changed.
- Optionally runs `md diff` for the full patch.
- Reports to the user: task status (success/error), cost, files changed, diff
  summary.

### FR-4: Git Integration

- After the agent finishes, caic runs `md pull` to bring changes into the local
  branch.
- Multiple agents may finish concurrently; their pulls must be serialized
  (mutex) to avoid conflicts.
- After a successful pull, caic pushes the branch to `origin`.
- If a push fails due to a race, caic rebases and retries.

### FR-5: Parallel Execution

- caic can run multiple tasks concurrently, each in its own container.
- Each task is independent: separate branch, separate container, separate
  agent.
- A status display shows all running tasks and their state.

### FR-6: Cleanup

- After successfully pushing, caic kills the container (`md kill`).
- On failure, the container is left running for manual inspection.

## Non-Functional Requirements

### NFR-1: Simplicity

- Single binary, no config file required. CLI flags and environment variables
  only.

### NFR-2: Observability

- Log all agent streaming output to a file per task for post-mortem.
- Print a summary line per task on completion.

### NFR-3: Error Handling

- If Claude errors out (non-zero exit, error result), report the error and
  leave the container for debugging.
- If `md pull` fails (merge conflict), report and leave the container.

## Architecture

```
                  ┌──────────────┐
                  │   User CLI   │
                  └──────┬───────┘
                         │ task descriptions
                         ▼
                  ┌──────────────┐
                  │    caic      │
                  │  (Go binary) │
                  └──┬───┬───┬──┘
                     │   │   │        one goroutine per task
                     ▼   ▼   ▼
               ┌─────┐┌─────┐┌─────┐
               │md #1││md #2││md #3│  isolated containers
               └──┬──┘└──┬──┘└──┬──┘
                  │      │      │     SSH + streaming JSON
                  ▼      ▼      ▼
               claude  claude  claude  agents
```

### Key Packages

- `main` — CLI entry point, flag parsing, task dispatch.
- `internal/agent` — Claude Code streaming JSON protocol: types, process
  lifecycle, output parsing.
- `internal/container` — md container management: start, diff, pull, kill.
- `internal/git` — git operations: branch creation, push with retry, conflict
  detection.
- `internal/task` — task orchestration: ties agent + container + git together.

### Claude Code Streaming JSON Types

Input (stdin NDJSON):
```json
{"type":"user","message":{"role":"user","content":"<task>"}}
```

Output (stdout NDJSON) — key types:

| type           | subtype    | Description                          |
|----------------|------------|--------------------------------------|
| `system`       | `init`     | Session start, tools, model info     |
| `assistant`    |            | Model response (text or tool_use)    |
| `user`         |            | Tool results from Claude Code        |
| `result`       | `success`  | Task completed successfully          |
| `result`       | `error_*`  | Task failed                          |
| `stream_event` |            | Partial tokens (with --include-partial-messages) |

### Git Flow

1. Create branch `caic/<slug>` from current HEAD.
2. `md start --no-ssh` on that branch.
3. Agent runs inside container, makes commits.
4. `md pull` rebases container commits onto local branch.
5. `git push origin caic/<slug>` (serialized across tasks).
6. `md kill` on success.

## CLI Interface

```
caic run "fix the auth bug in login.go"
caic run "add pagination to /api/users" "refactor database layer"
caic status
caic logs <task-id>
```

### `caic run <task>...`

- Accepts one or more task descriptions.
- Runs all tasks in parallel.
- Blocks until all complete.
- Prints summary on completion.

### `caic status`

- Shows running tasks: branch, container, elapsed time, state.

### `caic logs <task-id>`

- Tails the streaming JSON log for a task.
