// Tests incremental root routing, completed context reuse, live boundaries, and reset/disposal ownership.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { EventMessage } from "@sdk/types.gen";
import { isChildEvent, SessionGrouper } from "./grouping";
import { RootConversationProjection } from "./rootConversation";

const keep = (event: EventMessage): boolean => !isChildEvent(event);
const init = (id: string, ts: number): EventMessage => ({
  kind: "init",
  ts,
  init: { sessionID: id, reportedModel: "m", agentVersion: "1", cwd: "/", harness: "codex" },
});
const text = (value: string, ts: number): EventMessage => ({ kind: "text", ts, text: { text: value } });
function result(ts: number): EventMessage {
  return {
    kind: "result",
    ts,
    result: {
      subtype: "success",
      isError: false,
      result: "",
      totalCostUSD: 0,
      duration: 1,
      durationAPI: 1,
      numTurns: 1,
      usage: {
        inputTokens: 0,
        outputTokens: 0,
        cacheCreationInputTokens: 0,
        cacheReadInputTokens: 0,
        reportedModel: "m",
      },
    },
  };
}

describe("RootConversationProjection", () => {
  it("matches cold grouping when boundaries and carried inputs arrive in separate batches", () => {
    for (const subtype of ["compact_boundary", "context_cleared"]) {
      const prompt: EventMessage = { kind: "userInput", ts: 4, userInput: { text: "next" } };
      const messages: EventMessage[] = [
        init("a", 1),
        text("old", 2),
        result(3),
        prompt,
        { kind: "system", ts: 5, system: { subtype } },
        init("b", 6),
        text("new", 7),
        result(8),
        init("b", 9),
        text("same context", 10),
        result(11),
      ];
      const projection = new RootConversationProjection();
      for (let end = 0; end <= messages.length; end++) {
        const prefix = messages.slice(0, end);
        const snapshot = projection.derive(prefix, keep, false);
        const split = prefix.findLastIndex((event) => event.kind === "result" && !isChildEvent(event)) + 1;
        assert.deepEqual(snapshot.sessions, new SessionGrouper().append(prefix.slice(0, split).filter(keep)));
      }
      const value = projection.derive(messages, keep, false);
      assert.equal(value.sessions.length, 2);
      assert.equal(value.sessions[1]?.boundaryEvent, messages[4]);
      assert.ok(
        value.sessions[1]?.turns.flatMap((turn) => turn.groups.flatMap((group) => group.events)).includes(prompt),
      );
    }
  });

  it("does not revisit closed contexts or the canonical prefix on later completions", () => {
    let reads = 0;
    const old: EventMessage = {
      kind: "text",
      ts: 2,
      get text() {
        reads++;
        return { text: "closed" };
      },
    };
    const messages = [
      init("a", 1),
      old,
      result(3),
      { kind: "system", ts: 4, system: { subtype: "compact_boundary" } } satisfies EventMessage,
      init("b", 5),
      text("current", 6),
      result(7),
    ];
    let routed = 0;
    const route = (event: EventMessage) => {
      routed++;
      return keep(event);
    };
    const projection = new RootConversationProjection();
    const first = projection.derive(messages, route, false);
    const closed = first.pastSessions;
    const session = closed[0];
    const previousReads = reads;
    const next = projection.derive([...messages, text("later", 8), result(9)], route, false);
    assert.equal(next.pastSessions, closed);
    assert.equal(next.sessions[0], session);
    assert.equal(reads, previousReads);
    assert.equal(routed, messages.length + 2);
    assert.equal(next.sessions[1]?.turns.length, 2);
  });

  it("retains context-wide tool joins across root results", () => {
    const source = { threadID: "root", turnID: "turn" };
    const use: EventMessage = {
      kind: "toolUse",
      ts: 2,
      source,
      toolUse: { toolUseID: "tool", name: "Bash", input: {} },
    };
    const finish: EventMessage = {
      kind: "toolResult",
      ts: 4,
      source,
      toolResult: { toolUseID: "tool", duration: 0.004 },
    };
    const first = [init("a", 1), use, result(3)];
    const projection = new RootConversationProjection();
    projection.derive(first, keep, false);
    const next = projection.derive([...first, finish, result(5)], keep, false);
    assert.equal(next.sessions[0]?.turns[0]?.groups[0]?.toolCalls[0]?.result, finish.toolResult);
    assert.equal(next.sessions[0]?.toolCount, 1);
  });

  it("keeps live compaction at its original position until a root result commits it", () => {
    const before = [init("a", 1), text("completed", 2), result(3)];
    const boundary: EventMessage = { kind: "system", ts: 4, system: { subtype: "compact_boundary" } };
    const projection = new RootConversationProjection();
    const first = projection.derive(before, keep, false);
    const current = text("live", 5);
    const live = projection.derive([...before, boundary, current], keep, false);
    assert.equal(live.sessions, first.sessions);
    assert.equal(live.pastSessions.length, 0);
    assert.equal(live.groups[0]?.events[0], boundary);
    const complete = projection.derive([...before, boundary, current, result(6)], keep, false);
    assert.equal(complete.pastSessions[0], first.sessions[0]);
    assert.equal(complete.sessions[1]?.boundaryEvent, boundary);
  });

  it("ignores child contexts/results and handles explicit rerouting of unchanged-length history", () => {
    const child = { agentID: "child" };
    const old = text("routed later", 2);
    const messages = [
      init("a", 1),
      old,
      { ...init("child", 3), source: child },
      { kind: "system", ts: 4, source: child, system: { subtype: "compact_boundary" } } satisfies EventMessage,
      { ...result(5), source: child },
      result(6),
    ];
    const projection = new RootConversationProjection();
    const first = projection.derive(messages, keep, false);
    assert.equal(first.sessions.length, 1);
    assert.equal(first.sessions[0]?.textCount, 1);
    const reset = projection.derive(messages, (event) => keep(event) && event !== old, true);
    assert.equal(reset.sessions.length, 1);
    assert.equal(reset.sessions[0]?.textCount, 0);
    assert.notEqual(reset.sessions, first.sessions);
  });

  it("seeds quota warning deduplication without replaying completed messages", () => {
    const warning = (utilization: number, ts: number): EventMessage => ({
      kind: "rateLimit",
      ts,
      rateLimit: { status: "allowed_warning", rateLimitType: "five_hour", utilization },
    });
    const projection = new RootConversationProjection();
    const prefix = [init("a", 1), warning(0.8, 2), result(3)];
    projection.derive(prefix, keep, false);
    const repeated = warning(0.804, 4);
    assert.equal(projection.derive([...prefix, repeated], keep, false).groups.length, 0);
    assert.equal(
      projection.derive([...prefix, repeated, warning(0.81, 5)], keep, false).groups[0]?.events[0]?.rateLimit
        ?.utilization,
      0.81,
    );
  });

  it("clears the current memo snapshot on disposal and starts a fresh projection", () => {
    const projection = new RootConversationProjection();
    const value = projection.derive([init("a", 1), text("old", 2), result(3)], keep, false);
    projection.clear();
    assert.deepEqual(value, { sessions: [], pastSessions: [], groups: [], boundary: null });
    const fresh = projection.derive([init("b", 4), text("new", 5)], keep, false);
    assert.equal(fresh.boundary?.init?.sessionID, "b");
    assert.equal(fresh.sessions.length, 0);
    assert.equal(fresh.groups[0]?.events[0]?.text?.text, "new");
  });
});
