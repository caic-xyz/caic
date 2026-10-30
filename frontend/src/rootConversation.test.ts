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
