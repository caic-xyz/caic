# Claude Code Agent Backend

Implements `agent.Backend` for Claude Code. Manages the widget plugin
(`widget-plugin/`) deployed to containers via `embed.FS`.

## References

Source code:
- https://github.com/anthropics/claude-code

Claude Code headless:
- https://code.claude.com/docs/en/headless: headless mode overview
- https://code.claude.com/docs/en/prompt-caching: Claude Code main-conversation, subagent, authentication, and override TTL behavior
- https://platform.claude.com/docs/en/agent-sdk/streaming-output: streaming protocol wire format
- https://platform.claude.com/docs/en/build-with-claude/prompt-caching: cache TTLs and usage-field semantics
- git clone https://github.com/anthropics/claude-agent-sdk-python for SDK types (`src/claude_agent_sdk/types.py`)
- git clone https://github.com/anthropics/claude-agent-sdk-python to get the SDK and understand types, in particular `src/claude_agent_sdk/types.py`

## Native subagent evidence

Claude Code 2.1.270 completed the standardized native-subagent request in a
local CLI recording (not an md relay): `system/task_started` with
`task_type: "local_agent"` carries `task_id`, `tool_use_id`, `subagent_type`,
and the child prompt; `system/task_updated` reports `patch.status`; the
matching `system/task_notification` carries the terminal status and summary.
The retained v3 fixture is minimized from that recording and includes the
child stream message that carries `parent_tool_use_id`, which cannot settle the
parent lifecycle. Earlier account quota errors delayed but did not invalidate
this evidence.

`native_subagent.go` keys one canonical card per native `task_id`, which every
task record carries, and uses the parent tool use ID only for tool-card
correlation; a card is created from task records, never from the tool use alone,
so a resumed session cannot split one agent into two cards. It maps the reported
task statuses onto the canonical lifecycle: `running`/`in_progress` runs, `paused` is the resumable non-terminal
state, `completed`/`failed` settle, `killed`/`stopped`/`interrupted` are
interruptions, and `pending` or anything unrecognized stays unknown. The Agent
tool's `run_in_background` and a task record's `is_backgrounded`
(`patch.is_backgrounded` repeats it) mark the card `Background`: a background
card can keep a task running after the parent result, as can a detached shell
command.

The card's kind is the Agent tool's `subagent_type`, which `task_started`
repeats. Every `local_agent` `task_started` in 17 local Claude task logs (136
records) carried it: `general-purpose` (115) and `web-fetch` (21).

`SendMessage` to a settled agent resumes it: a new `task_started` with the same
`task_id`, a newer `run_id`, and the SendMessage `tool_use_id`. Its tool
result (`tool_use_result.resumedAgentId`) only acknowledges the resumption and
never settles the card.

## Settled stream trimming

Claude Code emits each content block in full as an `assistant` record when the
block's `stream_event` `content_block_stop` arrives: text, tool input, and the
thinking signature. Thinking text is the exception: the final `thinking` block
carries only `signature`, so `thinking_delta` holds the only copy. Stream
events name their block by `session_id`, `parent_tool_use_id`,
`api_message_id`, and `event.index`; message-level events carry no index.
`stream_trim.go` selects `text_delta`, `input_json_delta`, and
`signature_delta` records of stopped blocks; terminal compression replaces each
with an `elided` record, so history positions and cached immutable slices stay
valid. Across every locally readable Claude v3 log, that elides 57% of the
records, and every other message keeps its record and position.

A sanitized v3 task record proves the observable lifecycle:
`system/task_started` carries `task_id`, `tool_use_id`, `description`,
`is_backgrounded`, and `task_type`; the matching `system/task_notification`
carries the IDs, terminal `status`, `summary`, and `output_file`. That source
record's `task_type` is `local_bash`, so task events alone do not prove a
model subagent: only `task_type: "local_agent"` establishes one.
`system/task_updated` can expose `patch.status`. These records do not expose
a separately versioned child identity.

Claude Code 2.1 reports a `Monitor` tool watch with the same records as
`Bash` with `run_in_background`: `task_type: "local_bash"` and
`is_backgrounded: true`. Only the `tool_use_id`, which names the `Monitor`
call, tells them apart. A watch lasts until its source ends or its timeout
expires (`task_updated` `killed`, then `task_notification` `stopped`), so it
outlives the turn while the agent waits for the user. caic marks its card as
a watch, which never keeps the task running.

When a background task that the parent spawned ends, Claude Code starts a
turn by itself to deliver the notification: `task_updated` (terminal
`patch.status`), `task_notification`, `background_tasks_changed`, then `init`
about 20 ms later, assistant output, and a `result` with `origin.kind:
"task-notification"`. In 1,585 local task logs, 67 such notifications arrived
while the parent was idle: 43 started a turn within 1 s, 21 were followed by
another notification first, and 3 by user input first. None ended without a
turn. The statuses were `local_agent` completed and `local_bash` completed,
failed, and stopped (a watch expiry). `turnQueue` in `native_subagent.go`
owns this mapping, and every background command card sets `WakesAgent`.

A notification that arrives during a parent turn joins its next model request.
When no root `tool_result` user record follows the terminal record before the
root result, Claude Code starts a notification turn about 30 ms after the
result: 5 of 5 such results in 116 local Claude task logs. None of the 63
results with such a record, TaskStop kills included, was followed by a turn
within 2 s. The result itself does not distinguish them
(`queued_turn_count` is 0); `turnQueue` sets `ResultMessage.TurnQueued`.

A task that a subagent spawned notifies that subagent. Its task records reach
the parent stream without `parent_tool_use_id`, and newer versions add
`owned_by_subagent` and `parent_task_id` to `task_started`. In one local
log, the expiry of such a watch resumed its subagent and started no
parent turn.

`system/background_tasks_changed` lists every live background task:
`local_bash` commands and watches, background `local_agent` runs, and tasks
that a subagent spawned (with `parent_task_id`). In 106 local Claude task logs
(2.1.270 to 2.1.325), no snapshot listed an agent that stayed in the
foreground; 3 agents were listed when moved to the background. No entry set
`ambient`. Each of 294 listed tasks appeared in a snapshot before its
`task_started`, and a settled agent can stay listed. Claude Code 2.1.295 and
later sent the terminal `task_updated` before the snapshot that dropped the
task (267 of 267). Versions 2.1.278 to 2.1.289 sent that snapshot one record
before it (21 of 21). No dropped task lacked a terminal record, and a replay
of the same logs retires no card. `liveTasks` in `native_subagent.go` owns
this mapping.

Claude Code plugins:
- https://code.claude.com/docs/en/plugins: plugin creation, `--plugin-dir`, plugin structure overview
- https://code.claude.com/docs/en/plugins-reference: full schema for plugin.json, MCP/LSP/hooks config, debugging
- https://code.claude.com/docs/en/mcp: MCP server configuration, plugin MCP servers, `${CLAUDE_PLUGIN_ROOT}` variable
- https://code.claude.com/docs/en/skills: skill authoring (SKILL.md format, frontmatter, progressive disclosure)

## Prompt Cache Controls

Claude Code divides requests into a main-conversation bucket and an auxiliary
bucket that includes subagents, forks, compaction, titles, and other helper
requests. Subscription main conversations default to a one-hour cache. The
auxiliary bucket defaults to five minutes, except that server-controlled helper
requests use one hour on a subscription. API-key, cloud-provider, and
usage-credit requests default to five minutes in both buckets. A named subagent
starts a separate cache prefix; a fork can read the parent's existing prefix on
its first request, but the fork's cache writes use the auxiliary bucket.

Claude Code 2.1.242 and later accepts only `5m` or `1h` for the direct TTL
controls. Their documented precedence is:

1. `FORCE_PROMPT_CACHING_5M=1` forces both buckets to five minutes.
2. `CLAUDE_CODE_PROMPT_CACHE_TTL` controls the main bucket, and
   `CLAUDE_CODE_SUBAGENT_PROMPT_CACHE_TTL` controls the auxiliary bucket.
3. The corresponding settings are `promptCacheTtl` and
   `subagentPromptCacheTtl`.
4. A subagent's `experimental.cacheTtl` frontmatter applies next on Claude Code
   2.1.248 and later. A `1h` value there is ignored while a subscription is
   using usage credits.
5. `ENABLE_PROMPT_CACHING_1H=1` selects one hour for both buckets.
6. Otherwise Claude Code applies the defaults above.

The older `ENABLE_PROMPT_CACHING_1H_BEDROCK` variable is deprecated but remains
supported according to the Claude Code changelog.

`DISABLE_PROMPT_CACHING` disables prompt caching globally. The model-specific
variants are `DISABLE_PROMPT_CACHING_HAIKU`,
`DISABLE_PROMPT_CACHING_SONNET`, `DISABLE_PROMPT_CACHING_OPUS`, and
`DISABLE_PROMPT_CACHING_FABLE`.

Authentication and provider selection can indirectly change the default TTL.
In particular, `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` selects non-plan
authentication, while `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, and
`CLAUDE_CODE_USE_FOUNDRY` select cloud providers. caic removes
`ANTHROPIC_API_KEY` when it supplies Claude subscription OAuth credentials; see
`claude.go`. `ANTHROPIC_BASE_URL` and `ANTHROPIC_BEDROCK_BASE_URL` are routing
controls, not TTL controls. A configured gateway can nevertheless affect
caching by forwarding, rejecting, or removing `cache_control` fields.

Do not infer an applied TTL from any setting, environment variable,
authentication method, or agent role. Populate `agent.Usage.CacheTTLSeconds`
only from the response's `cache_creation.ephemeral_5m_input_tokens` and
`cache_creation.ephemeral_1h_input_tokens` fields. A `message_delta` stream
event omits the top-level `cache_creation` and reports it per entry of
`iterations`.

## Widget Plugin

The `widget-plugin/` directory is a Claude Code plugin providing the
`show_widget` MCP tool and the widget design skill.

Key rules from the official docs:
- **Use `${CLAUDE_PLUGIN_ROOT}`** for all paths in `.mcp.json` and hooks.
  Hardcoded paths cause "MCP server fails" (documented common issue).
- `.mcp.json` at plugin root uses flat format (no `mcpServers` wrapper).
- `plugin.json` goes in `.claude-plugin/`; all other dirs (`skills/`, `commands/`, `agents/`) at plugin root.
- Plugin MCP tool naming: `mcp__plugin_<plugin-name>_<server-name>__<tool-name>`.
- **MCP stdio transport uses NDJSON** (newline-delimited JSON), NOT Content-Length
  framing. The server must read lines from stdin and write JSON lines to stdout.
- Haiku does not support Tool Search (`tool_reference` blocks). If tool search
  auto-enables (MCP tools >10% context), Haiku cannot discover deferred tools.

## Skill Reads

The `Skill` tool reports a load directly. Its `{"skill": "<name>", "args":
"..."}` input becomes a `SkillReadMessage` carrying the tool use ID, and the
wire layer suppresses the paired tool result, so neither appears in the
transcript. A block naming no skill still yields a message, so the wire layer
can track the ID; it drops the read itself. The read feeds usage analytics at
the invocation, whatever the call's outcome.

After a successful call, Claude Code 2.1 injects a user record with
`isSynthetic: true` whose text starts with `Base directory for this skill:
<dir>`. The wire layer emits a `SkillLoadMessage` on the record after the
successful result, with path `<dir>/SKILL.md` when that record is the
injection and no path otherwise. A failed call (`is_error`) loads nothing.

A subagent's assistant and user records carry `parent_tool_use_id`, the
delegating tool use. Its `Skill` calls and file reads keep their analytics
read but load nothing into the main context. A `role: user` record's inline
`tool_result.tool_use_id` pairs with the child's own invocation; the enclosing
`parent_tool_use_id` identifies delegation ownership, not the completed tool.
Child prompts remain scoped user messages. Role-less direct result bodies keep
their top-level tool reference without claiming delegation ownership.

Assistant, user, and stream-event DTOs expose session and delegation metadata;
control requests expose an optional `agent_id` and their tool use ID. The
recorded AskUserQuestion control follows its assistant tool invocation. These
fields do not expose a complete turn/item tuple: do not manufacture one from
UUIDs or content-block positions. Ownership and control correlation live in
`parse.go` and `claude.go`.

The model can also open a skill as an ordinary file. `parse.go` infers a read
and a pending load from `Read` (`file_path`) and `Bash` (`command`). Those
tool calls stay in the timeline, and their inferred read carries no tool use
ID so it suppresses nothing.

## Not Yet Exposed

`control_request` subtypes Claude Code accepts that caic does not send. caic
compacts with a `/compact` user message and resumes with the `--resume` flag
instead.

- Interrupt the running turn — `interrupt`
- Switch model mid-session — `set_model`
- Switch permission mode — `set_permission_mode`
- Cap thinking tokens — `set_max_thinking_tokens`
- Report context-window usage — `get_context_usage` (`raw_max_tokens`)
- Report session cost — `get_session_cost`
- Report account usage — `get_usage`
- List models — `list_models`; this is the only per-model discovery the CLI
offers, since caic has no Claude `ModelFetcher` and its inventory is the
hardcoded alias list in `claude.go`
- Rewind files — `rewind_files`
- Cancel a queued async message — `cancel_async_message`

caic answers the CLI's own `can_use_tool` requests and sends no other subtype.
Claude Code blocks on each request until it reads the reply, including a
subagent's. Recorded subagent requests (Claude Code 2.1, `Bash`, `WebFetch`,
`WebSearch`) carry `request.agent_id` and no top-level `session_id` or
`parent_tool_use_id`. No recorded log holds a subagent `AskUserQuestion`
request.
