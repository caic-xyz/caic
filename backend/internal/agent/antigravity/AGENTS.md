# Antigravity CLI Backend

caic runs `agy acp --dangerously-skip-permissions` over shared ACP
([`../acp/`](../acp/)). Wire DTOs belong to `github.com/maruel/genai/acp`.
Keep agy-specific interpretation here; shared ACP must not depend on this
package.

## ACP evidence

`testdata/*.jsonl` are `record-trace --local` recordings with
`gemini-3.8-flash`: read/edit/command, a failed read, two prompts in one
session, and a resumed process with agy 1.5.9, and a native delegation and a
compaction with agy 1.5.14 at low effort. The delegation recording continued
past the prompt response to the parent's wake-up. They ran with HOME holding only
`~/.gemini/antigravity-cli/antigravity-oauth-token`, so no user instructions
or skills reached the model. The subscription tier in the `authenticate` and
`session/new` results is replaced with `Example Tier`. `record-trace` owns
regeneration; `export_golden_test.go` owns the `.md` goldens.

caic supports only `oauth-personal`; `oauth-business` and API-key sign-in are
out of scope. agy reports `agentInfo.version` as its release. Without an
`authenticate` request, ACP prefers `GEMINI_API_KEY` over the CLI's Google
sign-in and offers the Gemini API catalog; `oauth-personal` selects the
sign-in, whose `model` values match the base IDs `sortModels` derives from `agy
models`. `thought_level` holds the selected model's efforts. `session/resume`
echoes `sessionId`, replays no transcript, and resets the model to agy's
default, so caic selects it again. The ACP session ID is the conversation ID
under `~/.gemini/antigravity-cli/conversations`, so a stream-json conversation
resumes over ACP. agy starts ACP-registered MCP servers at `session/new` and
opens them with `server/discover`.

An md container receives the Google sign-in only as
`~/.gemini/antigravity-cli/antigravity-oauth-token`, which md shares with the
host. Without that file, `agy models` exits 1 and writes "Please sign in" to
stderr only, and `authenticate` with `oauth-personal` fails with JSON-RPC error
-32603. A `GEMINI_API_KEY` in the container does not replace the sign-in.

agy 1.5.14 stores the token in the OS keyring unless a detector selects the
file: an SSH variable (`SSH_TTY`, `SSH_CLIENT`, `SSH_CONNECTION`), a container
(`/.dockerenv`, or `docker`, `containerd` or `kubepods` in `/proc/1/cgroup`),
no usable D-Bus session bus, Termux, or WSL. Each keyring save, such as an
hourly refresh, deletes the token file, so one host run in keyring mode signs
out every container. In the disassembly, `saveTokenWithExistingFileState`
calls `purgeTokenStorage` on the file storage after `writeKeyring` succeeds.
The quota fetcher in `internal/usage` sets `SSH_CONNECTION` for this reason,
and genai's antigravity provider sets it by default. agy reads the SSH
variables elsewhere only for interactive sign-in, the clipboard, and terminal
detection.

agy echoes each live prompt as `user_message_chunk`; shared ACP drops it. Tool
calls start `in_progress` with `rawInput` and agy's `name` and `args`
extensions, and end `completed` with `rawOutput.output` or `failed` with no
error detail. agy 1.5.14 can announce a failed call as a `tool_call` with
status `failed` and send no update. It adds a required `toolSummary` argument
to each tool and removes it before the MCP `tools/call`. A prompt that asks for
empty arguments can make the model omit it. agy then rejects the call before it
announces it and reports the error as agent text. `tools.go` maps `Path`,
`CommandLine`, `Target`, and `Replacement` to the shared ACP input keys, adds
the completion output, and infers skill reads from
`~/.gemini/antigravity-cli/skills`. Prompt responses carry turn usage, which
caic prices at base rates; `usage_update` reports context occupancy only. A
probe confirmed native image prompts. An invalid API key reports its error as
an `agent_message_chunk` ending in `end_turn`.

`live_test.go` covers tools, task MCP, SSH reconnect, and resume. It passed in
an md container with agy 1.5.14, `gemini-3.8-flash`, and an invalid
`GEMINI_API_KEY` in the environment. The mounted `~/.gemini/AGENTS.md` reaches
the model, so a turn can include a failed read of a missing workspace
`AGENTS.md`. `cmd/caic/harness_smoke_test.go` runs agy tasks through the
server with `CAIC_SMOKE_HARNESS=antigravity`.

## Compaction

agy advertises `compact` in `availableCommands`, and caic sends `/compact` as a
`session/prompt`. In `testdata/compact.jsonl`, a first prompt runs `seq 1
3000` and ends with `usage_update` `used` 16925. During `/compact`, agy sends
no prompt echo, text, or tool update; one `usage_update` reports 18129, and the
response is `end_turn` with usage 2951 input, 14879 cached read, and 299
output tokens. These figures are the summarization request, not the
compacted context: `used` equals the response's `totalTokens`. The next
prompt reports `used` 9134 with no cached read, and its answer recalls the
marker from the first prompt. Shared ACP adds the compaction boundary before
the `/compact` result, so caic's context meter is unknown from there until the
next prompt reports the compacted size.

Each prompt response prices its own turn, so the compaction turn adds the
summarization call's cost once. agy's changelog in the 1.5.14 binary says
`/compact <message>` compacts and then answers the message, so text after the
command is a prompt, not summary instructions; caic rejects custom compaction
instructions.

## Native subagents

agy delegates with its `invoke_subagent` tool. In
`testdata/native-subagent-joke.jsonl`, ACP reports it as one `tool_call` with
`name` `invoke_subagent`, kind `other`, title `Run invoke_subagent`, and
`rawInput` holding `Prompt`, `Role`, and `TypeName`. A `completed` update
without `rawOutput` follows within 20 ms: it acknowledges the spawn, and the
child runs asynchronously. No `session/update` carries the child's session ID,
tools, end, or report. agy delivers each report to the parent outside ACP and
runs a parent step for it; that step's text is the parent model's own.

Task logs show two orderings. With one child, the parent ended its turn while
the child ran; when the child reported, agy ran a parent step without a
`session/prompt` that streamed `agent_message_chunk` text and a
`usage_update` and sent no prompt response. With 33 children, the parent
stayed in its turn: reports arriving in batches started further steps ("25 of
33 subagents have reported back"), and the turn ended after the last report.

ACP cannot tell these apart, so `tools.go` settles the cards when the
spawning prompt's response arrives, as dsh does: `end_turn` completes them,
`cancelled` interrupts them, and any other end retires them as unknown. The
card takes the `Role` label, `TypeName` kind, and `Prompt`, and its ID derives
from the tool call ID. It holds no result; the parent's text stays in the
transcript. In the first ordering, the card completes before its child
reports, and the parent's step is a woken turn; see below. That step reports no
token usage, so caic does not price it. A failed `invoke_subagent`
call fails its card.

`testdata/evidence/native-subagent-joke-v3.jsonl` minimizes that recording to
the `session/prompt` request, the spawn, its acknowledgment, and the prompt
response. The request lets the wire correlate the response; the smoke replay
sends it as the task's prompt.

## Background commands and woken turns

`run_command` takes a required `WaitMsBeforeAsync` (500 to 10000 ms): a
command still running after it continues in the background. ACP reports no
such transition; the call stays `in_progress`. agy's system prompt tells the
model that "the system will automatically notify you with a message when the
command finishes", and its changelog fixes headless runs "exiting before the
agent could respond to a background command that finished after its first
turn". `IsDaemon` marks a command not expected to end, such as a dev server.

In a task log, the model started `make verify && make test` with
`WaitMsBeforeAsync` 10000, said it would report back, and the prompt returned
`end_turn` 18 s later. 13 s after that, a `tool_call_update` completed the
command with `Command exited with code 0.` in `rawOutput.output`. agy then ran
model calls without a `session/prompt`: one ran `git status`, one streamed the
report, and no prompt response followed.

In another task log, such a woken model call started `make fix && make build
&& make verify && make test` with `WaitMsBeforeAsync` 10000, streamed "waiting
for completion", and ended with a `usage_update` 14 s later while the command
still ran.

`tools.go` therefore turns each `run_command` still running at `end_turn`, or
at the end of a woken turn, into a background command bound to its tool call,
whose end wakes the agent unless `IsDaemon` is set. Its completion settles the card with the reported exit code
and attaches the tool output and result to the launching row, using the
session and call ID. After another prompt end, no card is kept.

A step agy runs without a prompt, after a background command ends or a child
reports, is a turn: it starts before the step's first activity. Each model call
ends with a `usage_update`; tool calls follow it when the model requested any.
Across the ACP recordings and task logs, 25 thought-only calls were followed
by a tool call, and 19 calls that streamed text were followed by the prompt
response, a new prompt, the end of the log, or another woken step, never by
a tool call. The woken turn therefore ends at the `usage_update` of a call
that streamed text. A prompt response ends the turn too.

## Stream-json history

Tasks that ran before ACP recorded agy's print-mode NDJSON. `streamjson.go`
keeps them readable, and the backend routes each record by its top-level
`jsonrpc` member. A task's log never returns to stream-json after its first ACP
record. The reader is permanent: Versioned Contracts' dated removal does not
apply to it. caic encodes prompts only over ACP, so a stream-json process
adopted across the upgrade rejects prompts; reviving the task resumes its
conversation over ACP.

Use `github.com/maruel/genai/providers/antigravity` for stream-json DTOs. The
DTOs and `~/src/genai/providers/antigravity/AGENTS.md` describe agy 1.5.9. A
failed tool step ends in state `ERROR` with `tool_info.error` set and never
reaches `DONE`.

`testdata/turn.ndjson` comes from genai's `GenSync_session_turn1.ndjson`.
The session ID and temporary workspace path are sanitized. It pins init,
streamed text, DONE-step usage, and the terminal result.
`testdata/cached-tools.ndjson` is a minimized recording with sanitized session
IDs and workspace paths. Its init event omits the model. Keep that absence
intact.

Step activity retains its actual conversation ID and step index. A differing
conversation ID alone does not prove a child. The DTO's explicit
`subagent_info.subagents[].conversation_id` can establish a relationship to
the initialized root, but does not establish native lifecycle statuses or a
canonical card ID. Tests using that DTO shape are synthetic protocol cases,
not recorded lifecycle evidence.

agy reports `init` once per process. Each turn opens with one root
`user_input` step, which reports a turn start (`TurnStartedMessage`), and ends
at the root result. A prompt delivered during a turn gets its own turn right
after the result: 22 of 23 such results in local logs; an exit followed the
other. The result then sets `ResultMessage.TurnContinues`.

Result usage, duration, and turn count include the entire conversation,
including resumed processes. Sum DONE-step usage for per-turn totals. Each
DONE-step usage is `PerCall`: agy sets usage once per model call, so caic
prices it at the vendor's API rates while the result's conversation-wide usage
stays out of the estimate.

Stream-json input accepted only `text` blocks, so prompt images were sent as a
text block naming the attachment path; its `caic_image` field
(`data/v1/prompt.go`) keeps the image for history. Older records carry images as
text data URLs.

Run `go run ./backend/internal/cmd/check-agent-logs` for offline DTO drift
checks of both protocols. Live parsers accept unknown fields.
