// Tests single-context root grouping, append identity, ownership changes, and context disposal.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { EventMessage } from "@sdk/types.gen";
import { RootConversationProjection } from "./rootConversation";
const text = (value: string, ts: number): EventMessage => ({ kind: "text", ts, text: { text: value } });
const result = (ts: number): EventMessage => ({
  kind: "result",
  ts,
  result: {
    subtype: "success",
    isError: false,
    result: "",
    duration: 1,
    durationAPI: 1,
    numTurns: 1,
    totalCostUSD: 0,
    usage: {
      inputTokens: 1,
      outputTokens: 1,
      cacheCreationInputTokens: 0,
      cacheReadInputTokens: 0,
      reportedModel: "m",
    },
  },
});
describe("one current root context", () => {
  it("keeps all completed turns when an unresolved child init appears", () => {
    const projection = new RootConversationProjection();
    const init: EventMessage = {
      kind: "init",
      ts: 3,
      source: { delegationToolUseID: "unresolved" },
      init: { sessionID: "child", reportedModel: "m", agentVersion: "1", cwd: "/", harness: "claude" },
    };
    const value = projection.derive(
      [text("first", 1), result(2), init, text("second", 4), result(5)],
      () => true,
      false,
    );
    assert.equal(value.turns.length, 2);
    assert.equal(
      value.turns[0]?.groups.some((g) => g.events.some((e) => e.text?.text === "first")),
      true,
    );
  });
  it("keeps completed turns stable through ordinary live deltas", () => {
    const projection = new RootConversationProjection();
    const events = [text("complete", 1), result(2)];
    const first = projection.derive(events, () => true, false);
    events.push({ kind: "textDelta", ts: 3, textDelta: { text: "live" } });
    const next = projection.derive(events, () => true, false);
    assert.equal(next.turns, first.turns);
    assert.equal(
      next.groups.some((g) => g.events.some((e) => e.textDelta?.text === "live")),
      true,
    );
  });
  it("keeps a detached command in its original turn after the prompt returns", () => {
    const projection = new RootConversationProjection();
    const source = { threadID: "session" };
    const completion = result(8);
    assert.ok(completion.result);
    completion.source = source;
    completion.result.pendingToolUseIDs = ["verify"];
    const events: EventMessage[] = [
      { kind: "thinkingDelta", ts: 1, thinkingDelta: { text: "Think" } },
      {
        kind: "toolUse",
        ts: 2,
        source,
        toolUse: { toolUseID: "verify", name: "Bash", input: {}, inputTruncated: false, background: false },
      },
      { kind: "textDelta", ts: 3, textDelta: { text: "Testing…" } },
      { kind: "thinking", ts: 7, thinking: { text: "Think" } },
      completion,
    ];
    const first = projection.derive(events, () => true, false);
    assert.equal(first.turns[0]?.groups.flatMap((group) => group.toolCalls)[0]?.done, false);
    assert.equal(first.turns[0]?.textCount, 1);

    events.push(
      { kind: "toolOutputDelta", ts: 9, source, toolOutputDelta: { toolUseID: "verify", delta: "ok" } },
      { kind: "toolResult", ts: 10, source, toolResult: { toolUseID: "verify", duration: 0.008, error: "" } },
      text("Verification passed.", 11),
    );
    const next = projection.derive(events, () => true, false);
    assert.equal(next.turns.length, 1);
    const calls = next.turns[0]?.groups.flatMap((group) => group.toolCalls);
    assert.equal(calls?.length, 1);
    assert.equal(calls[0]?.done, true);
    assert.equal(calls[0]?.output, "ok");
    assert.deepEqual(
      next.groups.flatMap((group) => group.events).map((event) => event.text?.text),
      ["Verification passed."],
    );
    assert.deepEqual(
      next,
      new RootConversationProjection().derive(events, () => true, false),
    );

    events.push(
      {
        kind: "toolUse",
        ts: 12,
        source,
        toolUse: { toolUseID: "status", name: "Bash", input: {}, inputTruncated: false, background: false },
      },
      { kind: "toolResult", ts: 13, source, toolResult: { toolUseID: "status", duration: 0.001, error: "" } },
      result(14),
    );
    const second = projection.derive(events, () => true, false);
    const [firstTurn, secondTurn] = second.turns.map((turn) => turn.groups.find((group) => group.toolCalls.length > 0));
    assert.equal(firstTurn?.toolCalls[0]?.use.toolUseID, "verify");
    assert.equal(secondTurn?.toolCalls[0]?.use.toolUseID, "status");
    assert.notEqual(firstTurn?.messageKey, secondTurn?.messageKey);
  });
  it("reprojects ownership only when the server routing revision changes", () => {
    const projection = new RootConversationProjection();
    const event = text("pending correlation", 1);
    const events = [event, result(2)];
    assert.equal(projection.derive(events, () => true, false).turns[0]?.textCount, 1);
    const next = projection.derive(events, (e) => e !== event, true);
    assert.equal(next.turns[0]?.textCount, 0);
  });
  it("releases the old context on replacement and cleanup", () => {
    const projection = new RootConversationProjection();
    projection.derive([text("old", 1), result(2)], () => true, false);
    const next = projection.derive([text("new", 3)], () => true, true);
    assert.equal(next.turns.length, 0);
    assert.equal(next.groups[0]?.events[0]?.text?.text, "new");
    projection.clear();
    assert.deepEqual(next, { turns: [], groups: [] });
  });
});
