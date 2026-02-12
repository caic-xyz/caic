# Claude Code Built-in Commands — CAIC Frontend Support

Which Claude Code slash commands should CAIC's frontend handle natively,
propose to users, or ignore entirely.

## Context

CAIC sends user input as text prompts to Claude Code agents running in
containers. Some slash commands are better handled by the frontend (UI
controls, dropdowns) rather than passed through as raw text. Others are
irrelevant in a containerized multi-agent context.

## Commands to Handle in Frontend

These should be intercepted by the frontend and translated into proper UI
actions or API calls.

### High Priority — Build native UI

| Command | Frontend Action | Notes |
|:--|:--|:--|
| `/model` | Dropdown/picker to select model | Show available models (opus, sonnet, haiku). Pass to agent via `--model` flag or session command |
| `/compact [instructions]` | Button or auto-trigger | Useful for long sessions. Could auto-suggest when context is high |
| `/resume [session]` | Session picker in task view | Already partially supported via `--resume` flag in agent launch |
| `/plan` | Toggle button for plan mode | Switch agent into plan mode |
| `/permissions` | Display/configure permissions | Show current permission state, allow toggling auto-accept |
| `/cost` | Display in task summary | Already showing cost in TaskItemSummary; could add detailed view |
| `/usage` | Already implemented | UsageBadges.tsx shows 5h/7d windows |
| `/todos` | Task list panel | Show agent's current TODO items in a sidebar widget |
| `/fast` | Toggle button | Toggle fast mode mid-session. Same model, faster output |

### Medium Priority — Useful additions

| Command | Frontend Action | Notes |
|:--|:--|:--|
| `/context` | Context usage bar/indicator | Show how much context window is consumed |
| `/export [filename]` | "Export conversation" button | Download conversation as file |
| `/copy` | Copy button on messages | Already partially there with message rendering |
| `/clear` | "New conversation" button | Clear agent history, start fresh within same container |
| `/rewind` | Undo/rewind button | Restore code to previous checkpoint |

## Commands to Ignore

Irrelevant in CAIC's containerized web UI context.

| Command | Why Ignore |
|:--|:--|
| `/theme` | Styling is controlled by CAIC's own frontend |
| `/vim` | No terminal input; CAIC has its own text input |
| `/config` | Agent config is set at task creation, not interactively |
| `/statusline` | Terminal-only UI feature |
| `/doctor` | Installation health — containers are pre-configured |
| `/help` | CAIC has its own help; Claude Code help is irrelevant |
| `/exit` | Task termination handled by CAIC's terminate button |
| `/init` | CLAUDE.md setup done at container provisioning |
| `/mcp` | MCP servers configured at container/server level |
| `/debug` | Internal debug log — not useful in web UI |
| `/stats` | Terminal-only visualization |
| `/rename` | CAIC has its own task naming |
| `/teleport` | Remote session resume — N/A in containerized setup |
| `/terminal-setup` | Terminal-specific keybinding setup |
| `/memory` | CLAUDE.md editing — handle at project/container config level |
| `/status` | Version/model info available through CAIC's own UI |

## Implementation Notes

- Commands the frontend handles should be intercepted before `sendInput()` — never sent as raw text
- `/model` is highest priority: a dropdown in the task creation form AND a switcher during active sessions
- `/compact` could be auto-suggested when SSE events indicate high context usage
- `/todos` data could be extracted from SSE events if the agent emits TodoWrite tool calls
