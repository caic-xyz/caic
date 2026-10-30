// Tests root ownership from explicit server relationships, ambiguity observations, and global quota exceptions.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { EventMessage, HistoryRoute } from "@sdk/types.gen";
import { displayedAtRoot, historyOwner } from "./historyRouting";
describe("history routing", () => {
  it("requires unique namespace-aware delegation and preserves unresolved root output", () => {
    const event: EventMessage = {
      kind: "text",
      ts: 1,
      source: { delegationToolUseID: "spawn", threadID: "one" },
      text: { text: "body" },
    };
    assert.equal(historyOwner(event, []), null);
    assert.equal(displayedAtRoot(event, []), true);
    const facts: HistoryRoute[] = [
      { toolUseID: "spawn", namespace: "one", agentID: "child-a" },
      { toolUseID: "spawn", namespace: "two", agentID: "child-b" },
    ];
    assert.equal(historyOwner(event, facts), "child-a");
    assert.equal(displayedAtRoot(event, facts), false);
    assert.equal(historyOwner({ ...event, source: { delegationToolUseID: "spawn" } }, facts), null);
    assert.equal(
      historyOwner(event, [
        { toolUseID: "spawn", namespace: "", agentID: "child" },
        { toolUseID: "spawn", namespace: "one" },
        { toolUseID: "spawn", namespace: "two" },
      ]),
      null,
    );
  });
  it("keeps global quota observations at root while thread-goal quotas stay with children", () => {
    const event: EventMessage = {
      kind: "rateLimit",
      ts: 1,
      source: { agentID: "child" },
      rateLimit: { status: "rejected", rateLimitType: "five_hour", utilization: 1 },
    };
    assert.equal(displayedAtRoot(event, []), true);
    assert.ok(event.rateLimit);
    assert.equal(
      displayedAtRoot({ ...event, rateLimit: { ...event.rateLimit, rateLimitType: "thread_goal" } }, []),
      false,
    );
  });
});
