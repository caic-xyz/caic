# DeepSeek Harness ACP Backend

`dsh.go` owns the DeepSeek variant and remote model discovery. `New` uses
`dsh --profile acp`, `session/resume`, JSON-encoded provider/model route values,
and the `reasoning_effort` configuration option. Shared transport and metadata parsing live in `../acp/`. Native tool
projection belongs to this package; evidence from another harness must never
establish DeepSeek delegation. Image prompts and compaction remain disabled until verified.

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
`prompt`, then returns `started subagent <id>` on completion. Project that as a
native card with unknown status and `Background: true`; the generic tool call
is machinery. The child's later result is not an ACP record, so it cannot be
attached to the card. The parent model sees that result, but the journal does
not.

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

[`dsh_smoke_test.go`](../../../cmd/caic/dsh_smoke_test.go) exercises native
server task history, task-scoped MCP, active relay adoption, imported recovery
after an unexpected container exit, deliberate stop and explicit revival,
and purge. Its container fork preserves the edited workspace while creating
an independent native session and retaining caic's source-task lineage.
The separate discovery scenario uses actual temporary md instances and custom
storage beneath the persistent `.dsh` mount, without inference prompts.

Authoritative protocol sources:
- https://github.com/deepseek-ai/deepseek-harness/tree/master/packages/acp/acp
- https://github.com/maruel/genai/tree/main/providers/dsh
