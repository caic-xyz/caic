# Codex CLI Agent Backend

Implements `agent.Backend` for OpenAI Codex CLI in app-server mode.
Translates Codex's JSON-RPC 2.0 wire protocol into normalized `agent.Message` types.

## Protocol

Codex CLI runs in **app-server mode** — a JSON-RPC 2.0 NDJSON protocol over stdin/stdout.

**Handshake sequence** (30s timeout):
1. `initialize` request → response
2. `initialized` notification
3. `model/list` request → response (populates model list dynamically)
4. `thread/start` or `thread/resume` → response with thread ID

**Prompt delivery**: `turn/start` JSON-RPC request with text + optional images as data URLs.

**Streaming events**: `item/agentMessage/delta`, `item/reasoning/summaryTextDelta`,
`item/commandExecution/outputDelta`, `item/mcpToolCall/progress`.

## Architecture

- `codex.go` — Backend lifecycle, handshake, `wireFormat` state machine
- `parse.go` — Stateless parser: JSON-RPC notifications → `agent.Message`
- `stream_trim.go` — Selects deltas a settled log elides

Wire types are provided by `github.com/maruel/genai/providers/codex` (imported as `cx`).

`wireFormat` wraps the stateless parser to accumulate per-turn token usage
from `thread/tokenUsage/updated` notifications, emitting a final `ResultMessage`
with totals on `turn/completed`.

## Settled stream trimming

`item/completed` repeats an `agentMessage`'s full `text`, a `reasoning`
item's full `summary`, and a `commandExecution`'s `aggregatedOutput`.
`stream_trim.go` selects `item/agentMessage/delta`,
`item/reasoning/summaryTextDelta`, and `item/commandExecution/outputDelta`
records whose `threadId` and `itemId` completed; terminal compression replaces
each with an `elided` record. Item IDs are unique within a thread across turns.

Codex streams `outputDelta` for some commands only. When a command completed
without any, the wire projects its `aggregatedOutput` as one output delta before
the result; across the local logs, 71,633 commands had output but no deltas,
against 17,066 whose deltas equal `aggregatedOutput`. Output deltas go only in
that equal case. In another 4,357 commands, `aggregatedOutput` adds a prefix the
deltas lack, so the projected output misses its beginning; those deltas stay.

Across 396 local Codex v3 logs, trimming elides 69% of the records and 10.3%
of the compressed bytes; the folded turn metrics, exit status, result text,
tool output, and every other message keep their values and record positions.

## Upstream Source

Wire type names in the genai package match the upstream Rust definitions:

- `codex-rs/app-server-protocol/src/protocol/v2.rs` — notification and item structs
- `codex-rs/app-server-protocol/src/protocol/common.rs` — method string ↔ struct mapping

When updating wire types, update `github.com/maruel/genai` and diff against
the upstream Rust definitions to find new fields, item types, or notification methods.

## Native subagent evidence

An earlier Codex CLI 0.154.0 recording from the same CLI contained only a
receiverless `wait`; the current recording contains both that wait and a completed
activity spawn, and the minimized fixture pins both.

The Codex CLI 0.154.0 recording uses the app-server transport, and it reports
delegation through two item types, both of which `native_subagent.go` maps:

- `subAgentActivity` (`started`, `interacted`, `interrupted`, `completed`) is
  how a spawn and its lifecycle are reported. Its identity is the
  `agentThreadId`, and `agentPath` names the agent (`/root/readme_joke`). Only
  `started` creates a card, and `agentPath: "/root"` is the parent thread, never
  a card: children interact with the root, and folding that interaction once
  produced a root card that never settled. The standardized recording shows
  `started` followed by `completed` for one agent.
- `collabAgentToolCall` covers the collaboration tools: a `spawnAgent` call with
  `receiverThreadIds` maps each receiver thread to a card, and `agentsStates`
  entries settle threads the session has already seen for `wait`, `sendInput`,
  `resumeAgent`, `closeAgent`, and similar calls. A `wait` with empty
  `receiverThreadIds` and empty `agentsStates`, which the recording also
  contains, is not evidence of anything and produces no card.

Neither item type names an agent role. Codex's `Thread` carries `agentRole`
for an `AgentControl`-spawned sub-agent, but caic receives only the root
thread's `thread/started`: all 404 in 403 local Codex logs, and their responses,
have `source: "vscode"` and a null `agentRole`. A Codex card therefore has no
kind.

A collaborative agent runs independently of the parent turn, so a `spawnAgent`
receiver or `started` activity marks its card `Background`; only such a card can
keep a task running after the parent result. A child thread answers for itself:
`thread/status/changed` maps `active` to running and `idle` to paused (resumable
after a later turn), `systemError` to failed, and a `turn/completed` whose status
is `failed` or `interrupted` is terminal. The wire knows the root thread ID, so a
child's `turn/completed` must not emit a parent result or end the parent turn.

Legacy multi-agent v1 traffic from the same CLI may report a spawn through
either type, and both use the agent thread ID as the canonical identity, so one
agent can never produce two cards. It is not an unsupported verdict: the matching
`rust-v0.154.0` source implements `spawn_agent` and collaboration tool events.
When present, preserve the tool name, sender thread ID, receiver thread IDs,
prompt, agent states, and item status. `wait`, `send_message`, and other
coordination tools are not spawns; a missing receiver or prompt must remain
unknown rather than being invented.

The authoritative mapping is
https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/app-server-protocol/src/protocol/event_mapping.rs:
it emits `spawn_agent` items from spawn begin/end events and separate `wait`,
send-input, close, and resume items.

## Background work

Codex runs every command as a unified-exec process: all 117,659 command items
in 403 local Codex logs have `source: unifiedExecStartup`, and 117,647 have a
`processId`.
The process can outlive its turn. Codex calls it a background terminal
(`thread/backgroundTerminals/list`) and stops it only at session shutdown, on
`thread/backgroundTerminals/clean`, which caic does not send, or by pruning.
An exit watcher sends the item's `item/completed` with the original `turnId`
when the process exits:
https://github.com/openai/codex/blob/rust-v0.169.0/codex-rs/core/src/unified_exec/async_watcher.rs
(`spawn_exit_watcher`). Relay replays excluded, the logs contain:

- 20 root-thread commands still running at `turn/completed` (14 completed and
  6 failed turns). 13 reported their exit 0.8 s to 18.5 h later; 7 ended with
  the host. Their 423 `outputDelta` records arrived after the turn. In one
  task, the final answer says the smoke suite still runs; its exit
  arrived 8 minutes later.
- 28 child-thread commands still running at the child's `turn/completed`: 22
  reported their exit, 9 of them by a signal (exit code 130, 137, or 143), and
  6 never did: 4 until the host exit and 2 in logs without an exit record.

`native_subagent.go` maps a root or child command that outlives its turn to
a background command, keyed by thread and item. caic opts out of
`item/commandExecution/terminalInteraction`, so no log contains one.

A root `turn/started` reports a turn start (`TurnStartedMessage`); a
compaction is a turn too. `codex.go` drops a child thread's start, which
cannot start the parent turn.

Codex starts no turn by itself. Of 3,168 root `turn/started` records, 3,162
have a `turn/start` response; the other 6 are `thread/compact/start`
compactions that caic requested. A process exit starts no turn, so the card
does not wake the agent and does not keep the task running: in one
task, a `sudo` password prompt stayed open across 48 turns.

## References

Source code:
- https://github.com/openai/codex
- https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs: prompt-cache key construction and Responses request assembly
- https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/common.rs: serialized Responses request fields
- https://github.com/openai/codex/tree/50fffd5ed367aa99491d9ec58575626fce4e9dd4: source revision inspected for the prompt-cache controls below

Documentation:
- https://developers.openai.com/codex/cli: CLI documentation
- https://developers.openai.com/codex/cli/reference: CLI reference
- https://developers.openai.com/api/docs/guides/prompt-caching: prompt-cache behavior and retention
- https://developers.openai.com/api/reference/cli/resources/responses/methods/create: prompt-cache options, retention, and usage fields
- https://community.openai.com/t/28-days-of-quality-of-life-improvements-and-or-a-full-reset/1403525/85: 12-hour default prompt caching in Codex

## Prompt Cache Controls

Current Codex sends `prompt_cache_key`, but it does not send
`prompt_cache_options` or `prompt_cache_retention`. Its current source has no
environment variable that directly controls the model prompt-cache TTL. The
OpenAI service therefore selects the default from the model, organization, and
data-retention policy. Authentication and endpoint environment variables may
change which account or provider handles a request, but they are not TTL
controls and must not be interpreted as an applied cache duration.

OpenAI announced on 2026-10-21 that Codex and ChatGPT Work enable 12-hour
prompt caching by default:
https://community.openai.com/t/28-days-of-quality-of-life-improvements-and-or-a-full-reset/1403525/85
The announcement is a service policy, not a per-response report.

Do not confuse the Codex model-catalog cache with model prompt caching.
`DEFAULT_MODEL_CACHE_TTL = 300` and the behavior changed by
https://github.com/openai/codex/commit/7cde2323f3712999e9ab98b16287e08b7735d52f
apply to Codex's local `/models` metadata cache, not to prompt tokens sent to a
model.

Codex's `thread/tokenUsage/updated` event reports cached and cache-write token
counts but not the service's applied retention policy or TTL. Leave
`agent.Usage.CacheTTLSeconds` unknown unless that wire protocol gains an
explicit applied-policy or duration field.

## Key Design Decisions

- **Upstream naming**: Go types mirror upstream Rust struct names (e.g. `ThreadStartedNotification`,
  not `ThreadStartedParams`) to simplify syncing with the Codex source.
- **Dynamic model list**: initial `["gpt-5.4"]` replaced after handshake with live list from `model/list`.
- **Error suppression**: notifications with `willRetry=true` are silently dropped.
- **Two-phase file changes**: tool name (`Write` vs `Edit`) determined by checking `kind.type=="add"`.
- **Widget plugin disabled**: TODO comment — needs fixing for Codex.
- **Opt-out capabilities**: handshake disables verbose notifications caic doesn't need
  (e.g., `turn/diff/updated`, `turn/plan/updated`).

## Skill Reads

Codex has neither a Skill tool nor a read tool. It opens a skill by shelling
out, so `parse.go` infers a read from the `commandExecution` item's `command` and `cwd`:

```
sed -n '1,240p' /home/user/.agents/skills/code-quality/SKILL.md
```

Relative shell operands resolve against the recorded `cwd`. Directory changes
and shell expansion omit an unresolved path while preserving the skill name.

One command often loads several skills. Codex also re-reads a skill on every
turn, 4.8 times per skill per task across the retained task-log cache and 50
times in the worst one, which is why the day aggregate counts tasks rather than
reads.

A spawned child thread streams its items on the root session's wire, each
`item/*` notification naming its `threadId`. A child's skill read keeps its
analytics read, but once `thread/started` names the root thread the wire layer
drops the child's load: it is not in the root thread's context.

`skills/changed` carries empty params and only invalidates the catalog.
`TurnInputTypeSkill` is a client-to-Codex input type, and caic sends plain text
prompts. Neither proves a load.

## Not Yet Exposed

JSON-RPC requests the app-server accepts that caic does not send. caic already
resumes with `thread/resume` and compacts with `thread/compact/start`.

- Interrupt the running turn — `turn/interrupt`
- Steer a running turn — `turn/steer`
- Roll back turns — `thread/rollback`
- Switch the model for a turn — `model` on `turn/start`; caic selects the model
  once at `thread/start` or `thread/resume`

Codex can also fork a thread and run a code review, but the pinned `genai` DTOs
model neither request — `ThreadStartParams` carries no fork field — so extending
the provider package comes before caic can send them.
