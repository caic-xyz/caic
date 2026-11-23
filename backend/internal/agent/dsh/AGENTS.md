# DeepSeek Harness ACP Backend

`dsh.go` owns the DeepSeek variant and remote model discovery. The backend uses
`dsh --profile acp`, `session/resume`, JSON-encoded provider/model route values,
and the `reasoning_effort` configuration option. Shared transport and metadata parsing live in `../acp/`. Native tool
projection belongs to this package; evidence from another harness must never
establish DeepSeek delegation. Compaction remains disabled until verified.
Prompt images go by path through shared ACP `ImagesByPath`; dsh `read_image`
rejects them on routes without image input, including `deepseek-v4-flash`.

`testdata/dsh-handshake.ndjson` retains the initialization and configuration
responses from genai's `providers/dsh/testdata/GenSync_session_turn1.ndjson`.
This proves native wire shape, not md container lifecycle.

`testdata/` retains complete production-relay recordings from
`@deepseek-ai/dsh` `0.2.2-rc.1`: read/edit/command execution, a failed read,
an explicit harmless sandbox escalation with native permission request ID `0`,
two prompts in one session, and process restart with `session/resume`.
`record-trace` owns regeneration and the corresponding exported discussion
goldens. The resume recording uses the same persisted workspace and session as
the two-turn recording, changes effort from `off` to `low`, recalls the earlier
marker, and receives no replayed transcript updates. Its response omits the
session ID, so the correlated resume request remains authoritative.

`model-flash` and `model-pro` retain explicit native model selections using
the `session-turns` recording scenario, followed by requested `low` and `max`
efforts respectively. Each selection returns complete model and effort
controls. Both selected routes advertise `off`, `low`, `high`, and `max`;
the default-route recording independently retains its own effort catalog.
Grouped model choices contain routes, not selectable group headings. The
shipped `deepseek-v4-flash` default is added to the model choices while current
but disappears after changing to an actual catalog route; the live catalog
offers `deepseek-flash` and `deepseek-v4-pro`. Preserve those actual route IDs.

Bash skill reads use the native `command` field and the shared ACP per-call
deduplication. The native `skill` tool reports the skill name in its `rawInput`;
a successful completion confirms the inferred load. The `read-edit-bash`
recording retains that tool call and its completion content.

The native `subagent` tool names a delegated task in `description` and
`prompt`, then returns `started subagent <name>` on completion. Project that as
a running foreground native card; the generic tool call is machinery. dsh emits
no child start or end update and projects only the root session over ACP. The
prompt response settles the prompt's cards: `end_turn` completes them, and any
other stop reason, including `cancelled`, or an error retires them as unknown.
A child outlives a cancelled prompt and its end reaches no ACP record. The
child's result is not an ACP record either, so it cannot be attached to the
card. The parent model sees that result, but the journal does not.
The card ID is `dsh:subagent:<sessionId>/<name>`. A name is unique only within
one dsh session, and a cleared context starts a new session whose first child
is `agent-1` again. `testdata/subagent.jsonl` trims and anonymizes records of
a local dsh task (not a `record-trace` recording) from a dsh that predates
names, so its spawn output carries the child's Session ID.
`testdata/native-subagent-joke.jsonl` records the standardized delegation with
dsh 0.2.2-rc.5, which then waits with `wait_agent`. That tool
waits for any input to the caller's inbox, names no child, and returns only
`{"timedOut":false}`, so the parser drops it like the spawn call.
`testdata/evidence/native-subagent-joke-v3.jsonl` minimizes it to the
`session/prompt` request, the spawn, and the prompt response. The request lets
the wire correlate the response; the smoke replay sends it as the task's prompt.

Upstream follow-up: dsh should emit child subagent start and end session
updates over ACP so each card reports its own end.

Successful read and command output arrives only in terminal tool updates;
failed tool updates carry their error in content and still precede a normal
terminal prompt response. Native permissions select an advertised option ID
and retain the numeric request ID, including zero. These recordings contain
context occupancy and window size but no billed usage or native costs.
Strict direction-specific validation accepts every record against the pinned
genai ACP schemas. This evidence establishes local protocol behavior; it does
not establish md container lifecycle acceptance.

`live_test.go` provides opt-in paid checks against a disposable md SSH
target: native remote discovery and cache refresh, tools and permissions,
task-scoped MCP, an SSH disconnect during a running command, persisted
conversation resume, live/restored event uniqueness, and failed-startup cleanup.
ACP MCP registration uses an absolute `/usr/bin/env` executable because dsh
rejects a relative command. A failed handshake must explicitly stop its relay
daemon; killing the SSH attachment alone deliberately leaves the daemon alive.
Discovery launches from `/`, matching its inventory session workspace. Dsh
reads the process working directory's `.env` before ACP initialization and
rejects launch-control keys such as `DSH_HOME` there; md's `~/.env` remains the
shell-owned environment source rather than a dsh project configuration file.

[`harness_smoke_test.go`](../../../cmd/caic/harness_smoke_test.go) runs dsh
through the server with `CAIC_SMOKE_HARNESS=dsh` and
`CAIC_SMOKE_HARNESS_ENV=DEEPSEEK_API_KEY`.

No dsh work outlives a turn in the local logs. The 3 local dsh logs hold 24
prompts, all answered with `end_turn`, and no `session/update` outside a
prompt. An `end_turn` prompt settles only after the root agent and its
subagent tree are idle. A cancelled prompt does not wait for the tree; a
child's later completion can start root work outside any prompt, which no
recording shows yet. In one local task, the `subagent` call started the child
at record 1142, the parent reported its completion at 1147, and the prompt
response followed at 1152.

`bash` with `run_in_background` (1 call) returns `started background job
<id>`, and dsh moves a foreground command past its timeout to a background
job. `job_kill` (1 call) returns only `requested cancellation of job <id>`.
No ACP record reports a job's end, so caic does not map jobs. The recorded job
started and was killed inside one prompt.

## Ground every native DTO in dsh source

Each dsh record shape, field, tool name, output string, and lifecycle rule
that this package reads must cite the dsh source of the recorded version.
Recordings show one run; the source states the contract, such as which
values a field can take and when an update is sent. Clone the source and
check out the tag of the version under test, outside this repository:

```bash
git clone https://github.com/deepseek-ai/deepseek-harness
git -C deepseek-harness checkout dsh-v0.2.2-rc.5
```

Cite the file path, relative to the deepseek-harness root, in the code comment
or in this file. Do not infer a field's meaning from recordings, documentation
summaries, or another harness. The facts below are from `dsh-v0.2.2-rc.5`:

| Fact | Source |
| --- | --- |
| `tool_call` title is the tool name, `kind` is `other`, `rawInput` is the parsed arguments | `packages/acp/acp/src/updates.ts` |
| `subagent` takes `description`, `prompt`, and an optional `name`; it outputs `started subagent <name>` | `packages/subagent/tool-subagent/src/index.ts` |
| A name is `agent-N` or the model's choice, unique among one parent session's children | `packages/subagent/subagent/src/names.ts`, `catalog.ts` |
| A child from before names reports its Session ID as its name | `agentAddress` in `packages/subagent/subagent/src/names.ts` |
| `wait_agent` waits for any inbox input and returns only `{"timedOut":…}` | `packages/subagent/tool-subagent-control/src/wait-agent.ts` |
| `end_turn` waits for the subagent tree; `cancelled` stops only the root | `settleAfterQuiescence` in `packages/acp/acp/src/session.ts`, `packages/acp/acp/README.md` |

The genai DTOs in https://github.com/maruel/genai/tree/main/providers/dsh
must agree with that source.
