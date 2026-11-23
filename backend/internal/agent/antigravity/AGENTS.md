# Antigravity CLI Backend

caic runs `agy acp --dangerously-skip-permissions` over shared ACP
([`../acp/`](../acp/)). Wire DTOs belong to `github.com/maruel/genai/acp`.
Keep agy-specific interpretation here; shared ACP must not depend on this
package.

## ACP evidence

`testdata/*.jsonl` are `record-trace --local` recordings with
`gemini-3.8-flash`: read/edit/command, a failed read, two prompts in one
session, and a resumed process with agy 1.5.9, and a native delegation and a
compaction with agy 1.5.14 at low effort. They ran with HOME holding only
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
`rawInput` holding `Prompt`, `Role`, `TypeName`, and `Model`. A `completed`
update without `rawOutput` follows 9 ms later. That completion acknowledges the
spawn: agy's step output, which it keeps outside ACP, names the child
conversation and says the child "will send you a message when it has
completed". The child runs asynchronously and reports with its `send_message`
tool; agy adds the report to the parent conversation as a system-message step
that ACP does not emit. No `session/update` carries the child's session ID,
tools, or end.

caic does not map this delegation to a native-subagent card. The recording
supplies an identity (the tool call ID), a label (`Role`), and a prompt, but no
child status: a card completed by the spawn acknowledgment invents an outcome,
and a running card never settles. The delegation stays a plain tool call, and
`record-trace` accepts that call as the `native-subagent-joke` delegation.
Revisit when agy's ACP output reports the child's lifecycle.

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
